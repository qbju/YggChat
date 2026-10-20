# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 h6e and YggChat contributors
"""Thin wrapper over the YggChat local HTTP API (see API.md)."""
import base64
import json
import mimetypes
import os
import time
import urllib.error
import urllib.request

# The node only listens on loopback: never route through HTTP(S)_PROXY.
_open = urllib.request.build_opener(urllib.request.ProxyHandler({})).open

IMG_MAX = 600 * 1024  # the node's limit for an image after compression
IMG_TYPES = {"image/jpeg", "image/png", "image/webp", "image/gif"}


class APIError(Exception):
    """The node answered with an error (status code + its message)."""

    def __init__(self, status, message):
        super().__init__("%s: %s" % (status, message))
        self.status = status
        self.message = message


class Locked(APIError):
    """The node's data is locked with a passcode (HTTP 423)."""


class Client:
    """Talks to one YggChat node.

    url:      the node's UI address, e.g. "http://127.0.0.1:7780" (loopback only).
    key:      the UI key; by default read from <data_dir>/ui-key.
    data_dir: the node's -data directory (default ~/.yggchat).
    """

    def __init__(self, url="http://127.0.0.1:7780", key=None, data_dir="~/.yggchat", timeout=15):
        self.url = url.rstrip("/")
        self.data_dir = os.path.expanduser(data_dir) if data_dir else None
        self._key = key
        self.timeout = timeout

    # ---------- plumbing ----------

    @property
    def key(self):
        if self._key:
            return self._key
        if not self.data_dir:
            return ""
        try:
            with open(os.path.join(self.data_dir, "ui-key")) as f:
                return f.read().strip()  # re-read: the node may have been re-created
        except OSError:
            return ""

    def _headers(self, extra=None):
        h = {"X-YggChat-Key": self.key, "Origin": self.url}
        h.update(extra or {})
        return h

    def request(self, path, body=None, timeout=None):
        """GET (body None) or POST JSON to /api/<path>; returns parsed JSON."""
        data = None
        headers = self._headers()
        if body is not None:
            data = json.dumps(body).encode()
            headers["Content-Type"] = "application/json"
        req = urllib.request.Request(self.url + "/api/" + path.lstrip("/"), data=data, headers=headers)
        try:
            with _open(req, timeout=timeout or self.timeout) as r:
                raw = r.read()
        except urllib.error.HTTPError as e:
            msg = e.read().decode("utf-8", "replace").strip()
            if e.code == 423:
                raise Locked(e.code, msg or "locked") from None
            raise APIError(e.code, msg or e.reason) from None
        if not raw:
            return {}
        try:
            return json.loads(raw)
        except ValueError:
            return raw.decode("utf-8", "replace")

    def events(self, timeout=90):
        """Yields {type, data} events forever-ish (until the stream drops).

        Raises on connection errors; Bot.run() reconnects for you.
        """
        req = urllib.request.Request(self.url + "/api/events", headers=self._headers())
        with _open(req, timeout=timeout) as r:
            for raw in r:
                line = raw.decode("utf-8", "replace").strip()
                if line.startswith("data:"):
                    try:
                        yield json.loads(line[5:].strip())
                    except ValueError:
                        pass

    # ---------- state ----------

    def state(self):
        return self.request("state")

    def addr(self):
        """Our own Yggdrasil address."""
        return self.state()["addr"]

    def contacts(self):
        return self.state().get("contacts") or {}

    def messages(self, peer=None):
        ms = self.state().get("messages") or []
        return [m for m in ms if peer is None or m.get("peer") == peer]

    def wait_ready(self, peers=1, timeout=120):
        """Blocks until the node answers and has at least `peers` Yggdrasil peers up."""
        end = time.time() + timeout
        while True:
            try:
                st = self.state()
                if st.get("locked"):
                    raise Locked(423, "locked")
                if (st.get("peers") or 0) >= peers:
                    return st
            except (OSError, APIError) as e:
                if isinstance(e, Locked):
                    raise
            if time.time() > end:
                raise TimeoutError("node not ready")
            time.sleep(1)

    # ---------- messages ----------

    def send(self, to, text):
        """Sends text to an address or a group ("g:<id>"). Returns the message."""
        return self.request("send", {"To": to, "Text": text})

    def retry(self, to, msg_id):
        return self.request("send", {"To": to, "Retry": msg_id})

    def send_image(self, to, image, caption="", mime=None):
        """Sends an image (path, bytes, or data: URL). Max 600 KB; not for groups."""
        if isinstance(image, str) and image.startswith("data:"):
            url = image
        else:
            if isinstance(image, (bytes, bytearray)):
                raw = bytes(image)
            else:
                with open(image, "rb") as f:
                    raw = f.read()
                mime = mime or mimetypes.guess_type(image)[0]
            mime = mime or "image/jpeg"
            if mime not in IMG_TYPES:
                raise ValueError("unsupported image type: %s" % mime)
            if len(raw) > IMG_MAX:
                raise ValueError("image too large (max 600 KB)")
            url = "data:%s;base64,%s" % (mime, base64.b64encode(raw).decode())
        body = {"To": to, "Img": url}
        if caption:
            body["Text"] = caption
        return self.request("send", body)

    def edit(self, to, msg_id, text):
        return self.request("message/edit", {"Peer": to, "ID": msg_id, "Text": text})

    def unsend(self, to, msg_id):
        return self.request("message/delete", {"Peer": to, "ID": msg_id, "Everyone": True})

    # ---------- contacts / profile ----------

    def add_contact(self, addr, nick=None):
        body = {"Addr": addr}
        if nick:
            body["Nick"] = nick
        return self.request("contact", body)

    def accept(self, addr):
        """Accepts a message request (shares our profile with them)."""
        return self.request("contact", {"Addr": addr, "Accept": True})

    def delete_contact(self, addr):
        return self.request("contact/delete", {"Addr": addr})

    def block(self, addr, block=True):
        return self.request("block", {"Addr": addr, "Block": block})

    def set_profile(self, nick=None, bio=None, avatar=None, domain=None):
        """Sets profile fields (None = keep). Domain needs an AAAA = our address."""
        st = self.state()
        body = {"Nick": nick if nick is not None else st.get("nick", ""),
                "Bio": bio if bio is not None else st.get("bio", ""),
                "Avatar": avatar if avatar is not None else st.get("avatar", "")}
        if domain is not None:
            body["Domain"] = domain
        return self.request("profile", body)

    def resolve(self, name):
        """Domain -> Yggdrasil address (its 200::/7 AAAA)."""
        return self.request("resolve", {"Name": name})["addr"]

    # ---------- groups ----------

    def create_group(self, name, members):
        return self.request("group/create", {"Name": name, "Members": list(members)})

    def groups(self):
        """Our groups: {"g:<id>": view} (view has members, owner, admins, set, pend, max)."""
        return {k: v for k, v in self.contacts().items() if v.get("group")}

    def group(self, group_id):
        return self.contacts().get("g:" + _gid(group_id))

    def update_group(self, group_id, name=None, add=None, remove=None, settings=None,
                     password=None, admins=None):
        """Changes a group. add/remove need host or co-admin; settings/password/
        admins need the host. settings is a dict of the host's group settings:
        {"nf": no photos, "ap": approve link joins, "hi": history for newcomers,
        "ttl": seconds, "mx": member limit (24..1024), "pb": PoW bits per post
        (0 = default 3, -1 = none)}; it replaces the old ones."""
        body = {"ID": _gid(group_id)}
        if name:
            body["Name"] = name
        if add:
            body["Add"] = list(add)
        if remove:
            body["Remove"] = list(remove)
        if settings is not None:
            body["Set"] = settings
        if password is not None:
            body["Pass"] = password
        if admins is not None:
            body["Admins"] = list(admins)
        return self.request("group/update", body)

    def group_settings(self, group_id, **changes):
        """Merges changes into the current settings, e.g. group_settings(g, ap=True, hi=True)."""
        g = self.group(group_id) or {}
        cur = dict(g.get("set") or {})
        cur.update(changes)
        return self.update_group(group_id, settings=cur)

    def remove_members(self, group_id, addrs):
        return self.update_group(group_id, remove=addrs)

    def invite_link(self, group_id, reset=False):
        """Returns {"code", "link"} (host or co-admin). reset=True makes a new one."""
        return self.request("group/invite", {"ID": _gid(group_id), "Reset": bool(reset)})

    def join_group(self, code, password=""):
        """Joins with an invite link or its code."""
        if "#j=" in code:
            code = code.split("#j=", 1)[1]
        elif code.startswith("yggchat://join/"):
            code = code[len("yggchat://join/"):]
        return self.request("group/join", {"Code": code, "Pw": password})

    def pending(self, group_id):
        """Join requests waiting for approval: [{"addr", "nick", "ts"}]."""
        return (self.group(group_id) or {}).get("pend") or []

    def approve(self, group_id, addr, ok=True):
        """Lets a pending join request in (ok=False refuses it)."""
        return self.request("group/approve", {"ID": _gid(group_id), "Addr": addr, "OK": bool(ok)})

    def leave_group(self, group_id):
        return self.request("group/leave", {"ID": _gid(group_id)})


def _gid(g):
    """Accepts "g:<id>" or "<id>"."""
    return g[2:] if g.startswith("g:") else g
