YggChat v0.4.2 - NAT越え不要のP2Pチャット (Yggdrasil内蔵)

■ 使い方
PWA(インストール不要): https://yggchat.surge.sh/
Windows: yggchat-windows-amd64.exe → ブラウザで http://127.0.0.1:7780/
Android: APK版推奨。Termuxなら yggchat-android-arm64
Linux:   ./yggchat-linux-amd64
データ: ~/.yggchat/ (node.json=秘密鍵, chat.json=履歴, どちらも0600)
オプション: -peers -listen -ui -data

■ 暗号と通信の仕組み
- メッセージはYggdrasilのセッション暗号で端末間エンドツーエンドに暗号化されます。
  アドレスは公開鍵から導出されるため、QR/招待リンクでアドレスを確認すれば相手の鍵も確認できます。
- 受信メッセージの送信者は、Yggdrasilが暗号的に保証する送信元IPv6アドレスで判定しています(自己申告は使いません)。
- ネイティブ版: ノードは端末内で動作。UIは127.0.0.1のみで待ち受け、Host/Origin/Content-Typeを検証します。
- PWA版: ノードはブラウザ内(WebAssembly)で動作し、秘密鍵と履歴はブラウザのlocalStorageにだけ保存されます。
  ブラウザは生のTCP/UDPを開けないため、WebSocket中継(yggws.i2p.workers.dev)経由で公開ピアにつなぎます。
  中継が見るのは暗号化済みのYggdrasilリンク通信だけで、鍵や本文は見えません(接続先と通信量・時刻は見えます)。
  PWAはタブ/アプリを開いている間だけ動きます。
- 端末内の履歴は現状平文保存です(パスコード暗号化はv0.5予定)。

■ 検証
SHA256SUMS で各ファイルのハッシュを確認できます。バイナリは未署名のため、WindowsではSmartScreenの警告が出ます。

■ 履歴
v0.4.2: 日付をまたぐとチャット画面が空になる不具合を修正。属性値のエスケープ、招待リンクの検証、
        受信IDとニックネームの検証、ローカルAPIのHost/Origin/Content-Type検証、鍵・履歴のアトミック書き込みと0600化。
v0.4:   相手がオフラインのときは「オンライン待ち」になり、双方オンラインで自動再送。
v0.3:   Signal/Session風UI、送信状態、QRと招待リンク、ピア管理、公式public-peers一覧、通知。
v0.2以降はUDP/7777。v0.1とは非互換。
