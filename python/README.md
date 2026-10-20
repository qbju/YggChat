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

`examples/` に echo（🦜）、dice（`/roll 2d6`）、public_group（公開グループの番人）があります。

## 公開グループBot

`examples/public_group.py` は、招待リンクを知っていれば誰でも入れるグループを常時運用するボットです。ボットのノードがホストになって起動しっぱなしにするので、人間が寝ていても参加できます。

- 参加申請を自動承認（BAN済みは拒否、1時間あたりの承認数に上限、満員なら拒否）
- 入ったあとは連投（既定 10秒に6件超）とNGワード（`--ng ファイル`）で自動で外してBAN
- `/rules` `/link`、副管理者は `/ban <アドレス>` `/unban <アドレス>`
- 新メンバーにルールを案内、履歴共有オン、既定で写真なし・PoW 8ビット

```sh
python3 examples/public_group.py --data ~/.yggchat-bot --url http://127.0.0.1:7790 \
    --name "Ygg雑談" --seed <自分のアドレス> --admin <自分のアドレス>
```

グループは最初に1人以上のメンバーが要るので、初回だけ `--seed` に自分のアドレスを渡します（`--admin` で自分を副管理者に）。グループID・BAN一覧は `<data>/public_group.json` に保存されます。誰でも入れる以上、中身は実質公開だと考えてください。

## API

- `Bot(url, key=None, data_dir, auto_accept=True, prefix="/", threaded=False)` — `key` を省くと `<data_dir>/ui-key` を読みます。`auto_accept` は知らない人からのメッセージリクエストを自動で承認（相手にボットの名前とアイコンが見える）。
- デコレータ: `@bot.on_message` / `@bot.command("name", "alias")` → `fn(msg, args)` / `@bot.on("status")` などの生イベント
- `@bot.on_join_request` → `fn(req)`（承認制グループの参加申請。`req.group, addr, nick, approve(), reject()`）
- `Message`: `id, chat`（返信先。個人ならアドレス、グループなら `g:<id>`）`, sender, nick, text, ts, image`（data: URL）`, is_group, reply(text), reply_image(path)`
- `Client`（`Bot` も同じものを持つ）: `state(), addr(), contacts(), messages(peer), wait_ready(), send(to, text), send_image(to, path, caption), edit(), unsend(), add_contact(), accept(), block(), set_profile(nick, bio, avatar, domain), resolve(domain), create_group(), groups(), group(), update_group(name, add, remove, settings, password, admins), group_settings(**changes), remove_members(), invite_link(reset), join_group(link), pending(), approve(), leave_group(), events()`

## 注意

- ベータです。YggChat本体と同じく互換性のない変更がよく入ります。
- ボットのノードが落ちている間に届かなかったメッセージは、送り主の端末にキューされて、ボットが戻ったら再送されます（Nostr置き配をオンにしていればそれ経由でも届きます）。
- 起動前に溜まっていた履歴には返事しません。
- 画像は600KBまで、グループは文字だけです（本体の制限）。

GPL-3.0-or-later
