"""Group-friendly dice bot: /roll 2d6"""
import random, re
from yggchat import Bot

bot = Bot(data_dir="~/.yggchat-dice", url="http://127.0.0.1:7781")


@bot.command("roll", "r")
def roll(msg, args):
    m = re.fullmatch(r"(\d*)d(\d+)", args[0] if args else "1d6")
    if not m:
        return msg.reply("使い方: /roll 2d6")
    n, s = int(m.group(1) or 1), int(m.group(2))
    if not (1 <= n <= 20 and 2 <= s <= 1000):
        return msg.reply("1〜20個、2〜1000面まで")
    rs = [random.randint(1, s) for _ in range(n)]
    msg.reply("🎲 %s = %d" % (" + ".join(map(str, rs)), sum(rs)))


bot.run()
