# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 h6e and YggChat contributors
"""YggChat Python SDK for bots.

A bot is an ordinary YggChat node (the native app/daemon) plus a script that
talks to the node's local API on 127.0.0.1. The node does Yggdrasil, crypto,
retries and offline queueing; this package only wraps its HTTP API and event
stream. Standard library only.

    from yggchat import Bot

    bot = Bot(data_dir="~/.yggchat-bot")   # reads <data>/ui-key

    @bot.command("ping")
    def ping(msg, args):
        msg.reply("pong")

    @bot.on_message
    def echo(msg):
        msg.reply("🦜 " + msg.text)

    bot.run()
"""
from .client import Client, APIError, Locked
from .bot import Bot, Message, Contact

__all__ = ["Client", "Bot", "Message", "Contact", "APIError", "Locked"]
__version__ = "0.1.0"
