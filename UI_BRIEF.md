# Task: rewrite web/index.html as a Signal/Session-style messenger UI

Rewrite `web/index.html` from scratch (keep `web/sw.js` and `web/manifest.webmanifest` untouched; you may update manifest colors only).
Read `API.md` for the backend contract — use ONLY those endpoints/events, exactly as documented.

## Hard constraints
- Single self-contained file: inline CSS + inline JS, vanilla JS, no frameworks, no CDN, no external fonts/images.
  The only external script allowed is `<script src="qrcode.js"></script>` (qrcode-generator 1.5: `const q=qrcode(0,'M'); q.addData(str); q.make(); q.createSvgTag({cellSize:4,margin:2,scalable:true})`).
- Keep exactly one main `<script>` block for app code, placed AFTER `<script src="qrcode.js"></script>`.
  Another build step inserts `<script src="wasm_exec.js"></script><script src="shim.js"></script>` before the first `<script` tag, so do not use `type="module"` and do not depend on anything before it.
- Must define `window.ygBack = () => boolean` (Android back button): close the topmost open sheet/dialog/screen and return true; return false when on the conversation list root.
- Service worker registration: `if('serviceWorker' in navigator) navigator.serviceWorker.register('sw.js').catch(()=>{})`.
- Escape all user-provided text (nicks, messages) — never innerHTML untrusted strings.
- Japanese UI labels (casual, short). App name "YggChat".
- Mobile-first (360px wide phone) but on ≥ 820px show a two-pane layout (list left, chat right).
- Respect safe-area insets; input bar sticks to bottom; works with on-screen keyboard (use 100dvh).

## Look & feel (Signal / Session inspired)
- Clean, calm. Dark theme by default (Session-like: near-black #121212 background, surface #1c1c1e, accent green #00f782 Session-green or a softer #2ecc71), plus light theme following `prefers-color-scheme`.
- System font stack. Rounded corners, generous spacing, subtle dividers.
- Avatars: circle with 1–2 initials of nick (or first hex chars of address), background color derived deterministically from address hash.

## Screens
1. **Conversation list (home)**
   - Header: own avatar (tap → Settings), title "YggChat", connection pill: green dot "接続中 N" when peers>0, amber "接続待ち…" when 0.
   - Search field filtering by nick/address.
   - Rows: avatar, nick (or shortened addr `200:abcd…ef12`), last message preview (prefix "あなた: " for outgoing), time (HH:MM today / 昨日 / M/D), unread badge.
   - Sorted by last activity desc; contacts with no messages at the bottom.
   - Empty state: friendly text + buttons "自分のIDを共有" and "連絡先を追加".
   - Floating action button (pencil/plus) → New chat sheet.
2. **New chat / add contact sheet** (bottom sheet)
   - Input "Yggdrasil アドレス" + "名前（任意）", button "チャットを開始" → POST /api/contact then open the conversation.
   - Validate address looks like Yggdrasil (starts with 2xx: or 3xx:) before submit; show inline error.
   - "クリップボードから貼り付け" button (navigator.clipboard.readText, ignore failures).
3. **Conversation screen**
   - Header: back arrow, avatar, nick, small mono shortened address; tap header → Contact info sheet.
   - Messages as bubbles: outgoing right (accent bg, dark text), incoming left (surface). Group consecutive bubbles from same side within 3 min (tighter spacing, rounded corners adjusted). Day separators ("今日", "昨日", "2026/10/18").
   - Under each outgoing bubble (small): time + status icon: sending = clock ⏱ (animated subtle), sent = ✓, failed = red "!" with text "送信失敗・タップで再送" → POST /api/send {To, Retry:id}.
   - Linkify http(s) URLs safely (text nodes + <a target=_blank rel=noopener>).
   - Composer: auto-growing textarea (max ~5 lines), send button (disabled when empty). Enter sends on desktop (Shift+Enter newline); on touch devices Enter = newline.
   - Auto-scroll to bottom on open and on new message if already near bottom; otherwise show a "↓ 新着" pill.
   - Long-press / right-click a bubble → copy text.
   - Mark conversation read when open (unread counts tracked client-side in localStorage key `yggchat_read` = {addr: lastReadTs}).
4. **Contact info sheet**: big avatar, nick, full address (tap to copy), QR of invite link, buttons: "名前を変更" (prompt-like inline input → POST /api/contact), "アドレスをコピー", "会話を削除" (confirm dialog → POST /api/contact/delete).
5. **Settings screen** (full screen, from own avatar)
   - Profile card: avatar, editable display name (POST /api/nick on blur/enter), own address in mono with copy button.
   - "IDを共有": QR code of the invite link + buttons "リンクをコピー" and "共有" (navigator.share when available).
     Invite link format: `https://yggchat.surge.sh/#add=<addr>&n=<urlencoded nick>`.
   - "ピア" section: row showing "接続中 N / M" → opens Peers screen.
   - "通知" toggle: request Notification permission; when enabled and document.hidden, show a notification for incoming messages (title=nick, body=text). Store toggle in localStorage.
   - About: "Yggdrasil上のサーバーレスP2Pチャット。メッセージは端末間で直接届きます。" + platform (native/web).
6. **Peers screen**
   - List from GET /api/peers: status dot (green up / grey down / red when error), uri (mono, ellipsized; on web show a readable form: if uri contains `/r?u=` decode the `u` param and show that), latency ms when up, "受信" tag for inbound. Swipe not needed: a remove (×) button on saved peers → POST {Remove}.
   - Refresh every 3 s while open.
   - Buttons: "おすすめに戻す" (POST {Reset:true}, confirm), "URIで追加" (input → POST {Add}).
   - "公開ピアから追加" → Public peers screen.
7. **Public peers screen**
   - GET /api/peers/public (show loading spinner; error message on failure). Source note: "github.com/yggdrasil-network/public-peers より".
   - Search box + region chips (アジア / ヨーロッパ / 北米 / その他 / すべて, default アジア; use `continent` when present else `asia` flag).
   - Rows: region name, peer (label || uri), protocol tag (tls/quic/tcp/ws/wss), status dot if `up` known, reliability % if known. Already-added peers show "追加済み". Tap "追加" → POST /api/peers {Add: uri}, toast.
   - Render at most 200 rows at once (filtering narrows it).

## Behaviour
- On load: GET /api/state, then subscribe to /api/events; on each event update state and re-render the affected parts. If EventSource errors, retry with backoff and re-fetch state.
- Handle `#add=<addr>&n=<nick>` in location.hash on load: open the New chat sheet prefilled, then clear the hash (history.replaceState).
- Toast component for small confirmations/errors (bottom, 2.5 s).
- All network errors surface as toasts with the server's text.
- Keep code readable, ~800–1200 lines total is fine.
- After writing, run `node -e` syntax check on the extracted script yourself if possible.
