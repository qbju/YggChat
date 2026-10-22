# Find Yggdrasil public wss peers that accept browser Origin https://yggchat.surge.sh (HTTP 101).
import os
import sys, re, io, zipfile, subprocess, datetime
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from common import *
DEFAULT = "wss://ygg.being.pet/ygg"
cands = {DEFAULT}
rx = re.compile(r"wss://[^\s\"'<>`)\]]+")
for u in ["https://publicpeers.neilalexander.dev/publicnodes.json", "https://yggpeers.fsky.io/peers.json"]:
    try: cands |= set(rx.findall(get(u).decode("utf-8", "ignore")))
    except Exception as e: print("fetch fail", u, e)
try:
    z = zipfile.ZipFile(io.BytesIO(get("https://codeload.github.com/yggdrasil-network/public-peers/zip/refs/heads/master", 60)))
    for n in z.namelist():
        if n.endswith(".md"): cands |= set(rx.findall(z.read(n).decode("utf-8", "ignore")))
except Exception as e: print("zip fail", e)
cands = {c.split("?")[0].rstrip(".,") for c in cands}
def probe(u):
    url = "https://" + u[len("wss://"):]
    for _ in range(2):
        r = subprocess.run(["curl", "-s", "-o", "/dev/null", "-w", "%{http_code}", "--http1.1", "-m", "8",
            "-H", "Connection: Upgrade", "-H", "Upgrade: websocket", "-H", "Sec-WebSocket-Version: 13",
            "-H", "Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==", "-H", "Origin: https://yggchat.surge.sh", url],
            capture_output=True, text=True).stdout.strip()
        if r != "000": return r
    return r
res = {c: probe(c) for c in sorted(cands)}
ok = sorted(c for c, r in res.items() if r == "101")
today = (datetime.datetime.utcnow() + datetime.timedelta(hours=9)).strftime("%Y-%m-%d")
open(os.environ.get("STATE_DIR","state")+"/ygg_peers.log", "a").write(f"{today} 101: {' '.join(ok) or '-'} (checked {len(res)})\n")
SF = os.environ.get("STATE_DIR","state")+"/ygg_peers.json"
st = load(SF, {"reported": [], "default_fail_days": 0, "down_reported": False})
new = [c for c in ok if c != DEFAULT and c not in st["reported"]]
if new:
    notify("[YggChat] 新しくブラウザから繋がるピアが見つかった", "PWAの既定に足す？\n" + "\n".join(f"- {c}" for c in new))
    st["reported"] += new
if DEFAULT in ok:
    st["default_fail_days"], st["down_reported"] = 0, False
else:
    st["default_fail_days"] += 1
    if st["default_fail_days"] >= 2 and not st["down_reported"]:
        others = [c for c in ok if c != DEFAULT]
        notify("[YggChat] being.petが落ちてるっぽい", "2日連続で101が返らない。PWAが繋がらないかも。\n" + ("ほかに101のピア:\n" + "\n".join(others) if others else "ほかに101のピアなし"))
        st["down_reported"] = True
save(SF, st)
print(res)
