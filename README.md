# ngntun

NTT 社フレッツ光、ひかり電話オプションサービス **データコネクト** 接続を Linux から確立するためのコマンド。
HGW へ SIP で発信し、確立したデータコネクトのフローと tun デバイスを橋渡しする。

設計の根拠と詳細は [DESIGN.md](DESIGN.md) を参照。
動作は NTT 東日本社フレッツ光と実機の HGW 1 機種 (RX-400MI) で確認したのみであり、他の環境における動作を保証するものではない。

> **課金に注意**:
> データコネクトは従量課金制のサービスであり、このコマンドの実行は利用料の課金を伴うため注意されたい。
> また、不適切なパラメータの設定は意図しない長時間発信とそれに伴う高額な課金を発生させる可能性がある。
> 使用前には `--dry-run` と `--register-only`（どちらも発信しない）で望ましい動作かどうか確認し、実行時も `--max-duration` を短く設定して長時間接続を防ぐことを推奨する。
> 作者はこのコマンドを利用することによって起こった利用者への不利益等に関して責任を負わないものとする。

## 仕組み

```
[アプリ] → dc0 (tun) → ngntun → UDP/IPv6 → NGN 網内リレー → 相手拠点
                          ↑
                    SIP: REGISTER は IPv4 で HGW へ
                         INVITE 以降は IPv6 で HGW へ
```

メディアの UDP ペイロードは、**追加ヘッダなしの生 IP パケット**そのもの。
したがって tun から読んだバイト列をそのまま UDP で送り、受けたものをそのまま tun へ書けばよい。

## ビルド

```sh
go build -o ngntun ./cmd/ngntun
# 実験機 (Raspberry Pi など) 向け
GOOS=linux GOARCH=arm64 go build -o ngntun ./cmd/ngntun
```

Linux 専用 (tun と netlink を使うため)。macOS でもビルドとテストは通るが、実行はできない。

## 準備

### 1. dhclient にリース情報を書かせる

```sh
sudo apt install isc-dhcp-client
sudo install -m 755 scripts/ngntun-dhclient-script /usr/local/sbin/
sudo mkdir -p /etc/ngntun && sudo cp scripts/dhclient6.conf /etc/ngntun/
```

**Vendor Class (option 16) を送らないと HGW は内線番号を割り当てない。** これが最大の要点。
ISC dhclient は `dhcp6.vendor-class` を定義していないので生バイト列として自前で定義しており、
定義を忘れると dhclient はエラーを出しつつ**そのまま送信を続けてしまう**ので気づきにくい。

**引き金になるのはオプションを送ること自体で、中身は問われない。** 構造は
「enterprise-number + 長さ + MAC アドレス」だが、実機で確かめたところ enterprise-number も
MAC も HGW は検証していない。**書き換えは不要**で、`scripts/dhclient6.conf` の値のまま使える。

それでもこの形で送っているのは、**対応ルーター (RTX810) が実際に送っていたものに合わせている**
ため。将来ファームウェアが検証を始めたときに壊れないよう、実機に倣った形を保っている。

**内線番号が紐づいている識別子は DUID のほう。** ベンダオプション code 201 で返ってくる MAC も、
Vendor Class に書いた値ではなく DUID (Client-ID) 内の MAC である。dhclient の DUID-LLT は
その IF の MAC から作られるので、普段は両者がたまたま一致する。**DUID が変わると別の端末と
みなされ、新しい内線番号が払い出される**ので、リース DB を消すときは注意する
(`/var/lib/dhcp/ngntun6-<IF>.leases` の `default-duid` を残しておけば内線番号は変わらない)。

<details>
<summary>実験: 3 つの MAC を別々の値にしたときの挙動</summary>

| どこ | 値 |
|---|---|
| 実 MAC (L2 フレームの送信元) | `a0:ce:c8:cf:1d:ae` |
| DUID (Client-ID) 内の LL アドレス | `02:11:33:44:55:66` |
| Vendor Class に書いた MAC | `02:4e:8f:1c:6b:d3` |

返ってきたベンダオプションと登録結果。

```
0:c9(201) 0:6 02:11:33:44:55:66   ← DUID の値が返る
0:ca(202) 0:1 0x36 = "6"          ← 新しい内線番号
INFO SIP に登録しました extension=6
```

Vendor Class の値はどこにも現れず、L2 の実 MAC も使われない。
Vendor Class だけを架空の値にして DUID を保ったときは、内線番号は元のまま変わらなかった。

</details>

```sh
IF=eth1
sudo dhclient -6 -P -cf /etc/ngntun/dhclient6.conf \
  -sf /usr/local/sbin/ngntun-dhclient-script \
  -lf /var/lib/dhcp/ngntun6.leases -pf /run/ngntun-d6.pid $IF
```

`/run/ngntun/lease6.json` ができる。`vendor_opts` が空でなければ成功で、
中に内線番号 (code 202) と SIP ドメイン (code 204) が入っている。
常用するなら手で起動せず、1.7 の unit を使う。

`-sf` で渡すスクリプトは**経路に一切触らない**。通常の dhclient-script は
デフォルトゲートウェイを書き換えるため、既にインターネット側の経路を持つ機器では疎通が切れる。
このスクリプトが行うのはリースの JSON 書き出しと、1.5 の送信元アドレスの付与だけ。
その代わり HGW への経路は用意されないので、RA を受けていない機器では 1.6 の手当てが要る。

**DHCPv4 のリースは要らない。** REGISTER は IPv4 で行うが、そこで必要になるのは
「HGW の IPv4 (= REGISTER の宛先)」だけで、これは `--registrar4` に書けば済む。
自機の IPv4 は `--wan-interface` に**実際に付いているアドレス**から選ぶ。

```sh
sudo ngntun --wan-interface eth1 --registrar4 192.168.1.1 ...
```

`dhclient -4` を足すこともできる (`routers` から `--registrar4` を省ける) が、勧めない。
インタフェースに IPv4 を付けている主体 (NetworkManager など) とは別のクライアントとして
DHCP を引くので、**別のアドレスがリースされる**。リースの `ip_address` は誰も
インタフェースに付けないため、それを送信元にすると
`bind: cannot assign requested address` になる。ngntun はリースより実アドレスを優先するので
壊れはしないが、リースを引く意味がない。

### 1.5 送信元 IPv6 アドレス

**手当ては要らない。** `-sf` のスクリプトが、委譲プレフィックスの先頭 /64 に EUI-64 でアドレスを付ける (`ip -6 addr replace ... nodad`)。

```
# 例: 240b:10:dd83:c970::/60 が委譲され、MAC が a0:ce:c8:cf:1d:ae の場合
240b:10:dd83:c970:a2ce:c8ff:fecf:1dae/64 dev eth1
```

付けたアドレスは `/run/ngntun/source-addr6` に控えてあり、**プレフィックスが変わったら古いものを消して付け直す**。失効した委譲プレフィックスを送信元にすると HGW は INVITE をエラーも返さず捨てるので、古いアドレスを残さないことが重要になる。プレフィックスが使えなくなったとき (`EXPIRE6` など) も消す。

スクリプトが触るのはこのアドレスだけで、**経路には一切触らない**。自分で管理したい場合は `NGNTUN_SKIP_ADDR=1` を環境に置けば、この処理を行わない。

**送信元を ngntun に教える必要もない。** `--wan-interface` があれば、その IF 上のグローバル IPv6 から**委譲プレフィックス内のものを選ぶ**。HGW と同じ /64 のオンリンク側アドレスが同居していても、そちらは選ばれない。IPv4 も同じ考え方で、その IF に付いている IPv4 から**レジストラ (HGW) と同じサブネットのもの**を選ぶ。つまり `--source-addr` / `--source-addr4` は指定しなくてよい。

### 1.6 HGW への経路

IPv6 で発信するには 2 つ揃っている必要がある。**委譲プレフィックス側の送信元アドレス**と、**HGW が居る /64 への経路**である。前者は 1.5 のスクリプトが面倒を見るが、**後者は誰も面倒を見ない**。スクリプトは経路に触らない方針なので、意図的にここは範囲外になっている。

HGW から RA を受けている普通の構成なら、オンリンクの経路は RA で降ってくるので何もしなくてよい。

```
240b:10:dd83:c900::/64 dev eth1 proto ra metric 100
```

問題になるのは RA を受けていない構成で、`ip -6 route get <SIP サーバのアドレス>` が `Network is unreachable` を返す。SIP を送る前の段階で失敗するので、`--dry-run` は通るのに発信だけができない。NetworkManager 管理下で `ipv6.method` が `link-local` や `disabled` になっている IF が典型で、このとき NM は `accept_ra` を 0 にする。

推奨は NM 自身に RA を処理させる方法。

```sh
sudo nmcli connection modify eth1 \
  ipv6.method auto \
  ipv6.never-default yes \
  ipv6.ignore-auto-dns yes
sudo nmcli device reapply eth1
```

`never-default` で RA のデフォルト経路だけを捨て、オンリンクの /64 経路は受け取る。`ignore-auto-dns` は resolv.conf を触らせないため。SLAAC のアドレスが 1 つ増えるが、1.5 のとおり送信元には選ばれないので影響しない。

**`/etc/sysctl.d/` に `net.ipv6.conf.<IF>.accept_ra=1` を書いても効かない。** `systemd-sysctl` は NM より先に走るので、NM が接続を上げるときに 0 へ戻される。sysctl で通したい場合は `/etc/NetworkManager/dispatcher.d/` に置いて NM より後に走らせる。ただし `accept_ra` を後から 1 にしてもカーネルは Router Solicitation を送り直さないので、経路が入るのは次の RA を受けてからになる。確実にするなら静的経路を併記する。

```sh
sudo ip -6 route replace 240b:10:dd83:c900::/64 dev eth1
```

**NM は `nmcli device reapply` や再接続・再起動で、1.5 のスクリプトが付けたアドレスを消す。** スクリプトが NM を壊さないのとは別の話で、逆向きは成り立たない。こちらの手当ては 1.7 にまとめてある。

なお HGW の RA に Route Information Option は入っておらず、降ってくる経路はオンリンクの /64 とデフォルト経路の 2 本だけである。メディア宛先 (網内リレー) 向けの経路は RA からは得られないので、そちらは `--media-route` が別に面倒を見る。

### 1.7 systemd で常駐させる

dhclient を手で起動する代わりに、`scripts/` の unit と dispatcher を入れる。

```sh
sudo install -m 644 scripts/ngntun-dhclient6@.service /etc/systemd/system/
sudo install -m 755 scripts/50-ngntun /etc/NetworkManager/dispatcher.d/
# 50-ngntun の NGNTUN_IFACE の既定値を対象の IF に合わせる
sudo systemctl daemon-reload
sudo systemctl enable --now ngntun-dhclient6@eth1.service
```

インスタンス名がインタフェース名になる。リースと PID のファイルも IF ごとに分かれるので、複数の IF で並べても衝突しない。

**`After=network-online.target` だけでは足りない。** それで足りるかどうかは `NetworkManager-wait-online.service` が有効かどうかに左右され、無効な環境では dhclient のほうが先に走る。dhclient は起動時にリンクローカルアドレスへ bind するので、DAD が終わる前だと即座に落ちる。

```
dhclient: Can't bind to dhcp address: Cannot assign requested address
dhclient: exiting.
```

そこでこの unit は `sys-subsystem-net-devices-<IF>.device` に紐づけたうえで、`ExecStartPre` でリンクローカルが tentative でなくなるのを最大 30 秒待つ。これで環境に依存しなくなる。

**そして起動順を直しても、それだけでは足りない。** dhclient が成功した後に NM が接続に触れると、管理外である送信元アドレスは消される。このとき **dhclient は生きたまま「付けた」と記録しているので、自力では復旧しない**。プロセスは死んでいないので `Restart=on-failure` でも検知できず、次に何かが起きるのは Renew のタイミングになる。

順序では勝てないので、**NM がインタフェースを上げた後に呼ばれる** dispatcher から unit を起動し直す。

```sh
case "$ACTION" in
up | reapply)
	systemctl restart --no-block "ngntun-dhclient6@${IFACE}.service"
	;;
```

`reapply` を拾うのが要点。`nmcli device reapply` はインタフェースを落とさないままアドレスだけ消していくので、`up` だけ見ていると取りこぼす。

停止時は `dhclient -6 -P -r` (Release) を呼ぶ。スクリプトが `RELEASE6` で動いて送信元アドレスが片づき、HGW 側のバインディングも解放される。**`-x` (単に停止) ではスクリプトが呼ばれず、アドレスが残る**ので使わない。失効したプレフィックスのアドレスが残っていると、HGW は INVITE をエラーも返さず捨てる。

NM 管理外のインタフェースなら dispatcher は要らない (`unmanaged-devices` で外す構成も採れる)。ただし起動順のほうは残るので、`ExecStartPre` は入れておく。

### 2. 権限

`CAP_NET_ADMIN` が必要。systemd から動かすなら root ではなく次のようにする。

```ini
[Service]
AmbientCapabilities=CAP_NET_ADMIN
CapabilityBoundingSet=CAP_NET_ADMIN
```

## 使い方

以下の例で `03XXXXXXXX` は自局の番号、`03YYYYYYYY` は相手の番号を表す伏せ字。
実際の番号に置き換えて使う。インタフェースは eth0 がインターネット側、eth1 が HGW 側とする。

`--registrar4` には HGW の IPv4 を指定する (準備 1 のとおり `dhclient -4` も動かしているなら省略できる)。

まず `--dry-run` で解決結果を確認する。SIP は一切送らないので課金は発生しない。

```sh
ngntun --wan-interface eth1 --registrar4 192.168.1.1 \
  --peer-number 03YYYYYYYY --dry-run
```

実際に発信する:

```sh
sudo ngntun \
  --wan-interface eth1 \
  --registrar4 192.168.1.1 \
  --peer-number 03YYYYYYYY \
  --self-number 03XXXXXXXX \
  --tun-name dc0 \
  --tun-addr 172.21.0.100/30 \
  --route 192.168.200.0/24 \
  --idle-timeout 10s \
  --max-duration 5m
```

着信を待ち受ける:

```sh
sudo ngntun --answer --allow-from 03XXXXXXXX \
  --wan-interface eth1 --registrar4 192.168.1.1 \
  --tun-name dc0 --tun-addr 172.21.0.101/30
```

### トンネルが張れたらコマンドを実行する

フラグの後ろ (`--` 以降) にコマンドを書くと、**トンネルが確立してから**そのコマンドを実行する。
コマンドが終了した時点で呼を切り、経路と tun を巻き戻して ngntun も終了する。
必要な間だけ呼を張るので、`--idle-timeout` 頼みにするより課金の範囲が読みやすい。

```sh
sudo ngntun \
  --wan-interface eth1 \
  --registrar4 192.168.1.1 \
  --peer-number 03YYYYYYYY \
  --tun-addr 172.21.0.100/32 --route 192.168.200.0/24 \
  --max-duration 5m \
  -- ntpdate -q 192.168.200.65
```

- 標準入出力はそのまま引き継ぐので、対話的なコマンドも動く
- `--` は省略してもよいが、コマンド側のオプションが ngntun のフラグと解釈されるのを防ぐため付けたほうが安全
- 呼が先に終わった場合 (無通信切断・最大保持時間・相手からの BYE・シグナル) は、
  コマンドに `SIGTERM` を送る。5 秒で終わらなければ `SIGKILL`
- コマンドの起動に失敗したら (実行ファイルが無いなど) 直ちに呼を切る
- `--register-only` はトンネルを張らないので併用できない

コマンドには次の環境変数を渡す。

| 変数 | 例 |
|---|---|
| `NGNTUN_TUN` | `dc0` |
| `NGNTUN_TUN_MTU` | `1452` |
| `NGNTUN_TUN_ADDR` / `NGNTUN_TUN_IP` | `172.21.0.100/32` / `172.21.0.100` |
| `NGNTUN_TUN_PEER` | `172.21.0.101` (`--tun-peer` 指定時) |
| `NGNTUN_PEER_NUMBER` | 相手の電話番号 (着信時は発番号) |
| `NGNTUN_REMOTE_MEDIA` | メディアの宛先 `[2001:db8::1]:5004` |

### 主なオプション

| オプション | 既定値 | 説明 |
|---|---|---|
| `--peer-number` | — | 相手の電話番号 (発信時は必須) |
| `--self-number` | — | 発信者番号。`P-Preferred-Identity` に載せる |
| `--lease` / `--lease4` | `/run/ngntun/lease{6,4}.json` | リース JSON のパス。`--lease4` は任意 |
| `--registrar4` | リースの `routers` | REGISTER の宛先 (HGW の IPv4)。**DHCPv4 リースを引かないなら必須** |
| `--tun-name` | `dc0` | 作成する tun デバイス名 |
| `--tun-addr` / `--tun-peer` | — | tun に付けるアドレス / 対向 |
| `--route` | — | この tun に向ける経路 (複数指定可) |
| `--media-route` | `auto` | メディア宛先への経路の扱い (`auto` / `off`) |
| `--media-gateway` | SIP サーバ (HGW) | `--media-route` で足す経路のゲートウェイ |
| `--tun-mtu` | `1452` | 外側 IPv6(40)+UDP(8) を引いた値 |
| `--bandwidth` | `64k` | SDP の `b=AS` に反映 |
| `--idle-timeout` | `30s` | 無通信での自動切断 |
| `--max-duration` | `10m` | **課金への安全装置**。超えたら強制切断 |
| `--media-timeout` | `10s` | 確立後この時間メディアが来なければ切断 |
| `--answer` / `--allow-from` | — | 着信待ち受けと発番号の許可リスト |
| `--wan-interface` | — | 送信元アドレスの自動選択に使う IF。既定で `--bind-device` も兼ねる |
| `--bind-device` | `--wan-interface` と同じ | ソケットをこの IF に固定する (`SO_BINDTODEVICE`) |
| `--source-addr` / `--source-addr4` | どちらも自動選択 | 送信元。`--wan-interface` 上のアドレスから選ぶので**通常は指定不要** |
| `--dry-run` | false | 解決した設定と生成予定の SDP を表示して終了。**SIP を一切送らない** |
| `--register-only` | false | 登録だけして待機する。**発信しないので課金されない** |
| `--register-transport` | `ipv4` | REGISTER のトランスポート。実機では ipv6 は通らなかった |
| `--force` | false | 同名の tun が残っていれば削除して作り直す |
| `--log-level` | `info` | `trace` にすると SIP メッセージを全文ダンプする |

上記は主なものだけで、全一覧は `ngntun --help` を参照
(`--sip-server` / `--sip-domain` / `--extension` といったリースの上書きのほか、
`--sip-port` / `--media-port` / `--route-table` / `--strict-peer-port` / `--log-format` などもある)。

`--bind-device` は多ホーム環境で効く。同じプレフィックスの直結経路が複数の IF にあると、
経路表任せでは HGW 宛が別の IF から出てしまう (実機で踏んだ)。IF を固定すれば経路表に手を入れずに済む。

### メディア宛先への経路 (`--media-route`)

メディアの宛先は NGN 網内のリレーで、本来は HGW が出す RA のデフォルト経路に乗る。
ところが**別にインターネット回線を持つ機器では HGW をデフォルトゲートウェイにできない**ので、
宛先だけが経路表から漏れて `sendto: network is unreachable` になる。

既定の `auto` は、**`--wan-interface` 経由で宛先に到達できないときだけ**、呼が確立してから
ホスト経路 (`/128`) を足し、終了時に消す。宛先は 200 OK の SDP で初めて確定するので、
事前に `2404:1a8::/32` のような網側のアドレス空間を決め打ちする必要がない。
到達できる環境 (HGW から RA を受けている普通の構成) では何もしない。

```
INFO メディア宛先への経路を追加しました dst=2404:1a8:7100:1300::360 via=2001:db8:1::1 dev=eth1
```

ゲートウェイは既定でリースの SIP サーバ (= HGW) を使う。別の機器を経由させたいときだけ
`--media-gateway` で指定する。自分で経路を管理したいなら `--media-route off`。

リースから取れる値 (`--sip-server` / `--registrar4` / `--sip-domain` / `--extension` /
`--source-addr` / `--source-addr4`) は個別にフラグで上書きできる。
**DHCPv6 のリースが正しく取れている限り、指定が要るのは `--registrar4` だけ** (DHCPv4 のリースも
引いているなら、それも要らない)。ほかの上書きが要るのは、リースの値が実態と合わないときと、
リースファイルなしで動かしたいときだけ (全部手で指定すれば、リースファイルがなくても動く)。

### 終了コード

| コード | 意味 |
|---|---|
| 0 | 正常終了 (無通信切断・最大保持時間・相手からの BYE を含む) |
| 1 | 起動オプションの誤り |
| 2 | 設定・リースの不備 |
| 3 | REGISTER の失敗 |
| 4 | INVITE の失敗 (話中・圏外など) |
| 5 | メディアが届かなかった |
| 6 | 実行時エラー |
| 7 | HGW が Digest 認証を要求した (未対応) |

コマンドを指定した場合、**コマンドの終了が終了契機なら ngntun の終了コードはコマンドのもの**になる
(シグナルで終わった場合はシェルと同じ 128+シグナル番号)。呼のほうが先に終わったときは上表のとおり。

INVITE が失敗しても**自動リトライはしない**。ダイヤルのリトライループは
そのまま課金と網への迷惑につながるので、判断は systemd の `Restart=` など上位に委ねる。

## 設計上の注意

- **呼と tun のライフサイクルは 1:1**。呼が消えれば経路も tun も消える。
  コマンドを指定した場合はコマンドの寿命も同じ線に乗せる (どちらが先に終わってももう一方を終わらせる)
- **後始末では BYE を最優先**する。課金が止まるのは BYE が届いた時点なので、
  経路や tun の巻き戻しより先に BYE を投げる
- メディアの送信元は **IP のみ照合**し、ポートは最初に受けたものを学習する。
  網内リレーが SDP と違うポートから送ってくる可能性があるため
  (厳密に照合したい場合は `--strict-peer-port`)

## テスト

```sh
go test ./...
```

- `internal/lease` — HGW が配布するベンダオプションのバイト列をゴールデンデータにしている
- `internal/sip` — 擬似 HGW を localhost に立て、REGISTER → INVITE → ACK → BYE を実際に流している
- `internal/media` — 擬似 tun と擬似リレーの間で、両方向の転送と送信元検証を確認している

## 実機での確認結果

Raspberry Pi (Debian 13, aarch64) と ひかり電話ルータ (HGW) の構成で、NICT が提供する [光テレホンJJY](https://jjy.nict.go.jp/hteljjy/) に発信し、データコネクトによって確立されてトンネル越しに NTP で時刻取得ができることを確認した。
なお、光テレホンJJY の利用には事前申込が必要である。
下記の例では発信先の番号・トンネル内のアドレス・接続条件は適宜マスクして示す。

```sh
sudo ngntun \
  --wan-interface eth1 \
  --peer-number 03YYYYYYYY \
  --tun-name dc0 --tun-addr 172.21.0.100/32 \
  --route 192.168.200.0/24 \
  --idle-timeout 25s --max-duration 60s
# 確立直後に別プロセスから 192.168.200.65 NTP サーバへ問い合わせを行う。
```

```
セッション概要: peer=03YYYYYYYY duration=22.71s reason=remote-bye
  送信: 6 パケット / 372 バイト   受信: 3 パケット / 228 バイト
  破棄: 想定外の送信元=0 不正なパケット=0 tun 書き込みエラー=0
→ 192.168.200.65 から stratum=1 refid='NICT' の応答、RTT 16〜18ms
```

確立から切断・後始末まで一通り動作し、デフォルトゲートウェイは終始無傷だった。

### ユーザランドを通すコスト

1 つの呼の中で 2 箇所を同時に測った。トンネル内に ICMP を流して `ping` が報告する往復時間 (tun への
書き込みから読み出しまで = **ngntun の処理を含む**) と、その裏で WAN 側 IF をキャプチャして得た外側
パケットの往復時間 (**ngntun の処理を含まない**) である。同じ呼・同じ計器・同じ時刻なので、差が
そのままユーザランドを通すコストになる。

```sh
sudo tcpdump -ni eth1 -tt -w media.pcap "ip6 and udp" &
sudo ngntun ... --max-duration 90s -- ping -i 0.2 -c 100 192.168.200.65
```

105 往復の結果 (ミリ秒)。

| 測定点 | 最小 | 中央値 | 平均 | 標準偏差 |
|---|---|---|---|---|
| 外側 (キャプチャ) | 17.619 | 18.311 | 18.333 | 0.340 |
| 内側 (`ping`) | 18.156 | — | 18.852 | 0.354 |

- **上乗せは平均 0.519 ms**。tun 読み → UDP 書き → UDP 読み → tun 書きの 4 回のコピーと syscall の合計
- **ジッタはほとんど増えない**。標準偏差 0.340 → 0.354
- 全体 18.852 ms のうち ngntun が関与するのは 0.5 ms で、残りは網の往復

`SO_BUSY_POLL` や実時間スケジューリングによるチューニングも検討したが、削れる余地がこの範囲に
収まるため見送っている。同じ呼で NTP も流しており、誤差は ±0.010 秒前後で安定していた。

### 確立後のコマンド実行 (同じ相手で実測)

同じ相手に、疎通確認そのものをコマンドとして渡した例。

```sh
sudo ngntun \
  --wan-interface eth1 \
  --peer-number 03YYYYYYYY \
  --tun-name dc0 --tun-addr 172.21.0.100/32 \
  --route 192.168.200.0/24 \
  --idle-timeout 25s --max-duration 60s \
  -- ntpdate -q 192.168.200.65
```

```
INFO データコネクトのセッションを確立しました peer=03YYYYYYYY media=[2404:1a8:7100:1300::240]:26758
INFO コマンドを実行します cmd="[ntpdate -q 192.168.200.65]" pid=24966
2026-07-31 19:09:40.830267 (+0100) -0.002937 +/- 0.013847 192.168.200.65 s1 no-leap
INFO コマンドが終了したためセッションを終了します
INFO BYE を送信しました
INFO コマンドが終了しました exit_code=0

セッション概要: peer=03YYYYYYYY duration=866ms reason=command-exit
  送信: 2 パケット / 124 バイト   受信: 1 パケット / 76 バイト
  破棄: 想定外の送信元=0 不正なパケット=0 tun 書き込みエラー=0
  コマンド終了コード: 0
→ ngntun 自身の終了コードも 0
```

**呼の保持は 866 ミリ秒**で、`--idle-timeout` 頼みだった上の例 (22.71 秒) の 1/26。
用が済んだ瞬間に BYE が出るので、従量課金との相性がよい。後始末 (dc0 の削除・経路の巻き戻し・
デフォルトゲートウェイ) も上の例と同じく問題なかった。

**踏んだ落とし穴** (どれも一度ハマると原因が見えにくい):

1. **Vendor Class を送らないと内線番号が来ない**。SIP サーバと委譲プレフィックスだけが降ってきて、
   `vendor_opts` が空のままになる
2. **Contact の user 部・`Allow`・`Supported: path` を揃えないと INVITE が 488 で弾かれる**
   (`Warning: 304 "Media type not available"`)。Contact の user 部にランダムな 12 桁 16 進
   トークンを使い (内線番号ではない)、`Allow` を付け、REGISTER では `Supported: path` も送る。
   この 3 つを同時に入れて通ったので、どれが必須かは切り分けていない
3. **HGW は IPv6 の REGISTER に一切応答しない**。送信元を委譲プレフィックス側にしても
   オンリンク側にしても無反応で、IPv4 での登録が必須
4. **多ホーム環境の経路の曖昧さ**。同じ /64 が複数 IF に付いていると HGW 宛が別 IF へ出る。
   `--bind-device` で回避できる。さらに、HGW をデフォルトゲートウェイにできない機器では
   メディア宛先への経路が無く、**呼は確立するのに `sendto: network is unreachable`** になる。
   既定の `--media-route auto` が呼の間だけホスト経路を足すので、通常は意識しなくてよい
5. **そもそも HGW 宛の経路が無い**。4 は経路が曖昧なケースだが、RA を受けていない構成では
   HGW が居る /64 への経路が 1 本も無い。送信元アドレスはスクリプトが付けてくれるので、
   アドレスだけ見ていると気づけない。HGW と同じ /64 に手で付けたアドレスが直結経路を
   作っていて、たまたま動いていたという状態にもなりやすい。そのアドレスを掃除した
   とたんに発信できなくなる (1.6)
6. **HGW の DHCPv6-PD のバインディングが切れると INVITE が黙殺される**。`100 Trying` すら
   返らないので 488 などより分かりにくい。`dhclient -6` を二重に起動していて RENEW が通らず
   REBIND に落ちていた例では、リースは残っているのに HGW 側では無効になっていた。
   取り直したら別のプレフィックスが降ってきて、そのアドレスからは一発で通った
7. 相手によっては無通信が続くと向こうから BYE してくる (光テレホンJJY では 22 秒で切られた)。
   `--media-timeout` を短くしすぎるとこちらが先に切ってしまうので、疎通確認は確立直後に流すこと
   (確立後のコマンド実行を使えば、確立直後に流れることが構造的に保証される)

## 未実装 / 今後

- `--dial-on-demand` (tun を先に作り、最初のパケットで発信する ISDN 的な動作)
- Digest 認証 (検証環境では不要だった。要求されたら明示的にエラーにする)
- DHCPv6 の内蔵 (今は dhclient に依存している。Vendor Class の送出まで含めて自前で持てば
  設定ファイルの手書きが要らなくなる)
- リースの健全性の確認 (委譲プレフィックスが失効すると発信が黙って失敗するので、発信前に ngntun 側から気づけるようにしたい)
- 着信 (`--answer`) の実機検証

## 免責

このソフトウェアは無保証で提供されます (MIT ライセンス)。

データコネクトは従量課金のサービスであり、**このコマンドは実際に電話を発信します**。
料金・回線・接続先に関する責任は利用者が負うものとします。
動作は特定の HGW 1 機種での実測に基づいており、他の機種やファームウェアでは
挙動が異なる可能性があります。

## ライセンス

[MIT License](LICENSE)
