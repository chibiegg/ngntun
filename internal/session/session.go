// Package session はデータコネクトのセッション全体の状態機械を持つ。
//
// 「呼が生きている間だけ tun が存在する」という 1 対 1 の対応を守るのがこの層の役目で、
// SIP・メディア・ネットワーク設定の後始末は必ずここを通る。
package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/chibiegg/ngntun/internal/media"
	"github.com/chibiegg/ngntun/internal/netcfg"
	"github.com/chibiegg/ngntun/internal/sdp"
	"github.com/chibiegg/ngntun/internal/sip"
	"github.com/chibiegg/ngntun/internal/tundev"
)

// 終了理由。
const (
	ReasonIdleTimeout  = "idle-timeout"
	ReasonMaxDuration  = "max-duration"
	ReasonRemoteBye    = "remote-bye"
	ReasonSignal       = "signal"
	ReasonMediaTimeout = "media-timeout"
	ReasonCommandExit  = "command-exit"
	ReasonError        = "error"
)

// Config はセッション 1 本の設定。
type Config struct {
	SIP sip.Config

	PeerNumber   string   // 発信先の電話番号
	AnswerMode   bool     // true なら着信待ち受け
	AllowFrom    []string // 着信を許可する発番号 (前方一致)。空なら全許可
	RegisterOnly bool     // 登録だけして待機する (発信しないので課金されない)

	SelfV6        netip.Addr // メディアの送信元
	MediaPort     int        // 0 で自動割当
	BandwidthKbps int

	TunName    string
	TunAddr    netip.Prefix
	TunPeer    netip.Addr
	TunMTU     int
	Routes     []netip.Prefix
	RouteTable int
	Force      bool

	// MediaRoute が true なら、メディア宛先へ WAN 経由で到達できないときだけ
	// 呼の間だけホスト経路を足す。MediaGateway が無効なら on-link 扱いになる。
	MediaRoute   bool
	MediaGateway netip.Addr

	// Command はトンネル確立後に実行するコマンドと引数。空なら何も実行しない。
	// 指定した場合、このコマンドの終了がセッションの終了条件になる。
	Command []string

	IdleTimeout    time.Duration
	MaxDuration    time.Duration
	MediaTimeout   time.Duration
	StrictPeerPort bool
	BindDevice     string // メディアソケットを結び付けるインタフェース

	Log *slog.Logger
}

// Result はセッションの結果。
type Result struct {
	Reason     string
	PeerNumber string
	Duration   time.Duration
	Media      media.Stats
	Remote     netip.AddrPort

	CommandRun      bool // Config.Command を起動したか
	CommandExitCode int  // 起動した場合の終了コード (シグナル終了は 128+シグナル番号)
}

// Run はセッションを 1 本張り、終了するまでブロックする。
func Run(ctx context.Context, cfg Config) (*Result, error) {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}

	// 呼を張る前に tun を作っておく。権限不足などはここで落としたい
	// (課金の発生する発信を済ませてから失敗するのは避ける)。
	if netcfg.Exists(cfg.TunName) {
		if !cfg.Force {
			return nil, fmt.Errorf("インタフェース %s は既に存在します。前回の残骸なら --force で作り直せます", cfg.TunName)
		}
		if err := netcfg.DeleteTun(cfg.TunName); err != nil {
			return nil, err
		}
		log.Warn("既存のインタフェースを削除しました", "dev", cfg.TunName)
	}

	tun, err := tundev.Create(cfg.TunName)
	if err != nil {
		return nil, err
	}
	defer tun.Close()
	log.Info("tun デバイスを作成しました", "dev", cfg.TunName)

	ua, err := sip.New(cfg.SIP)
	if err != nil {
		return nil, err
	}
	defer ua.Close()
	ua.Start(ctx)

	// 1. 登録
	regCtx, cancelReg := context.WithTimeout(ctx, 35*time.Second)
	expires, err := ua.Register(regCtx)
	cancelReg()
	if err != nil {
		return nil, err
	}
	log.Info("SIP に登録しました", "extension", cfg.SIP.Extension, "expires", expires)
	defer unregister(ua, log)

	regDone := make(chan struct{})
	defer close(regDone)
	go refreshRegistration(ua, expires, regDone, log)

	// 登録の確認だけしたい場合はここで待機する。発信しないので課金は発生しない。
	if cfg.RegisterOnly {
		log.Info("登録のみモードです。終了シグナルまで登録を維持します")
		<-ctx.Done()
		return &Result{Reason: ReasonSignal}, nil
	}

	// 2. メディア用ソケット (呼が確立する前に開いて、SDP に載せるポートを確定させる)
	conn, err := media.Dial(netip.AddrPortFrom(cfg.SelfV6, uint16(cfg.MediaPort)), cfg.BindDevice, log)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	localMedia, _ := netip.ParseAddrPort(conn.LocalAddr().String())
	offer := &sdp.Offer{
		Addr:          cfg.SelfV6,
		Port:          int(localMedia.Port()),
		BandwidthKbps: cfg.BandwidthKbps,
		Setup:         sdp.SetupActive,
		SessionID:     uint64(time.Now().Unix()),
		SessionVer:    1,
	}

	// 3. 呼の確立
	var (
		dialog *sip.Dialog
		answer *sdp.Answer
		peer   string
	)
	if cfg.AnswerMode {
		offer.Setup = sdp.SetupPassive
		dialog, answer, peer, err = waitForCall(ctx, ua, offer, cfg, log)
	} else {
		peer = cfg.PeerNumber
		dialog, answer, err = placeCall(ctx, ua, offer, cfg, log)
	}
	if err != nil {
		return nil, err
	}
	established := time.Now()
	log.Info("データコネクトのセッションを確立しました",
		"peer", peer, "media", answer.AddrPort(), "bandwidth_kbps", answer.BandwidthKbps,
		"session_expires", dialog.SessionExpires, "refresher", dialog.Refresher)

	result := &Result{PeerNumber: peer, Remote: answer.AddrPort()}

	// 4. ネットワーク設定 (呼が確立してから入れ、終了時に必ず巻き戻す)
	applied, err := netcfg.Apply(netcfg.Config{
		Name:   cfg.TunName,
		MTU:    cfg.TunMTU,
		Addr:   cfg.TunAddr,
		Peer:   cfg.TunPeer,
		Routes: cfg.Routes,
		Table:  cfg.RouteTable,
	}, log)
	if err != nil {
		// 設定に失敗したら課金を止めるため即座に切る。
		hangup(dialog, log)
		return nil, err
	}

	// 4.5. メディア宛先への経路。宛先は 200 OK の SDP で初めて確定するので、
	// ここまで来ないと足せない。既に到達できるなら何もしない。
	var mediaRoute *netcfg.Applied
	if cfg.MediaRoute {
		mediaRoute, err = netcfg.EnsureHostRoute(netcfg.HostRoute{
			Dst: answer.AddrPort().Addr(),
			Via: cfg.MediaGateway,
			Dev: cfg.BindDevice,
		}, log)
		if err != nil {
			// 経路が無ければメディアは流れない。トンネルを維持する意味がないので切る。
			hangup(dialog, log)
			applied.Rollback()
			return nil, err
		}
	}

	// 5. 転送開始
	fwd := media.New(media.Config{
		Tun:            tun,
		Conn:           conn,
		Remote:         answer.AddrPort(),
		StrictPeerPort: cfg.StrictPeerPort,
		MTU:            cfg.TunMTU,
		Log:            log,
	})
	fwdCtx, stopFwd := context.WithCancel(ctx)
	fwdErr := make(chan error, 1)
	go func() { fwdErr <- fwd.Run(fwdCtx) }()

	// 6. トンネル確立後のコマンド。呼とコマンドの寿命はここで一致させる。
	cmdCtx, stopCmd := context.WithCancel(ctx)
	defer stopCmd()

	var runner *commandRunner
	// 後始末。どの経路から来ても同じ手順を通す。
	// コマンドの終了要求は非同期に届くだけなので、BYE より前に出しても課金の停止は遅れない。
	finish := func(reason string, runErr error) (*Result, error) {
		stopCmd()
		stopFwd()
		hangup(dialog, log)
		applied.Rollback()
		mediaRoute.Rollback()

		if runner != nil {
			res := runner.Wait()
			result.CommandRun = true
			result.CommandExitCode = res.exitCode
			if res.err != nil {
				log.Warn("コマンドの終了を待てませんでした", "err", res.err)
			}
			log.Info("コマンドが終了しました", "exit_code", res.exitCode)
		}

		result.Reason = reason
		result.Duration = time.Since(established)
		result.Media = fwd.Stats()
		return result, runErr
	}

	if len(cfg.Command) > 0 {
		runner, err = startCommand(cmdCtx, cfg, result, log)
		if err != nil {
			// 起動できないならトンネルを維持する意味がない。即座に切る。
			return finish(ReasonError, err)
		}
	}

	// 7. 終了条件の監視
	reason, runErr := supervise(ctx, cfg, dialog, fwd, fwdErr, runner, log)

	// 8. 後始末。まず BYE を送って課金を止め、そのあとで経路と tun を戻す。
	return finish(reason, runErr)
}

// placeCall は発信して呼を確立する。
func placeCall(ctx context.Context, ua *sip.UA, offer *sdp.Offer, cfg Config, log *slog.Logger) (*sip.Dialog, *sdp.Answer, error) {
	log.Info("発信します", "peer", cfg.PeerNumber, "bandwidth_kbps", cfg.BandwidthKbps)

	dialog, resp, err := ua.Invite(ctx, cfg.PeerNumber, sip.InviteOptions{SDP: offer.Marshal()})
	if err != nil {
		return nil, nil, err
	}
	answer, err := sdp.ParseAnswer(resp.Body)
	if err != nil {
		hangup(dialog, log)
		return nil, nil, fmt.Errorf("200 OK の SDP を解釈できません: %w", err)
	}
	if answer.Setup == sdp.SetupActive {
		// 相手も active を主張した場合、双方が送信開始を待つ状況にはならないが、
		// 想定と違う挙動なので記録しておく。
		log.Warn("SDP アンサーが active でした (通常は passive)", "setup", answer.Setup)
	}
	return dialog, answer, nil
}

// waitForCall は着信を待ち、許可された発番号なら応答する。
func waitForCall(ctx context.Context, ua *sip.UA, offer *sdp.Offer, cfg Config, log *slog.Logger) (*sip.Dialog, *sdp.Answer, string, error) {
	log.Info("着信を待っています", "extension", cfg.SIP.Extension, "allow_from", cfg.AllowFrom)

	for {
		select {
		case <-ctx.Done():
			return nil, nil, "", ctx.Err()

		case call := <-ua.Incoming():
			from := call.CallerNumber()
			if !allowed(from, cfg.AllowFrom) {
				log.Warn("許可されていない発番号からの着信を拒否しました", "from", from)
				call.Reject(403, "Forbidden")
				continue
			}

			// 相手のオファーからメディアの宛先を取る。
			answer, err := sdp.ParseAnswer(call.SDP())
			if err != nil {
				log.Warn("着信 INVITE の SDP を解釈できません", "from", from, "err", err)
				call.Reject(488, "Not Acceptable Here")
				continue
			}

			dialog, err := call.Accept(offer.Marshal())
			if err != nil {
				return nil, nil, "", fmt.Errorf("着信に応答できません: %w", err)
			}
			log.Info("着信に応答しました", "from", from)
			return dialog, answer, from, nil
		}
	}
}

// allowed は発番号が許可リストに合致するかを返す。
func allowed(from string, allow []string) bool {
	if len(allow) == 0 {
		return true
	}
	return slices.ContainsFunc(allow, func(p string) bool {
		return strings.HasPrefix(from, p)
	})
}

// supervise は終了条件を監視し、終了理由を返す。
func supervise(ctx context.Context, cfg Config, dialog *sip.Dialog, fwd *media.Forwarder, fwdErr <-chan error, cmd *commandRunner, log *slog.Logger) (string, error) {
	var maxDuration <-chan time.Time
	if cfg.MaxDuration > 0 {
		t := time.NewTimer(cfg.MaxDuration)
		defer t.Stop()
		maxDuration = t.C
	}

	// メディアが一切来ない状態を検出する。
	var mediaDeadline <-chan time.Time
	if cfg.MediaTimeout > 0 {
		t := time.NewTimer(cfg.MediaTimeout)
		defer t.Stop()
		mediaDeadline = t.C
	}

	// セッションタイマ。網 (uas) が更新してくる想定なので、
	// 期限を過ぎても更新が来なければこちらから切る。
	sessionTimer := time.NewTimer(sessionWatchdog(dialog.SessionExpires))
	defer sessionTimer.Stop()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info("シグナルを受け取りました。セッションを終了します")
			return ReasonSignal, nil

		case err := <-fwdErr:
			if err != nil {
				return ReasonError, err
			}
			return ReasonError, errors.New("メディアの転送が予期せず終了しました")

		case <-dialog.Terminated():
			log.Info("相手側から切断されました", "reason", dialog.TermReason())
			return ReasonRemoteBye, nil

		case <-cmd.Exited():
			// コマンドが終わればトンネルの用は済んでいる。課金を止めるため即座に切る。
			log.Info("コマンドが終了したためセッションを終了します")
			return ReasonCommandExit, nil

		case <-maxDuration:
			log.Warn("最大保持時間に達しました", "max_duration", cfg.MaxDuration)
			return ReasonMaxDuration, nil

		case <-mediaDeadline:
			if fwd.Stats().RxPackets == 0 {
				log.Warn("メディアが届かないためセッションを終了します", "media_timeout", cfg.MediaTimeout)
				return ReasonMediaTimeout, nil
			}

		case <-dialog.Refreshes():
			log.Debug("セッションタイマが更新されました")
			sessionTimer.Reset(sessionWatchdog(dialog.SessionExpires))

		case <-sessionTimer.C:
			// 更新が来ないまま期限を過ぎた。呼が死んでいる可能性が高い。
			log.Warn("セッションタイマの更新が来ませんでした", "session_expires", dialog.SessionExpires)
			return ReasonError, errors.New("セッションタイマが更新されませんでした")

		case <-ticker.C:
			if cfg.IdleTimeout > 0 && fwd.IdleFor() > cfg.IdleTimeout {
				log.Info("無通信のためセッションを終了します", "idle_timeout", cfg.IdleTimeout)
				return ReasonIdleTimeout, nil
			}
		}
	}
}

// sessionWatchdog はセッションタイマの監視期限を返す (更新の取りこぼしに少し余裕を持たせる)。
func sessionWatchdog(sessionExpires time.Duration) time.Duration {
	if sessionExpires <= 0 {
		return time.Hour
	}
	return sessionExpires + 30*time.Second
}

// hangup は BYE を送る。既に終了していれば何もしない。
func hangup(d *sip.Dialog, log *slog.Logger) {
	select {
	case <-d.Terminated():
		return
	default:
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := d.Bye(ctx); err != nil {
		log.Warn("BYE を送信できませんでした", "err", err)
		return
	}
	log.Info("BYE を送信しました")
}

// unregister は終了時に登録を解除する。
func unregister(ua *sip.UA, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := ua.Unregister(ctx); err != nil {
		log.Warn("登録を解除できませんでした", "err", err)
	}
}

// refreshRegistration は登録の有効期限の半分ごとに再 REGISTER する。
func refreshRegistration(ua *sip.UA, expires time.Duration, done <-chan struct{}, log *slog.Logger) {
	interval := expires / 2
	if interval < time.Minute {
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-done:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
			_, err := ua.Register(ctx)
			cancel()
			if err != nil {
				// 呼が確立済みなら登録が切れても通話は続く。致命的には扱わない。
				log.Warn("登録の更新に失敗しました", "err", err)
				continue
			}
			log.Debug("登録を更新しました")
		}
	}
}
