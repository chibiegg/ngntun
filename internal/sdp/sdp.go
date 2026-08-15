// Package sdp はデータコネクトが使う SDP のオファー生成とアンサー解析を行う。
//
// 音声 (RTP) ではなく、メディア記述は次の形をとる:
//
//	m=application <port> udp octet-stream
//	b=AS:<kbps>
//	a=udp-setup:active|passive
//
// このフローの UDP ペイロードに、追加ヘッダなしで生の IP パケットが入る。
package sdp

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// メディア記述の固定部分。データコネクトはこの組み合わせのみを使う。
const (
	MediaType   = "application"
	MediaProto  = "udp"
	MediaFormat = "octet-stream"
)

// udp-setup の値。どちら側からメディアの送信を始めるかを決める。
const (
	SetupActive  = "active"  // 自分から送り始める (発信側)
	SetupPassive = "passive" // 相手の最初のパケットを待つ (着信側)
)

// Offer は自機が出す SDP オファー。
type Offer struct {
	Addr          netip.Addr // c= と o= に載せる自機アドレス
	Port          int        // m= のポート
	BandwidthKbps int        // b=AS の値
	Setup         string     // SetupActive / SetupPassive
	SessionID     uint64
	SessionVer    uint64
}

// Answer は相手 (網) から返る SDP アンサーのうち、ngntun が使う部分。
type Answer struct {
	Addr          netip.Addr // メディアの宛先。網内のリレー装置になる
	Port          int
	BandwidthKbps int
	Setup         string
}

// Marshal はオファーを SDP のバイト列にする。行末は CRLF。
func (o *Offer) Marshal() []byte {
	var b strings.Builder
	nettype := addrType(o.Addr)

	fmt.Fprintf(&b, "v=0\r\n")
	fmt.Fprintf(&b, "o=- %d %d IN %s %s\r\n", o.SessionID, o.SessionVer, nettype, o.Addr)
	fmt.Fprintf(&b, "s=-\r\n")
	fmt.Fprintf(&b, "c=IN %s %s\r\n", nettype, o.Addr)
	fmt.Fprintf(&b, "t=0 0\r\n")
	fmt.Fprintf(&b, "m=%s %d %s %s\r\n", MediaType, o.Port, MediaProto, MediaFormat)
	if o.BandwidthKbps > 0 {
		fmt.Fprintf(&b, "b=AS:%d\r\n", o.BandwidthKbps)
	}
	if o.Setup != "" {
		fmt.Fprintf(&b, "a=udp-setup:%s\r\n", o.Setup)
	}
	return []byte(b.String())
}

// ParseAnswer は 200 OK などに載ってきた SDP を解析する。
// c= はセッションレベル・メディアレベルのどちらにあってもよく、
// メディアレベルの指定があればそちらを優先する。
func ParseAnswer(body []byte) (*Answer, error) {
	var (
		sessionAddr netip.Addr
		mediaAddr   netip.Addr
		inMedia     bool
		found       bool
		ans         Answer
	)

	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimRight(line, "\r")
		if len(line) < 2 || line[1] != '=' {
			continue
		}
		key, val := line[0], strings.TrimSpace(line[2:])

		switch key {
		case 'c':
			addr, err := parseConnection(val)
			if err != nil {
				return nil, err
			}
			if inMedia {
				mediaAddr = addr
			} else {
				sessionAddr = addr
			}

		case 'm':
			// 目的のメディア以外 (音声など) が並んでいても取り違えないよう、
			// application/udp/octet-stream の行だけを対象にする。
			fields := strings.Fields(val)
			if len(fields) < 4 {
				return nil, fmt.Errorf("m= 行を解釈できません: %q", val)
			}
			if fields[0] != MediaType || fields[2] != MediaProto || fields[3] != MediaFormat {
				inMedia = false
				continue
			}
			port, err := strconv.Atoi(fields[1])
			if err != nil {
				return nil, fmt.Errorf("m= 行のポートを解釈できません: %q", val)
			}
			inMedia, found = true, true
			ans.Port = port

		case 'b':
			if inMedia && strings.HasPrefix(val, "AS:") {
				if n, err := strconv.Atoi(strings.TrimPrefix(val, "AS:")); err == nil {
					ans.BandwidthKbps = n
				}
			}

		case 'a':
			if inMedia && strings.HasPrefix(val, "udp-setup:") {
				ans.Setup = strings.TrimPrefix(val, "udp-setup:")
			}
		}
	}

	if !found {
		return nil, fmt.Errorf("SDP に %s/%s/%s のメディア記述がありません", MediaType, MediaProto, MediaFormat)
	}
	if ans.Port <= 0 || ans.Port > 65535 {
		// ポート 0 は「このメディアを拒否した」の意。呼としては成立していても使えない。
		return nil, fmt.Errorf("メディアのポートが不正です: %d", ans.Port)
	}

	switch {
	case mediaAddr.IsValid():
		ans.Addr = mediaAddr
	case sessionAddr.IsValid():
		ans.Addr = sessionAddr
	default:
		return nil, fmt.Errorf("SDP に c= 行がありません")
	}
	if ans.Addr.IsUnspecified() {
		return nil, fmt.Errorf("メディアの宛先アドレスが未指定 (%s) です", ans.Addr)
	}

	return &ans, nil
}

// AddrPort はアンサーのメディア宛先を netip.AddrPort として返す。
func (a *Answer) AddrPort() netip.AddrPort {
	return netip.AddrPortFrom(a.Addr, uint16(a.Port))
}

func parseConnection(val string) (netip.Addr, error) {
	fields := strings.Fields(val)
	if len(fields) < 3 {
		return netip.Addr{}, fmt.Errorf("c= 行を解釈できません: %q", val)
	}
	// TTL やアドレス数の指定 (例: "224.0.0.1/127/2") が付く場合がある。
	host, _, _ := strings.Cut(fields[2], "/")
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("c= 行のアドレス %q を解釈できません: %w", host, err)
	}
	return addr.Unmap(), nil
}

func addrType(a netip.Addr) string {
	if a.Is4() {
		return "IP4"
	}
	return "IP6"
}
