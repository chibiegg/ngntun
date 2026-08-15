package lease

import (
	"strings"
	"testing"
)

// HGW が配布するベンダオプションの実例。
// MAC は例示用の値 (RFC 7042 の文書用アドレス) に置き換えてある。
//
//	enterprise = 210
//	code 201 len 6  = クライアント MAC 00:00:5e:00:53:11
//	code 202 len 1  = "3" (内線番号)
//	code 204 len 16 = "ntt-east.ne.jp" (DNS ワイヤ形式)
const captureVendorOpts = "00:00:00:d2:" +
	"00:c9:00:06:00:00:5e:00:53:11:" +
	"00:ca:00:01:33:" +
	"00:cc:00:10:08:6e:74:74:2d:65:61:73:74:02:6e:65:02:6a:70:00"

func TestDecodeVendorOptsFromCapture(t *testing.T) {
	vo, err := DecodeVendorOpts(captureVendorOpts)
	if err != nil {
		t.Fatalf("DecodeVendorOpts: %v", err)
	}
	if vo.Enterprise != NTTEnterpriseNumber {
		t.Errorf("Enterprise = %d, want %d", vo.Enterprise, NTTEnterpriseNumber)
	}
	if got, want := vo.ClientMAC.String(), "00:00:5e:00:53:11"; got != want {
		t.Errorf("ClientMAC = %q, want %q", got, want)
	}
	if got, want := vo.Extension, "3"; got != want {
		t.Errorf("Extension = %q, want %q", got, want)
	}
	if got, want := vo.SIPDomain, "ntt-east.ne.jp"; got != want {
		t.Errorf("SIPDomain = %q, want %q", got, want)
	}
	if len(vo.Unknown) != 0 {
		t.Errorf("Unknown = %v, want empty", vo.Unknown)
	}
}

func TestDecodeVendorOptsUnknownSubOption(t *testing.T) {
	// 未知のサブオプション (code 250) が混ざっても、後続を読み飛ばして処理を続けられること。
	in := "00:00:00:d2:" +
		"00:fa:00:02:de:ad:" +
		"00:ca:00:01:33"
	vo, err := DecodeVendorOpts(in)
	if err != nil {
		t.Fatalf("DecodeVendorOpts: %v", err)
	}
	if vo.Extension != "3" {
		t.Errorf("Extension = %q, want %q", vo.Extension, "3")
	}
	if got, ok := vo.Unknown[250]; !ok || len(got) != 2 {
		t.Errorf("Unknown[250] = %v, want 2 バイト", got)
	}
}

func TestDecodeVendorOptsTruncated(t *testing.T) {
	// 長さフィールドが実データより長い場合はエラーにする (黙って握りつぶさない)。
	in := "00:00:00:d2:00:ca:00:08:33"
	if _, err := DecodeVendorOpts(in); err == nil {
		t.Fatal("エラーを期待したが nil だった")
	}
}

func TestDecodeHexishForms(t *testing.T) {
	cases := map[string]string{
		"コロン区切り":   "00:c9:00:06",
		"先頭 0 の省略": "0:c9:0:6",
		"コロンなし":    "00c9000 6",
		"引用符付き":    `"00:c9:00:06"`,
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			b, err := decodeHexish(in)
			if err != nil {
				t.Fatalf("decodeHexish(%q): %v", in, err)
			}
			if len(b) != 4 || b[1] != 0xc9 || b[3] != 0x06 {
				t.Errorf("decodeHexish(%q) = % x", in, b)
			}
		})
	}
}

func TestBuildFromLeases(t *testing.T) {
	v6 := &V6{
		Interface:           "eth0",
		IP6Prefix:           "2001:db8:2::/60",
		SIPServersAddresses: "2001:db8:1:0:200:5eff:fe00:5322",
		VendorOpts:          captureVendorOpts,
	}
	v4 := &V4{
		Interface: "eth0",
		IPAddress: "192.168.1.3",
		Routers:   "192.168.1.1",
	}

	p, err := Build(v6, v4)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got, want := p.SIPServer.String(), "2001:db8:1:0:200:5eff:fe00:5322"; got != want {
		t.Errorf("SIPServer = %s, want %s", got, want)
	}
	if got, want := p.Registrar4.String(), "192.168.1.1"; got != want {
		t.Errorf("Registrar4 = %s, want %s", got, want)
	}
	if got, want := p.SelfV4.String(), "192.168.1.3"; got != want {
		t.Errorf("SelfV4 = %s, want %s", got, want)
	}
	if got, want := p.Delegated.String(), "2001:db8:2::/60"; got != want {
		t.Errorf("Delegated = %s, want %s", got, want)
	}
	if p.SIPDomain != "ntt-east.ne.jp" || p.Extension != "3" {
		t.Errorf("SIPDomain/Extension = %q/%q", p.SIPDomain, p.Extension)
	}
	if err := p.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestValidateReportsMissingFields(t *testing.T) {
	p := &Provisioning{}
	err := p.Validate()
	if err == nil {
		t.Fatal("エラーを期待したが nil だった")
	}
	// 不足項目はまとめて 1 度に報告する (1 つずつ潰させない)。
	for _, want := range []string{"SIP サーバ", "レジストラ IPv4", "内線番号"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("エラーメッセージに %q が含まれていない: %v", want, err)
		}
	}
}
