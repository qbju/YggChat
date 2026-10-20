#!/usr/bin/env python3
"""Build the official site (yggchat.pages.dev) into site/public/.
Hand-written pages live in src/; guide, security and changelog are generated
from ../README.md with pandoc so they never drift from it. Usage: python3 gen.py"""
import os, re, shutil, subprocess

HERE = os.path.dirname(os.path.abspath(__file__))
SRC, OUT = os.path.join(HERE, 'src'), os.path.join(HERE, 'public')
README = open(os.path.join(HERE, '..', 'README.md'), encoding='utf-8').read()
VER = re.search(r'現在のバージョン: \*\*(v[\d.]+)\*\*', README).group(1)
GH = 'https://github.com/qbju/YggChat'

def sections(md):
    out, cur = {}, None
    for line in md.splitlines():
        m = re.match(r'^## (.+)', line)
        if m: cur = m.group(1).strip(); out[cur] = []
        elif cur: out[cur].append(line)
    return {k: '\n'.join(v).strip() for k, v in out.items()}

S = sections(README)
def sec(prefix):
    for k, v in S.items():
        if k.startswith(prefix): return k, v
    raise KeyError(prefix)

def md2html(md):
    md = re.sub(r'\]\((LICENSE|THIRD_PARTY_NOTICES\.txt)\)', rf']({GH}/blob/main/\1)', md)
    h = subprocess.run(['pandoc', '-f', 'gfm', '-t', 'html5', '--no-highlight'], input=md,
                       capture_output=True, text=True, check=True).stdout
    h = re.sub(r'<table>', '<div class="tbl"><table>', h).replace('</table>', '</table></div>')
    h = re.sub(r'<h(\d) id="[^"]*">', r'<h\1>', h)
    return h

def doc(items):
    """items: [(id, title, prefix)] -> (toc, body)"""
    toc, body = ['<b>目次</b>'], []
    for i, title, prefix in items:
        _, md = sec(prefix)
        toc.append(f'<a href="#{i}">{title}</a>')
        body.append(f'<h2 id="{i}">{title}</h2>\n' + md2html(md))
    return '\n'.join(toc), '\n'.join(body)

def docpage(h1, lead, items):
    toc, body = doc(items)
    return (f'<header class="phead"><div class="wrap"><h1>{h1}</h1><p>{lead}</p></div></header>\n'
            f'<div class="wrap doc"><aside class="toc">{toc}</aside><div class="prose">{body}</div></div>\n')

NAV = [('/guide', '使い方'), ('/security', 'セキュリティ'), ('/faq', 'FAQ'), ('/changelog', '更新履歴'), ('/download', 'ダウンロード')]

def layout(path, title, desc, body):
    links = ''.join(f'<a class="{"on " if p == path else ""}{"" if p == "/download" else "opt"}" href="{p}">{t}</a>' for p, t in NAV)
    full = 'YggChat — NAT越え不要のP2Pチャット' if path == '/' else f'{title} — YggChat'
    url = 'https://yggchat.pages.dev' + path
    return f'''<!doctype html>
<html lang="ja">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>{full}</title>
<meta name="description" content="{desc}">
<meta name="theme-color" content="#0b0f0c">
<link rel="canonical" href="{url}">
<meta property="og:title" content="{full}">
<meta property="og:description" content="{desc}">
<meta property="og:type" content="website">
<meta property="og:url" content="{url}">
<meta property="og:image" content="https://yggchat.pages.dev/icon.png">
<meta name="twitter:card" content="summary">
<link rel="icon" href="/icon.png">
<link rel="stylesheet" href="/style.css">
</head>
<body>
<nav><div class="wrap">
  <a class="brand" href="/"><img src="/icon.png" alt="">YggChat</a>
  <div class="links">{links}<a class="opt" href="{GH}">GitHub</a></div>
</div></nav>
{body}
<footer><div class="wrap">
  <span>🌲 YggChat {VER} · GPL-3.0</span>
  <div class="r">
    <a href="/guide">使い方</a><a href="/security">セキュリティ</a><a href="/faq">FAQ</a><a href="/changelog">更新履歴</a>
    <a href="{GH}">GitHub</a><a href="https://yggchat.surge.sh/">ブラウザ版</a>
  </div>
</div></footer>
</body>
</html>
'''

def read(n): return open(os.path.join(SRC, n), encoding='utf-8').read().replace('{VER}', VER)

pages = {}
idx = re.sub(r'最新 v[\d.]+', f'最新 {VER}', read('index.html'))
idx = re.sub(r'最新版はv[\d.]+', f'最新版は{VER}', idx)
pages['/'] = ('YggChat', 'サーバーなし・NAT越え不要。Yggdrasilを内蔵した、端末どうしで直接つながるエンドツーエンド暗号化チャット。Android・Windows・Linux・ブラウザ。', idx)

pages['/guide'] = ('使い方', 'YggChatの機能ごとの使い方。グループ、招待リンク、端末移行、ドメイン、LAN、finger、Tor、Android VPNモード、CLI。',
    docpage('使い方', '機能ごとの説明です。「アプリ版のみ」はAndroid・Windows・Linux版で使え、ブラウザ版では使えません。', [
        ('start', 'インストールと起動', '使い方'),
        ('group', 'グループチャット', 'グループチャット'),
        ('links', 'リンクとURLスキーム', 'リンクとURLスキーム'),
        ('edit', 'メッセージの編集・削除', 'メッセージの編集・削除'),
        ('move', '端末移行', '端末移行'),
        ('domain-add', 'ドメイン名で追加', 'ドメイン名で追加'),
        ('domain', '確認済みドメイン', '確認済みドメイン'),
        ('site', 'プロフィールサイト', 'プロフィールサイト'),
        ('lan', 'LANチャット', 'LANチャット'),
        ('finger', 'finger', 'finger'),
        ('tor', 'Tor / SOCKS5', 'Tor / SOCKS5'),
        ('sysygg', '端末のYggdrasilを使う', '端末のYggdrasilを使う'),
        ('vpn', 'Android VPNモード', 'YggChatを端末のYggdrasilにする'),
        ('cli', '端末チャット（CLI）', '端末チャット'),
    ]))

pages['/security'] = ('セキュリティ', 'YggChatの暗号と通信の仕組み、プルーフ・オブ・ワーク、見えてしまう情報。',
    docpage('セキュリティと仕組み', '何が守られて、何が見えるのか。正直に書いています。', [
        ('crypto', '暗号と通信の仕組み', '暗号と通信の仕組み'),
        ('pow', '投稿のPoW', '投稿のPoW'),
        ('verify', 'ファイルの検証', '検証'),
        ('license', 'ライセンスとクレジット', 'ライセンス'),
    ]))

_, hist = sec('履歴')
pages['/changelog'] = ('更新履歴', 'YggChatのバージョンごとの変更点。',
    f'<header class="phead"><div class="wrap"><h1>更新履歴</h1><p>最新は{VER}。リリースごとのファイルは<a href="{GH}/releases">GitHub Releases</a>にあります。</p></div></header>\n'
    f'<div class="wrap" style="padding:28px 22px 80px"><div class="prose" style="max-width:none">{md2html(hist)}</div></div>\n')

pages['/faq'] = ('よくある質問', 'YggChatについてよくある質問。料金、アカウント、オフラインの相手、機種変更、安全性。', read('faq.html'))
_, build = sec('ビルド')
pages['/download'] = ('ダウンロード', f'YggChat {VER} のダウンロード。Android APK、Windows、Linux、ブラウザ版。',
    read('download.html').replace('{BUILD}', '<h2>ソースからビルド</h2>\n' + md2html(build)))

shutil.rmtree(OUT, ignore_errors=True); os.makedirs(OUT)
for path, (title, desc, body) in pages.items():
    name = 'index.html' if path == '/' else path.strip('/') + '.html'
    open(os.path.join(OUT, name), 'w', encoding='utf-8').write(layout(path, title, desc, body))
for f in ('style.css', 'icon.png'): shutil.copy(os.path.join(SRC, f), OUT)
open(os.path.join(OUT, '404.html'), 'w', encoding='utf-8').write(layout('/404', 'ページが見つかりません', '', '<header class="phead" style="padding-bottom:120px"><div class="wrap"><h1>ページが見つかりません</h1><p><a href="/">トップへ戻る</a></p></div></header>'))
print('built', VER, sorted(os.listdir(OUT)))
