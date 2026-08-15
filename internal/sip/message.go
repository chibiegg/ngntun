// Package sip はデータコネクトの発着信に必要な最小限の SIP UA を実装する。
//
// 相手は HGW 1 機種、ダイアログは常に 1 本、認証なし、という前提に絞ってあり、
// 汎用の SIP スタックではない。対応するのは REGISTER / INVITE / ACK / BYE /
// UPDATE / OPTIONS / PRACK のみ。
package sip

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

const crlf = "\r\n"

// Header は 1 本のヘッダ行。順序を保つためにスライスで保持する。
type Header struct {
	Name  string
	Value string
}

// Headers はヘッダの並び。同名ヘッダ (Contact など) を複数持てる。
type Headers []Header

// compactForms は SIP のコンパクト形式。HGW がどちらで送ってきても拾えるようにする。
var compactForms = map[string]string{
	"v": "via",
	"f": "from",
	"t": "to",
	"i": "call-id",
	"m": "contact",
	"l": "content-length",
	"c": "content-type",
	"k": "supported",
	"s": "subject",
	"e": "content-encoding",
	"r": "refer-to",
}

func canon(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	if full, ok := compactForms[n]; ok {
		return full
	}
	return n
}

// Get は最初に現れた同名ヘッダの値を返す。存在しなければ空文字列。
func (h Headers) Get(name string) string {
	c := canon(name)
	for _, hd := range h {
		if canon(hd.Name) == c {
			return hd.Value
		}
	}
	return ""
}

// Values は同名ヘッダの値をすべて返す。
func (h Headers) Values(name string) []string {
	c := canon(name)
	var out []string
	for _, hd := range h {
		if canon(hd.Name) == c {
			out = append(out, hd.Value)
		}
	}
	return out
}

// Add は末尾にヘッダを追加する。
func (h *Headers) Add(name, value string) {
	*h = append(*h, Header{Name: name, Value: value})
}

// Set は同名ヘッダを 1 本に置き換える。
func (h *Headers) Set(name, value string) {
	c := canon(name)
	out := (*h)[:0]
	replaced := false
	for _, hd := range *h {
		if canon(hd.Name) != c {
			out = append(out, hd)
			continue
		}
		if !replaced {
			out = append(out, Header{Name: name, Value: value})
			replaced = true
		}
	}
	if !replaced {
		out = append(out, Header{Name: name, Value: value})
	}
	*h = out
}

// Del は同名ヘッダをすべて削除する。
func (h *Headers) Del(name string) {
	c := canon(name)
	out := (*h)[:0]
	for _, hd := range *h {
		if canon(hd.Name) != c {
			out = append(out, hd)
		}
	}
	*h = out
}

// Message は SIP のリクエストまたはレスポンス。
type Message struct {
	Response   bool
	Method     string // リクエストのとき
	RequestURI string // リクエストのとき
	Status     int    // レスポンスのとき
	Reason     string // レスポンスのとき
	Headers    Headers
	Body       []byte
}

// ParseMessage は受信したデータグラムを 1 通の SIP メッセージとして解釈する。
func ParseMessage(b []byte) (*Message, error) {
	s := string(b)
	// 行末は CRLF が正だが、実装によっては LF だけのこともあるので両方許す。
	idx := strings.Index(s, "\r\n\r\n")
	sep := 4
	if idx < 0 {
		idx = strings.Index(s, "\n\n")
		sep = 2
	}
	head := s
	var body string
	if idx >= 0 {
		head = s[:idx]
		body = s[idx+sep:]
	}

	lines := unfold(head)
	if len(lines) == 0 {
		return nil, fmt.Errorf("空のメッセージです")
	}

	m := &Message{}
	if strings.HasPrefix(lines[0], "SIP/2.0") {
		parts := strings.SplitN(lines[0], " ", 3)
		if len(parts) < 2 {
			return nil, fmt.Errorf("ステータス行を解釈できません: %q", lines[0])
		}
		code, err := strconv.Atoi(parts[1])
		if err != nil {
			return nil, fmt.Errorf("ステータスコードを解釈できません: %q", lines[0])
		}
		m.Response = true
		m.Status = code
		if len(parts) == 3 {
			m.Reason = parts[2]
		}
	} else {
		parts := strings.Fields(lines[0])
		if len(parts) != 3 || !strings.HasPrefix(parts[2], "SIP/2.0") {
			return nil, fmt.Errorf("リクエスト行を解釈できません: %q", lines[0])
		}
		m.Method = strings.ToUpper(parts[0])
		m.RequestURI = parts[1]
	}

	for _, line := range lines[1:] {
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("ヘッダ行を解釈できません: %q", line)
		}
		m.Headers.Add(strings.TrimSpace(name), strings.TrimSpace(value))
	}

	// Content-Length があればそれを信用してボディを切り出す。
	if v := m.Headers.Get("Content-Length"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 && n <= len(body) {
			body = body[:n]
		}
	}
	m.Body = []byte(body)

	return m, nil
}

// unfold はヘッダの行継続 (次行が空白始まり) を 1 行にまとめる。
func unfold(head string) []string {
	var out []string
	for _, raw := range strings.Split(head, "\n") {
		line := strings.TrimRight(raw, "\r")
		if line == "" {
			continue
		}
		if (line[0] == ' ' || line[0] == '\t') && len(out) > 0 {
			out[len(out)-1] += " " + strings.TrimSpace(line)
			continue
		}
		out = append(out, line)
	}
	return out
}

// Marshal はメッセージをバイト列にする。Content-Length は常に実際の長さで上書きする。
func (m *Message) Marshal() []byte {
	var b strings.Builder
	if m.Response {
		fmt.Fprintf(&b, "SIP/2.0 %d %s%s", m.Status, m.Reason, crlf)
	} else {
		fmt.Fprintf(&b, "%s %s SIP/2.0%s", m.Method, m.RequestURI, crlf)
	}

	hs := make(Headers, len(m.Headers))
	copy(hs, m.Headers)
	hs.Set("Content-Length", strconv.Itoa(len(m.Body)))

	for _, h := range hs {
		fmt.Fprintf(&b, "%s: %s%s", h.Name, h.Value, crlf)
	}
	b.WriteString(crlf)
	b.Write(m.Body)
	return []byte(b.String())
}

// CSeq は CSeq ヘッダの番号とメソッドを返す。
func (m *Message) CSeq() (uint32, string) {
	f := strings.Fields(m.Headers.Get("CSeq"))
	if len(f) < 2 {
		return 0, ""
	}
	n, err := strconv.ParseUint(f[0], 10, 32)
	if err != nil {
		return 0, ""
	}
	return uint32(n), strings.ToUpper(f[1])
}

// Branch は先頭 Via の branch パラメータを返す。トランザクションの照合に使う。
func (m *Message) Branch() string {
	return Param(m.Headers.Get("Via"), "branch")
}

// CallID は Call-ID ヘッダを返す。
func (m *Message) CallID() string { return m.Headers.Get("Call-ID") }

func (m *Message) IsProvisional() bool { return m.Response && m.Status >= 100 && m.Status < 200 }
func (m *Message) IsSuccess() bool     { return m.Response && m.Status >= 200 && m.Status < 300 }
func (m *Message) IsFinal() bool       { return m.Response && m.Status >= 200 }

// Summary はログ用の 1 行表現。
func (m *Message) Summary() string {
	cseq, method := m.CSeq()
	if m.Response {
		return fmt.Sprintf("%d %s (CSeq %d %s)", m.Status, m.Reason, cseq, method)
	}
	return fmt.Sprintf("%s %s (CSeq %d)", m.Method, m.RequestURI, cseq)
}

// Param は "<sip:a@b>;tag=xyz" のようなヘッダ値からパラメータを取り出す。
func Param(value, name string) string {
	for _, part := range strings.Split(value, ";")[1:] {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(k), name) {
			return strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	return ""
}

// HasParam はフラグ形式のパラメータ (例: ";lr") の有無を返す。
func HasParam(value, name string) bool {
	for _, part := range strings.Split(value, ";")[1:] {
		k, _, _ := strings.Cut(part, "=")
		if strings.EqualFold(strings.TrimSpace(k), name) {
			return true
		}
	}
	return false
}

// URIInAngle は "表示名 <sip:a@b>;tag=1" から "sip:a@b" を取り出す。
// 山括弧がなければ、パラメータを除いた全体を URI とみなす。
func URIInAngle(value string) string {
	if i := strings.Index(value, "<"); i >= 0 {
		if j := strings.Index(value[i:], ">"); j > 0 {
			return value[i+1 : i+j]
		}
	}
	uri, _, _ := strings.Cut(value, ";")
	return strings.TrimSpace(uri)
}

// AddrPortFromURI は SIP URI のホスト部を netip.AddrPort にする。
// ホスト部が IP アドレスでない (FQDN の) 場合はエラーにする。
// データコネクトでは相手は常に IP リテラルで来るため、名前解決は行わない。
func AddrPortFromURI(uri string, defaultPort uint16) (netip.AddrPort, error) {
	s := uri
	for _, scheme := range []string{"sip:", "sips:"} {
		if rest, ok := strings.CutPrefix(s, scheme); ok {
			s = rest
			break
		}
	}
	if _, rest, ok := strings.Cut(s, "@"); ok { // ユーザ部を落とす
		s = rest
	}
	s, _, _ = strings.Cut(s, ";") // パラメータを落とす
	s = strings.TrimSpace(s)

	host, portStr := s, ""
	if strings.HasPrefix(s, "[") {
		end := strings.Index(s, "]")
		if end < 0 {
			return netip.AddrPort{}, fmt.Errorf("URI のホスト部を解釈できません: %q", uri)
		}
		host = s[1:end]
		if rest := s[end+1:]; strings.HasPrefix(rest, ":") {
			portStr = rest[1:]
		}
	} else if h, p, ok := strings.Cut(s, ":"); ok {
		host, portStr = h, p
	}

	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("URI のホスト %q は IP アドレスではありません", host)
	}
	port := defaultPort
	if portStr != "" {
		n, err := strconv.ParseUint(portStr, 10, 16)
		if err != nil {
			return netip.AddrPort{}, fmt.Errorf("URI のポート %q を解釈できません", portStr)
		}
		port = uint16(n)
	}
	return netip.AddrPortFrom(addr.Unmap(), port), nil
}

// FormatURI はホストとユーザから SIP URI を組み立てる。IPv6 は角括弧で囲む。
func FormatURI(user string, ap netip.AddrPort) string {
	host := ap.Addr().String()
	if ap.Addr().Is6() {
		host = "[" + host + "]"
	}
	if user == "" {
		return fmt.Sprintf("sip:%s:%d", host, ap.Port())
	}
	return fmt.Sprintf("sip:%s@%s:%d", user, host, ap.Port())
}

// HostPort は Via の sent-by などに使うホスト表記を返す。
func HostPort(ap netip.AddrPort) string {
	if ap.Addr().Is6() {
		return fmt.Sprintf("[%s]:%d", ap.Addr(), ap.Port())
	}
	return fmt.Sprintf("%s:%d", ap.Addr(), ap.Port())
}

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand が失敗する状況では続行しても安全に呼を張れない。
		panic("sip: 乱数を生成できません: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// NewBranch は RFC 3261 のマジッククッキー付き branch を生成する。
func NewBranch() string { return "z9hG4bK" + randomToken(8) }

// NewTag は From/To タグを生成する。
func NewTag() string { return randomToken(6) }

// NewCallID は Call-ID を生成する。
func NewCallID(host string) string { return randomToken(10) + "@" + host }
