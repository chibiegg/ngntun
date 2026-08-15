// Package lease は ISC dhclient のフックが書き出したリース情報 (JSON) を読み、
// データコネクトの発信に必要なプロビジョニング情報へ正規化する。
//
// DHCPv6 側からは SIP サーバ (Option 22)、NTT ベンダオプション (Enterprise 210)、
// 委譲プレフィックス (IA_PD) を得る。DHCPv4 側からは REGISTER 先となる HGW の
// アドレス (routers) と、Contact に載せる自機アドレスを得る。
package lease

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
)

// NTTEnterpriseNumber は NTT のベンダオプションに使われる Enterprise Number。
const NTTEnterpriseNumber = 210

// ベンダオプションのサブオプション番号 (実機で確認したもの)。
const (
	SubOptClientMAC = 201
	SubOptExtension = 202
	SubOptSIPDomain = 204
)

// V6 は dhclient-exit-hook が書き出す DHCPv6 リースの JSON 表現。
// 値は dhclient の環境変数をそのまま文字列で受け取り、解釈はこのパッケージで行う。
type V6 struct {
	Reason              string `json:"reason"`
	Interface           string `json:"interface"`
	IP6Address          string `json:"ip6_address"`
	IP6Prefix           string `json:"ip6_prefix"`
	SIPServersAddresses string `json:"sip_servers_addresses"`
	SIPServersNames     string `json:"sip_servers_names"`
	VendorOpts          string `json:"vendor_opts"`
	DomainSearch        string `json:"domain_search"`
	NameServers         string `json:"name_servers"`
}

// V4 は dhclient-exit-hook が書き出す DHCPv4 リースの JSON 表現。
type V4 struct {
	Reason            string `json:"reason"`
	Interface         string `json:"interface"`
	IPAddress         string `json:"ip_address"`
	SubnetMask        string `json:"subnet_mask"`
	Routers           string `json:"routers"`
	DomainNameServers string `json:"domain_name_servers"`
}

// Provisioning はリースから正規化した、ngntun が実際に使う値。
// 各フィールドはコマンドラインフラグで上書きされうる。
type Provisioning struct {
	Interface  string           // WAN 側インタフェース名
	SIPServer  netip.Addr       // Option 22。INVITE の Route 先 (IPv6)
	Registrar4 netip.Addr       // REGISTER の宛先 (IPv4)。既定は routers の先頭
	SelfV4     netip.Addr       // Contact に載せる自機 IPv4
	Delegated  netip.Prefix     // IA_PD
	SIPDomain  string           // ベンダオプション code 204
	Extension  string           // ベンダオプション code 202 (内線番号)
	ClientMAC  net.HardwareAddr // ベンダオプション code 201
}

// VendorOpts は DHCPv6 Option 17 (vendor-opts) をデコードした結果。
type VendorOpts struct {
	Enterprise uint32
	ClientMAC  net.HardwareAddr
	Extension  string
	SIPDomain  string
	Unknown    map[uint16][]byte
}

// LoadV6 は DHCPv6 リース JSON を読む。
func LoadV6(path string) (*V6, error) {
	var v V6
	if err := loadJSON(path, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// LoadV4 は DHCPv4 リース JSON を読む。
func LoadV4(path string) (*V4, error) {
	var v V4
	if err := loadJSON(path, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

func loadJSON(path string, dst any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("リースファイルを読めません: %w", err)
	}
	if err := json.Unmarshal(b, dst); err != nil {
		return fmt.Errorf("リースファイル %s の JSON を解釈できません: %w", path, err)
	}
	return nil
}

// Build は読み込んだリースを Provisioning へ正規化する。
// 引数はどちらも nil でよい (フラグで全項目を指定する運用を許すため)。
// 値の欠落はここではエラーにせず、フラグ適用後に Validate で検査する。
func Build(v6 *V6, v4 *V4) (*Provisioning, error) {
	p := &Provisioning{}

	if v6 != nil {
		p.Interface = v6.Interface
		if a, ok := firstAddr(v6.SIPServersAddresses); ok {
			p.SIPServer = a
		}
		if v6.IP6Prefix != "" {
			pfx, err := netip.ParsePrefix(strings.TrimSpace(v6.IP6Prefix))
			if err != nil {
				return nil, fmt.Errorf("ip6_prefix %q を解釈できません: %w", v6.IP6Prefix, err)
			}
			p.Delegated = pfx
		}
		if v6.VendorOpts != "" {
			vo, err := DecodeVendorOpts(v6.VendorOpts)
			if err != nil {
				return nil, fmt.Errorf("vendor_opts のデコードに失敗しました: %w", err)
			}
			p.Extension = vo.Extension
			p.SIPDomain = vo.SIPDomain
			p.ClientMAC = vo.ClientMAC
		}
	}

	if v4 != nil {
		if p.Interface == "" {
			p.Interface = v4.Interface
		}
		if a, ok := firstAddr(v4.Routers); ok {
			p.Registrar4 = a
		}
		if a, ok := firstAddr(v4.IPAddress); ok {
			p.SelfV4 = a
		}
	}

	return p, nil
}

// Validate は発信に必要な項目が揃っているかを検査する。
func (p *Provisioning) Validate() error {
	var missing []string
	if !p.SIPServer.IsValid() {
		missing = append(missing, "SIP サーバ (--sip-server / DHCPv6 Option 22)")
	}
	if !p.Registrar4.IsValid() {
		missing = append(missing, "レジストラ IPv4 (--registrar4 / DHCPv4 routers)")
	}
	if !p.SelfV4.IsValid() {
		missing = append(missing, "自機 IPv4 (--source-addr4 / --wan-interface 上の IPv4)")
	}
	if p.SIPDomain == "" {
		missing = append(missing, "SIP ドメイン (--sip-domain / ベンダオプション code 204)")
	}
	if p.Extension == "" {
		missing = append(missing, "内線番号 (--extension / ベンダオプション code 202)")
	}
	if len(missing) > 0 {
		return fmt.Errorf("プロビジョニング情報が不足しています: %s", strings.Join(missing, ", "))
	}
	if p.SIPServer.Is4() {
		return fmt.Errorf("SIP サーバは IPv6 アドレスである必要があります (得られた値: %s)", p.SIPServer)
	}
	if !p.Registrar4.Is4() {
		return fmt.Errorf("レジストラは IPv4 アドレスである必要があります (得られた値: %s)", p.Registrar4)
	}
	return nil
}

// DecodeVendorOpts は dhclient が渡すベンダオプションの文字列をデコードする。
//
// 期待する形式:
//
//	enterprise-number (4 bytes) | { code(2) len(2) value(len) } ...
func DecodeVendorOpts(s string) (*VendorOpts, error) {
	b, err := decodeHexish(s)
	if err != nil {
		return nil, err
	}
	if len(b) < 4 {
		return nil, fmt.Errorf("vendor-opts が短すぎます (%d バイト)", len(b))
	}

	vo := &VendorOpts{
		Enterprise: binary.BigEndian.Uint32(b[:4]),
		Unknown:    map[uint16][]byte{},
	}

	for i := 4; i < len(b); {
		if i+4 > len(b) {
			return nil, fmt.Errorf("サブオプションのヘッダが途中で切れています (offset %d)", i)
		}
		code := binary.BigEndian.Uint16(b[i : i+2])
		length := int(binary.BigEndian.Uint16(b[i+2 : i+4]))
		i += 4
		if i+length > len(b) {
			return nil, fmt.Errorf("サブオプション %d の値が途中で切れています (offset %d, len %d)", code, i, length)
		}
		val := b[i : i+length]
		i += length

		switch code {
		case SubOptClientMAC:
			vo.ClientMAC = net.HardwareAddr(append([]byte(nil), val...))
		case SubOptExtension:
			vo.Extension = string(val)
		case SubOptSIPDomain:
			name, err := decodeDNSName(val)
			if err != nil {
				return nil, fmt.Errorf("サブオプション %d (SIP ドメイン) をデコードできません: %w", code, err)
			}
			vo.SIPDomain = name
		default:
			// 将来 HGW のファームウェア更新で増える可能性があるので、無視して読み飛ばす。
			vo.Unknown[code] = append([]byte(nil), val...)
		}
	}

	return vo, nil
}

// decodeHexish は "00:d2:.." 形式および "00d2.." 形式の 16 進文字列を受け付ける。
func decodeHexish(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, `"`)
	if s == "" {
		return nil, fmt.Errorf("空の値です")
	}
	if strings.ContainsAny(s, ":") {
		parts := strings.Split(s, ":")
		out := make([]byte, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if len(p) == 1 {
				p = "0" + p // dhclient は先頭の 0 を落とすことがある
			}
			if len(p) != 2 {
				return nil, fmt.Errorf("16 進バイト %q を解釈できません", p)
			}
			v, err := hex.DecodeString(p)
			if err != nil {
				return nil, fmt.Errorf("16 進バイト %q を解釈できません: %w", p, err)
			}
			out = append(out, v[0])
		}
		return out, nil
	}
	s = strings.ReplaceAll(s, " ", "")
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("16 進文字列を解釈できません (先頭: %.16s): %w", s, err)
	}
	return b, nil
}

// decodeDNSName は DNS のワイヤ形式 (長さ prefix 付きラベル列) をドット区切りに戻す。
func decodeDNSName(b []byte) (string, error) {
	var labels []string
	for i := 0; i < len(b); {
		n := int(b[i])
		i++
		if n == 0 {
			break
		}
		if n&0xc0 != 0 {
			return "", fmt.Errorf("圧縮ポインタには対応していません")
		}
		if i+n > len(b) {
			return "", fmt.Errorf("ラベルが途中で切れています")
		}
		labels = append(labels, string(b[i:i+n]))
		i += n
	}
	if len(labels) == 0 {
		return "", fmt.Errorf("ラベルがありません")
	}
	return strings.Join(labels, "."), nil
}

// firstAddr はカンマ・空白区切りのアドレス列から先頭の 1 つを取り出す。
func firstAddr(s string) (netip.Addr, bool) {
	for _, f := range strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	}) {
		if a, err := netip.ParseAddr(strings.TrimSpace(f)); err == nil {
			return a.Unmap(), true
		}
	}
	return netip.Addr{}, false
}
