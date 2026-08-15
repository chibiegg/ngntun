package sdp

import (
	"net/netip"
	"strings"
	"testing"
)

func TestOfferMarshalMatchesCapture(t *testing.T) {
	// 実機で受け付けられるオファーの形になること。
	o := &Offer{
		Addr:          netip.MustParseAddr("2001:db8:2:0:200:5eff:fe00:5311"),
		Port:          4502,
		BandwidthKbps: 64,
		Setup:         SetupActive,
		SessionID:     1,
		SessionVer:    1,
	}
	got := string(o.Marshal())

	for _, want := range []string{
		"c=IN IP6 2001:db8:2:0:200:5eff:fe00:5311\r\n",
		"m=application 4502 udp octet-stream\r\n",
		"b=AS:64\r\n",
		"a=udp-setup:active\r\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("オファーに %q が含まれていない:\n%s", want, got)
		}
	}
	if !strings.HasPrefix(got, "v=0\r\n") {
		t.Errorf("v=0 で始まっていない:\n%s", got)
	}
}

// 200 OK に載ってくるアンサー相当。メディア宛先は網内リレー。
const captureAnswer = "v=0\r\n" +
	"o=- 0 0 IN IP6 2001:db8:ffff::318\r\n" +
	"s=-\r\n" +
	"c=IN IP6 2001:db8:ffff::318\r\n" +
	"t=0 0\r\n" +
	"m=application 14672 udp octet-stream\r\n" +
	"b=AS:64\r\n" +
	"a=udp-setup:passive\r\n"

func TestParseAnswerFromCapture(t *testing.T) {
	a, err := ParseAnswer([]byte(captureAnswer))
	if err != nil {
		t.Fatalf("ParseAnswer: %v", err)
	}
	if got, want := a.AddrPort().String(), "[2001:db8:ffff::318]:14672"; got != want {
		t.Errorf("AddrPort = %s, want %s", got, want)
	}
	if a.BandwidthKbps != 64 {
		t.Errorf("BandwidthKbps = %d, want 64", a.BandwidthKbps)
	}
	if a.Setup != SetupPassive {
		t.Errorf("Setup = %q, want %q", a.Setup, SetupPassive)
	}
}

func TestParseAnswerPrefersMediaLevelConnection(t *testing.T) {
	body := "v=0\r\n" +
		"c=IN IP6 2001:db8:ffff::1\r\n" +
		"m=application 14672 udp octet-stream\r\n" +
		"c=IN IP6 2001:db8:ffff::318\r\n"
	a, err := ParseAnswer([]byte(body))
	if err != nil {
		t.Fatalf("ParseAnswer: %v", err)
	}
	if got, want := a.Addr.String(), "2001:db8:ffff::318"; got != want {
		t.Errorf("Addr = %s, want %s (メディアレベルの c= を優先すべき)", got, want)
	}
}

func TestParseAnswerSkipsOtherMedia(t *testing.T) {
	// 音声のメディア記述が先に並んでいても、そちらのポートを拾ってはいけない。
	body := "v=0\r\n" +
		"c=IN IP6 2001:db8:ffff::318\r\n" +
		"m=audio 5004 RTP/AVP 0\r\n" +
		"a=udp-setup:active\r\n" +
		"m=application 14672 udp octet-stream\r\n" +
		"a=udp-setup:passive\r\n"
	a, err := ParseAnswer([]byte(body))
	if err != nil {
		t.Fatalf("ParseAnswer: %v", err)
	}
	if a.Port != 14672 {
		t.Errorf("Port = %d, want 14672", a.Port)
	}
	if a.Setup != SetupPassive {
		t.Errorf("Setup = %q, want %q", a.Setup, SetupPassive)
	}
}

func TestParseAnswerErrors(t *testing.T) {
	cases := map[string]string{
		"メディア記述なし":   "v=0\r\nc=IN IP6 2001:db8:ffff::1\r\n",
		"ポート 0 (拒否)": "v=0\r\nc=IN IP6 2001:db8:ffff::1\r\nm=application 0 udp octet-stream\r\n",
		"c= 行なし":     "v=0\r\nm=application 14672 udp octet-stream\r\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseAnswer([]byte(body)); err == nil {
				t.Fatal("エラーを期待したが nil だった")
			}
		})
	}
}
