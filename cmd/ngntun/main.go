// Command ngntun は NTT フレッツのデータコネクトを Linux から張るためのコマンド。
//
// ISC dhclient が書いたリース情報を読み、HGW へ SIP REGISTER したうえで相手の
// 電話番号へ発信し、確立したデータコネクトのフローと tun デバイスを橋渡しする。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/chibiegg/ngntun/internal/lease"
	"github.com/chibiegg/ngntun/internal/sdp"
	"github.com/chibiegg/ngntun/internal/session"
	"github.com/chibiegg/ngntun/internal/sip"
)

// 終了コード。運用スクリプトから理由を判別できるように分けてある。
const (
	exitOK           = 0
	exitUsage        = 1
	exitConfig       = 2
	exitRegister     = 3
	exitInvite       = 4
	exitMediaTimeout = 5
	exitRuntime      = 6
	exitAuthRequired = 7
)

// levelTrace は SIP メッセージ全文をダンプするレベル。
const levelTrace = slog.LevelDebug - 4

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

type options struct {
	peerNumber string
	selfNumber string

	leasePath  string
	lease4Path string
	sipServer  string
	registrar4 string
	sipDomain  string
	extension  string
	sourceAddr string
	sourceV4   string
	wanIface   string
	bindDevice string

	bandwidth string
	sipPort   int
	mediaPort int

	tunName    string
	tunAddr    string
	tunPeer    string
	tunMTU     int
	routes     stringList
	routeTable int

	mediaRoute   string
	mediaGateway string

	registerTransport string
	registerOnly      bool

	idleTimeout  time.Duration
	maxDuration  time.Duration
	mediaTimeout time.Duration
	answer       bool
	allowFrom    stringList
	strictPeer   bool
	force        bool

	logLevel  string
	logFormat string
	dryRun    bool

	command []string // トンネル確立後に実行するコマンド (フラグの後ろの位置引数)
}

func main() {
	os.Exit(run())
}

func run() int {
	opt := parseFlags()

	log, err := newLogger(opt)
	if err != nil {
		fmt.Fprintln(os.Stderr, "エラー:", err)
		return exitUsage
	}

	cfg, err := buildConfig(opt, log)
	if err != nil {
		log.Error("設定を組み立てられません", "err", err)
		return exitConfig
	}

	if opt.dryRun {
		printDryRun(cfg, opt)
		return exitOK
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	result, err := session.Run(ctx, *cfg)
	if result != nil {
		printSummary(result)
	}
	if err != nil {
		log.Error("セッションが失敗しました", "err", err)
		return exitCodeFor(err)
	}
	if result != nil && result.Reason == session.ReasonMediaTimeout {
		return exitMediaTimeout
	}
	// コマンドの終了が終了契機なら、その終了コードをそのまま引き継ぐ。
	// 呼が先に切れた場合 (無通信・最大保持など) は ngntun 自身の終了コードを優先する。
	if result != nil && result.Reason == session.ReasonCommandExit {
		return result.CommandExitCode
	}
	return exitOK
}

func parseFlags() *options {
	opt := &options{}

	flag.StringVar(&opt.peerNumber, "peer-number", "", "相手の電話番号 (発信モードでは必須)")
	flag.StringVar(&opt.selfNumber, "self-number", "", "発信者番号。P-Preferred-Identity に載せる")

	flag.StringVar(&opt.leasePath, "lease", "/run/ngntun/lease6.json", "DHCPv6 リース JSON のパス")
	flag.StringVar(&opt.lease4Path, "lease4", "/run/ngntun/lease4.json", "DHCPv4 リース JSON のパス (任意。無ければ --registrar4 を指定する)")
	flag.StringVar(&opt.sipServer, "sip-server", "", "呼制御の宛先 IPv6 (リースの値を上書き)")
	flag.StringVar(&opt.registrar4, "registrar4", "", "REGISTER の宛先 IPv4 (リースの値を上書き)")
	flag.StringVar(&opt.sipDomain, "sip-domain", "", "SIP ドメイン (リースの値を上書き)")
	flag.StringVar(&opt.extension, "extension", "", "内線番号 (リースの値を上書き)")
	flag.StringVar(&opt.sourceAddr, "source-addr", "", "呼制御・メディアの送信元 IPv6 (省略時は自動選択)")
	flag.StringVar(&opt.sourceV4, "source-addr4", "", "REGISTER の送信元 IPv4 (省略時は自動選択)")
	flag.StringVar(&opt.wanIface, "wan-interface", "", "送信元アドレスの自動選択に使うインタフェース名")
	flag.StringVar(&opt.bindDevice, "bind-device", "",
		"SIP/メディアのソケットをこのインタフェースに固定する (省略時は --wan-interface と同じ)")

	flag.StringVar(&opt.bandwidth, "bandwidth", "64k", "確保する帯域 (例: 64k, 128k, 1m)。SDP の b=AS に反映")
	flag.IntVar(&opt.sipPort, "sip-port", 5060, "自機の SIP ポート")
	flag.IntVar(&opt.mediaPort, "media-port", 0, "自機のメディアポート (0 で自動)")

	flag.StringVar(&opt.tunName, "tun-name", "dc0", "作成する tun デバイス名")
	flag.StringVar(&opt.tunAddr, "tun-addr", "", "tun に付与するアドレス (例: 172.21.0.100/30)")
	flag.StringVar(&opt.tunPeer, "tun-peer", "", "point-to-point の対向アドレス")
	flag.IntVar(&opt.tunMTU, "tun-mtu", 1452, "tun の MTU (外側 IPv6+UDP の 48 バイトを引いた値)")
	flag.Var(&opt.routes, "route", "この tun に向ける経路 (複数指定可)")
	flag.IntVar(&opt.routeTable, "route-table", 0, "経路を入れるテーブル (0 で main)")

	flag.StringVar(&opt.mediaRoute, "media-route", "auto",
		"メディア宛先への経路の扱い (auto|off)。auto は WAN 経由で到達できないときだけ足し、終了時に消す")
	flag.StringVar(&opt.mediaGateway, "media-gateway", "",
		"--media-route で足す経路のゲートウェイ (省略時は SIP サーバ = HGW)")

	flag.DurationVar(&opt.idleTimeout, "idle-timeout", 30*time.Second, "無通信でこの時間が経過したら切断 (0 で無効)")
	flag.DurationVar(&opt.maxDuration, "max-duration", 10*time.Minute, "呼の最大保持時間。従量課金への安全装置 (0 で無制限)")
	flag.DurationVar(&opt.mediaTimeout, "media-timeout", 10*time.Second, "確立後この時間メディアが来なければ切断 (0 で無効)")
	flag.StringVar(&opt.registerTransport, "register-transport", "ipv4",
		"REGISTER に使うトランスポート (ipv4|ipv6)。既定は ipv4")
	flag.BoolVar(&opt.registerOnly, "register-only", false,
		"登録だけして待機する (発信しないので課金されない。登録の疎通確認用)")
	flag.BoolVar(&opt.answer, "answer", false, "着信待ち受けモード")
	flag.Var(&opt.allowFrom, "allow-from", "着信を許可する発番号 (前方一致・複数指定可)")
	flag.BoolVar(&opt.strictPeer, "strict-peer-port", false, "メディアの送信元ポートも厳密に照合する")
	flag.BoolVar(&opt.force, "force", false, "同名の tun が残っていれば削除して作り直す")

	flag.StringVar(&opt.logLevel, "log-level", "info", "ログレベル (trace|debug|info|warn|error)")
	flag.StringVar(&opt.logFormat, "log-format", "text", "ログ形式 (text|json)")
	flag.BoolVar(&opt.dryRun, "dry-run", false, "SIP を送らず、解決した設定と生成予定の SDP を表示して終了")

	flag.Usage = usage
	flag.Parse()
	opt.command = flag.Args()
	return opt
}

func usage() {
	w := flag.CommandLine.Output()
	fmt.Fprintln(w, "使い方: ngntun [オプション] [-- コマンド [引数...]]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "コマンドを指定すると、トンネルが確立してから実行する。")
	fmt.Fprintln(w, "コマンドが終了した時点で呼を切り、ngntun もコマンドの終了コードで終了する。")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "オプション:")
	flag.PrintDefaults()
}

func newLogger(opt *options) (*slog.Logger, error) {
	var level slog.Level
	switch strings.ToLower(opt.logLevel) {
	case "trace":
		level = levelTrace
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		return nil, fmt.Errorf("不明なログレベルです: %q", opt.logLevel)
	}

	hopt := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	switch strings.ToLower(opt.logFormat) {
	case "text":
		h = slog.NewTextHandler(os.Stderr, hopt)
	case "json":
		h = slog.NewJSONHandler(os.Stderr, hopt)
	default:
		return nil, fmt.Errorf("不明なログ形式です: %q", opt.logFormat)
	}

	log := slog.New(h)
	slog.SetDefault(log)
	return log, nil
}

// buildConfig はリースとフラグからセッション設定を組み立てる。
func buildConfig(opt *options, log *slog.Logger) (*session.Config, error) {
	if !opt.answer && !opt.registerOnly && opt.peerNumber == "" {
		return nil, errors.New("--peer-number を指定してください (着信待ち受けなら --answer、登録確認だけなら --register-only)")
	}
	// --register-only はトンネルを張らないので、実行すべき瞬間が存在しない。
	if opt.registerOnly && len(opt.command) > 0 {
		return nil, errors.New("--register-only ではトンネルを張らないので、コマンドは実行できません")
	}

	var registerOverIPv6 bool
	switch strings.ToLower(opt.registerTransport) {
	case "ipv4", "4":
		registerOverIPv6 = false
	case "ipv6", "6":
		registerOverIPv6 = true
	default:
		return nil, fmt.Errorf("--register-transport は ipv4 か ipv6 です: %q", opt.registerTransport)
	}

	v6, err := lease.LoadV6(opt.leasePath)
	if err != nil {
		log.Warn("DHCPv6 リースを読めませんでした。フラグでの指定が必要です", "path", opt.leasePath, "err", err)
	}
	// DHCPv4 リースは任意。ここから使うのは REGISTER 先だけで、自機 IPv4 は
	// インタフェースから選ぶ。無いだけなら --registrar4 で足りるので黙って進み、
	// 壊れている場合だけ知らせる。足りなければ Validate が具体的に報告する。
	v4, err := lease.LoadV4(opt.lease4Path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Warn("DHCPv4 リースを読めませんでした。--registrar4 での指定が必要です", "path", opt.lease4Path, "err", err)
	}

	prov, err := lease.Build(v6, v4)
	if err != nil {
		return nil, err
	}
	if err := applyOverrides(prov, opt); err != nil {
		return nil, err
	}
	resolveSourceV4(opt, prov, log)
	if err := prov.Validate(); err != nil {
		return nil, err
	}

	selfV6, err := resolveSourceV6(opt, prov)
	if err != nil {
		return nil, err
	}

	bw, err := parseBandwidth(opt.bandwidth)
	if err != nil {
		return nil, err
	}

	mediaRoute, err := parseMediaRoute(opt.mediaRoute)
	if err != nil {
		return nil, err
	}
	// ゲートウェイは HGW。呼制御の宛先と同じなので、リースから来た値をそのまま使える。
	mediaGateway := prov.SIPServer
	if opt.mediaGateway != "" {
		a, err := netip.ParseAddr(opt.mediaGateway)
		if err != nil {
			return nil, fmt.Errorf("--media-gateway を解釈できません: %w", err)
		}
		mediaGateway = a
	}

	// 同じプレフィックスの直結経路が複数のインタフェースにある環境では、
	// 経路表任せだと HGW 宛が別のインタフェースへ出てしまう。WAN が分かっているなら固定する。
	bindDevice := opt.bindDevice
	if bindDevice == "" {
		bindDevice = opt.wanIface
	}

	cfg := &session.Config{
		SIP: sip.Config{
			Domain:           prov.SIPDomain,
			Extension:        prov.Extension,
			SelfNumber:       opt.selfNumber,
			Registrar:        netip.AddrPortFrom(prov.Registrar4, 5060),
			Proxy:            netip.AddrPortFrom(prov.SIPServer, 5060),
			LocalV4:          netip.AddrPortFrom(prov.SelfV4, uint16(opt.sipPort)),
			LocalV6:          netip.AddrPortFrom(selfV6, uint16(opt.sipPort)),
			UserAgent:        "ngntun",
			BindDevice:       bindDevice,
			RegisterOverIPv6: registerOverIPv6,
			Log:              log,
		},
		PeerNumber:     opt.peerNumber,
		Command:        opt.command,
		AnswerMode:     opt.answer,
		AllowFrom:      opt.allowFrom,
		RegisterOnly:   opt.registerOnly,
		SelfV6:         selfV6,
		MediaPort:      opt.mediaPort,
		BandwidthKbps:  bw,
		TunName:        opt.tunName,
		TunMTU:         opt.tunMTU,
		RouteTable:     opt.routeTable,
		Force:          opt.force,
		MediaRoute:     mediaRoute,
		MediaGateway:   mediaGateway,
		IdleTimeout:    opt.idleTimeout,
		MaxDuration:    opt.maxDuration,
		MediaTimeout:   opt.mediaTimeout,
		StrictPeerPort: opt.strictPeer,
		BindDevice:     bindDevice,
		Log:            log,
	}

	if opt.tunAddr != "" {
		p, err := netip.ParsePrefix(opt.tunAddr)
		if err != nil {
			return nil, fmt.Errorf("--tun-addr を解釈できません: %w", err)
		}
		cfg.TunAddr = p
	}
	if opt.tunPeer != "" {
		a, err := netip.ParseAddr(opt.tunPeer)
		if err != nil {
			return nil, fmt.Errorf("--tun-peer を解釈できません: %w", err)
		}
		cfg.TunPeer = a
	}
	for _, r := range opt.routes {
		p, err := netip.ParsePrefix(r)
		if err != nil {
			return nil, fmt.Errorf("--route %q を解釈できません: %w", r, err)
		}
		cfg.Routes = append(cfg.Routes, p)
	}

	return cfg, nil
}

// applyOverrides はフラグでリースの値を上書きする。
func applyOverrides(p *lease.Provisioning, opt *options) error {
	if opt.sipServer != "" {
		a, err := netip.ParseAddr(opt.sipServer)
		if err != nil {
			return fmt.Errorf("--sip-server を解釈できません: %w", err)
		}
		p.SIPServer = a
	}
	if opt.registrar4 != "" {
		a, err := netip.ParseAddr(opt.registrar4)
		if err != nil {
			return fmt.Errorf("--registrar4 を解釈できません: %w", err)
		}
		p.Registrar4 = a
	}
	if opt.sourceV4 != "" {
		a, err := netip.ParseAddr(opt.sourceV4)
		if err != nil {
			return fmt.Errorf("--source-addr4 を解釈できません: %w", err)
		}
		p.SelfV4 = a
	}
	if opt.sipDomain != "" {
		p.SIPDomain = opt.sipDomain
	}
	if opt.extension != "" {
		p.Extension = opt.extension
	}
	if opt.wanIface != "" {
		p.Interface = opt.wanIface
	}
	return nil
}

// resolveSourceV4 は REGISTER に使う送信元 IPv4 を決める。
//
// 優先するのは --wan-interface に実際に付いているアドレスで、リースの ip_address は
// フォールバックに留める。リースを書いた DHCP クライアントと、インタフェースに
// アドレスを付けている主体 (NetworkManager など) が別だと両者は一致せず、
// リースの値を信じると bind: cannot assign requested address になるため。
// --source-addr4 が指定されていれば、それを最優先で尊重する。
func resolveSourceV4(opt *options, p *lease.Provisioning, log *slog.Logger) {
	if opt.sourceV4 != "" {
		return // applyOverrides が設定済み
	}

	addr, err := pickSourceV4(p.Interface, p.Registrar4)
	if err != nil {
		// リースの値が残っていればそれで続行する。無ければ Validate が報告する。
		if p.SelfV4.IsValid() {
			log.Debug("送信元 IPv4 を自動選択できないので、リースの値を使います", "addr", p.SelfV4, "err", err)
		}
		return
	}
	if p.SelfV4.IsValid() && p.SelfV4 != addr {
		log.Info("リースの IPv4 はインタフェースに付いていないので、実際のアドレスを使います",
			"lease", p.SelfV4, "interface", p.Interface, "addr", addr)
	}
	p.SelfV4 = addr
}

// pickSourceV4 はインタフェース上の IPv4 から送信元を選ぶ。
// レジストラ (HGW) へ届くこと、つまり同じサブネットにあることを条件にする。
func pickSourceV4(ifname string, reach netip.Addr) (netip.Addr, error) {
	if ifname == "" {
		return netip.Addr{}, errors.New("インタフェースが分かりません")
	}
	ifi, err := net.InterfaceByName(ifname)
	if err != nil {
		return netip.Addr{}, err
	}
	if ifi.Flags&net.FlagUp == 0 {
		return netip.Addr{}, fmt.Errorf("%s は up していません", ifname)
	}
	addrs, err := ifi.Addrs()
	if err != nil {
		return netip.Addr{}, err
	}
	addr, err := selectV4(addrs, reach)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("%s: %w", ifname, err)
	}
	return addr, nil
}

// selectV4 はアドレスの一覧から reach へ届くものを選ぶ。pickSourceV4 の判定部分。
func selectV4(addrs []net.Addr, reach netip.Addr) (netip.Addr, error) {
	var fallback netip.Addr
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		addr, ok := netip.AddrFromSlice(ipnet.IP)
		if !ok {
			continue
		}
		addr = addr.Unmap()
		if !addr.Is4() || !addr.IsGlobalUnicast() {
			continue
		}
		ones, bits := ipnet.Mask.Size()
		if bits == 32 && reach.IsValid() {
			if netip.PrefixFrom(addr, ones).Masked().Contains(reach) {
				return addr, nil
			}
		}
		if !fallback.IsValid() {
			fallback = addr
		}
	}

	if reach.IsValid() && fallback.IsValid() {
		return netip.Addr{}, fmt.Errorf("%s と同じサブネットのアドレスがありません (候補: %s)", reach, fallback)
	}
	if fallback.IsValid() {
		return fallback, nil
	}
	return netip.Addr{}, errors.New("IPv4 アドレスがありません")
}

// resolveSourceV6 は呼制御とメディアに使う送信元 IPv6 を決める。
func resolveSourceV6(opt *options, p *lease.Provisioning) (netip.Addr, error) {
	if opt.sourceAddr != "" {
		a, err := netip.ParseAddr(opt.sourceAddr)
		if err != nil {
			return netip.Addr{}, fmt.Errorf("--source-addr を解釈できません: %w", err)
		}
		if !a.Is6() {
			return netip.Addr{}, fmt.Errorf("--source-addr は IPv6 である必要があります: %s", a)
		}
		return a, nil
	}

	addr, err := pickSourceV6(p.Interface, p.Delegated)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("送信元 IPv6 を自動選択できません (--source-addr で指定してください): %w", err)
	}
	return addr, nil
}

// pickSourceV6 はインタフェース上のグローバル IPv6 から送信元を選ぶ。
// 委譲プレフィックスが分かっていれば、その範囲内のものを優先する。
func pickSourceV6(ifname string, within netip.Prefix) (netip.Addr, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return netip.Addr{}, err
	}

	var fallback netip.Addr
	for _, ifi := range ifaces {
		if ifname != "" && ifi.Name != ifname {
			continue
		}
		if ifi.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			addr, ok := netip.AddrFromSlice(ipnet.IP)
			if !ok {
				continue
			}
			addr = addr.Unmap()
			if !addr.Is6() || !addr.IsGlobalUnicast() || addr.IsPrivate() {
				continue
			}
			if within.IsValid() && within.Contains(addr) {
				return addr, nil
			}
			if !fallback.IsValid() {
				fallback = addr
			}
		}
	}

	if within.IsValid() && fallback.IsValid() {
		return netip.Addr{}, fmt.Errorf("委譲プレフィックス %s 内のアドレスが見つかりません (候補: %s)", within, fallback)
	}
	if fallback.IsValid() {
		return fallback, nil
	}
	return netip.Addr{}, errors.New("グローバル IPv6 アドレスが見つかりません")
}

// parseMediaRoute は --media-route の値を解釈する。
func parseMediaRoute(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "auto", "":
		return true, nil
	case "off":
		return false, nil
	default:
		return false, fmt.Errorf("--media-route は auto か off です: %q", s)
	}
}

// parseBandwidth は "64k" / "1m" / "128" を kbps の数値にする。
func parseBandwidth(s string) (int, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 64, nil
	}
	mult := 1
	switch {
	case strings.HasSuffix(s, "k"):
		s = strings.TrimSuffix(s, "k")
	case strings.HasSuffix(s, "m"):
		s = strings.TrimSuffix(s, "m")
		mult = 1000
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("--bandwidth を解釈できません: %q (例: 64k, 128k, 1m)", s)
	}
	return n * mult, nil
}

func printDryRun(cfg *session.Config, opt *options) {
	offer := &sdp.Offer{
		Addr:          cfg.SelfV6,
		Port:          opt.mediaPort,
		BandwidthKbps: cfg.BandwidthKbps,
		Setup:         sdp.SetupActive,
		SessionID:     1,
		SessionVer:    1,
	}
	if cfg.AnswerMode {
		offer.Setup = sdp.SetupPassive
	}

	w := os.Stdout
	fmt.Fprintln(w, "# 解決した設定")
	fmt.Fprintf(w, "SIP ドメイン        : %s\n", cfg.SIP.Domain)
	fmt.Fprintf(w, "内線番号            : %s\n", cfg.SIP.Extension)
	fmt.Fprintf(w, "発信者番号          : %s\n", orNone(cfg.SIP.SelfNumber))
	fmt.Fprintf(w, "REGISTER 先 (IPv4)  : %s\n", cfg.SIP.Registrar)
	fmt.Fprintf(w, "呼制御の Route 先   : %s\n", cfg.SIP.Proxy)
	fmt.Fprintf(w, "自機 SIP (IPv4)     : %s\n", cfg.SIP.LocalV4)
	fmt.Fprintf(w, "自機 SIP (IPv6)     : %s\n", cfg.SIP.LocalV6)
	fmt.Fprintf(w, "相手番号            : %s\n", orNone(cfg.PeerNumber))
	fmt.Fprintf(w, "tun                 : %s (MTU %d, addr %s, peer %s)\n",
		cfg.TunName, cfg.TunMTU, orNone(cfg.TunAddr.String()), orNone(cfg.TunPeer.String()))
	fmt.Fprintf(w, "経路                : %s\n", orNone(joinPrefixes(cfg.Routes)))
	fmt.Fprintf(w, "メディア宛先の経路  : %s\n", mediaRouteDesc(cfg))
	fmt.Fprintf(w, "確立後のコマンド    : %s\n", orNone(strings.Join(cfg.Command, " ")))
	fmt.Fprintf(w, "無通信切断 / 最大保持: %s / %s\n", cfg.IdleTimeout, cfg.MaxDuration)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "# 送信予定の SDP オファー (メディアポートは実行時に確定)")
	fmt.Fprint(w, strings.ReplaceAll(string(offer.Marshal()), "\r\n", "\n"))
}

// mediaRouteDesc は dry-run に出すメディア宛先の経路の扱い。
func mediaRouteDesc(cfg *session.Config) string {
	if !cfg.MediaRoute {
		return "off"
	}
	if cfg.BindDevice == "" {
		return "auto (--wan-interface 未指定のため何もしない)"
	}
	return fmt.Sprintf("auto (到達できなければ via %s dev %s)", cfg.MediaGateway, cfg.BindDevice)
}

func printSummary(r *session.Result) {
	s := r.Media
	w := os.Stderr
	fmt.Fprintf(w, "\nセッション概要: peer=%s duration=%s reason=%s\n", orNone(r.PeerNumber), r.Duration.Round(time.Millisecond), r.Reason)
	fmt.Fprintf(w, "  送信: %d パケット / %d バイト (%s)\n", s.TxPackets, s.TxBytes, rate(s.TxBytes, r.Duration))
	fmt.Fprintf(w, "  受信: %d パケット / %d バイト (%s)\n", s.RxPackets, s.RxBytes, rate(s.RxBytes, r.Duration))
	fmt.Fprintf(w, "  破棄: 想定外の送信元=%d 不正なパケット=%d tun 書き込みエラー=%d\n",
		s.DropUnexpectedSrc, s.DropMalformed, s.TunWriteErrors)
	fmt.Fprintf(w, "  メディア宛先: %s\n", r.Remote)
	if r.CommandRun {
		fmt.Fprintf(w, "  コマンド終了コード: %d\n", r.CommandExitCode)
	}
}

func rate(bytes uint64, d time.Duration) string {
	if d <= 0 {
		return "0 bps"
	}
	bps := float64(bytes*8) / d.Seconds()
	switch {
	case bps >= 1_000_000:
		return fmt.Sprintf("平均 %.2f Mbps", bps/1_000_000)
	case bps >= 1_000:
		return fmt.Sprintf("平均 %.2f kbps", bps/1_000)
	default:
		return fmt.Sprintf("平均 %.0f bps", bps)
	}
}

func exitCodeFor(err error) int {
	if errors.Is(err, sip.ErrAuthRequired) {
		return exitAuthRequired
	}
	var se *sip.StatusError
	if errors.As(err, &se) {
		switch se.Method {
		case sip.MethodRegister:
			return exitRegister
		case sip.MethodInvite:
			return exitInvite
		}
	}
	if strings.Contains(err.Error(), "REGISTER") {
		return exitRegister
	}
	if strings.Contains(err.Error(), "INVITE") {
		return exitInvite
	}
	return exitRuntime
}

func orNone(s string) string {
	if s == "" || s == "invalid Prefix" || s == "invalid IP" {
		return "(なし)"
	}
	return s
}

func joinPrefixes(ps []netip.Prefix) string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.String())
	}
	return strings.Join(out, ", ")
}
