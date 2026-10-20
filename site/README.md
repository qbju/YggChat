# site/

Official site, https://yggchat.pages.dev/ (Cloudflare Pages project `yggchat`).

- `src/`: hand-written parts (landing `index.html`, `download.html`, `faq.html`, `style.css`, `icon.png`).
- `gen.py`: wraps them in the shared layout and generates `/guide`, `/security`, `/changelog` from `../README.md` with pandoc, so the docs never drift from the README. The version number is read from the README too.
- `public/`: build output (`python3 gen.py`). Served with clean URLs (`/guide` → `guide.html`).

On each release: update README, run `python3 gen.py`, deploy `public/`.
