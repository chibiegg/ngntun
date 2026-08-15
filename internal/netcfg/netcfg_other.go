//go:build !linux

// Package netcfg は tun デバイスへのアドレス・経路の設定を netlink 経由で行う。
// Linux 以外では動作しない (開発機でのビルド・テスト用スタブ)。
package netcfg

import (
	"fmt"
	"log/slog"
	"net/netip"
	"runtime"
)

// Config は tun に対して行うネットワーク設定。
type Config struct {
	Name   string
	MTU    int
	Addr   netip.Prefix
	Peer   netip.Addr
	Routes []netip.Prefix
	Table  int
}

// Applied は適用済みの設定と、その取り消し手順。
type Applied struct{}

// Apply は Linux 以外では常にエラーを返す。
func Apply(cfg Config, log *slog.Logger) (*Applied, error) {
	return nil, fmt.Errorf("ネットワーク設定は Linux でのみ対応しています (現在: %s)", runtime.GOOS)
}

// HostRoute はメディア宛先のような、tun ではなく WAN 側に足す単発のホスト経路。
type HostRoute struct {
	Dst netip.Addr
	Via netip.Addr
	Dev string
}

// EnsureHostRoute は Linux 以外では常にエラーを返す。
func EnsureHostRoute(r HostRoute, log *slog.Logger) (*Applied, error) {
	return nil, fmt.Errorf("経路の追加は Linux でのみ対応しています (現在: %s)", runtime.GOOS)
}

// Rollback は何もしない。
func (a *Applied) Rollback() {}

// Exists は常に false を返す。
func Exists(name string) bool { return false }

// DeleteTun は Linux 以外では常にエラーを返す。
func DeleteTun(name string) error {
	return fmt.Errorf("tun の削除は Linux でのみ対応しています (現在: %s)", runtime.GOOS)
}
