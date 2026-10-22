import json, os, subprocess, urllib.request
REPO = os.environ.get("GITHUB_REPOSITORY", "")
TOKEN = os.environ.get("GITHUB_TOKEN", "")
OWNER = "qbju"
def get(url, timeout=20):
    req = urllib.request.Request(url, headers={"User-Agent": "hark-watchers"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return r.read()
def getjson(url):
    return json.loads(get(url))
def load(path, default):
    try:
        return json.load(open(path))
    except Exception:
        return default
def save(path, obj):
    json.dump(obj, open(path, "w"), ensure_ascii=False, indent=1)
def notify(title, body):
    body = body + f"\n\n@{OWNER}"
    data = json.dumps({"title": title, "body": body, "assignees": [OWNER]}).encode()
    req = urllib.request.Request(f"https://api.github.com/repos/{REPO}/issues", data=data, method="POST",
        headers={"Authorization": f"Bearer {TOKEN}", "Accept": "application/vnd.github+json", "User-Agent": "hark-watchers"})
    print(urllib.request.urlopen(req, timeout=20).status, title)
