# Watch F-Droid fdroiddata MR !53716 (Cyclingo Local). Public API, no token.
import os
import sys; sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from common import *
P, MR = 36528, 53716
B = f"https://gitlab.com/api/v4/projects/{P}/merge_requests/{MR}"
URL = f"https://gitlab.com/fdroid/fdroiddata/-/merge_requests/{MR}"
SF = os.environ.get("STATE_DIR","state")+"/fdroid_mr.json"
st = load(SF, {})
try:
    mr = getjson(B)
    notes = getjson(B + "/notes?sort=desc&per_page=20")
    pipes = getjson(B + "/pipelines")
except Exception as e:
    err = getattr(e, "code", None)
    body = ""
    try: body = e.read()[:300].decode("utf-8", "ignore")
    except Exception: pass
    st["last_error"] = f"{type(e).__name__} {err} {body}"
    st["error_count"] = st.get("error_count", 0) + 1
    save(SF, st)
    print("ERROR", st["last_error"])
    if st["error_count"] == 8:
        notify("[F-Droid] MR見張りが2日間失敗してる", st["last_error"])
    sys.exit(0)
st["error_count"] = 0; st.pop("last_error", None)
msgs = []
last = st.get("last_note_id", 0)
for n in sorted(notes, key=lambda x: x["id"]):
    if n["id"] <= last: continue
    who = n["author"]["username"]
    if n.get("system"):
        b = n["body"]
        if any(k in b for k in ("label", "merged", "closed", "reopened")):
            msgs.append(f"- [system] {who}: {b[:300]}")
    elif who != "100lingo":
        msgs.append(f"- **{who}**: {n['body'][:1500]}")
if notes: st["last_note_id"] = max(last, max(n["id"] for n in notes))
if mr["state"] != st.get("state"):
    if st.get("state"): msgs.append(f"- state: {st.get('state')} → **{mr['state']}**")
    st["state"] = mr["state"]
if pipes:
    p = pipes[0]
    if p["status"] in ("success", "failed") and (p["id"], p["status"]) != (st.get("pipeline_id"), st.get("pipeline_status")):
        msgs.append(f"- pipeline {p['id']}: **{p['status']}** {p.get('web_url','')}")
    if p["status"] in ("success", "failed"):
        st["pipeline_id"], st["pipeline_status"] = p["id"], p["status"]
st["labels"] = mr.get("labels", [])
save(SF, st)
if msgs:
    head = "F-Droidに採用された！（一覧に出るまで数日かかる）" if mr["state"] == "merged" else ("MRがclosedになった" if mr["state"] == "closed" else "F-Droid MRに動きあり")
    notify(f"[F-Droid] {head}", f"{URL}\nlabels: {', '.join(mr.get('labels', []))}\n\n" + "\n".join(msgs))
else:
    print("no change")
