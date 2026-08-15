package media

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"
)

// fakeTun はテスト用の tun 代用。in に入れたものが Read で読め、
// Write されたものが out に流れる。
type fakeTun struct {
	in     chan []byte
	out    chan []byte
	closed chan struct{}
	once   sync.Once
}

func newFakeTun() *fakeTun {
	return &fakeTun{
		in:     make(chan []byte, 8),
		out:    make(chan []byte, 8),
		closed: make(chan struct{}),
	}
}

func (f *fakeTun) Read(p []byte) (int, error) {
	select {
	case b := <-f.in:
		return copy(p, b), nil
	case <-f.closed:
		return 0, net.ErrClosed
	}
}

func (f *fakeTun) Write(p []byte) (int, error) {
	b := make([]byte, len(p))
	copy(b, p)
	select {
	case f.out <- b:
		return len(p), nil
	case <-f.closed:
		return 0, net.ErrClosed
	}
}

func (f *fakeTun) Close() error {
	f.once.Do(func() { close(f.closed) })
	return nil
}

// ipv4Packet は最低限それらしい IPv4 パケットを作る (先頭ニブルが 4 で 20 バイト以上)。
func ipv4Packet(payload string) []byte {
	pkt := make([]byte, 20+len(payload))
	pkt[0] = 0x45
	copy(pkt[20:], payload)
	return pkt
}

type harness struct {
	tun  *fakeTun
	fwd  *Forwarder
	peer *net.UDPConn // 網内リレー役
	done chan error
}

func newHarness(t *testing.T, strictPort bool) *harness {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	peer, err := net.ListenUDP("udp6", net.UDPAddrFromAddrPort(netip.MustParseAddrPort("[::1]:0")))
	if err != nil {
		t.Fatalf("擬似リレーのソケットを開けません: %v", err)
	}
	conn, err := Dial(netip.MustParseAddrPort("[::1]:0"), "", log)
	if err != nil {
		t.Fatalf("メディアソケットを開けません: %v", err)
	}

	tun := newFakeTun()
	fwd := New(Config{
		Tun:            tun,
		Conn:           conn,
		Remote:         peer.LocalAddr().(*net.UDPAddr).AddrPort(),
		StrictPeerPort: strictPort,
		MTU:            1452,
		Log:            log,
	})

	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{tun: tun, fwd: fwd, peer: peer, done: make(chan error, 1)}
	go func() { h.done <- fwd.Run(ctx) }()

	t.Cleanup(func() {
		cancel()
		peer.Close()
		select {
		case <-h.done:
		case <-time.After(2 * time.Second):
			t.Error("Forwarder が停止しない")
		}
	})
	return h
}

func (h *harness) mediaAddr() netip.AddrPort {
	return h.fwd.cfg.Conn.LocalAddr().(*net.UDPAddr).AddrPort()
}

func TestForwarderBridgesBothDirections(t *testing.T) {
	h := newHarness(t, false)

	// tun → 網: 生 IP パケットが追加ヘッダなしでそのまま UDP ペイロードになる。
	sent := ipv4Packet("to-network")
	h.tun.in <- sent

	buf := make([]byte, 2048)
	h.peer.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err := h.peer.ReadFromUDPAddrPort(buf)
	if err != nil {
		t.Fatalf("擬似リレーが受信できません: %v", err)
	}
	if got := string(buf[:n]); got != string(sent) {
		t.Errorf("転送されたパケットが一致しない: % x", buf[:n])
	}

	// 網 → tun
	back := ipv4Packet("to-tun")
	if _, err := h.peer.WriteToUDPAddrPort(back, h.mediaAddr()); err != nil {
		t.Fatalf("擬似リレーから送信できません: %v", err)
	}
	select {
	case got := <-h.tun.out:
		if string(got) != string(back) {
			t.Errorf("tun へ書かれたパケットが一致しない: % x", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("tun へ書き込まれない")
	}

	stats := h.fwd.Stats()
	if stats.TxPackets != 1 || stats.RxPackets != 1 {
		t.Errorf("統計 = tx %d / rx %d, want 1 / 1", stats.TxPackets, stats.RxPackets)
	}
	if h.fwd.IdleFor() > time.Second {
		t.Errorf("通信直後なのに IdleFor が大きい: %s", h.fwd.IdleFor())
	}
}

func TestForwarderDropsPacketsFromUnexpectedPort(t *testing.T) {
	h := newHarness(t, false)

	// 先に正規のリレーから 1 発送って、送信元ポートを学習させる。
	if _, err := h.peer.WriteToUDPAddrPort(ipv4Packet("first"), h.mediaAddr()); err != nil {
		t.Fatalf("送信できません: %v", err)
	}
	select {
	case <-h.tun.out:
	case <-time.After(2 * time.Second):
		t.Fatal("最初のパケットが tun へ届かない")
	}

	// 別ソケット (＝別ポート) から送っても取り込まれないこと。
	other, err := net.ListenUDP("udp6", net.UDPAddrFromAddrPort(netip.MustParseAddrPort("[::1]:0")))
	if err != nil {
		t.Fatalf("別ソケットを開けません: %v", err)
	}
	defer other.Close()
	if _, err := other.WriteToUDPAddrPort(ipv4Packet("spoofed"), h.mediaAddr()); err != nil {
		t.Fatalf("送信できません: %v", err)
	}

	select {
	case got := <-h.tun.out:
		t.Fatalf("想定外の送信元のパケットが tun に書かれた: %q", got)
	case <-time.After(300 * time.Millisecond):
	}
	if got := h.fwd.Stats().DropUnexpectedSrc; got != 1 {
		t.Errorf("DropUnexpectedSrc = %d, want 1", got)
	}
}

func TestForwarderDropsMalformedPackets(t *testing.T) {
	h := newHarness(t, false)

	// IP パケットとして短すぎるもの、バージョンが 4/6 でないものは捨てる。
	short := []byte{0x45, 0x00, 0x00}
	if _, err := h.peer.WriteToUDPAddrPort(short, h.mediaAddr()); err != nil {
		t.Fatalf("送信できません: %v", err)
	}
	bogus := ipv4Packet("bad-version")
	bogus[0] = 0x35
	if _, err := h.peer.WriteToUDPAddrPort(bogus, h.mediaAddr()); err != nil {
		t.Fatalf("送信できません: %v", err)
	}

	select {
	case got := <-h.tun.out:
		t.Fatalf("壊れたパケットが tun に書かれた: % x", got)
	case <-time.After(300 * time.Millisecond):
	}
	if got := h.fwd.Stats().DropMalformed; got != 2 {
		t.Errorf("DropMalformed = %d, want 2", got)
	}
}
