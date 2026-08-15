package sip

import (
	"strings"
	"testing"
)

// 実機で受け付けられる INVITE の形。
// アドレスと電話番号は例示用の値 (RFC 3849 の 2001:db8::/32 など) に置き換えてある。
const captureInvite = "INVITE sip:03YYYYYYYY@ntt-east.ne.jp SIP/2.0\r\n" +
	"Via: SIP/2.0/UDP [2001:db8:2:0:200:5eff:fe00:5311]:5060;branch=z9hG4bK1234abcd;rport\r\n" +
	"Route: <sip:[2001:db8:1:0:200:5eff:fe00:5322];lr>\r\n" +
	"Max-Forwards: 70\r\n" +
	"From: <sip:3@ntt-east.ne.jp>;tag=8f3c1a\r\n" +
	"To: <sip:03YYYYYYYY@ntt-east.ne.jp>\r\n" +
	"Call-ID: 0123456789abcdef@2001:db8:2:0:200:5eff:fe00:5311\r\n" +
	"CSeq: 1 INVITE\r\n" +
	"Contact: <sip:3@[2001:db8:2:0:200:5eff:fe00:5311]:5060>\r\n" +
	"P-Preferred-Identity: <sip:03XXXXXXXX@ntt-east.ne.jp>\r\n" +
	"Supported: 100rel,timer\r\n" +
	"Session-Expires: 300\r\n" +
	"Content-Type: application/sdp\r\n" +
	"Content-Length: 8\r\n" +
	"\r\n" +
	"v=0\r\ns=-"

func TestParseInviteFromCapture(t *testing.T) {
	m, err := ParseMessage([]byte(captureInvite))
	if err != nil {
		t.Fatalf("ParseMessage: %v", err)
	}
	if m.Response {
		t.Fatal("リクエストとして解釈されるべき")
	}
	if m.Method != MethodInvite {
		t.Errorf("Method = %q", m.Method)
	}
	if m.RequestURI != "sip:03YYYYYYYY@ntt-east.ne.jp" {
		t.Errorf("RequestURI = %q", m.RequestURI)
	}
	if cseq, method := m.CSeq(); cseq != 1 || method != MethodInvite {
		t.Errorf("CSeq = %d %s", cseq, method)
	}
	if got, want := m.Branch(), "z9hG4bK1234abcd"; got != want {
		t.Errorf("Branch = %q, want %q", got, want)
	}
	if got, want := Param(m.Headers.Get("From"), "tag"), "8f3c1a"; got != want {
		t.Errorf("From tag = %q, want %q", got, want)
	}
	if got, want := string(m.Body), "v=0\r\ns=-"; got != want {
		t.Errorf("Body = %q, want %q", got, want)
	}
}

func TestParse200OKWithMultipleVia(t *testing.T) {
	// 網を経由すると Via と Record-Route が複数積まれて返ってくる。
	raw := "SIP/2.0 200 OK\r\n" +
		"Via: SIP/2.0/UDP [2001:db8:2::1]:5060;branch=z9hG4bKaaa\r\n" +
		"Via: SIP/2.0/UDP [2001:db8:ffff::2]:5060;branch=z9hG4bKbbb\r\n" +
		"Record-Route: <sip:[2001:db8:ffff::2];lr>\r\n" +
		"Record-Route: <sip:[2001:db8:1::1];lr>\r\n" +
		"To: <sip:03YYYYYYYY@ntt-east.ne.jp>;tag=remote99\r\n" +
		"Contact: <sip:03YYYYYYYY@[2001:db8:ffff::2]:5060>\r\n" +
		"Session-Expires: 300;refresher=uas\r\n" +
		"Content-Length: 0\r\n\r\n"

	m, err := ParseMessage([]byte(raw))
	if err != nil {
		t.Fatalf("ParseMessage: %v", err)
	}
	if !m.IsSuccess() {
		t.Errorf("2xx として扱われていない: %d", m.Status)
	}
	if got := m.Headers.Values("Via"); len(got) != 2 {
		t.Errorf("Via の本数 = %d, want 2", len(got))
	}
	if got := m.Headers.Values("Record-Route"); len(got) != 2 {
		t.Errorf("Record-Route の本数 = %d, want 2", len(got))
	}
	if got, want := Param(m.Headers.Get("To"), "tag"), "remote99"; got != want {
		t.Errorf("To tag = %q, want %q", got, want)
	}
	se, refresher := parseSessionExpires(m, 0)
	if se.Seconds() != 300 || refresher != "uas" {
		t.Errorf("Session-Expires = %s / %q, want 300s / uas", se, refresher)
	}
}

func TestParseHeaderFolding(t *testing.T) {
	raw := "SIP/2.0 200 OK\r\n" +
		"Supported: 100rel,\r\n" +
		" timer\r\n" +
		"Content-Length: 0\r\n\r\n"
	m, err := ParseMessage([]byte(raw))
	if err != nil {
		t.Fatalf("ParseMessage: %v", err)
	}
	if got, want := m.Headers.Get("Supported"), "100rel, timer"; got != want {
		t.Errorf("Supported = %q, want %q", got, want)
	}
}

func TestParseCompactHeaderForms(t *testing.T) {
	raw := "SIP/2.0 200 OK\r\n" +
		"i: call-123\r\n" +
		"f: <sip:3@ntt-east.ne.jp>;tag=abc\r\n" +
		"l: 0\r\n\r\n"
	m, err := ParseMessage([]byte(raw))
	if err != nil {
		t.Fatalf("ParseMessage: %v", err)
	}
	if got, want := m.CallID(), "call-123"; got != want {
		t.Errorf("Call-ID = %q, want %q", got, want)
	}
	if got, want := Param(m.Headers.Get("From"), "tag"), "abc"; got != want {
		t.Errorf("From tag = %q, want %q", got, want)
	}
}

func TestMarshalFixesContentLength(t *testing.T) {
	m := &Message{Method: MethodInvite, RequestURI: "sip:x@y"}
	m.Headers.Add("Content-Length", "999") // わざと嘘の値を入れる
	m.Body = []byte("v=0")

	out := string(m.Marshal())
	if !strings.Contains(out, "Content-Length: 3\r\n") {
		t.Errorf("Content-Length が実体で上書きされていない:\n%s", out)
	}
	if strings.Contains(out, "999") {
		t.Errorf("古い Content-Length が残っている:\n%s", out)
	}
}

func TestAddrPortFromURI(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"sip:[2001:db8:1::1];lr", "[2001:db8:1::1]:5060"},
		{"sip:[2001:db8:ffff::2]:5070", "[2001:db8:ffff::2]:5070"},
		{"sip:3@192.168.1.1:5060", "192.168.1.1:5060"},
		{"sip:192.168.1.1", "192.168.1.1:5060"},
	}
	for _, c := range cases {
		got, err := AddrPortFromURI(c.in, 5060)
		if err != nil {
			t.Errorf("AddrPortFromURI(%q): %v", c.in, err)
			continue
		}
		if got.String() != c.want {
			t.Errorf("AddrPortFromURI(%q) = %s, want %s", c.in, got, c.want)
		}
	}

	// FQDN は名前解決しない方針なのでエラーになること。
	if _, err := AddrPortFromURI("sip:ntt-east.ne.jp", 5060); err == nil {
		t.Error("FQDN はエラーになるべき")
	}
}

func TestHeadersSetReplacesAllDuplicates(t *testing.T) {
	var h Headers
	h.Add("Contact", "<sip:a>")
	h.Add("Contact", "<sip:b>")
	h.Set("Contact", "<sip:c>")

	if got := h.Values("Contact"); len(got) != 1 || got[0] != "<sip:c>" {
		t.Errorf("Contact = %v, want [<sip:c>]", got)
	}
}
