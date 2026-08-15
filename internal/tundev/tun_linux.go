//go:build linux

// Package tundev は tun デバイスの生成を行う。
//
// IFF_NO_PI を付けるので、Read/Write でやり取りするのは生の IP パケットそのもの。
// データコネクトのメディアは「UDP ペイロード = 生 IP パケット」なので、
// tun から読んだバイト列をそのまま UDP で送ればよい。
package tundev

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Device は開いた tun デバイス。
type Device struct {
	f    *os.File
	Name string
}

// ifreq は TUNSETIFF に渡す構造体 (name[IFNAMSIZ] + flags + パディング)。
type ifreq struct {
	name  [unix.IFNAMSIZ]byte
	flags uint16
	_     [22]byte
}

// Create は指定名の tun デバイスを作る。CAP_NET_ADMIN が必要。
func Create(name string) (*Device, error) {
	if name == "" {
		return nil, fmt.Errorf("tun デバイス名が空です")
	}
	if len(name) >= unix.IFNAMSIZ {
		return nil, fmt.Errorf("tun デバイス名が長すぎます (最大 %d 文字): %q", unix.IFNAMSIZ-1, name)
	}

	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("/dev/net/tun を開けません (CAP_NET_ADMIN が必要です): %w", err)
	}

	var req ifreq
	copy(req.name[:], name)
	req.flags = unix.IFF_TUN | unix.IFF_NO_PI

	if _, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		uintptr(fd),
		uintptr(unix.TUNSETIFF),
		uintptr(unsafe.Pointer(&req)),
	); errno != 0 {
		unix.Close(fd)
		if errno == unix.EBUSY {
			return nil, fmt.Errorf("tun デバイス %q は既に使われています", name)
		}
		return nil, fmt.Errorf("tun デバイス %q を作成できません: %w", name, errno)
	}

	return &Device{f: os.NewFile(uintptr(fd), "/dev/net/tun"), Name: name}, nil
}

func (d *Device) Read(p []byte) (int, error)  { return d.f.Read(p) }
func (d *Device) Write(p []byte) (int, error) { return d.f.Write(p) }

// Close は tun を閉じる。プロセスが最後の参照なら、カーネルがデバイスごと削除する。
func (d *Device) Close() error { return d.f.Close() }
