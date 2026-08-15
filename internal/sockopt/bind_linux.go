//go:build linux

package sockopt

import (
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

// BindToDevice はソケットを特定のインタフェースに結び付ける (SO_BINDTODEVICE)。
//
// 同じプレフィックスの直結経路が複数のインタフェースに存在する多ホーム環境では、
// 経路表任せだと HGW 宛のパケットが別のインタフェースへ出てしまう。
// 送信先の選択を経路表に委ねず、確実に WAN 側から出すために使う。
//
// CAP_NET_RAW が必要。
func BindToDevice(c syscall.Conn, ifname string) error {
	if ifname == "" {
		return nil
	}
	rc, err := c.SyscallConn()
	if err != nil {
		return fmt.Errorf("SyscallConn: %w", err)
	}
	var opErr error
	if err := rc.Control(func(fd uintptr) {
		opErr = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, ifname)
	}); err != nil {
		return fmt.Errorf("Control: %w", err)
	}
	if opErr != nil {
		return fmt.Errorf("インタフェース %s に bind できません (CAP_NET_RAW が必要です): %w", ifname, opErr)
	}
	return nil
}
