# YggChat

NAT越え不要のP2Pチャット（Yggdrasil内蔵）。現在のバージョン: **v0.11.2**

[![build](https://github.com/qbju/YggChat/actions/workflows/build.yml/badge.svg)](https://github.com/qbju/YggChat/actions/workflows/build.yml)

## 使い方

| 環境 | 使い方 |
| --- | --- |
| PWA（インストール不要） | <https://yggchat.surge.sh/> |
| Windows | `yggchat-windows-amd64.exe` を起動 → ブラウザで <http://127.0.0.1:7780/> |
| Android | APK版推奨。Termuxなら `yggchat-android-arm64` |
| Linux | `./yggchat-linux-amd64` |

- データ: `~/.yggchat/`（`node.json` = 秘密鍵、`chat.json` = 履歴、どちらも `0600`）
- オプション: `-peers` `-listen` `-ui` `-data` `-cli` `-licenses`
- ビルド済みファイルは [Releases](https://github.com/qbju/YggChat/releases) / [Actions](https://github.com/qbju/YggChat/actions) と、アプリの設定→ダウンロードから入手できます。

## 端末のYggdrasilを使う（VPN型アプリと併用）

Yggdrasil公式Androidアプリや、PCで常駐しているyggdrasilサービス（TUN）があれば、**設定→ピア→「端末のYggdrasilを使う」** でそちら経由に切り替えられます（またはデータフォルダに `system-ygg` ファイル、環境変数 `YGGCHAT_SYSTEM=1`）。

- アドレスは端末側Yggdrasilのものになり、ピア設定もそちらで行います。
- 内蔵ノードの鍵は残るので、オフにすれば元のアドレスに戻ります。
- このモードではNostr置き配は使えません。
- 受信はUDP 7777番をOS上で待ち受け、送信元が `200::/7` のものだけを受け付けます。

## YggChatを端末のYggdrasilにする（Android VPNモード）

**設定→ピア→「YggChatを端末のYggdrasilにする（VPN）」** をオンにすると、公式アプリの代わりになります。

- `200::/7` 宛てだけをVPN（TUN）で受け持ち、ブラウザやSSHなど他のアプリもYggdrasilに接続できます。
- アドレス・鍵はYggChatのまま。通常のネット通信はVPNを通りません（allowBypass / IPv4は素通し）。
- TUNのfdは同じアプリ（同じUID）からだけ受け取ります。公式アプリのVPNとは同時に使えません。
- **「外からの接続」** で、Yggdrasil上の他人からこの端末への接続を選べます: すべて閉じる（既定）／指定ポートだけ開ける／すべて開ける。
  閉じている間も、自分から開いた接続の返事・YggChatのチャット（UDP 7777）・オン時のfingerは通り、それ以外は黙って捨てる（pingにも答えない）ので、外からは空のホストに見えます。

## LANチャット（アプリ版のみ・既定オフ）

**設定→LANチャット** をオンにすると、Yggdrasilのマルチキャスト探索で同じWi-Fi・有線LAN・テザリング上のノードを見つけ、リンクローカルで直接ピアになります。インターネットや公開ピアがなくても同じLAN内で会話できます。

- 「近くのノード」に表示名（互いにLANで直接つながっている相手とだけ交換）とアドレスが出て、「話す」で開けます。
- LANパスワードを設定すると、同じパスワードのノードとだけピアになります（Yggdrasil公式のmulticast Password）。
- オンの間、同じLANの人にはあなたのアドレスと表示名が見えます。Androidでは有効中だけMulticastLockを取ります。
- システムYggdrasilモードでは端末側Yggdrasilの設定に従います。PWAはLANを使えません。

## finger（.plan公開・既定オフ）

**設定→finger** をオンにすると、Yggdrasil上の誰でも `finger @[あなたのアドレス]`（TCP 79番, RFC 1288）で表示名と `.plan` を見られます。

- オンライン状態・連絡先・鍵などは返しません。転送（`user@host@host`）は拒否します。
- オフの間はTCP 79番の待ち受け自体を閉じ、UDPの問い合わせにも無言なので、外からYggChatだと見分けられません。
- 他人をfingerすると、問い合わせ先にはあなたのアドレスが見えます。
- アプリ内の「fingerで調べる」や連絡先の「finger」ボタンで、他のYggChat（UDPの `{t:"fq"}`）や一般のfingerサーバー（TCP 79）を問い合わせられます。
- AndroidのVPNモードではOSが79番を開けないため、TUNの中継で79番⇔7979番を付け替えます。
- PWAはTCPを使えないので、YggChat同士の問い合わせだけ対応です。

## メッセージの編集・削除

吹き出しを長押し（PCは右クリック）→ コピー／編集／自分の画面から削除／相手の画面からも削除。

- 編集と送信取り消しは自分のメッセージだけで、相手は元の送信者のアドレスからの指示しか受け付けません。
- 相手がオフラインなら、つながったときに反映されます。
- すでに読まれた内容やNostr置き配に預けた分は消せません。

## 端末チャット（CLI）

```sh
./yggchat-linux-amd64 -cli        # Termux: ./yggchat-android-arm64 -cli
```

同じ `-ui` でノードが動いていればそれにつながり、なければその場でノードを起動します（ノードのログは `データ/node.log` へ）。

`/help` でコマンド一覧。`/list`、`/open <番号|名前>`、`/add <アドレス> [名前]`。それ以外の入力は開いている相手に送信。受信はリアルタイムで表示されます。

## 暗号と通信の仕組み

- メッセージはYggdrasilのセッション暗号で端末間エンドツーエンドに暗号化されます。アドレスは公開鍵から導出されるため、QR／招待リンクでアドレスを確認すれば相手の鍵も確認できます。
- 受信メッセージの送信者は、Yggdrasilが暗号的に保証する送信元IPv6アドレスで判定しています（自己申告は使いません）。
- **ネイティブ版**: ノードは端末内で動作。UIは `127.0.0.1` のみで待ち受け、Host／Origin／Content-Typeを検証します。
- **PWA版**: ノードはブラウザ内（WebAssembly）で動作し、秘密鍵と履歴はブラウザのIndexedDBにだけ保存されます。
  - ブラウザは生のTCP/UDPを開けないため、ブラウザのOriginを許可したYggdrasilピアにwssで直接つなぎます。
  - 既定は `ygg.being.pet`（2026-10-19時点で公開一覧から見つかった唯一のブラウザ可ピア）。
  - YggChat側の中継サーバーはありません。ピアが見るのは暗号化済みのYggdrasilリンク通信だけです（接続元IPと通信量・時刻は見えます）。
  - 自分でピアを立てる場合: Listen に `"ws://[::]:PORT?origin=https://yggchat.surge.sh"` を書き、TLS終端してwssで公開してください。
  - PWAはタブ／アプリを開いている間だけ動きます。
- パスコードを設定すると、端末内の履歴はAES-256-GCMで暗号化して保存されます（ネイティブ: scrypt、PWA: PBKDF2-SHA256 31万回）。ロック中は受信せず、相手側で保留されます。秘密鍵（`node.json`／PWAの鍵）は暗号化対象外です。
- オフラインの相手宛ては送信側の端末に「送信待ち」として残り、双方がオンラインになったときに自動で送ります。
  既定ではサーバーに預ける仕組み（置き配）はありません。設定で「置き配（Nostr）」をオンにした場合のみ（非推奨）、届かなかった文章を相手の鍵で封をして公開Nostrリレーに最大7日預けます（相手もオンにしている必要あり）。
- 知らない相手からはメッセージリクエストとして届き、承認するまでこちらの名前・自己紹介・アイコンは共有されません。

## ビルド

Go（`go.mod` のバージョン）が必要です。Twemojiの画像はリポジトリに含めていないので、最初に取得します。

```sh
./twemoji/fetch.sh                      # twemoji/svg.zip を作成
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -checklinkname=0" .
GOOS=js GOARCH=wasm go build -trimpath -ldflags="-s -w" -o pwa/ygg.wasm ./wasm
```

Android APK（Gradle不使用）: `libyggchat.so`（`GOOS=android GOARCH=arm64` のバイナリ）を `android/` に置いて `android/build.sh`。
`ANDROID_HOME`、`YGG_KEYSTORE`、`YGG_KS_PASS` で SDK と署名鍵を指定できます。GitHub Actions（`.github/workflows/build.yml`）が push ごとにAPKと各バイナリを作り、`v*` タグでReleaseに載せます。署名鍵は `android/yggchat.jks.enc`（AES-256で暗号化）で、復号キーはリポジトリのSecret `KEYSTORE_KEY` にだけあります。Secretがない場合のAPKは使い捨て鍵で署名され、インストール済みのYggChatを上書きできません。PWA（yggchat.surge.sh）の公開はActionsとは別に行っています。

## 検証

`SHA256SUMS` で各ファイルのハッシュを確認できます。バイナリは未署名のため、WindowsではSmartScreenの警告が出ます。

## 履歴

| バージョン | 内容 |
| --- | --- |
| v0.11 | メッセージの編集・削除、VPNの受信ファイアウォール、Twemoji同梱、スクロール修正。 |
| v0.10 | LANチャット、finger。ライセンス表記（GPL-3.0-or-later）。 |
| v0.9 | AndroidのVPNモード（YggChatが端末のYggdrasilになり、他アプリも `200::/7` へ）。 |
| v0.8 | CLIチャット（`-cli`）。端末のYggdrasil（VPN型アプリ／TUN）を使うモード。Android版はアプリ自身が通知を出すように（WebViewには通知機能がないため）、画面を閉じていても受信通知が出ます。設定に「置き配（Nostr）」を追加（非推奨・既定オフ）。届かなかった文章を相手の鍵で暗号化し、使い捨て鍵で署名したNostrイベントとして公開リレーに最大7日預ける。受信側は読んだら削除イベントを出す。リレーには宛先タグ・時刻・IPが見える。写真は対象外。 |
| v0.7.1 | PWAの自前入口ピアを撤去し、公開ピア `ygg.being.pet` のみに。 |
| v0.7 | Cloudflare中継と中継メールボックス（置き配）を廃止。PWAはOrigin許可済みピアにwssで直接接続。旧メールボックスのデータはサーバー側で削除済み。 |
| v0.6 | 写真の送信（端末内で最大1600px・約400KB以下のJPEGに縮小し、Ygg越しに約1KBずつ分割送信、SHA-256で検証）。タップで拡大・保存。PWAの保存先をIndexedDBに移行（自動移行）。写真は中継メールボックスを使わず、相手がオンラインになったときに直接送ります。 |
| v0.5 | パスコードによる履歴暗号化とロック、消えるメッセージ（会話ごと・相手にも反映）、メッセージリクエストと承認時のプロフィール共有（名前・自己紹介・絵文字アイコン）、ブロック、新しいIDの作成、通知の内容を隠す、画面のセキュリティ（Android）、中継メールボックス、テーマ（システム／ライト／ダーク）。 |
| v0.4.2 | 日付をまたぐとチャット画面が空になる不具合を修正。属性値のエスケープ、招待リンクの検証、受信IDとニックネームの検証、ローカルAPIのHost／Origin／Content-Type検証、鍵・履歴のアトミック書き込みと0600化。 |
| v0.4 | 相手がオフラインのときは「オンライン待ち」になり、双方オンラインで自動再送。 |
| v0.3 | Signal/Session風UI、送信状態、QRと招待リンク、ピア管理、公式public-peers一覧、通知。 |

v0.2以降はUDP/7777。v0.1とは非互換。

## ライセンスとクレジット

YggChat is free software, licensed under the GNU General Public License v3.0 or later ([LICENSE](LICENSE)).
Copyright (C) 2026 h6e and YggChat contributors.

- 内蔵しているYggdrasil Network（yggdrasil-go/yggstack: LGPLv3+静的リンク例外、ironwood/phony: MPL-2.0）、gVisor（Apache-2.0）、Go（BSD-3-Clause）、quic-go（MIT）、coder/websocket（ISC）、qrcode-generator（MIT, Kazuhiko Arase）などの一覧と全文は [THIRD_PARTY_NOTICES.txt](THIRD_PARTY_NOTICES.txt) にあります（アプリの設定→About、または `yggchat -licenses` でも表示）。
- yggdrasil-go は WebAssembly 対応のため一部ファイルを改変しています（該当ファイルに改変の注記あり）。
- 絵文字は[Twemoji](https://github.com/jdecked/twemoji)（© Twitter, Inc. and other contributors, CC-BY 4.0）の画像を同梱して表示します（CDN不使用）。
- 「QRコード」は株式会社デンソーウェーブの登録商標です。
