//go:build !linux

package sockopt

import (
	"fmt"
	"runtime"
	"syscall"
)

// BindToDevice は Linux 以外では対応していない。
func BindToDevice(c syscall.Conn, ifname string) error {
	if ifname == "" {
		return nil
	}
	return fmt.Errorf("インタフェースへの bind は Linux でのみ対応しています (現在: %s)", runtime.GOOS)
}
