package sip

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

// defaultSIPPort は URI にポートが書かれていない場合の既定値。
const defaultSIPPort = 5060

// Dialog は確立した 1 本の呼。データコネクトでは常に 1 プロセス 1 ダイアログ。
type Dialog struct {
	ua  *UA
	tr  *Transport
	log *slog.Logger

	CallID       string
	LocalTag     string
	RemoteTag    string
	LocalURI     string
	RemoteURI    string
	RemoteTarget string   // 相手の Contact。in-dialog リクエストの宛先
	RouteSet     []string // Record-Route から作った経路集合

	SessionExpires time.Duration
	Refresher      string // "uac" / "uas"

	mu         sync.Mutex
	cseq       uint32
	inviteCSeq uint32
	localSDP   []byte
	ack        *Message
	ackDst     netip.AddrPort

	ackReceived chan struct{}
	ackOnce     sync.Once

	refresh chan time.Time

	terminated chan struct{}
	termOnce   sync.Once
	termReason string
}

func newDialog(ua *UA, tr *Transport) *Dialog {
	return &Dialog{
		ua:          ua,
		tr:          tr,
		log:         ua.log,
		ackReceived: make(chan struct{}),
		refresh:     make(chan time.Time, 1),
		terminated:  make(chan struct{}),
	}
}

// newDialogUAC は発信側 (UAC) として 200 OK からダイアログを組み立てる。
func (u *UA) newDialogUAC(req, resp *Message, callID, localTag, localURI, remoteURI string) (*Dialog, error) {
	remoteTag := Param(resp.Headers.Get("To"), "tag")
	if remoteTag == "" {
		return nil, fmt.Errorf("200 OK の To に tag がありません")
	}
	target := URIInAngle(resp.Headers.Get("Contact"))
	if target == "" {
		return nil, fmt.Errorf("200 OK に Contact がありません")
	}

	d := newDialog(u, u.v6)
	d.CallID = callID
	d.LocalTag = localTag
	d.RemoteTag = remoteTag
	d.LocalURI = localURI
	d.RemoteURI = remoteURI
	d.RemoteTarget = target
	// UAC では Record-Route を逆順にしたものが経路集合になる。
	rr := resp.Headers.Values("Record-Route")
	for i := len(rr) - 1; i >= 0; i-- {
		d.RouteSet = append(d.RouteSet, rr[i])
	}
	d.inviteCSeq, _ = req.CSeq()
	d.cseq = d.inviteCSeq
	d.localSDP = req.Body
	d.SessionExpires, d.Refresher = parseSessionExpires(resp, u.cfg.SessionExpires)

	u.addDialog(d)
	return d, nil
}

// parseSessionExpires は Session-Expires ヘッダを読む。
func parseSessionExpires(m *Message, fallback time.Duration) (time.Duration, string) {
	v := m.Headers.Get("Session-Expires")
	if v == "" {
		return fallback, ""
	}
	sec, _, _ := strings.Cut(v, ";")
	d := fallback
	if n, err := strconv.Atoi(strings.TrimSpace(sec)); err == nil && n > 0 {
		d = time.Duration(n) * time.Second
	}
	return d, strings.ToLower(Param(v, "refresher"))
}

// Terminated は呼が終了したときに閉じられるチャネルを返す。
func (d *Dialog) Terminated() <-chan struct{} { return d.terminated }

// TermReason は終了理由を返す。
func (d *Dialog) TermReason() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.termReason
}

// Refreshes はセッションタイマの更新 (re-INVITE / UPDATE) を通知する。
func (d *Dialog) Refreshes() <-chan time.Time { return d.refresh }

// Bye は BYE を送って呼を切断する。
func (d *Dialog) Bye(ctx context.Context) error {
	d.mu.Lock()
	d.cseq++
	cseq := d.cseq
	d.mu.Unlock()

	req, dst, err := d.newRequest(MethodBye, cseq)
	if err != nil {
		return err
	}

	tx, err := d.ua.begin(d.tr, req, dst)
	if err != nil {
		return err
	}
	defer tx.close()

	resp, err := tx.wait(ctx)
	d.terminate("local-bye")
	if err != nil {
		return fmt.Errorf("BYE: %w", err)
	}
	if !resp.IsSuccess() {
		// 呼はいずれにせよ終わっているので、警告として扱い成功を返す。
		d.log.Warn("BYE に非 2xx が返りました", "status", resp.Status, "reason", resp.Reason)
	}
	return nil
}

// sendACK は 2xx に対する ACK を送り、再送に備えて保持する。
func (d *Dialog) sendACK() error {
	req, dst, err := d.newRequest(MethodAck, d.inviteCSeq)
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.ack, d.ackDst = req, dst
	d.mu.Unlock()
	return d.tr.Send(req, dst)
}

// resendACK は 200 OK が再送されたときに ACK を送り直す。
func (d *Dialog) resendACK() {
	d.mu.Lock()
	ack, dst := d.ack, d.ackDst
	d.mu.Unlock()
	if ack == nil {
		return
	}
	if err := d.tr.Send(ack, dst); err != nil {
		d.log.Warn("ACK を再送できませんでした", "err", err)
	}
}

// newRequest は in-dialog リクエストと、その送信先を組み立てる。
func (d *Dialog) newRequest(method string, cseq uint32) (*Message, netip.AddrPort, error) {
	req := &Message{Method: method, RequestURI: d.RemoteTarget}

	// HGW は Record-Route に ;lr を付ける (loose routing)。
	// 経路集合があればそちらの先頭へ送り、Request-URI は相手の Contact のままにする。
	dstURI := d.RemoteTarget
	if len(d.RouteSet) > 0 {
		dstURI = URIInAngle(d.RouteSet[0])
	}
	dst, err := AddrPortFromURI(dstURI, defaultSIPPort)
	if err != nil {
		return nil, netip.AddrPort{}, fmt.Errorf("%s の送信先を決められません: %w", method, err)
	}

	local := d.tr.LocalAddrPort()
	req.Headers.Add("Via", fmt.Sprintf("SIP/2.0/UDP %s;branch=%s;rport", HostPort(local), NewBranch()))
	for _, r := range d.RouteSet {
		req.Headers.Add("Route", r)
	}
	req.Headers.Add("Max-Forwards", "70")
	req.Headers.Add("From", fmt.Sprintf("<%s>;tag=%s", d.LocalURI, d.LocalTag))
	req.Headers.Add("To", fmt.Sprintf("<%s>;tag=%s", d.RemoteURI, d.RemoteTag))
	req.Headers.Add("Call-ID", d.CallID)
	req.Headers.Add("CSeq", fmt.Sprintf("%d %s", cseq, method))
	req.Headers.Add("Contact", fmt.Sprintf("<%s>", d.ua.contactURI(local)))
	req.Headers.Add("User-Agent", d.ua.cfg.UserAgent)
	return req, dst, nil
}

// handleRequest はダイアログ内で受けたリクエストを処理する。
func (d *Dialog) handleRequest(req *Message, src netip.AddrPort, tr *Transport) {
	switch req.Method {
	case MethodAck:
		d.ackOnce.Do(func() { close(d.ackReceived) })

	case MethodBye:
		d.ua.respond(tr, src, req, 200, "OK", d.LocalTag, nil)
		d.terminate("remote-bye")

	case MethodInvite, MethodUpdate:
		// セッションタイマの更新。今のメディア構成をそのまま返す。
		d.mu.Lock()
		body := d.localSDP
		d.mu.Unlock()

		resp := responseTo(req, 200, "OK", d.LocalTag)
		resp.Headers.Set("Contact", fmt.Sprintf("<%s>", d.ua.contactURI(tr.LocalAddrPort())))
		resp.Headers.Set("User-Agent", d.ua.cfg.UserAgent)
		if se := req.Headers.Get("Session-Expires"); se != "" {
			resp.Headers.Set("Session-Expires", se)
			resp.Headers.Set("Require", "timer")
		}
		if len(body) > 0 {
			resp.Headers.Set("Content-Type", "application/sdp")
			resp.Body = body
		}
		if err := tr.Send(resp, src); err != nil {
			d.log.Warn("セッション更新への応答を送信できませんでした", "err", err)
		}
		select {
		case d.refresh <- time.Now():
		default:
		}

	case MethodOptions:
		d.ua.respond(tr, src, req, 200, "OK", d.LocalTag, nil)

	default:
		d.ua.respond(tr, src, req, 501, "Not Implemented", d.LocalTag, nil)
	}
}

// terminate は呼の終了を記録し、Terminated チャネルを閉じる。
func (d *Dialog) terminate(reason string) {
	d.termOnce.Do(func() {
		d.mu.Lock()
		d.termReason = reason
		d.mu.Unlock()
		d.ua.removeDialog(d.CallID)
		close(d.terminated)
	})
}

// IncomingCall は着信した INVITE。Accept か Reject のどちらかを必ず呼ぶこと。
type IncomingCall struct {
	ua  *UA
	tr  *Transport
	req *Message
	src netip.AddrPort
}

// CallerNumber は発信者番号 (From の user 部) を返す。
func (c *IncomingCall) CallerNumber() string {
	uri := URIInAngle(c.req.Headers.Get("From"))
	uri = strings.TrimPrefix(uri, "sip:")
	user, _, _ := strings.Cut(uri, "@")
	return user
}

// SDP は着信 INVITE に載っていたオファーを返す。
func (c *IncomingCall) SDP() []byte { return c.req.Body }

// Reject は着信を拒否する。
func (c *IncomingCall) Reject(status int, reason string) {
	c.ua.respond(c.tr, c.src, c.req, status, reason, NewTag(), nil)
}

// Accept は 200 OK を返してダイアログを確立する。ACK が来るまで 200 OK を再送する。
func (c *IncomingCall) Accept(sdpAnswer []byte) (*Dialog, error) {
	remoteTag := Param(c.req.Headers.Get("From"), "tag")
	if remoteTag == "" {
		return nil, fmt.Errorf("INVITE の From に tag がありません")
	}
	target := URIInAngle(c.req.Headers.Get("Contact"))
	if target == "" {
		return nil, fmt.Errorf("INVITE に Contact がありません")
	}

	d := newDialog(c.ua, c.tr)
	d.CallID = c.req.CallID()
	d.LocalTag = NewTag()
	d.RemoteTag = remoteTag
	d.LocalURI = URIInAngle(c.req.Headers.Get("To"))
	d.RemoteURI = URIInAngle(c.req.Headers.Get("From"))
	d.RemoteTarget = target
	// UAS では Record-Route はそのままの順で経路集合になる。
	d.RouteSet = append(d.RouteSet, c.req.Headers.Values("Record-Route")...)
	d.inviteCSeq, _ = c.req.CSeq()
	d.cseq = d.inviteCSeq
	d.localSDP = sdpAnswer
	d.SessionExpires, _ = parseSessionExpires(c.req, c.ua.cfg.SessionExpires)
	// 更新は相手 (発信側) にやってもらう。こちらから re-INVITE を出す必要がなくなる。
	d.Refresher = "uac"

	c.ua.addDialog(d)

	resp := responseTo(c.req, 200, "OK", d.LocalTag)
	resp.Headers.Set("Contact", fmt.Sprintf("<%s>", c.ua.contactURI(c.tr.LocalAddrPort())))
	resp.Headers.Set("Supported", "100rel,timer")
	resp.Headers.Set("Session-Expires", fmt.Sprintf("%d;refresher=uac", int(d.SessionExpires.Seconds())))
	resp.Headers.Set("Require", "timer")
	resp.Headers.Set("Content-Type", "application/sdp")
	resp.Headers.Set("User-Agent", c.ua.cfg.UserAgent)
	resp.Body = sdpAnswer

	if err := c.tr.Send(resp, c.src); err != nil {
		c.ua.removeDialog(d.CallID)
		return nil, err
	}

	// ACK が来るまで 200 OK を再送する (RFC 3261 13.3.1.4)。
	go func() {
		interval := timerT1
		deadline := time.After(timerB)
		for {
			select {
			case <-d.ackReceived:
				return
			case <-d.terminated:
				return
			case <-deadline:
				d.log.Warn("ACK が届かないため呼を終了します", "call-id", d.CallID)
				d.terminate("no-ack")
				return
			case <-time.After(interval):
				if err := c.tr.Send(resp, c.src); err != nil {
					d.log.Warn("200 OK を再送できませんでした", "err", err)
				}
				if interval *= 2; interval > timerT2 {
					interval = timerT2
				}
			}
		}
	}()

	return d, nil
}

// handleIncomingInvite は着信 INVITE を受け付ける。
// 待ち受けていない (発信専用で動いている) 場合は 486 を返す。
func (u *UA) handleIncomingInvite(req *Message, src netip.AddrPort, tr *Transport) {
	u.respond(tr, src, req, 100, "Trying", "", nil)

	call := &IncomingCall{ua: u, tr: tr, req: req, src: src}
	select {
	case u.incoming <- call:
	default:
		u.log.Info("着信を受け付けられないため拒否しました", "from", call.CallerNumber())
		call.Reject(486, "Busy Here")
	}
}
