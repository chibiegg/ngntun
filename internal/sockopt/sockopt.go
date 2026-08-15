// Package sockopt は DSCP マーキングなどのソケットオプション設定をまとめる。
//
// SIP は EF (0xb8)、メディアは CS1 (0x20) を付けて送信する。
package sockopt

import (
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

// DSCP のトラフィッククラス値 (TOS / Traffic Class オクテットとしての値)。
const (
	DSCPEF  = 0xb8 // EF (46) — SIP シグナリング
	DSCPCS1 = 0x20 // CS1 (8) — データコネクトのメディア
)

// SetDSCP はソケットの送信パケットに DSCP を設定する。
func SetDSCP(c syscall.Conn, isV4 bool, tclass int) error {
	rc, err := c.SyscallConn()
	if err != nil {
		return fmt.Errorf("SyscallConn: %w", err)
	}
	var opErr error
	if err := rc.Control(func(fd uintptr) {
		if isV4 {
			opErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_TOS, tclass)
		} else {
			opErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_TCLASS, tclass)
		}
	}); err != nil {
		return fmt.Errorf("Control: %w", err)
	}
	if opErr != nil {
		return fmt.Errorf("DSCP (0x%02x) を設定できません: %w", tclass, opErr)
	}
	return nil
}
