package sip

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"

	"github.com/chibiegg/ngntun/internal/sockopt"
)

// maxDatagram は SIP メッセージの受信バッファ長。
const maxDatagram = 65535

// Transport は 1 本の UDP ソケット上の SIP トランスポート。
// ngntun は REGISTER 用の IPv4 と、呼制御用の IPv6 の 2 本を持つ。
type Transport struct {
	name string // ログ用の識別子 ("ipv4" / "ipv6")
	conn *net.UDPConn
	log  *slog.Logger
}

// NewTransport は指定アドレスに bind した SIP トランスポートを作る。
func NewTransport(name string, local netip.AddrPort, dscp int, device string, log *slog.Logger) (*Transport, error) {
	network := "udp6"
	if local.Addr().Is4() {
		network = "udp4"
	}
	conn, err := net.ListenUDP(network, net.UDPAddrFromAddrPort(local))
	if err != nil {
		return nil, fmt.Errorf("%s: %s を bind できません: %w", name, local, err)
	}
	if err := sockopt.BindToDevice(conn, device); err != nil {
		conn.Close()
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if err := sockopt.SetDSCP(conn, local.Addr().Is4(), dscp); err != nil {
		// DSCP が付かなくても通信自体は成立するため、警告に留めて続行する。
		log.Warn("SIP ソケットに DSCP を設定できませんでした", "transport", name, "err", err)
	}
	return &Transport{name: name, conn: conn, log: log}, nil
}

// LocalAddrPort は実際に bind されたアドレスとポートを返す。
func (t *Transport) LocalAddrPort() netip.AddrPort {
	if a, ok := t.conn.LocalAddr().(*net.UDPAddr); ok {
		return a.AddrPort()
	}
	return netip.AddrPort{}
}

// Serve は受信ループを回す。Close が呼ばれるまでブロックする。
//
// ctx が終了してもソケットは閉じない。シグナルで ctx がキャンセルされた直後に
// BYE と登録解除を送る必要があるためで、ソケットを閉じるのは UA.Close の役目。
func (t *Transport) Serve(ctx context.Context, handle func(*Message, netip.AddrPort)) {
	buf := make([]byte, maxDatagram)
	for {
		n, src, err := t.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			t.log.Warn("SIP の受信に失敗しました", "transport", t.name, "err", err)
			continue
		}
		raw := make([]byte, n)
		copy(raw, buf[:n])

		msg, err := ParseMessage(raw)
		if err != nil {
			t.log.Warn("SIP メッセージを解釈できませんでした", "transport", t.name, "src", src, "err", err)
			continue
		}
		t.log.Debug("SIP 受信", "transport", t.name, "src", src, "msg", msg.Summary())
		t.log.Log(ctx, levelTrace, "SIP 受信 (全文)", "transport", t.name, "src", src, "raw", string(raw))

		handle(msg, src)
	}
}

// Send は 1 通のメッセージを送信する。
func (t *Transport) Send(m *Message, dst netip.AddrPort) error {
	raw := m.Marshal()
	if _, err := t.conn.WriteToUDPAddrPort(raw, dst); err != nil {
		return fmt.Errorf("%s: %s への送信に失敗しました: %w", t.name, dst, err)
	}
	t.log.Debug("SIP 送信", "transport", t.name, "dst", dst, "msg", m.Summary())
	t.log.Log(context.Background(), levelTrace, "SIP 送信 (全文)", "transport", t.name, "dst", dst, "raw", string(raw))
	return nil
}

// Close はソケットを閉じる。
func (t *Transport) Close() error { return t.conn.Close() }

// levelTrace は SIP メッセージ全文ダンプ用のレベル。debug より 1 段細かい。
const levelTrace = slog.LevelDebug - 4
