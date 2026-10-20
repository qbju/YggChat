"""🦜 echo bot: repeats every message. Usage: python echo.py [data_dir] [ui_url]"""
import sys
from yggchat import Bot

bot = Bot(data_dir=sys.argv[1] if len(sys.argv) > 1 else "~/.yggchat",
          url=sys.argv[2] if len(sys.argv) > 2 else "http://127.0.0.1:7780")


@bot.command("help")
def help_(msg, args):
    msg.reply("🦜 送ったことをそのまま返すよ。/ping で生存確認。")


@bot.command("ping")
def ping(msg, args):
    msg.reply("pong")


@bot.on_message
def echo(msg):
    if msg.image:
        msg.reply("📷 写真を受け取ったよ" + ("：" + msg.text if msg.text else ""))
    elif msg.text:
        msg.reply("🦜 " + msg.text)


bot.run()
