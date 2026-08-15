//go:build linux

// Package netcfg は tun デバイスへのアドレス・経路の設定を netlink 経由で行う。
//
// 実施した操作はスタックに積み、Rollback で逆順に取り消す。
// 呼が終われば tun ごと消えるが、途中で失敗したときに中途半端な状態を残さないため。
package netcfg

import (
	"fmt"
	"log/slog"
	"net"
	"net/netip"

	"github.com/vishvananda/netlink"
)

// Config は tun に対して行うネットワーク設定。
type Config struct {
	Name   string         // tun デバイス名
	MTU    int            // 0 なら変更しない
	Addr   netip.Prefix   // 自機アドレス
	Peer   netip.Addr     // point-to-point の対向 (未指定可)
	Routes []netip.Prefix // このデバイスに向ける経路
	Table  int            // 経路を入れるテーブル (0 なら main)
}

// Applied は適用済みの設定と、その取り消し手順。
type Applied struct {
	log  *slog.Logger
	undo []undoStep
}

type undoStep struct {
	what string
	fn   func() error
}

// Apply は設定を適用する。途中で失敗した場合はそこまでの操作を巻き戻してエラーを返す。
func Apply(cfg Config, log *slog.Logger) (*Applied, error) {
	link, err := netlink.LinkByName(cfg.Name)
	if err != nil {
		return nil, fmt.Errorf("インタフェース %s が見つかりません: %w", cfg.Name, err)
	}

	a := &Applied{log: log}
	fail := func(err error) (*Applied, error) {
		a.Rollback()
		return nil, err
	}

	if cfg.MTU > 0 {
		if err := netlink.LinkSetMTU(link, cfg.MTU); err != nil {
			return fail(fmt.Errorf("MTU を %d に設定できません: %w", cfg.MTU, err))
		}
		log.Debug("MTU を設定しました", "dev", cfg.Name, "mtu", cfg.MTU)
	}

	if cfg.Addr.IsValid() {
		addr := &netlink.Addr{IPNet: toIPNet(cfg.Addr)}
		if cfg.Peer.IsValid() {
			addr.Peer = toIPNet(netip.PrefixFrom(cfg.Peer, cfg.Peer.BitLen()))
		}
		if err := netlink.AddrAdd(link, addr); err != nil {
			return fail(fmt.Errorf("アドレス %s を付与できません: %w", cfg.Addr, err))
		}
		a.push("アドレス "+cfg.Addr.String(), func() error { return netlink.AddrDel(link, addr) })
		log.Debug("アドレスを付与しました", "dev", cfg.Name, "addr", cfg.Addr, "peer", cfg.Peer)
	}

	if err := netlink.LinkSetUp(link); err != nil {
		return fail(fmt.Errorf("インタフェース %s を up にできません: %w", cfg.Name, err))
	}
	a.push("リンク up", func() error { return netlink.LinkSetDown(link) })

	for _, r := range cfg.Routes {
		route := &netlink.Route{
			LinkIndex: link.Attrs().Index,
			Dst:       toIPNet(r),
			Scope:     netlink.SCOPE_LINK,
			Table:     cfg.Table,
		}
		if err := netlink.RouteAdd(route); err != nil {
			return fail(fmt.Errorf("経路 %s を追加できません: %w", r, err))
		}
		a.push("経路 "+r.String(), func() error { return netlink.RouteDel(route) })
		log.Debug("経路を追加しました", "dev", cfg.Name, "dst", r, "table", cfg.Table)
	}

	return a, nil
}

// HostRoute はメディア宛先のような、tun ではなく WAN 側に足す単発のホスト経路。
type HostRoute struct {
	Dst netip.Addr // 宛先ホスト
	Via netip.Addr // ゲートウェイ。無効なら on-link 扱い
	Dev string     // 出力インタフェース
}

// EnsureHostRoute は Dst へ Dev 経由で到達できないときだけホスト経路を足す。
//
// メディアの宛先は NGN 網内のリレーで、本来は HGW が出す RA のデフォルト経路に乗る。
// ところが別にインターネット回線を持つ機器では HGW をデフォルトゲートウェイにできず、
// 宛先だけが経路表から漏れる。そこを呼の間だけ埋めるのがこの関数の役目。
//
// 既に到達できる場合は何もせず (nil, nil) を返す。返った *Applied は nil でも Rollback できる。
func EnsureHostRoute(r HostRoute, log *slog.Logger) (*Applied, error) {
	if !r.Dst.IsValid() {
		return nil, nil
	}
	if r.Dev == "" {
		// 出力インタフェースが分からないと、既存経路が正しい向きかを判断できない。
		log.Debug("メディア宛先の経路を確認できません (--wan-interface 未指定)", "dst", r.Dst)
		return nil, nil
	}

	link, err := netlink.LinkByName(r.Dev)
	if err != nil {
		return nil, fmt.Errorf("インタフェース %s が見つかりません: %w", r.Dev, err)
	}

	// メディアのソケットは SO_BINDTODEVICE で Dev に固定してあるので、
	// 経路の判定も同じ条件 (oif=Dev) で引く。デフォルト経路が別の
	// インタフェースにあっても「到達できる」と誤判定しないため。
	if _, err := netlink.RouteGetWithOptions(r.Dst.AsSlice(), &netlink.RouteGetOptions{Oif: r.Dev}); err == nil {
		log.Debug("メディア宛先は既に到達可能です", "dst", r.Dst, "dev", r.Dev)
		return nil, nil
	}

	route := &netlink.Route{
		LinkIndex: link.Attrs().Index,
		Dst:       toIPNet(netip.PrefixFrom(r.Dst, r.Dst.BitLen())),
	}
	if r.Via.IsValid() {
		route.Gw = net.IP(r.Via.AsSlice())
	} else {
		route.Scope = netlink.SCOPE_LINK
	}
	if err := netlink.RouteAdd(route); err != nil {
		return nil, fmt.Errorf("メディア宛先 %s への経路を追加できません: %w", r.Dst, err)
	}

	a := &Applied{log: log}
	a.push("メディア宛先への経路 "+r.Dst.String(), func() error { return netlink.RouteDel(route) })
	log.Info("メディア宛先への経路を追加しました", "dst", r.Dst, "via", r.Via, "dev", r.Dev)
	return a, nil
}

func (a *Applied) push(what string, fn func() error) {
	a.undo = append(a.undo, undoStep{what: what, fn: fn})
}

// Rollback は適用済みの設定を逆順に取り消す。失敗しても残りの取り消しは続行する。
func (a *Applied) Rollback() {
	if a == nil {
		return
	}
	for i := len(a.undo) - 1; i >= 0; i-- {
		step := a.undo[i]
		if err := step.fn(); err != nil {
			a.log.Warn("設定を取り消せませんでした", "what", step.what, "err", err)
			continue
		}
		a.log.Debug("設定を取り消しました", "what", step.what)
	}
	a.undo = nil
}

// Exists はインタフェースが既に存在するかを返す。
func Exists(name string) bool {
	_, err := netlink.LinkByName(name)
	return err == nil
}

// DeleteTun は残骸となった tun デバイスを削除する。
// 誤って別種のインタフェース (物理 NIC など) を消さないよう、tuntap 以外は拒否する。
func DeleteTun(name string) error {
	link, err := netlink.LinkByName(name)
	if err != nil {
		return fmt.Errorf("インタフェース %s が見つかりません: %w", name, err)
	}
	if link.Type() != "tuntap" {
		return fmt.Errorf("インタフェース %s は tun ではありません (type=%s)。削除を中止します", name, link.Type())
	}
	if err := netlink.LinkDel(link); err != nil {
		return fmt.Errorf("インタフェース %s を削除できません: %w", name, err)
	}
	return nil
}

func toIPNet(p netip.Prefix) *net.IPNet {
	return &net.IPNet{
		IP:   net.IP(p.Addr().AsSlice()),
		Mask: net.CIDRMask(p.Bits(), p.Addr().BitLen()),
	}
}
