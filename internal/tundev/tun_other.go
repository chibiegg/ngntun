//go:build !linux

// Package tundev は tun デバイスの生成を行う。Linux 以外では動作しない。
// (開発機が macOS でもビルド・テストできるようにするためのスタブ)
package tundev

import (
	"fmt"
	"runtime"
)

// Device は開いた tun デバイス。
type Device struct {
	Name string
}

// Create は Linux 以外では常にエラーを返す。
func Create(name string) (*Device, error) {
	return nil, fmt.Errorf("tun デバイスの作成は Linux でのみ対応しています (現在: %s)", runtime.GOOS)
}

func (d *Device) Read(p []byte) (int, error)  { return 0, fmt.Errorf("未対応") }
func (d *Device) Write(p []byte) (int, error) { return 0, fmt.Errorf("未対応") }
func (d *Device) Close() error                { return nil }
