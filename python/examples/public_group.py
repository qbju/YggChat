# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 h6e and YggChat contributors
"""Public group bot: keeps a group open to anyone with the invite link.

The bot's node is the group's host and stays online, so joins work while
the humans sleep. It approves join requests by simple rules and kicks
floods and NG words afterwards.

    python3 public_group.py --data ~/.yggchat-bot --url http://127.0.0.1:7790 \\
        --name "Ygg雑談" --seed <your address>

--seed is needed only the first time (a group starts with one other member;
make it your own address, and use --admin to make yourself a co-admin).
The group id, bans and counters are kept in <data>/public_group.json.
"""
import argparse
import json
import os
import re
import time
from collections import defaultdict, deque

from yggchat import Bot, APIError

ap = argparse.ArgumentParser()
ap.add_argument("--data", default="~/.yggchat-bot")
ap.add_argument("--url", default="http://127.0.0.1:7790")
ap.add_argument("--name", default="公開グループ")
ap.add_argument("--seed", help="first member (your address), only when creating")
ap.add_argument("--admin", action="append", default=[], help="make this member a co-admin (repeatable)")
ap.add_argument("--max", type=int, default=256, help="member limit 24..1024")
ap.add_argument("--pow", type=int, default=8, help="PoW bits per post (-1 none, 3..16)")
ap.add_argument("--joins-per-hour", type=int, default=30)
ap.add_argument("--flood", default="6/10", help="kick after N posts in S seconds")
ap.add_argument("--ng", default="", help="file with one NG word / regex per line")
ap.add_argument("--rules", default="ようこそ！荒らし・宣伝・連投は自動で外されます。/rules でこの文、/link で招待リンク。")
ap.add_argument("--photos", action="store_true", help="allow photos (default text only)")
args = ap.parse_args()

data_dir = os.path.expanduser(args.data)
STATE = os.path.join(data_dir, "public_group.json")
bot = Bot(url=args.url, data_dir=data_dir)


def load():
    try:
        with open(STATE) as f:
            return json.load(f)
    except (OSError, ValueError):
        return {}


def save():
    tmp = STATE + ".tmp"
    with open(tmp, "w") as f:
        json.dump(st, f, ensure_ascii=False, indent=1)
    os.replace(tmp, STATE)


st = load()
st.setdefault("banned", {})   # addr -> reason
bans = st["banned"]
ng = []
if args.ng:
    with open(os.path.expanduser(args.ng), encoding="utf-8") as f:
        ng = [re.compile(l.strip(), re.I) for l in f if l.strip() and not l.startswith("#")]
flood_n, flood_s = (int(x) for x in args.flood.split("/"))
posts = defaultdict(deque)     # addr -> recent post times
joins = deque()                # recent approval times


def setup():
    bot.wait_ready()
    g = st.get("group")
    if not g or not bot.group(g) or bot.group(g).get("left"):
        if not args.seed:
            raise SystemExit("no group yet: pass --seed <your address> to create it")
        g = bot.create_group(args.name, [args.seed])["addr"]
        st["group"] = g
        save()
    view = bot.group(g)
    want = dict(view.get("set") or {})
    want.update(ap=True, hi=True, nf=not args.photos, mx=args.max, pb=args.pow)
    if want != view.get("set"):
        bot.update_group(g, settings=want)
    admins = [a for a in args.admin if a in view.get("members", [])]
    if admins and sorted(admins) != sorted(view.get("admins") or []):
        bot.update_group(g, admins=admins)
    st["link"] = bot.invite_link(g)["link"]
    save()
    print("group", g, "invite link:", st["link"], flush=True)
    return g


GROUP = setup()


@bot.on_join_request
def on_join(req):
    if req.group != GROUP:
        return
    if req.addr in bans:
        req.reject()
        return
    now = time.time()
    while joins and now - joins[0] > 3600:
        joins.popleft()
    if len(joins) >= args.joins_per_hour:
        return  # leave it pending; looked at again after a restart or by a human admin
    if len(bot.group(GROUP).get("members", [])) >= args.max:
        req.reject()
        return
    try:
        req.approve()
        joins.append(now)
    except APIError as e:
        print("approve failed:", e, flush=True)


def kick(addr, reason):
    bans[addr] = reason
    save()
    try:
        bot.remove_members(GROUP, [addr])
    except APIError as e:
        print("kick failed:", e, flush=True)
        return
    bot.send(GROUP, "🚫 1人を外しました（%s）" % reason)


def is_staff(addr):
    g = bot.group(GROUP) or {}
    return addr == g.get("owner") or addr in (g.get("admins") or [])


@bot.command("rules", "help")
def rules(msg, a):
    msg.reply(args.rules)


@bot.command("link")
def link(msg, a):
    msg.reply(st.get("link", ""))


@bot.command("ban")
def ban(msg, a):
    """/ban <address> — co-admins can ban (kick + refuse rejoining)."""
    if msg.chat == GROUP and is_staff(msg.sender) and a:
        kick(a[0], "管理者による")


@bot.command("unban")
def unban(msg, a):
    if is_staff(msg.sender) and a and bans.pop(a[0], None) is not None:
        save()
        msg.reply("解除しました")


@bot.on_message
def watch(msg):
    if msg.chat != GROUP or is_staff(msg.sender):
        return
    q = posts[msg.sender]
    now = time.time()
    q.append(now)
    while q and now - q[0] > flood_s:
        q.popleft()
    if len(q) > flood_n:
        q.clear()
        kick(msg.sender, "連投")
        return
    for r in ng:
        if r.search(msg.text):
            kick(msg.sender, "NGワード")
            return


@bot.on("contact")
def greet(view):
    # welcome once per new member (the host's member list just grew)
    if view.get("addr") != GROUP:
        return
    known = set(st.get("known") or [])
    members = set(view.get("members") or [])
    new = members - known - {bot.me}
    if known and new:
        bot.send(GROUP, "👋 " + args.rules)
    st["known"] = sorted(members)
    save()


bot.set_profile(nick=args.name + "の番人", avatar="🛡️")
bot.run()
