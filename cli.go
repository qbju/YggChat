// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

// Terminal chat: `yggchat -cli`. Talks to the node's local API, so it attaches
// to a node that is already running (desktop, Termux, the Android app's node)
// or, if none answers on -ui, the same process starts one and attaches to it.
//
// Everything that came from the network (message text, names, system lines,
// error bodies) goes through termSafe/termSafeLine before it is printed.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// cliContact is a contact or (for "g:" keys) a group as /api/state shows it.
type cliContact struct {
	Addr    string   `json:"addr"`
	Nick    string   `json:"nick"`
	Req     bool     `json:"req"`
	Group   bool     `json:"group"`
	Left    bool     `json:"left"`
	Members []string `json:"members"`
}

type cliState struct {
	Locked   bool                  `json:"locked"`
	Addr     string                `json:"addr"`
	Nick     string                `json:"nick"`
	Peers    int                   `json:"peers"`
	Contacts map[string]cliContact `json:"contacts"`
	Messages []Message             `json:"messages"`
	Nostr    struct {
		On     bool     `json:"on"`
		Relays []string `json:"relays"`
	} `json:"nostr"`
	Joins []json.RawMessage `json:"joins"`
}

type cli struct {
	base string
	mu   sync.Mutex // guards everything below
	st   cliState
	cur  string // open conversation (address or g:<id>)
	out  *bufio.Writer

	ttyOut  bool   // stdout is a terminal: prompts and redraws
	edit    bool   // stdin is in per-key mode; buf is the line being typed
	buf     []rune // pending input (edit mode)
	drawn   int    // columns of prompt+buf currently on screen
	cols    int
	locked  bool
	confirm string          // "/block" or "/leave" waiting for "yes"
	gdone   map[string]bool // group messages whose ✓ n/N line was printed
	joins   int
	restore func()
}

var cliHTTP = &http.Client{Timeout: 30 * time.Second, Transport: keyTransport{}}

// apiStatus is the HTTP status of GET /api/state on ui (0: nothing answers).
// apiNoProof: something answers on ui but can't prove it holds our key
// (another program, a node of another data directory, a node older than
// v0.19.3). The key is never sent to it.
const apiNoProof = -1

func apiStatus(ui string) int {
	if ok, answered := checkProof(ui); !answered {
		return 0
	} else if !ok {
		return apiNoProof
	}
	proofMu.Lock()
	proofAt[ui] = time.Now()
	proofMu.Unlock()
	c := &http.Client{Timeout: 1500 * time.Millisecond, Transport: keyTransport{}}
	r, err := c.Get("http://" + ui + "/api/state")
	if err != nil {
		return 0
	}
	r.Body.Close()
	return r.StatusCode
}

// apiUp reports whether our node answers on ui. In -cli mode a node that
// rejects our key (another data directory) or a different program on the
// port is a hard error: starting a second node there would only create a new
// identity and fail to bind.
func apiUp(ui string) bool {
	st := apiStatus(ui)
	if st == 200 {
		return true
	}
	if st != 0 && cliFlag(os.Args[1:]) {
		if st == apiNoProof {
			fmt.Fprintf(os.Stderr, "エラー: %s で動いているのは、このデータフォルダのYggChatノードではありません（別のデータフォルダのノード、別のプログラム、またはv0.19.2以前のノード）。鍵は送っていません。\n"+
				"そのノードと同じ -data を指定するか、別の -ui ポートを使ってください（古いノードなら止めてから起動し直してください）。\n", ui)
		} else if st == 401 || st == 403 {
			fmt.Fprintf(os.Stderr, "エラー: %s で動いているノードは、別のデータフォルダのものです（データフォルダ/キーが違います）。\n"+
				"そのノードと同じ -data を指定するか、別の -ui ポートを使ってください。\n", ui)
		} else {
			fmt.Fprintf(os.Stderr, "エラー: %s は別のプログラムが使っています（HTTP %d）。別の -ui ポートを指定してください。\n", ui, st)
		}
		os.Exit(2)
	}
	return false
}

// cliFlag reports whether the command line asks for -cli.
func cliFlag(args []string) bool {
	for _, a := range args {
		if a == "--" {
			break
		}
		a = strings.TrimLeft(a, "-")
		if a == "cli" || a == "cli=true" || a == "cli=1" {
			return true
		}
	}
	return false
}

type apiErr struct {
	code int
	msg  string
}

func (e *apiErr) Error() string { return e.msg }

func (c *cli) call(path string, body any, out any) error {
	var r *http.Response
	var err error
	if body == nil {
		r, err = cliHTTP.Get(c.base + path)
	} else {
		b, _ := json.Marshal(body)
		r, err = cliHTTP.Post(c.base+path, "application/json", bytes.NewReader(b))
	}
	if err != nil {
		return err
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	if r.StatusCode != 200 {
		if r.StatusCode == 423 {
			go c.onLocked()
		}
		return &apiErr{r.StatusCode, termSafeLine(string(b))}
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

// friendly turns an API/network error into a short message.
func friendly(err error) string {
	var ae *apiErr
	if errors.As(err, &ae) {
		if ae.code == 423 || ae.msg == "locked" {
			return "ロック中です"
		}
		if ae.code == 401 {
			return "ノードに拒否されました（データフォルダ/キーが違います）"
		}
		if ae.msg == "" {
			return fmt.Sprintf("HTTP %d", ae.code)
		}
		return ae.msg
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return "ノードに接続できません（再起動待ち）"
	}
	return termSafeLine(err.Error())
}

// ---------- output ----------

func (c *cli) printf(f string, a ...any) { c.emit(fmt.Sprintf(f, a...)) }

// emit prints s above the input line and redraws the prompt and whatever the
// user has typed so far.
func (c *cli) emit(s string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ttyOut {
		c.clearLocked()
		fmt.Fprintf(c.out, "%s\n", s)
		c.promptLocked()
	} else {
		fmt.Fprintf(c.out, "%s\n", s)
	}
	c.out.Flush()
}

// clearLocked erases the prompt line (and the rows it wrapped onto).
func (c *cli) clearLocked() {
	if !c.ttyOut {
		return
	}
	if c.edit && c.cols > 0 && c.drawn > 0 {
		if up := (c.drawn - 1) / c.cols; up > 0 {
			fmt.Fprintf(c.out, "\r\033[%dA\033[J", up)
			c.drawn = 0
			return
		}
	}
	fmt.Fprint(c.out, "\r\033[K")
	c.drawn = 0
}

// promptLocked draws the prompt and the pending input (caller holds mu).
func (c *cli) promptLocked() {
	if !c.ttyOut {
		return
	}
	p := "（未選択）> "
	if c.locked {
		p = "パスコード: "
	} else if c.cur != "" {
		p = c.nameLocked(c.cur) + "> "
	}
	fmt.Fprint(c.out, p)
	c.drawn = strWidth(p)
	if c.edit && !c.locked {
		s := string(c.buf)
		fmt.Fprint(c.out, s)
		c.drawn += strWidth(s)
	}
}

func (c *cli) redraw() {
	c.mu.Lock()
	c.clearLocked()
	c.promptLocked()
	c.out.Flush()
	c.mu.Unlock()
}

func (c *cli) nameLocked(addr string) string {
	if ct, ok := c.st.Contacts[addr]; ok && ct.Nick != "" {
		if n := termSafeLine(ct.Nick); n != "" {
			return n
		}
	}
	if len(addr) > 14 {
		return termSafeLine(addr[:14]) + "…"
	}
	return termSafeLine(addr)
}

func (c *cli) name(addr string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.nameLocked(addr)
}

func (c *cli) load() error {
	var st cliState
	if err := c.call("/api/state", nil, &st); err != nil {
		return err
	}
	c.mu.Lock()
	c.setStateLocked(st)
	c.mu.Unlock()
	return nil
}

func (c *cli) setStateLocked(st cliState) {
	c.st = st
	if c.st.Contacts == nil {
		c.st.Contacts = map[string]cliContact{}
	}
	c.locked = st.Locked
	c.joins = len(st.Joins)
	if _, ok := c.st.Contacts[c.cur]; !ok {
		c.cur = ""
	}
}

func hm(ts int64) string { return time.Unix(ts, 0).Format("01/02 15:04") }

func sysText(s string) string {
	if v, ok := strings.CutPrefix(s, "ttl:"); ok {
		if v == "0" {
			return "消えるメッセージ: オフ"
		}
		return "消えるメッセージ: " + v + "秒"
	}
	return s
}

// lineLocked formats one message (caller holds mu).
func (c *cli) lineLocked(m Message) string {
	if m.Sys != "" {
		return fmt.Sprintf("  — %s %s", hm(m.TS), termSafeLine(sysText(m.Sys)))
	}
	who := c.nameLocked(m.Peer)
	if isGroupKey(m.Peer) {
		who = termSafeLine(m.Nick)
		if who == "" {
			who = c.nameLocked(m.From)
		}
	}
	if m.Out {
		who = "自分"
	}
	text := termSafe(m.Text)
	if m.Img != "" {
		text = strings.TrimSpace("📷 写真 " + text)
	}
	if m.Edited {
		text += " （編集済）"
	}
	mark := ""
	if m.Out {
		switch m.Status {
		case "sent":
			mark = " ✓"
			if isGroupKey(m.Peer) && m.Tot > 0 {
				mark = fmt.Sprintf(" ✓ %d/%d", m.Dlv, m.Tot)
			}
		case "sending":
			mark = " …"
		case "queued":
			mark = " 🕓"
		case "stored":
			mark = " 📮"
		case "failed":
			mark = " ✗"
		}
	}
	return fmt.Sprintf("[%s] %s: %s%s", hm(m.TS), who, text, mark)
}

func (c *cli) line(m Message) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lineLocked(m)
}

// noticeLocked is the one-line notification for a message in another conversation.
func (c *cli) noticeLocked(m Message) string {
	from := c.nameLocked(m.Peer)
	prev := ""
	switch {
	case m.Sys != "":
		prev = sysText(m.Sys)
	case isGroupKey(m.Peer):
		who := termSafeLine(m.Nick)
		if who == "" {
			who = c.nameLocked(m.From)
		}
		prev = who + ": " + strings.SplitN(m.Text, "\n", 2)[0]
	default:
		prev = strings.SplitN(m.Text, "\n", 2)[0]
	}
	if m.Img != "" {
		prev = "📷 " + prev
	}
	prev = termSafeLine(prev)
	if r := []rune(prev); len(r) > 80 {
		prev = string(r[:80]) + "…"
	}
	return fmt.Sprintf("📨 %s から: %s   （/open %s）", from, prev, from)
}

// ---------- events ----------

func (c *cli) events() {
	first := true
	for {
		r, err := (&http.Client{Transport: keyTransport{}}).Get(c.base + "/api/events")
		if err == nil && r.StatusCode == 200 {
			if !first {
				c.printf("  （ノードに再接続しました）")
			}
			c.resync()
			first = false
			sc := bufio.NewScanner(r.Body)
			sc.Buffer(make([]byte, 64<<10), 8<<20)
			for sc.Scan() {
				l := sc.Text()
				if strings.HasPrefix(l, "data: ") {
					c.onEvent([]byte(l[6:]))
				}
			}
			r.Body.Close()
			c.printf("  （ノードとの接続が切れました。再接続します…）")
		} else if err == nil {
			r.Body.Close()
		}
		time.Sleep(2 * time.Second)
	}
}

// resync reloads the state after (re)connecting the event stream and shows
// incoming messages that arrived while the stream was down.
func (c *cli) resync() {
	var st cliState
	if err := c.call("/api/state", nil, &st); err != nil {
		return
	}
	c.mu.Lock()
	if st.Locked && !c.locked {
		c.mu.Unlock()
		c.onLocked()
		return
	}
	if c.locked {
		c.mu.Unlock()
		return
	}
	known := map[string]bool{}
	for _, m := range c.st.Messages {
		known[m.Peer+"\x00"+m.ID] = true
	}
	hadState := len(c.st.Messages) > 0 || len(c.st.Contacts) > 0
	c.setStateLocked(st)
	var out []string
	if hadState {
		for _, m := range c.st.Messages {
			if known[m.Peer+"\x00"+m.ID] || (m.Out && m.Sys == "") {
				continue
			}
			if m.Peer == c.cur {
				out = append(out, c.lineLocked(m))
			} else {
				out = append(out, c.noticeLocked(m))
			}
		}
	}
	c.mu.Unlock()
	if len(out) > 0 {
		c.emit(strings.Join(out, "\n"))
	}
}

// onLocked wipes everything decrypted from memory and the screen and asks
// for the passcode again.
func (c *cli) onLocked() {
	c.mu.Lock()
	if c.locked {
		c.mu.Unlock()
		return
	}
	c.locked = true
	c.st = cliState{Locked: true, Addr: c.st.Addr, Contacts: map[string]cliContact{}}
	c.cur, c.buf, c.confirm = "", nil, ""
	c.gdone = map[string]bool{}
	if c.ttyOut {
		fmt.Fprint(c.out, "\r\033[H\033[2J\033[3J") // clear screen and scrollback
		c.drawn = 0
	}
	c.mu.Unlock()
	c.printf("🔒 ロックされました。表示していた会話は消去しました。パスコードを入力してください")
}

func (c *cli) onEvent(b []byte) {
	var ev struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(b, &ev) != nil {
		return
	}
	if ev.Type == "locked" {
		c.onLocked()
		return
	}
	c.mu.Lock()
	locked := c.locked
	c.mu.Unlock()
	if locked {
		return
	}
	switch ev.Type {
	case "message":
		var m Message
		if json.Unmarshal(ev.Data, &m) != nil || m.Hid {
			return
		}
		c.mu.Lock()
		c.st.Messages = append(c.st.Messages, m)
		if _, ok := c.st.Contacts[m.Peer]; !ok && !isGroupKey(m.Peer) {
			c.st.Contacts[m.Peer] = cliContact{Addr: m.Peer, Nick: m.Nick, Req: !m.Out}
		}
		s := ""
		if !m.Out || m.Sys != "" {
			if m.Peer == c.cur {
				s = c.lineLocked(m)
			} else {
				s = c.noticeLocked(m)
			}
		}
		c.mu.Unlock()
		if s != "" {
			c.emit(s)
		}
	case "msg_upd":
		var m Message
		if json.Unmarshal(ev.Data, &m) != nil {
			return
		}
		c.mu.Lock()
		found := false
		for i := range c.st.Messages {
			if x := &c.st.Messages[i]; x.ID == m.ID && x.Peer == m.Peer {
				x.Text, x.Edited, found = m.Text, true, true
				m = *x
			}
		}
		s := ""
		if found && m.Peer == c.cur {
			s = "  ✎ 編集されました: " + c.lineLocked(m)
		}
		c.mu.Unlock()
		if s != "" {
			c.emit(s)
		}
	case "msg_del":
		var d struct{ Peer, ID string }
		if json.Unmarshal(ev.Data, &d) != nil {
			return
		}
		c.mu.Lock()
		found := false
		kept := c.st.Messages[:0]
		for _, m := range c.st.Messages {
			if m.ID == d.ID && m.Peer == d.Peer {
				found = true
				continue
			}
			kept = append(kept, m)
		}
		c.st.Messages = kept
		cur := c.cur
		c.mu.Unlock()
		if found && d.Peer == cur {
			c.printf("  — メッセージが削除されました（取り消し／時間切れ）")
		}
	case "status":
		var s struct {
			ID, Peer, Status string
			Dlv              int
		}
		if json.Unmarshal(ev.Data, &s) != nil {
			return
		}
		c.mu.Lock()
		for i := range c.st.Messages {
			if m := &c.st.Messages[i]; m.ID == s.ID && m.Peer == s.Peer {
				m.Status = s.Status
				if s.Dlv > m.Dlv {
					m.Dlv = s.Dlv
				}
			}
		}
		cur := c.cur
		group := isGroupKey(s.Peer)
		pending := false
		if group && s.Status == "sent" {
			if c.gdone == nil {
				c.gdone = map[string]bool{}
			}
			if _, seen := c.gdone[s.ID]; !seen {
				c.gdone[s.ID] = false
				pending = true
			}
		}
		c.mu.Unlock()
		if s.Peer != cur {
			return
		}
		switch s.Status {
		case "sent":
			if group {
				if pending {
					// Members' copies arrive one by one: wait a moment and print one line.
					go c.groupDone(s.Peer, s.ID)
				}
				return
			}
			c.printf("  ✓ 届きました")
		case "queued":
			c.printf("  🕓 相手がオフライン。オンラインになったら自動で再送します")
		case "stored":
			c.printf("  📮 Nostrリレーに置き配しました")
		case "failed":
			c.printf("  ✗ 送信失敗（/retry で再送）")
		}
	case "contact":
		var ct cliContact
		if json.Unmarshal(ev.Data, &ct) != nil || ct.Addr == "" {
			return
		}
		c.mu.Lock()
		_, had := c.st.Contacts[ct.Addr]
		c.st.Contacts[ct.Addr] = ct
		s := ""
		if !had && ct.Group && !ct.Left {
			n := c.nameLocked(ct.Addr)
			s = fmt.Sprintf("👥 グループ「%s」に追加されました（/open %s）", n, n)
		}
		c.mu.Unlock()
		if s != "" {
			c.emit(s)
		}
	case "contact_del":
		var d struct{ Addr string }
		if json.Unmarshal(ev.Data, &d) != nil || d.Addr == "" {
			return
		}
		c.mu.Lock()
		n := c.nameLocked(d.Addr)
		_, had := c.st.Contacts[d.Addr]
		delete(c.st.Contacts, d.Addr)
		kept := c.st.Messages[:0]
		for _, m := range c.st.Messages {
			if m.Peer != d.Addr {
				kept = append(kept, m)
			}
		}
		c.st.Messages = kept
		wasCur := c.cur == d.Addr
		if wasCur {
			c.cur = ""
		}
		c.mu.Unlock()
		if had {
			c.printf("  — %s を連絡先から削除しました", n)
		}
	case "notice":
		var n struct{ Text string }
		if json.Unmarshal(ev.Data, &n) == nil && strings.TrimSpace(n.Text) != "" {
			c.printf("📢 %s", termSafeLine(n.Text))
		}
	case "joins":
		var j []json.RawMessage
		if json.Unmarshal(ev.Data, &j) != nil {
			return
		}
		c.mu.Lock()
		changed := len(j) != c.joins
		c.joins = len(j)
		c.mu.Unlock()
		if changed {
			c.printf("  （参加申請中のグループ: %d件）", len(j))
		}
	case "peers":
		var p struct{ Up int }
		if json.Unmarshal(ev.Data, &p) == nil {
			c.mu.Lock()
			c.st.Peers = p.Up
			c.mu.Unlock()
			c.printf("  （接続中ピア: %d）", p.Up)
		}
	}
}

// groupDone prints the single "✓ n/N" line for a group message.
func (c *cli) groupDone(peer, id string) {
	time.Sleep(1500 * time.Millisecond)
	c.mu.Lock()
	if c.gdone[id] || c.cur != peer {
		c.mu.Unlock()
		return
	}
	c.gdone[id] = true
	dlv, tot := 0, 0
	for _, m := range c.st.Messages {
		if m.ID == id && m.Peer == peer {
			dlv, tot = m.Dlv, m.Tot
		}
	}
	c.mu.Unlock()
	switch {
	case tot > 0 && dlv < tot:
		c.printf("  ✓ %d/%d 人に届きました（残りはオンラインになったら自動で再送）", dlv, tot)
	case tot > 0:
		c.printf("  ✓ %d/%d 人に届きました", dlv, tot)
	default:
		c.printf("  ✓ 届きました")
	}
}

// ---------- contacts ----------

func (c *cli) sortedContacts() []cliContact {
	c.mu.Lock()
	defer c.mu.Unlock()
	last := map[string]int64{}
	for _, m := range c.st.Messages {
		if m.TS > last[m.Peer] {
			last[m.Peer] = m.TS
		}
	}
	var out []cliContact
	for _, ct := range c.st.Contacts {
		out = append(out, ct)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if last[out[i].Addr] != last[out[j].Addr] {
			return last[out[i].Addr] > last[out[j].Addr]
		}
		return out[i].Addr < out[j].Addr
	})
	return out
}

// findContact resolves a list number, nickname or address in list. When no
// exact match exists and several contacts match by prefix/substring, it
// returns "" and the candidates.
func findContact(list []cliContact, arg string) (string, []cliContact) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return "", nil
	}
	if n, err := strconv.Atoi(arg); err == nil && n >= 1 && n <= len(list) {
		return list[n-1].Addr, nil
	}
	for _, ct := range list {
		if ct.Addr == arg {
			return ct.Addr, nil
		}
	}
	var exact []cliContact
	for _, ct := range list {
		if ct.Nick != "" && strings.EqualFold(ct.Nick, arg) {
			exact = append(exact, ct)
		}
	}
	if len(exact) == 1 {
		return exact[0].Addr, nil
	}
	if len(exact) > 1 {
		return "", exact
	}
	var cand []cliContact
	la := strings.ToLower(arg)
	for _, ct := range list {
		if strings.HasPrefix(ct.Addr, arg) || (ct.Nick != "" && strings.Contains(strings.ToLower(ct.Nick), la)) {
			cand = append(cand, ct)
		}
	}
	if len(cand) == 1 {
		return cand[0].Addr, nil
	}
	return "", cand
}

// parseHist parses the /hist argument: empty → 50.
func parseHist(arg string) (int, bool) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return 50, true
	}
	v, err := strconv.Atoi(arg)
	if err != nil || v <= 0 {
		return 0, false
	}
	return v, true
}

// parseInput splits a typed line: a command ("/open", "Bob") or text to
// send. "//text" sends the literal "/text".
func parseInput(l string) (cmd, arg, text string) {
	l = strings.TrimSpace(l)
	if strings.HasPrefix(l, "//") {
		return "", "", l[1:]
	}
	if strings.HasPrefix(l, "/") {
		f := strings.Fields(l)
		return f[0], strings.TrimSpace(strings.TrimPrefix(l, f[0])), ""
	}
	return "", "", l
}

func (c *cli) history(n int) {
	c.mu.Lock()
	var out []string
	for _, m := range c.st.Messages {
		if m.Peer == c.cur {
			out = append(out, c.lineLocked(m))
		}
	}
	c.mu.Unlock()
	if len(out) > n {
		out = out[len(out)-n:]
	}
	if len(out) == 0 {
		c.printf("  （まだメッセージはありません）")
		return
	}
	c.emit(strings.Join(out, "\n"))
}

var cliHelp = fmt.Sprintf(`コマンド:
  /list              連絡先・グループ一覧（番号つき）
  /open <番号|名前|アドレス>   会話を開く（最近20件を表示）
  /add <アドレス> [名前]       連絡先を追加して開く（名前は相手が名乗るまでの仮の名前）
  /hist [件数]       今の会話の履歴（既定 50件）
  /accept            今の相手・グループのリクエストを承認
  /rename <名前>     今の相手の表示名を変更
  /retry             今の会話の送れていないメッセージを再送
  /leave yes         今のグループから退出
  /block yes         今の相手をブロック（連絡先と履歴も消えます）
  /me                自分のアドレス・ピア数
  /nick <名前>       自分の名前を変更
  /nostr on|off      Nostr置き配の切り替え
  /quit              終了（-cli で起動したノードも止まります）
それ以外の入力は、開いている相手に送信します（最大 %d バイト）。
「/」で始まる文を送るときは「//」と書きます（//shrug → /shrug）。`, maxSendText)

// ---------- input ----------

type cliInput struct {
	r *bufio.Reader
}

// readLine returns the next line. In edit mode it echoes (or, while locked,
// hides) the keys itself and keeps the pending text in c.buf so incoming
// messages can redraw it.
func (c *cli) readLine(in *cliInput) (string, error) {
	if !c.edit {
		l, err := in.r.ReadString('\n')
		if err != nil && (err != io.EOF || l == "") {
			return "", err
		}
		return strings.TrimRight(l, "\r\n"), nil
	}
	for {
		ch, _, err := in.r.ReadRune()
		if err != nil {
			return "", err
		}
		c.mu.Lock()
		switch {
		case ch == '\r' || ch == '\n':
			l := string(c.buf)
			c.buf = nil
			fmt.Fprint(c.out, "\n")
			c.drawn = 0
			c.out.Flush()
			c.mu.Unlock()
			return l, nil
		case ch == 0x04: // Ctrl-D
			if len(c.buf) == 0 {
				c.mu.Unlock()
				return "", io.EOF
			}
		case ch == 0x7f || ch == 0x08:
			if len(c.buf) > 0 {
				c.buf = c.buf[:len(c.buf)-1]
			}
		case ch == 0x15: // Ctrl-U
			c.buf = nil
		case ch == 0x17: // Ctrl-W
			s := strings.TrimRight(string(c.buf), " ")
			if i := strings.LastIndex(s, " "); i >= 0 {
				c.buf = []rune(s[:i+1])
			} else {
				c.buf = nil
			}
		case ch == 0x0c: // Ctrl-L
			fmt.Fprint(c.out, "\033[H\033[2J")
			c.drawn = 0
		case ch == 0x1b: // escape sequence (arrows etc.): swallow it
			c.mu.Unlock()
			if nx, _, err := in.r.ReadRune(); err == nil && (nx == '[' || nx == 'O') {
				for {
					b, err := in.r.ReadByte()
					if err != nil || (b >= 0x40 && b <= 0x7e) {
						break
					}
				}
			}
			continue
		case ch == '\t':
			c.buf = append(c.buf, ' ')
		case ch < 0x20 || (ch >= 0x80 && ch <= 0x9f):
		default:
			c.buf = append(c.buf, ch)
		}
		c.clearLocked()
		c.promptLocked()
		c.out.Flush()
		c.mu.Unlock()
	}
}

func (c *cli) unlock(pass string) {
	if err := c.call("/api/unlock", map[string]string{"Pass": pass}, nil); err != nil {
		var ae *apiErr
		if errors.As(err, &ae) && ae.code == 403 {
			c.printf("パスコードが違います")
		} else {
			c.printf("解除できません: %s", friendly(err))
		}
		return
	}
	if err := c.load(); err != nil {
		c.printf("ノードに接続できません: %s", friendly(err))
		return
	}
	c.banner()
}

func (c *cli) banner() {
	c.mu.Lock()
	s := fmt.Sprintf("YggChat CLI  — %s（%s）  ピア %d\n/help でコマンド一覧", termSafeLine(c.st.Nick), c.st.Addr, c.st.Peers)
	c.mu.Unlock()
	c.emit(s)
	if len(c.sortedContacts()) > 0 {
		c.cmd("/list", "")
	}
}

func (c *cli) exit(code int) {
	if c.restore != nil {
		c.restore()
	}
	os.Exit(code)
}

func runCLI(ui string) {
	c := &cli{base: "http://" + ui, out: bufio.NewWriter(os.Stdout), gdone: map[string]bool{}}
	c.ttyOut = isCharDev(os.Stdout)
	in := &cliInput{r: bufio.NewReaderSize(os.Stdin, 64<<10)}
	if err := c.load(); err != nil {
		fmt.Fprintln(os.Stderr, "ノードに接続できません:", friendly(err))
		os.Exit(1)
	}
	if c.ttyOut {
		if restore, ok := ttyRaw(); ok {
			c.edit, c.restore, c.cols = true, restore, ttyCols()
			defer restore()
			sig := make(chan os.Signal, 1)
			signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
			go func() {
				<-sig
				restore()
				fmt.Println()
				os.Exit(130)
			}()
		}
	}
	go c.events()
	c.mu.Lock()
	locked := c.locked
	c.mu.Unlock()
	if locked {
		if !c.edit && isCharDev(os.Stdin) {
			c.printf("🔒 ロックされています。パスコードを入力してください（この端末では入力が表示されます）")
		} else {
			c.printf("🔒 ロックされています。パスコードを入力してください")
		}
	} else {
		c.banner()
	}
	for {
		l, err := c.readLine(in)
		if err != nil {
			if c.ttyOut {
				fmt.Fprintln(os.Stdout)
			}
			if err == io.EOF {
				return
			}
			fmt.Fprintln(os.Stderr, "入力エラー:", err)
			c.exit(1)
		}
		c.mu.Lock()
		locked := c.locked
		c.mu.Unlock()
		if locked {
			if strings.TrimSpace(l) == "/quit" || strings.TrimSpace(l) == "/exit" {
				return
			}
			c.unlock(l)
			continue
		}
		cmd, arg, text := parseInput(l)
		if cmd == "" && text == "" {
			c.redraw()
			continue
		}
		if cmd == "/quit" || cmd == "/exit" {
			return
		}
		if cmd != "" {
			c.cmd(cmd, arg)
			continue
		}
		c.send(text)
	}
}

func (c *cli) send(text string) {
	c.mu.Lock()
	cur := c.cur
	c.mu.Unlock()
	if cur == "" {
		c.printf("相手が選ばれていません。/list と /open で選んでください")
		return
	}
	if len(text) > maxSendText {
		c.printf("長すぎます（%d バイト。最大 %d バイト）。送信していません", len(text), maxSendText)
		return
	}
	var m Message
	if err := c.call("/api/send", map[string]string{"To": cur, "Text": text}, &m); err != nil {
		c.printf("送信エラー: %s", friendly(err))
		return
	}
	c.mu.Lock()
	dup := false
	for _, x := range c.st.Messages {
		if x.ID == m.ID && x.Peer == m.Peer {
			dup = true
		}
	}
	if !dup {
		c.st.Messages = append(c.st.Messages, m)
	}
	s := c.lineLocked(m)
	c.mu.Unlock()
	c.emit(s)
}

func (c *cli) cmd(name, arg string) {
	f := strings.Fields(arg)
	c.mu.Lock()
	cur := c.cur
	confirm := c.confirm
	c.confirm = ""
	c.mu.Unlock()
	switch name {
	case "/help", "/h", "/?":
		c.printf("%s", cliHelp)
	case "/list", "/ls":
		list := c.sortedContacts()
		if len(list) == 0 {
			c.printf("  連絡先はまだありません。/add <アドレス> で追加")
			return
		}
		var b []string
		for i, ct := range list {
			tag := ""
			if ct.Group {
				tag = fmt.Sprintf(" 👥%d人", len(ct.Members))
				if ct.Left {
					tag += " [退出済]"
				}
			}
			if ct.Req {
				tag += " [リクエスト]"
			}
			nick := termSafeLine(ct.Nick)
			if nick == "" {
				nick = "（名前なし）"
			}
			b = append(b, fmt.Sprintf("  %2d. %s  %s%s", i+1, nick, termSafeLine(ct.Addr), tag))
		}
		c.emit(strings.Join(b, "\n"))
	case "/open", "/o":
		if arg == "" {
			c.printf("使い方: /open <番号|名前|アドレス>")
			return
		}
		a, cand := findContact(c.sortedContacts(), arg)
		if a == "" {
			if len(cand) > 1 {
				var b []string
				for _, ct := range cand {
					b = append(b, fmt.Sprintf("    %s  %s", termSafeLine(ct.Nick), termSafeLine(ct.Addr)))
				}
				c.printf("「%s」に当てはまる相手が %d 人います。番号かアドレスで指定してください:\n%s", termSafeLine(arg), len(cand), strings.Join(b, "\n"))
				return
			}
			c.printf("見つかりません: %s", termSafeLine(arg))
			return
		}
		c.mu.Lock()
		c.cur = a
		s := fmt.Sprintf("── %s（%s）──", c.nameLocked(a), termSafeLine(a))
		c.mu.Unlock()
		c.emit(s)
		c.history(20)
	case "/add":
		if len(f) < 1 {
			c.printf("使い方: /add <アドレス> [名前]")
			return
		}
		body := map[string]any{"Addr": f[0], "Accept": true}
		if len(f) > 1 {
			body["Nick"] = strings.Join(f[1:], " ")
		}
		var ct cliContact
		if err := c.call("/api/contact", body, &ct); err != nil {
			c.printf("追加できません: %s", friendly(err))
			return
		}
		c.mu.Lock()
		c.st.Contacts[ct.Addr] = ct
		c.cur = ct.Addr
		s := "追加しました: " + c.nameLocked(ct.Addr)
		c.mu.Unlock()
		c.emit(s)
	case "/hist":
		n, ok := parseHist(arg)
		if !ok {
			c.printf("使い方: /hist [件数]  （件数は1以上の数字）")
			return
		}
		if cur == "" {
			c.printf("先に /open してください")
			return
		}
		c.history(n)
	case "/accept", "/rename":
		if cur == "" {
			c.printf("先に /open してください")
			return
		}
		body := map[string]any{"Addr": cur, "Accept": name == "/accept"}
		if name == "/rename" {
			if arg == "" {
				c.printf("使い方: /rename <名前>")
				return
			}
			body["Nick"] = arg
		}
		var ct cliContact
		if err := c.call("/api/contact", body, &ct); err != nil {
			c.printf("エラー: %s", friendly(err))
			return
		}
		if ct.Addr != "" {
			c.mu.Lock()
			c.st.Contacts[ct.Addr] = ct
			c.mu.Unlock()
		}
		c.printf("OK")
	case "/retry":
		c.mu.Lock()
		var ids []string
		for _, m := range c.st.Messages {
			if m.Peer == cur && m.Out && (m.Status == "failed" || m.Status == "queued" || m.Status == "stored") {
				ids = append(ids, m.ID)
			}
		}
		c.mu.Unlock()
		fail := 0
		var last error
		for _, id := range ids {
			if err := c.call("/api/send", map[string]string{"To": cur, "Retry": id}, nil); err != nil {
				fail++
				last = err
			}
		}
		if fail > 0 {
			c.printf("%d件を再送中（%d件はエラー: %s）", len(ids)-fail, fail, friendly(last))
		} else {
			c.printf("%d件を再送中", len(ids))
		}
	case "/leave":
		if !isGroupKey(cur) {
			c.printf("グループを /open してから使ってください")
			return
		}
		if arg != "yes" && confirm != "/leave" {
			c.mu.Lock()
			c.confirm = "/leave"
			c.mu.Unlock()
			c.printf("「%s」から退出します。よければ /leave yes", c.name(cur))
			return
		}
		if err := c.call("/api/group/leave", map[string]string{"ID": strings.TrimPrefix(cur, "g:")}, nil); err != nil {
			c.printf("退出できません: %s", friendly(err))
			return
		}
		c.printf("退出しました: %s", c.name(cur))
	case "/block":
		if cur == "" || isGroupKey(cur) {
			c.printf("ブロックする相手を /open してから使ってください（グループは /leave）")
			return
		}
		if arg != "yes" && confirm != "/block" {
			c.mu.Lock()
			c.confirm = "/block"
			c.mu.Unlock()
			c.printf("%s をブロックします（連絡先と履歴も消えます）。よければ /block yes", c.name(cur))
			return
		}
		n := c.name(cur)
		if err := c.call("/api/block", map[string]any{"Addr": cur, "Block": true}, nil); err != nil {
			c.printf("ブロックできません: %s", friendly(err))
			return
		}
		c.mu.Lock()
		delete(c.st.Contacts, cur)
		if c.cur == cur {
			c.cur = ""
		}
		c.mu.Unlock()
		c.printf("ブロックしました: %s", n)
	case "/me":
		if err := c.load(); err != nil {
			c.printf("ノードに接続できません: %s", friendly(err))
			return
		}
		c.mu.Lock()
		s := fmt.Sprintf("  %s  %s  ピア %d  置き配 %v", termSafeLine(c.st.Nick), c.st.Addr, c.st.Peers, c.st.Nostr.On)
		c.mu.Unlock()
		c.emit(s)
	case "/nick":
		if arg == "" {
			c.printf("使い方: /nick <名前>")
			return
		}
		if err := c.call("/api/nick", map[string]string{"Nick": arg}, nil); err != nil {
			c.printf("エラー: %s", friendly(err))
			return
		}
		_ = c.load()
		c.mu.Lock()
		s := "名前を " + termSafeLine(c.st.Nick) + " にしました"
		c.mu.Unlock()
		c.emit(s)
	case "/nostr":
		if arg != "on" && arg != "off" {
			c.printf("使い方: /nostr on|off  （公開リレーに暗号化して最大7日預けます。宛先タグ・時刻・IPはリレーに見えます）")
			return
		}
		var res struct{ On bool }
		if err := c.call("/api/nostr", map[string]bool{"On": arg == "on"}, &res); err != nil {
			c.printf("エラー: %s", friendly(err))
			return
		}
		c.printf("Nostr置き配: %v", res.On)
	default:
		c.printf("不明なコマンド: %s  （/help を見てください。「/」で始まる文は // で送れます）", termSafeLine(name))
	}
}
