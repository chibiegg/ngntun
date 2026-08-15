package sip

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chibiegg/ngntun/internal/sockopt"
)

// 対応するメソッド。これ以外は 501 を返す。
const (
	MethodRegister = "REGISTER"
	MethodInvite   = "INVITE"
	MethodAck      = "ACK"
	MethodBye      = "BYE"
	MethodCancel   = "CANCEL"
	MethodUpdate   = "UPDATE"
	MethodOptions  = "OPTIONS"
	MethodPrack    = "PRACK"
)

// ErrAuthRequired は HGW が Digest 認証を要求した場合に返る。
// 検証した HGW では認証は行われておらず、ngntun は未対応。
var ErrAuthRequired = errors.New("HGW が SIP の Digest 認証を要求しています。ngntun は未対応です (検証環境では認証なしで登録できていました)")

// StatusError は相手からエラー応答が返ったことを表す。
type StatusError struct {
	Method string
	Status int
	Reason string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s が %d %s で失敗しました", e.Method, e.Status, e.Reason)
}

// Config は UA の設定。
type Config struct {
	Domain     string // 例: ntt-east.ne.jp
	Extension  string // 内線番号。DHCPv6 ベンダオプション code 202
	SelfNumber string // 発信者番号。P-Preferred-Identity に載せる (空なら付けない)

	Registrar netip.AddrPort // REGISTER の宛先 (IPv4 = HGW)
	Proxy     netip.AddrPort // INVITE の Route 先 (IPv6 = HGW)

	LocalV4 netip.AddrPort // SIP over IPv4 の bind 先
	LocalV6 netip.AddrPort // SIP over IPv6 の bind 先

	RegisterExpires time.Duration
	SessionExpires  time.Duration
	UserAgent       string

	// BindDevice を指定すると SIP ソケットをそのインタフェースに結び付ける。
	// 同じプレフィックスの経路が複数インタフェースにある環境で必要。
	BindDevice string

	// RegisterOverIPv6 を立てると REGISTER を IPv6 で送る。
	// 既定 (false) は IPv4。HGW が IPv6 REGISTER を
	// 受け付けるかを実機で確かめるための切り替え (DESIGN.md §12.1)。
	RegisterOverIPv6 bool

	Log *slog.Logger
}

// UA はデータコネクト用の SIP ユーザエージェント。
//
// REGISTER は IPv4、呼制御は IPv6 という非対称な構成をとる (DESIGN.md §2.1)。
type UA struct {
	cfg Config
	log *slog.Logger

	v4 *Transport
	v6 *Transport

	mu      sync.Mutex
	txs     map[string]*clientTx
	dialogs map[string]*Dialog

	// contactUser は Contact の user 部に使うインスタンス識別子。
	// 内線番号ではなくランダムな 12 桁の 16 進トークンを使う。
	// これと Allow / Supported: path を揃えると INVITE が通る
	// (3 つ同時に入れて通ったので、個別の寄与は切り分けていない)。
	contactUser string

	regTag    string
	regCallID string
	regCSeq   uint32

	incoming chan *IncomingCall
}

// New は UA を作り、SIP のソケットを開く。
func New(cfg Config) (*UA, error) {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "ngntun"
	}
	if cfg.RegisterExpires <= 0 {
		cfg.RegisterExpires = time.Hour
	}
	if cfg.SessionExpires <= 0 {
		cfg.SessionExpires = 300 * time.Second
	}
	if !cfg.LocalV4.IsValid() || !cfg.LocalV6.IsValid() {
		return nil, fmt.Errorf("SIP の bind アドレスが IPv4/IPv6 とも必要です")
	}

	v4, err := NewTransport("ipv4", cfg.LocalV4, sockopt.DSCPEF, cfg.BindDevice, cfg.Log)
	if err != nil {
		return nil, err
	}
	v6, err := NewTransport("ipv6", cfg.LocalV6, sockopt.DSCPEF, cfg.BindDevice, cfg.Log)
	if err != nil {
		v4.Close()
		return nil, err
	}

	return &UA{
		cfg:         cfg,
		log:         cfg.Log,
		v4:          v4,
		v6:          v6,
		txs:         map[string]*clientTx{},
		dialogs:     map[string]*Dialog{},
		contactUser: randomToken(6),
		regTag:      NewTag(),
		regCallID:   NewCallID(cfg.LocalV4.Addr().String()),
		incoming:    make(chan *IncomingCall, 1),
	}, nil
}

// Start は 2 本のトランスポートの受信ループを開始する。
//
// ctx がキャンセルされてもソケットは開いたままにする。シグナルで ctx が終わった
// 直後に BYE と登録解除を送れなければ、呼が張り付いて課金が続いてしまうため。
// 受信ループを止めるには Close を呼ぶこと。
func (u *UA) Start(ctx context.Context) {
	go u.v4.Serve(ctx, func(m *Message, src netip.AddrPort) { u.handle(m, src, u.v4) })
	go u.v6.Serve(ctx, func(m *Message, src netip.AddrPort) { u.handle(m, src, u.v6) })
}

// Close はソケットを閉じる。
func (u *UA) Close() {
	u.v4.Close()
	u.v6.Close()
}

// Incoming は着信した INVITE を通知するチャネルを返す。
func (u *UA) Incoming() <-chan *IncomingCall { return u.incoming }

// handle は受信した 1 通を振り分ける。
func (u *UA) handle(m *Message, src netip.AddrPort, tr *Transport) {
	if m.Response {
		u.handleResponse(m, src)
		return
	}
	u.handleRequest(m, src, tr)
}

func (u *UA) handleResponse(m *Message, src netip.AddrPort) {
	_, method := m.CSeq()

	u.mu.Lock()
	tx := u.txs[txKey(m.Branch(), method)]
	u.mu.Unlock()
	if tx != nil {
		tx.receive(m)
		return
	}

	// 対応するトランザクションがない 2xx は、ACK が届かなかったことによる
	// 200 OK の再送とみなして ACK を打ち直す。
	if method == MethodInvite && m.IsSuccess() {
		if d := u.dialog(m.CallID()); d != nil {
			u.log.Debug("200 OK の再送を受信したので ACK を送り直します", "call-id", m.CallID())
			d.resendACK()
			return
		}
	}
	u.log.Debug("対応するトランザクションのない応答を無視しました", "src", src, "msg", m.Summary())
}

func (u *UA) handleRequest(m *Message, src netip.AddrPort, tr *Transport) {
	if d := u.dialog(m.CallID()); d != nil {
		d.handleRequest(m, src, tr)
		return
	}

	switch m.Method {
	case MethodInvite:
		u.handleIncomingInvite(m, src, tr)
	case MethodOptions:
		// 網や HGW からの死活監視。ダイアログ外でも 200 を返しておく。
		u.respond(tr, src, m, 200, "OK", NewTag(), nil)
	case MethodAck:
		// ダイアログのない ACK は無視してよい。
	default:
		u.respond(tr, src, m, 481, "Call/Transaction Does Not Exist", NewTag(), nil)
	}
}

// Register は REGISTER を送り、登録が有効な期間を返す。
func (u *UA) Register(ctx context.Context) (time.Duration, error) {
	return u.register(ctx, int(u.cfg.RegisterExpires.Seconds()))
}

// Unregister は Expires: 0 の REGISTER で登録を解除する。
func (u *UA) Unregister(ctx context.Context) error {
	_, err := u.register(ctx, 0)
	return err
}

func (u *UA) register(ctx context.Context, expires int) (time.Duration, error) {
	u.mu.Lock()
	u.regCSeq++
	cseq := u.regCSeq
	u.mu.Unlock()

	v4 := u.v4.LocalAddrPort()
	v6 := u.v6.LocalAddrPort()
	aor := fmt.Sprintf("sip:%s@%s", u.cfg.Extension, u.cfg.Domain)

	// 既定は IPv4 で登録する (HGW は IPv6 の REGISTER に応答しない)。
	tr, via, dst := u.v4, v4, u.cfg.Registrar
	if u.cfg.RegisterOverIPv6 {
		tr, via, dst = u.v6, v6, u.cfg.Proxy
	}

	req := &Message{
		Method:     MethodRegister,
		RequestURI: "sip:" + u.cfg.Domain,
	}
	req.Headers.Add("Via", fmt.Sprintf("SIP/2.0/UDP %s;branch=%s;rport", HostPort(via), NewBranch()))
	req.Headers.Add("Max-Forwards", "70")
	req.Headers.Add("From", fmt.Sprintf("<%s>;tag=%s", aor, u.regTag))
	req.Headers.Add("To", fmt.Sprintf("<%s>", aor))
	req.Headers.Add("Call-ID", u.regCallID)
	req.Headers.Add("CSeq", fmt.Sprintf("%d %s", cseq, MethodRegister))
	// Contact は IPv4 と IPv6 の両方を登録する。着信は IPv6 Contact に来る。
	req.Headers.Add("Contact", fmt.Sprintf("<%s>", u.contactURI(v4)))
	req.Headers.Add("Contact", fmt.Sprintf("<%s>", u.contactURI(v6)))
	req.Headers.Add("Expires", strconv.Itoa(expires))
	req.Headers.Add("Allow", allowedMethods)
	req.Headers.Add("Supported", "path")
	req.Headers.Add("User-Agent", u.cfg.UserAgent)

	tx, err := u.begin(tr, req, dst)
	if err != nil {
		return 0, err
	}
	defer tx.close()

	resp, err := tx.wait(ctx)
	if err != nil {
		return 0, fmt.Errorf("REGISTER: %w", err)
	}

	switch {
	case resp.IsSuccess():
		return registeredExpires(resp, time.Duration(expires)*time.Second), nil
	case resp.Status == 401 || resp.Status == 407:
		return 0, ErrAuthRequired
	default:
		return 0, &StatusError{Method: MethodRegister, Status: resp.Status, Reason: resp.Reason}
	}
}

// registeredExpires は 200 OK から実際の登録有効期間を読む。
// Expires ヘッダ、Contact の expires パラメータの順に見る。
func registeredExpires(resp *Message, fallback time.Duration) time.Duration {
	if v := resp.Headers.Get("Expires"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	for _, c := range resp.Headers.Values("Contact") {
		if v := Param(c, "expires"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				return time.Duration(n) * time.Second
			}
		}
	}
	return fallback
}

// InviteOptions は発信時の追加指定。
type InviteOptions struct {
	SDP           []byte
	BandwidthKbps int
}

// Invite は電話番号へ発信し、確立したダイアログを返す。
func (u *UA) Invite(ctx context.Context, peerNumber string, opts InviteOptions) (*Dialog, *Message, error) {
	local := u.v6.LocalAddrPort()
	callID := NewCallID(local.Addr().String())
	localTag := NewTag()
	localURI := fmt.Sprintf("sip:%s@%s", u.cfg.Extension, u.cfg.Domain)
	remoteURI := fmt.Sprintf("sip:%s@%s", peerNumber, u.cfg.Domain)

	req := &Message{Method: MethodInvite, RequestURI: remoteURI}
	req.Headers.Add("Via", fmt.Sprintf("SIP/2.0/UDP %s;branch=%s;rport", HostPort(local), NewBranch()))
	req.Headers.Add("Route", fmt.Sprintf("<sip:%s;lr>", HostPort(u.cfg.Proxy)))
	req.Headers.Add("Max-Forwards", "70")
	req.Headers.Add("From", fmt.Sprintf("<%s>;tag=%s", localURI, localTag))
	req.Headers.Add("To", fmt.Sprintf("<%s>", remoteURI))
	req.Headers.Add("Call-ID", callID)
	req.Headers.Add("CSeq", fmt.Sprintf("1 %s", MethodInvite))
	req.Headers.Add("Contact", fmt.Sprintf("<%s>", u.contactURI(local)))
	req.Headers.Add("Allow", allowedMethods)
	if u.cfg.SelfNumber != "" {
		req.Headers.Add("P-Preferred-Identity", fmt.Sprintf("<sip:%s@%s>", u.cfg.SelfNumber, u.cfg.Domain))
	}
	req.Headers.Add("Supported", "100rel,timer")
	req.Headers.Add("Session-Expires", strconv.Itoa(int(u.cfg.SessionExpires.Seconds())))
	req.Headers.Add("User-Agent", u.cfg.UserAgent)
	req.Headers.Add("Content-Type", "application/sdp")
	req.Body = opts.SDP

	tx, err := u.begin(u.v6, req, u.cfg.Proxy)
	if err != nil {
		return nil, nil, err
	}
	defer tx.close()

	tx.onProvisional = func(m *Message) {
		u.log.Info("呼の進行", "status", m.Status, "reason", m.Reason)
		u.maybePrack(m, req, localTag)
	}

	resp, err := tx.wait(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("INVITE: %w", err)
	}
	if !resp.IsSuccess() {
		// 非 2xx の最終応答は、同じ branch の ACK で確認応答する必要がある。
		u.ackFailure(req, resp)
		return nil, nil, &StatusError{Method: MethodInvite, Status: resp.Status, Reason: resp.Reason}
	}

	d, err := u.newDialogUAC(req, resp, callID, localTag, localURI, remoteURI)
	if err != nil {
		return nil, nil, err
	}
	if err := d.sendACK(); err != nil {
		return nil, nil, fmt.Errorf("ACK を送信できません: %w", err)
	}
	return d, resp, nil
}

// maybePrack は Require: 100rel の暫定応答に PRACK を返す。
func (u *UA) maybePrack(resp *Message, invite *Message, localTag string) {
	if !strings.Contains(strings.ToLower(resp.Headers.Get("Require")), "100rel") {
		return
	}
	rseq := strings.TrimSpace(resp.Headers.Get("RSeq"))
	if rseq == "" {
		return
	}
	cseq, _ := invite.CSeq()

	target := URIInAngle(resp.Headers.Get("Contact"))
	if target == "" {
		target = invite.RequestURI
	}
	local := u.v6.LocalAddrPort()

	req := &Message{Method: MethodPrack, RequestURI: target}
	req.Headers.Add("Via", fmt.Sprintf("SIP/2.0/UDP %s;branch=%s;rport", HostPort(local), NewBranch()))
	req.Headers.Add("Route", fmt.Sprintf("<sip:%s;lr>", HostPort(u.cfg.Proxy)))
	req.Headers.Add("Max-Forwards", "70")
	req.Headers.Add("From", fmt.Sprintf("<%s>;tag=%s", URIInAngle(invite.Headers.Get("From")), localTag))
	req.Headers.Add("To", resp.Headers.Get("To"))
	req.Headers.Add("Call-ID", invite.CallID())
	req.Headers.Add("CSeq", fmt.Sprintf("%d %s", cseq+1, MethodPrack))
	req.Headers.Add("RAck", fmt.Sprintf("%s %d %s", rseq, cseq, MethodInvite))
	req.Headers.Add("User-Agent", u.cfg.UserAgent)

	tx, err := u.begin(u.v6, req, u.cfg.Proxy)
	if err != nil {
		u.log.Warn("PRACK を送信できませんでした", "err", err)
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), timerF)
		defer cancel()
		defer tx.close()
		if _, err := tx.wait(ctx); err != nil {
			u.log.Warn("PRACK の応答がありませんでした", "err", err)
		}
	}()
}

// ackFailure は非 2xx 最終応答に対する ACK を送る (INVITE と同じ branch)。
func (u *UA) ackFailure(invite *Message, resp *Message) {
	cseq, _ := invite.CSeq()
	ack := &Message{Method: MethodAck, RequestURI: invite.RequestURI}
	ack.Headers.Add("Via", invite.Headers.Get("Via"))
	ack.Headers.Add("Max-Forwards", "70")
	ack.Headers.Add("From", invite.Headers.Get("From"))
	ack.Headers.Add("To", resp.Headers.Get("To"))
	ack.Headers.Add("Call-ID", invite.CallID())
	ack.Headers.Add("CSeq", fmt.Sprintf("%d %s", cseq, MethodAck))
	if err := u.v6.Send(ack, u.cfg.Proxy); err != nil {
		u.log.Warn("失敗応答への ACK を送信できませんでした", "err", err)
	}
}

// respond はリクエストへの応答を組み立てて送る。
func (u *UA) respond(tr *Transport, dst netip.AddrPort, req *Message, status int, reason, localTag string, body []byte) {
	resp := responseTo(req, status, reason, localTag)
	if len(body) > 0 {
		resp.Headers.Set("Content-Type", "application/sdp")
		resp.Body = body
	}
	resp.Headers.Set("User-Agent", u.cfg.UserAgent)
	if err := tr.Send(resp, dst); err != nil {
		u.log.Warn("応答を送信できませんでした", "status", status, "err", err)
	}
}

// responseTo は受信リクエストから応答の骨格を作る。
func responseTo(req *Message, status int, reason, localTag string) *Message {
	resp := &Message{Response: true, Status: status, Reason: reason}
	for _, v := range req.Headers.Values("Via") {
		resp.Headers.Add("Via", v)
	}
	resp.Headers.Add("From", req.Headers.Get("From"))

	to := req.Headers.Get("To")
	if Param(to, "tag") == "" && localTag != "" {
		to += ";tag=" + localTag
	}
	resp.Headers.Add("To", to)
	resp.Headers.Add("Call-ID", req.CallID())
	resp.Headers.Add("CSeq", req.Headers.Get("CSeq"))
	return resp
}

// allowedMethods は Allow ヘッダの値。
const allowedMethods = "INVITE,CANCEL,ACK,BYE,UPDATE,PRACK"

// contactURI は Contact に載せる URI を組み立てる。
func (u *UA) contactURI(ap netip.AddrPort) string {
	return FormatURI(u.contactUser, ap)
}

func (u *UA) dialog(callID string) *Dialog {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.dialogs[callID]
}

func (u *UA) addDialog(d *Dialog) {
	u.mu.Lock()
	u.dialogs[d.CallID] = d
	u.mu.Unlock()
}

func (u *UA) removeDialog(callID string) {
	u.mu.Lock()
	delete(u.dialogs, callID)
	u.mu.Unlock()
}
