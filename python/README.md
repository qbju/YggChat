# yggchat — YggChat Bot SDK for Python (beta)

YggChatのボットをPythonで書くためのSDKです。標準ライブラリだけで動きます（Python 3.8+）。

ボット＝**普通のYggChatノード（アプリ版のバイナリ）＋それを操作するスクリプト**です。Yggdrasil・暗号・再送・オフライン時のキューはノードがやり、このSDKはノードのローカルAPI（127.0.0.1、`API.md`）を包むだけです。ボット用の秘密鍵はノードのデータフォルダから出ません。

## 使い方

```sh
# 1. ボット用のノードを起動（データとUIポートを分ける）
yggchat-linux-amd64 -data ~/.yggchat-bot -ui 127.0.0.1:7790 -nowindow

# 2. SDKを入れる
pip install "git+https://github.com/qbju/YggChat.git@bot/python#subdirectory=python"
```

```python
from yggchat import Bot

bot = Bot(data_dir="~/.yggchat-bot", url="http://127.0.0.1:7790")

@bot.command("ping")          # 「/ping」
def ping(msg, args):
    msg.reply("pong")

@bot.on_message               # コマンド以外の全メッセージ
def echo(msg):
    msg.reply("🦜 " + msg.text)

bot.set_profile(nick="おうむ返し", avatar="🦜")
bot.run()                     # ノードが再起動しても自動で再接続
```

`examples/` に echo（🦜）と dice（`/roll 2d6`）があります。

## API

- `Bot(url, key=None, data_dir, auto_accept=True, prefix="/", threaded=False)` — `key` を省くと `<data_dir>/ui-key` を読みます。`auto_accept` は知らない人からのメッセージリクエストを自動で承認（相手にボットの名前とアイコンが見える）。
- デコレータ: `@bot.on_message` / `@bot.command("name", "alias")` → `fn(msg, args)` / `@bot.on("status")` などの生イベント
- `Message`: `id, chat`（返信先。個人ならアドレス、グループなら `g:<id>`）`, sender, nick, text, ts, image`（data: URL）`, is_group, reply(text), reply_image(path)`
- `Client`（`Bot` も同じものを持つ）: `state(), addr(), contacts(), messages(peer), wait_ready(), send(to, text), send_image(to, path, caption), edit(), unsend(), add_contact(), accept(), block(), set_profile(nick, bio, avatar, domain), resolve(domain), create_group(), update_group(), leave_group(), events()`

## 注意

- ベータです。YggChat本体と同じく互換性のない変更がよく入ります。
- ボットのノードが落ちている間に届かなかったメッセージは、送り主の端末にキューされて、ボットが戻ったら再送されます（Nostr置き配をオンにしていればそれ経由でも届きます）。
- 起動前に溜まっていた履歴には返事しません。
- 画像は600KBまで、グループは文字だけです（本体の制限）。

GPL-3.0-or-later
