package main

import (
	"net"
	"net/netip"
	"testing"
)

// cidrs は "192.168.1.7/24" の並びを net.Addr の一覧にする。
func cidrs(t *testing.T, ss ...string) []net.Addr {
	t.Helper()
	var out []net.Addr
	for _, s := range ss {
		ip, ipnet, err := net.ParseCIDR(s)
		if err != nil {
			t.Fatalf("ParseCIDR(%q): %v", s, err)
		}
		out = append(out, &net.IPNet{IP: ip, Mask: ipnet.Mask})
	}
	return out
}

func TestSelectV4(t *testing.T) {
	registrar := netip.MustParseAddr("192.168.1.1")

	tests := []struct {
		name    string
		addrs   []net.Addr
		reach   netip.Addr
		want    string
		wantErr bool
	}{
		{
			// 実機で踏んだ形。NM が付けた .7 を選び、リースの .8 は関係ない。
			name:  "レジストラと同じサブネット",
			addrs: cidrs(t, "192.168.1.7/24"),
			reach: registrar,
			want:  "192.168.1.7",
		},
		{
			name:  "複数から同じサブネットのものを選ぶ",
			addrs: cidrs(t, "10.0.0.5/8", "192.168.1.7/24"),
			reach: registrar,
			want:  "192.168.1.7",
		},
		{
			name:  "IPv6 が混ざっていても無視する",
			addrs: cidrs(t, "240b:10:dd83:c980::1/64", "192.168.1.7/24"),
			reach: registrar,
			want:  "192.168.1.7",
		},
		{
			// 届かないアドレスを黙って使うと bind は通っても REGISTER が届かない。
			name:    "同じサブネットが無ければエラー",
			addrs:   cidrs(t, "10.0.0.5/8"),
			reach:   registrar,
			wantErr: true,
		},
		{
			name:  "レジストラ不明ならとりあえず 1 つ選ぶ",
			addrs: cidrs(t, "10.0.0.5/8"),
			want:  "10.0.0.5",
		},
		{
			name:    "IPv4 が無ければエラー",
			addrs:   cidrs(t, "240b:10:dd83:c980::1/64"),
			reach:   registrar,
			wantErr: true,
		},
		{
			name:    "ループバックは選ばない",
			addrs:   cidrs(t, "127.0.0.1/8"),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := selectV4(tt.addrs, tt.reach)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("selectV4 はエラーになるべき (got %s)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("selectV4 が失敗した: %v", err)
			}
			if got.String() != tt.want {
				t.Errorf("selectV4 = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestParseMediaRoute(t *testing.T) {
	tests := []struct {
		in      string
		want    bool
		wantErr bool
	}{
		{in: "auto", want: true},
		{in: "AUTO", want: true},
		{in: " auto ", want: true},
		{in: "", want: true}, // 既定は auto
		{in: "off", want: false},
		{in: "on", wantErr: true}, // 「常に足す」は無いので受け付けない
		{in: "yes", wantErr: true},
	}

	for _, tt := range tests {
		got, err := parseMediaRoute(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("parseMediaRoute(%q) はエラーになるべき", tt.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseMediaRoute(%q) が失敗した: %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("parseMediaRoute(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
