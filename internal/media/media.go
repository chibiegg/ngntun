// Package media はデータコネクトのメディアフローと tun デバイスの橋渡しを行う。
//
// メディアの UDP ペイロードは、追加ヘッダなしの生 IP パケットそのもの。
// したがって tun から読んだバイト列をそのまま UDP で送り、UDP で受けたバイト列を
// そのまま tun へ書けばよい。
package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chibiegg/ngntun/internal/sockopt"
)

// maxDatagram は受信バッファ長。
const maxDatagram = 65535

// Stats は転送の統計。終了時のサマリとアイドル判定に使う。
type Stats struct {
	TxPackets uint64
	TxBytes   uint64
	RxPackets uint64
	RxBytes   uint64

	DropUnexpectedSrc uint64 // 想定外の送信元から届いたパケット
	DropMalformed     uint64 // IP パケットとして壊れているもの
	TunWriteErrors    uint64

	FirstTx time.Time
	LastTx  time.Time
	FirstRx time.Time
	LastRx  time.Time
}

// Tun は tun デバイスに求める操作。
type Tun interface {
	io.ReadWriteCloser
}

// Config は Forwarder の設定。
type Config struct {
	Tun    Tun
	Conn   *net.UDPConn
	Remote netip.AddrPort // SDP アンサーで指定された網内リレー

	// StrictPeerPort が true なら送信元ポートまで一致を要求する。
	// 既定では IP のみ照合し、ポートは最初に受けたものを学習する。
	StrictPeerPort bool

	MTU int
	Log *slog.Logger
}

// Forwarder は tun ⇔ メディアフローの双方向転送を行う。
type Forwarder struct {
	cfg Config
	log *slog.Logger

	mu    sync.Mutex
	stats Stats

	lastActivity atomic.Int64 // UnixNano
	learnedPort  atomic.Uint32
}

// Dial はメディア用の UDP ソケットを開く。DSCP は CS1 を付ける。
func Dial(local netip.AddrPort, device string, log *slog.Logger) (*net.UDPConn, error) {
	conn, err := net.ListenUDP("udp6", net.UDPAddrFromAddrPort(local))
	if err != nil {
		return nil, fmt.Errorf("メディア用ソケットを開けません (%s): %w", local, err)
	}
	if err := sockopt.BindToDevice(conn, device); err != nil {
		conn.Close()
		return nil, err
	}
	if err := sockopt.SetDSCP(conn, false, sockopt.DSCPCS1); err != nil {
		log.Warn("メディアソケットに DSCP を設定できませんでした", "err", err)
	}
	return conn, nil
}

// New は Forwarder を作る。
func New(cfg Config) *Forwarder {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.MTU <= 0 {
		cfg.MTU = 1452
	}
	f := &Forwarder{cfg: cfg, log: cfg.Log}
	f.touch()
	return f
}

// Run は双方向の転送を開始し、ctx の終了または致命的なエラーまでブロックする。
func (f *Forwarder) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// ctx が終わったら読み取りをブロックしている両者を叩き起こす。
	go func() {
		<-ctx.Done()
		f.cfg.Conn.Close()
		f.cfg.Tun.Close()
	}()

	errc := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		errc <- f.tunToNetwork(ctx)
	}()
	go func() {
		defer wg.Done()
		errc <- f.networkToTun(ctx)
	}()

	err := <-errc
	cancel()
	wg.Wait()

	if ctx.Err() != nil && isClosed(err) {
		return nil
	}
	return err
}

// tunToNetwork は tun から読んだ生 IP パケットをメディアフローへ送る。
func (f *Forwarder) tunToNetwork(ctx context.Context) error {
	buf := make([]byte, f.cfg.MTU+128)
	for {
		n, err := f.cfg.Tun.Read(buf)
		if err != nil {
			if ctx.Err() != nil || isClosed(err) {
				return nil
			}
			return fmt.Errorf("tun の読み取りに失敗しました: %w", err)
		}
		if n < 20 { // IPv4 ヘッダにも満たない
			f.countMalformed()
			continue
		}
		if _, err := f.cfg.Conn.WriteToUDPAddrPort(buf[:n], f.remote()); err != nil {
			if ctx.Err() != nil || isClosed(err) {
				return nil
			}
			return fmt.Errorf("メディアの送信に失敗しました: %w", err)
		}
		f.countTx(n)
	}
}

// networkToTun はメディアフローで受けた生 IP パケットを tun へ書く。
func (f *Forwarder) networkToTun(ctx context.Context) error {
	buf := make([]byte, maxDatagram)
	for {
		n, src, err := f.cfg.Conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if ctx.Err() != nil || isClosed(err) {
				return nil
			}
			return fmt.Errorf("メディアの受信に失敗しました: %w", err)
		}
		if !f.acceptSource(src) {
			f.countUnexpectedSrc(src)
			continue
		}
		if n < 20 || !validIPVersion(buf[0]) {
			f.countMalformed()
			continue
		}
		if _, err := f.cfg.Tun.Write(buf[:n]); err != nil {
			if ctx.Err() != nil || isClosed(err) {
				return nil
			}
			f.countTunWriteError()
			f.log.Warn("tun への書き込みに失敗しました", "err", err)
			continue
		}
		f.countRx(n)
	}
}

// acceptSource は受信パケットの送信元を検証する。
//
// 網内リレーが SDP と異なるポートから送ってくる可能性があるため、既定では
// 送信元 IP のみを照合し、ポートは最初に受けたものを学習して以降それに固定する。
func (f *Forwarder) acceptSource(src netip.AddrPort) bool {
	if src.Addr().Unmap() != f.cfg.Remote.Addr().Unmap() {
		return false
	}
	if f.cfg.StrictPeerPort {
		return src.Port() == f.cfg.Remote.Port()
	}
	learned := uint16(f.learnedPort.Load())
	if learned == 0 {
		f.learnedPort.Store(uint32(src.Port()))
		if src.Port() != f.cfg.Remote.Port() {
			f.log.Info("メディアの送信元ポートが SDP と異なります。以降このポートを相手とみなします",
				"sdp_port", f.cfg.Remote.Port(), "actual_port", src.Port())
		}
		return true
	}
	return src.Port() == learned
}

func (f *Forwarder) remote() netip.AddrPort { return f.cfg.Remote }

func validIPVersion(b byte) bool {
	v := b >> 4
	return v == 4 || v == 6
}

func (f *Forwarder) touch() { f.lastActivity.Store(time.Now().UnixNano()) }

// LastActivity は最後にどちらかの方向でパケットが流れた時刻を返す。
func (f *Forwarder) LastActivity() time.Time {
	return time.Unix(0, f.lastActivity.Load())
}

// IdleFor は最後の通信からの経過時間を返す。
func (f *Forwarder) IdleFor() time.Duration { return time.Since(f.LastActivity()) }

// Stats は現在の統計のコピーを返す。
func (f *Forwarder) Stats() Stats {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stats
}

func (f *Forwarder) countTx(n int) {
	now := time.Now()
	f.mu.Lock()
	f.stats.TxPackets++
	f.stats.TxBytes += uint64(n)
	if f.stats.FirstTx.IsZero() {
		f.stats.FirstTx = now
	}
	f.stats.LastTx = now
	f.mu.Unlock()
	f.touch()
}

func (f *Forwarder) countRx(n int) {
	now := time.Now()
	f.mu.Lock()
	f.stats.RxPackets++
	f.stats.RxBytes += uint64(n)
	if f.stats.FirstRx.IsZero() {
		f.stats.FirstRx = now
	}
	f.stats.LastRx = now
	f.mu.Unlock()
	f.touch()
}

func (f *Forwarder) countMalformed() {
	f.mu.Lock()
	f.stats.DropMalformed++
	f.mu.Unlock()
}

func (f *Forwarder) countTunWriteError() {
	f.mu.Lock()
	f.stats.TunWriteErrors++
	f.mu.Unlock()
}

func (f *Forwarder) countUnexpectedSrc(src netip.AddrPort) {
	f.mu.Lock()
	n := f.stats.DropUnexpectedSrc
	f.stats.DropUnexpectedSrc++
	f.mu.Unlock()
	if n == 0 {
		// 毎パケット出すと洪水になるので最初の 1 回だけ知らせる。
		f.log.Warn("想定外の送信元からメディアを受信しました (以降は統計のみ)",
			"src", src, "expected", f.cfg.Remote)
	}
}

func isClosed(err error) bool {
	return err == nil || errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed)
}
