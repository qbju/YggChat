# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 h6e and YggChat contributors
"""Event loop + handler decorators for bots."""
import shlex
import sys
import threading
import time
import traceback

from .client import Client, APIError, Locked


class Contact:
    def __init__(self, d):
        self.raw = d
        self.addr = d.get("addr", "")
        self.nick = d.get("nick", "")
        self.bio = d.get("bio", "")
        self.avatar = d.get("avatar", "")
        self.request = bool(d.get("req"))        # message request not accepted yet
        self.group = bool(d.get("group"))
        self.members = d.get("members") or []
        self.domain = d.get("dom") if d.get("domok") else None  # verified domain only

    def __repr__(self):
        return "Contact(%r, %r)" % (self.addr, self.nick)


class Message:
    """An incoming message. `chat` is where to answer (the peer or "g:<id>")."""

    def __init__(self, bot, d):
        self.bot = bot
        self.raw = d
        self.id = d.get("id", "")
        self.chat = d.get("peer", "")
        self.sender = d.get("from") or self.chat  # in groups: the member who wrote it
        self.nick = d.get("nick", "")
        self.text = d.get("text") or ""
        self.ts = d.get("ts", 0)
        self.image = d.get("img")  # data: URL or None
        self.ttl = d.get("ttl", 0)
        self.edited = bool(d.get("edited"))

    @property
    def is_group(self):
        return self.chat.startswith("g:")

    def reply(self, text):
        return self.bot.send(self.chat, text)

    def reply_image(self, image, caption=""):
        return self.bot.send_image(self.chat, image, caption)

    def __repr__(self):
        return "Message(%r from %r: %r)" % (self.id, self.sender, self.text[:40])


class Bot(Client):
    """A Client with handlers. Handlers run one at a time on the event thread
    (use threaded=True to run each message handler in its own thread).

    auto_accept: accept message requests from strangers automatically, so they
                 get our profile (nick/avatar). Off = only reply.
    prefix:      command prefix for @bot.command (default "/").
    """

    def __init__(self, url="http://127.0.0.1:7780", key=None, data_dir="~/.yggchat",
                 auto_accept=True, prefix="/", threaded=False, log=True, **kw):
        super().__init__(url=url, key=key, data_dir=data_dir, **kw)
        self.auto_accept = auto_accept
        self.prefix = prefix
        self.threaded = threaded
        self.log = log
        self._msg = []
        self._cmd = {}
        self._ev = {}
        self._seen = {}
        self._stop = threading.Event()
        self.me = None

    # ---------- decorators ----------

    def on_message(self, fn):
        """@bot.on_message — called with Message for every incoming text/image
        that no command handled."""
        self._msg.append(fn)
        return fn

    def command(self, name, *aliases):
        """@bot.command("help") — "/help a b" calls fn(msg, ["a", "b"])."""
        def deco(fn):
            for n in (name,) + aliases:
                self._cmd[n.lower()] = fn
            return fn
        return deco

    def on(self, event_type):
        """@bot.on("status") — raw event handler: fn(data). Types: message,
        status, contact, contact_del, msg_upd, msg_del, peers, locked, move."""
        def deco(fn):
            self._ev.setdefault(event_type, []).append(fn)
            return fn
        return deco

    # ---------- loop ----------

    def stop(self):
        self._stop.set()

    def _say(self, *a):
        if self.log:
            print(time.strftime("%H:%M:%S"), *a, file=sys.stderr, flush=True)

    def _call(self, fn, *args):
        try:
            fn(*args)
        except Exception:
            traceback.print_exc()

    def _handle_message(self, m):
        if m.text.startswith(self.prefix) and len(m.text) > len(self.prefix):
            body = m.text[len(self.prefix):]
            try:
                parts = shlex.split(body)
            except ValueError:
                parts = body.split()
            if parts:
                fn = self._cmd.get(parts[0].lower())
                if fn:
                    fn(m, parts[1:])
                    return
        for fn in self._msg:
            fn(m)

    def _dispatch(self, ev):
        typ, data = ev.get("type"), ev.get("data") or {}
        for fn in self._ev.get(typ, []):
            self._call(fn, data)
        if typ == "contact" and self.auto_accept and data.get("req") and not data.get("group"):
            try:
                self.accept(data["addr"])
            except APIError as e:
                self._say("accept failed", e)
        if typ != "message" or data.get("out") or data.get("sys") or data.get("hid"):
            return
        key = (data.get("peer"), data.get("id"))
        if key in self._seen:
            return
        self._seen[key] = time.time()
        if len(self._seen) > 5000:
            cut = sorted(self._seen.values())[2500]
            self._seen = {k: v for k, v in self._seen.items() if v > cut}
        m = Message(self, data)
        self._say("<-", m.sender, repr(m.text[:60]))
        if self.threaded:
            threading.Thread(target=self._call, args=(self._handle_message, m), daemon=True).start()
        else:
            self._call(self._handle_message, m)

    def run(self):
        """Connects to the node and handles events until stop() / Ctrl-C.
        Reconnects automatically when the node restarts."""
        delay = 1
        while not self._stop.is_set():
            try:
                st = self.state()
                if st.get("locked"):
                    raise Locked(423, "locked")
                self.me = st.get("addr")
                # don't answer history that was already there before we started
                for m in st.get("messages") or []:
                    self._seen[(m.get("peer"), m.get("id"))] = time.time()
                self._say("bot up as", self.me, "(%s)" % st.get("nick", ""))
                delay = 1
                for ev in self.events():
                    if self._stop.is_set():
                        break
                    self._dispatch(ev)
            except KeyboardInterrupt:
                break
            except Locked:
                self._say("node is locked with a passcode; unlock it in the app")
            except Exception as e:
                self._say("connection lost:", e)
            if self._stop.wait(delay):
                break
            delay = min(delay * 2, 30)
