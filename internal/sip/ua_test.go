package sip

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// fakeHGW はテスト用の擬似 HGW。実機の HGW と同じ形の応答を返す。
// REGISTER は IPv4 で、呼制御は IPv6 で受ける (実機と同じ非対称構成)。
type fakeHGW struct {
	t  *testing.T
	v4 *net.UDPConn
	v6 *net.UDPConn

	reqs chan *Message
	opts hgwOptions
}

// hgwOptions は擬似 HGW の応答の方針。受信ループを起動する前に確定させる
// (起動後に書き換えるとデータ競合になる)。
type hgwOptions struct {
	registerStatus int    // 0 なら 200 OK
	inviteStatus   int    // 0 なら 200 OK
	provisional    int    // 0 以外なら 200 OK の前にこの暫定応答を送る
	answerSDP      []byte // 200 OK に載せる SDP
}

func startFakeHGW(t *testing.T, opts hgwOptions) *fakeHGW {
	t.Helper()

	if opts.answerSDP == nil {
		opts.answerSDP = []byte("v=0\r\nc=IN IP6 2001:db8:ffff::318\r\nm=application 14672 udp octet-stream\r\n")
	}

	v4, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(netip.MustParseAddrPort("127.0.0.1:0")))
	if err != nil {
		t.Fatalf("擬似 HGW の IPv4 ソケットを開けません: %v", err)
	}
	v6, err := net.ListenUDP("udp6", net.UDPAddrFromAddrPort(netip.MustParseAddrPort("[::1]:0")))
	if err != nil {
		t.Fatalf("擬似 HGW の IPv6 ソケットを開けません: %v", err)
	}

	h := &fakeHGW{
		t:    t,
		v4:   v4,
		v6:   v6,
		reqs: make(chan *Message, 32),
		opts: opts,
	}
	t.Cleanup(func() {
		v4.Close()
		v6.Close()
	})

	go h.serve(v4)
	go h.serve(v6)
	return h
}

func (h *fakeHGW) v4AddrPort() netip.AddrPort {
	return h.v4.LocalAddr().(*net.UDPAddr).AddrPort()
}

func (h *fakeHGW) v6AddrPort() netip.AddrPort {
	return h.v6.LocalAddr().(*net.UDPAddr).AddrPort()
}

func (h *fakeHGW) serve(conn *net.UDPConn) {
	buf := make([]byte, 65535)
	for {
		n, src, err := conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			return
		}
		msg, err := ParseMessage(buf[:n])
		if err != nil {
			continue
		}
		if msg.Response {
			continue
		}
		select {
		case h.reqs <- msg:
		default:
		}
		h.handle(conn, msg, src)
	}
}

func (h *fakeHGW) handle(conn *net.UDPConn, req *Message, src netip.AddrPort) {
	send := func(m *Message) {
		conn.WriteToUDPAddrPort(m.Marshal(), src)
	}

	switch req.Method {
	case MethodRegister:
		status := h.opts.registerStatus
		if status == 0 {
			status = 200
		}
		resp := responseTo(req, status, statusText(status), "hgwtag")
		if status == 200 {
			for _, c := range req.Headers.Values("Contact") {
				resp.Headers.Add("Contact", c)
			}
			resp.Headers.Add("Expires", "3600")
		}
		if status == 401 {
			resp.Headers.Add("WWW-Authenticate", `Digest realm="ntt-east.ne.jp", nonce="abc"`)
		}
		send(resp)

	case MethodInvite:
		send(responseTo(req, 100, "Trying", ""))
		if h.opts.provisional != 0 {
			send(responseTo(req, h.opts.provisional, statusText(h.opts.provisional), "hgwtag"))
		}

		status := h.opts.inviteStatus
		if status == 0 {
			status = 200
		}
		resp := responseTo(req, status, statusText(status), "hgwtag")
		if status == 200 {
			resp.Headers.Add("Record-Route", fmt.Sprintf("<sip:%s;lr>", HostPort(h.v6AddrPort())))
			resp.Headers.Add("Contact", fmt.Sprintf("<sip:03YYYYYYYY@%s>", HostPort(h.v6AddrPort())))
			resp.Headers.Add("Session-Expires", "300;refresher=uas")
			resp.Headers.Add("Content-Type", "application/sdp")
			resp.Body = h.opts.answerSDP
		}
		send(resp)

	case MethodBye, MethodOptions, MethodPrack, MethodUpdate:
		send(responseTo(req, 200, "OK", "hgwtag"))

	case MethodAck:
		// ACK には応答しない。
	}
}

func statusText(code int) string {
	switch code {
	case 100:
		return "Trying"
	case 180:
		return "Ringing"
	case 200:
		return "OK"
	case 401:
		return "Unauthorized"
	case 486:
		return "Busy Here"
	case 503:
		return "Service Unavailable"
	default:
		return "Unknown"
	}
}

// waitFor は指定メソッドのリクエストが届くまで待つ。
func (h *fakeHGW) waitFor(t *testing.T, method string, timeout time.Duration) *Message {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case m := <-h.reqs:
			if m.Method == method {
				return m
			}
		case <-deadline:
			t.Fatalf("%s が届きませんでした", method)
			return nil
		}
	}
}

func newTestUA(t *testing.T, h *fakeHGW) (*UA, context.Context) {
	t.Helper()

	ua := newUnstartedUA(t, h)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		ua.Close()
	})
	ua.Start(ctx)
	return ua, ctx
}

func newUnstartedUA(t *testing.T, h *fakeHGW) *UA {
	t.Helper()

	ua, err := New(Config{
		Domain:     "ntt-east.ne.jp",
		Extension:  "3",
		SelfNumber: "03XXXXXXXX",
		Registrar:  h.v4AddrPort(),
		Proxy:      h.v6AddrPort(),
		LocalV4:    netip.MustParseAddrPort("127.0.0.1:0"),
		LocalV6:    netip.MustParseAddrPort("[::1]:0"),
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return ua
}

// シグナルで ctx が終わった直後に BYE と登録解除を送れなければ、呼が張り付いて
// 課金が続いてしまう。実機テストで踏んだ不具合の回帰テスト。
func TestSocketsStayUsableAfterContextCancel(t *testing.T) {
	h := startFakeHGW(t, hgwOptions{})
	ua := newUnstartedUA(t, h)
	defer ua.Close()

	ctx, cancel := context.WithCancel(context.Background())
	ua.Start(ctx)

	if _, err := ua.Register(ctx); err != nil {
		t.Fatalf("Register: %v", err)
	}
	h.waitFor(t, MethodRegister, 2*time.Second)

	cancel() // シグナル受信を模す

	shutdown, cancelShutdown := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelShutdown()
	if err := ua.Unregister(shutdown); err != nil {
		t.Fatalf("ctx キャンセル後に登録を解除できない: %v", err)
	}
}

func TestRegisterUsesIPv4AndBothContacts(t *testing.T) {
	h := startFakeHGW(t, hgwOptions{})
	ua, ctx := newTestUA(t, h)

	expires, err := ua.Register(ctx)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if expires != time.Hour {
		t.Errorf("expires = %s, want 1h (200 OK の Expires: 3600)", expires)
	}

	req := h.waitFor(t, MethodRegister, 2*time.Second)

	// REGISTER は IPv4 で送られる (DESIGN.md §2.1)。
	if via := req.Headers.Get("Via"); strings.Contains(via, "[") {
		t.Errorf("Via が IPv6 になっている: %q", via)
	}
	// Contact は IPv4 と IPv6 の両方。着信は IPv6 Contact に来る。
	contacts := req.Headers.Values("Contact")
	if len(contacts) != 2 {
		t.Fatalf("Contact の本数 = %d, want 2 (%v)", len(contacts), contacts)
	}
	if !strings.Contains(contacts[0], "127.0.0.1") {
		t.Errorf("1 本目の Contact が IPv4 でない: %q", contacts[0])
	}
	if !strings.Contains(contacts[1], "[::1]") {
		t.Errorf("2 本目の Contact が IPv6 でない: %q", contacts[1])
	}
	if got, want := req.RequestURI, "sip:ntt-east.ne.jp"; got != want {
		t.Errorf("Request-URI = %q, want %q", got, want)
	}
}

func TestRegisterAuthChallengeIsReportedClearly(t *testing.T) {
	h := startFakeHGW(t, hgwOptions{registerStatus: 401})
	ua, ctx := newTestUA(t, h)

	_, err := ua.Register(ctx)
	if !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("err = %v, want ErrAuthRequired", err)
	}
}

func TestInviteEstablishesDialogAndSendsACK(t *testing.T) {
	h := startFakeHGW(t, hgwOptions{})
	ua, ctx := newTestUA(t, h)

	if _, err := ua.Register(ctx); err != nil {
		t.Fatalf("Register: %v", err)
	}

	offer := []byte("v=0\r\nm=application 4502 udp octet-stream\r\nb=AS:64\r\na=udp-setup:active\r\n")
	d, resp, err := ua.Invite(ctx, "03YYYYYYYY", InviteOptions{SDP: offer})
	if err != nil {
		t.Fatalf("Invite: %v", err)
	}

	invite := h.waitFor(t, MethodInvite, 2*time.Second)
	if got, want := invite.RequestURI, "sip:03YYYYYYYY@ntt-east.ne.jp"; got != want {
		t.Errorf("Request-URI = %q, want %q", got, want)
	}
	if got, want := invite.Headers.Get("P-Preferred-Identity"), "<sip:03XXXXXXXX@ntt-east.ne.jp>"; got != want {
		t.Errorf("P-Preferred-Identity = %q, want %q", got, want)
	}
	if !strings.Contains(invite.Headers.Get("Route"), HostPort(h.v6AddrPort())) {
		t.Errorf("Route が HGW を指していない: %q", invite.Headers.Get("Route"))
	}
	if string(invite.Body) != string(offer) {
		t.Errorf("SDP オファーが一致しない:\n%s", invite.Body)
	}

	// 200 OK に続いて ACK が飛ぶこと。
	ack := h.waitFor(t, MethodAck, 2*time.Second)
	if got, want := Param(ack.Headers.Get("To"), "tag"), "hgwtag"; got != want {
		t.Errorf("ACK の To tag = %q, want %q", got, want)
	}
	if ack.Headers.Get("Route") == "" {
		t.Error("ACK に Record-Route 由来の Route が入っていない")
	}

	// ダイアログにセッションタイマの情報が取り込まれること。
	if d.SessionExpires != 300*time.Second || d.Refresher != "uas" {
		t.Errorf("Session-Expires = %s / %q, want 300s / uas", d.SessionExpires, d.Refresher)
	}
	if !strings.Contains(string(resp.Body), "octet-stream") {
		t.Errorf("200 OK の SDP が取れていない: %q", resp.Body)
	}

	// BYE を送ると呼が終了すること。
	if err := d.Bye(ctx); err != nil {
		t.Fatalf("Bye: %v", err)
	}
	bye := h.waitFor(t, MethodBye, 2*time.Second)
	if got, want := bye.CallID(), d.CallID; got != want {
		t.Errorf("BYE の Call-ID = %q, want %q", got, want)
	}
	select {
	case <-d.Terminated():
	case <-time.After(time.Second):
		t.Error("ダイアログが終了していない")
	}
	if got, want := d.TermReason(), "local-bye"; got != want {
		t.Errorf("TermReason = %q, want %q", got, want)
	}
}

func TestInviteFailureSendsACKAndReturnsStatus(t *testing.T) {
	h := startFakeHGW(t, hgwOptions{inviteStatus: 486})
	ua, ctx := newTestUA(t, h)

	_, _, err := ua.Invite(ctx, "03YYYYYYYY", InviteOptions{SDP: []byte("v=0\r\n")})
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *StatusError", err)
	}
	if se.Status != 486 {
		t.Errorf("Status = %d, want 486", se.Status)
	}

	// 非 2xx 最終応答にも ACK を返さないと、網が応答を再送し続ける。
	h.waitFor(t, MethodAck, 2*time.Second)
}

func TestRemoteByeTerminatesDialog(t *testing.T) {
	h := startFakeHGW(t, hgwOptions{})
	ua, ctx := newTestUA(t, h)

	d, _, err := ua.Invite(ctx, "03YYYYYYYY", InviteOptions{SDP: []byte("v=0\r\n")})
	if err != nil {
		t.Fatalf("Invite: %v", err)
	}
	h.waitFor(t, MethodAck, 2*time.Second)

	// HGW 側から BYE を投げる。
	bye := &Message{Method: MethodBye, RequestURI: "sip:3@[::1]"}
	bye.Headers.Add("Via", fmt.Sprintf("SIP/2.0/UDP %s;branch=%s", HostPort(h.v6AddrPort()), NewBranch()))
	bye.Headers.Add("From", fmt.Sprintf("<sip:03YYYYYYYY@ntt-east.ne.jp>;tag=%s", d.RemoteTag))
	bye.Headers.Add("To", fmt.Sprintf("<sip:3@ntt-east.ne.jp>;tag=%s", d.LocalTag))
	bye.Headers.Add("Call-ID", d.CallID)
	bye.Headers.Add("CSeq", "1 BYE")

	uaAddr, err := netip.ParseAddrPort(strings.TrimPrefix(HostPort(uaLocalV6(ua)), ""))
	if err != nil {
		t.Fatalf("UA のアドレスを取得できません: %v", err)
	}
	if _, err := h.v6.WriteToUDPAddrPort(bye.Marshal(), uaAddr); err != nil {
		t.Fatalf("BYE を送信できません: %v", err)
	}

	select {
	case <-d.Terminated():
	case <-time.After(2 * time.Second):
		t.Fatal("BYE を受けてもダイアログが終了しない")
	}
	if got, want := d.TermReason(), "remote-bye"; got != want {
		t.Errorf("TermReason = %q, want %q", got, want)
	}
}

func uaLocalV6(u *UA) netip.AddrPort { return u.v6.LocalAddrPort() }
