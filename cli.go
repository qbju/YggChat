// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

// Terminal chat: `yggchat -cli`. Talks to the node's local API, so it attaches
// to a node that is already running (desktop, Termux, the Android app's node)
// or, if none answers on -ui, the same process starts one and attaches to it.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type cliState struct {
	Locked   bool               `json:"locked"`
	Addr     string             `json:"addr"`
	Nick     string             `json:"nick"`
	Peers    int                `json:"peers"`
	Contacts map[string]Contact `json:"contacts"`
	Messages []Message          `json:"messages"`
	Nostr    struct {
		On     bool     `json:"on"`
		Relays []string `json:"relays"`
	} `json:"nostr"`
}

type cli struct {
	base string
	mu   sync.Mutex
	st   cliState
	cur  string // open conversation (address)
	out  *bufio.Writer
}

var cliHTTP = &http.Client{Timeout: 30 * time.Second, Transport: keyTransport{}}

func apiUp(ui string) bool {
	c := &http.Client{Timeout: 1500 * time.Millisecond, Transport: keyTransport{}}
	r, err := c.Get("http://" + ui + "/api/state")
	if err != nil {
		return false
	}
	r.Body.Close()
	return r.StatusCode == 200
}

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
		return fmt.Errorf("%s", strings.TrimSpace(string(b)))
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

func (c *cli) printf(f string, a ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprintf(c.out, "\r\033[K"+f+"\n", a...)
	c.prompt()
	c.out.Flush()
}

// prompt redraws the input prompt (caller holds mu).
func (c *cli) prompt() {
	name := "（未選択）"
	if c.cur != "" {
		name = c.name(c.cur)
	}
	fmt.Fprintf(c.out, "%s> ", name)
}

func (c *cli) name(addr string) string {
	if ct, ok := c.st.Contacts[addr]; ok && ct.Nick != "" {
		return ct.Nick
	}
	if len(addr) > 14 {
		return addr[:14] + "…"
	}
	return addr
}

func (c *cli) load() error {
	var st cliState
	if err := c.call("/api/state", nil, &st); err != nil {
		return err
	}
	c.mu.Lock()
	c.st = st
	if c.st.Contacts == nil {
		c.st.Contacts = map[string]Contact{}
	}
	c.mu.Unlock()
	return nil
}

func hm(ts int64) string { return time.Unix(ts, 0).Format("01/02 15:04") }

func (c *cli) line(m Message) string {
	if m.Sys != "" {
		return fmt.Sprintf("  — %s %s", hm(m.TS), m.Sys)
	}
	who := c.name(m.Peer)
	if m.Out {
		who = "自分"
	}
	text := m.Text
	if m.Img != "" {
		text = "📷 写真 " + text
	}
	mark := ""
	if m.Out {
		switch m.Status {
		case "sent":
			mark = " ✓"
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

func (c *cli) events() {
	for {
		r, err := (&http.Client{Transport: keyTransport{}}).Get(c.base + "/api/events")
		if err == nil {
			sc := bufio.NewScanner(r.Body)
			sc.Buffer(make([]byte, 64<<10), 4<<20)
			for sc.Scan() {
				l := sc.Text()
				if strings.HasPrefix(l, "data: ") {
					c.onEvent([]byte(l[6:]))
				}
			}
			r.Body.Close()
		}
		time.Sleep(2 * time.Second)
	}
}

func (c *cli) onEvent(b []byte) {
	var ev struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(b, &ev) != nil {
		return
	}
	switch ev.Type {
	case "message":
		var m Message
		if json.Unmarshal(ev.Data, &m) != nil {
			return
		}
		c.mu.Lock()
		c.st.Messages = append(c.st.Messages, m)
		if _, ok := c.st.Contacts[m.Peer]; !ok {
			c.st.Contacts[m.Peer] = Contact{Addr: m.Peer, Nick: m.Nick, Req: !m.Out}
		}
		cur := c.cur
		c.mu.Unlock()
		if m.Out {
			return
		}
		if m.Peer == cur {
			c.printf("%s", c.line(m))
		} else {
			c.printf("📨 %s から: %s   （/open %s）", c.name(m.Peer), strings.SplitN(m.Text, "\n", 2)[0], c.name(m.Peer))
		}
	case "status":
		var s struct{ ID, Peer, Status string }
		if json.Unmarshal(ev.Data, &s) != nil {
			return
		}
		c.mu.Lock()
		for i := range c.st.Messages {
			if c.st.Messages[i].ID == s.ID && c.st.Messages[i].Peer == s.Peer {
				c.st.Messages[i].Status = s.Status
			}
		}
		cur := c.cur
		c.mu.Unlock()
		if s.Peer == cur {
			switch s.Status {
			case "sent":
				c.printf("  ✓ 届きました")
			case "queued":
				c.printf("  🕓 相手がオフライン。オンラインになったら自動で再送します")
			case "stored":
				c.printf("  📮 Nostrリレーに置き配しました")
			case "failed":
				c.printf("  ✗ 送信失敗（/retry で再送）")
			}
		}
	case "contact":
		var ct Contact
		if json.Unmarshal(ev.Data, &ct) == nil && ct.Addr != "" {
			c.mu.Lock()
			c.st.Contacts[ct.Addr] = ct
			c.mu.Unlock()
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

func (c *cli) sortedContacts() []Contact {
	c.mu.Lock()
	defer c.mu.Unlock()
	last := map[string]int64{}
	for _, m := range c.st.Messages {
		if m.TS > last[m.Peer] {
			last[m.Peer] = m.TS
		}
	}
	var out []Contact
	for _, ct := range c.st.Contacts {
		out = append(out, ct)
	}
	sort.Slice(out, func(i, j int) bool { return last[out[i].Addr] > last[out[j].Addr] })
	return out
}

// find resolves a list number, nickname or address.
func (c *cli) find(arg string) string {
	arg = strings.TrimSpace(arg)
	list := c.sortedContacts()
	if n, err := strconv.Atoi(arg); err == nil && n >= 1 && n <= len(list) {
		return list[n-1].Addr
	}
	for _, ct := range list {
		if ct.Addr == arg || strings.EqualFold(ct.Nick, arg) {
			return ct.Addr
		}
	}
	for _, ct := range list {
		if strings.HasPrefix(ct.Addr, arg) || (ct.Nick != "" && strings.Contains(strings.ToLower(ct.Nick), strings.ToLower(arg))) {
			return ct.Addr
		}
	}
	return ""
}

func (c *cli) history(n int) {
	c.mu.Lock()
	var ms []Message
	for _, m := range c.st.Messages {
		if m.Peer == c.cur {
			ms = append(ms, m)
		}
	}
	c.mu.Unlock()
	if len(ms) > n {
		ms = ms[len(ms)-n:]
	}
	if len(ms) == 0 {
		c.printf("  （まだメッセージはありません）")
	}
	for _, m := range ms {
		c.printf("%s", c.line(m))
	}
}

const cliHelp = `コマンド:
  /list              連絡先一覧（番号つき）
  /open <番号|名前|アドレス>   会話を開く（最近20件を表示）
  /add <アドレス> [名前]       連絡先を追加して開く
  /hist [件数]       今の会話の履歴
  /accept            今の相手のメッセージリクエストを承認
  /rename <名前>     今の相手の表示名を変更
  /retry             今の会話の送れていないメッセージを再送
  /me                自分のアドレス・ピア数
  /nick <名前>       自分の名前を変更
  /nostr on|off      Nostr置き配の切り替え
  /quit              終了（-cli で起動したノードも止まります）
それ以外の入力は、開いている相手に送信します。`

func runCLI(ui string) {
	c := &cli{base: "http://" + ui, out: bufio.NewWriter(os.Stdout)}
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 64<<10), 64<<10)
	if err := c.load(); err != nil {
		fmt.Println("ノードに接続できません:", err)
		return
	}
	for c.st.Locked {
		fmt.Print("パスコード: ")
		if !in.Scan() {
			return
		}
		if err := c.call("/api/unlock", map[string]string{"Pass": in.Text()}, nil); err != nil {
			fmt.Println(err)
			continue
		}
		_ = c.load()
	}
	go c.events()
	fmt.Printf("YggChat CLI  — %s（%s）  ピア %d\n/help でコマンド一覧\n", c.st.Nick, c.st.Addr, c.st.Peers)
	if list := c.sortedContacts(); len(list) > 0 {
		c.cmd("/list")
	}
	c.mu.Lock()
	c.prompt()
	c.out.Flush()
	c.mu.Unlock()
	for in.Scan() {
		l := strings.TrimSpace(in.Text())
		if l == "" {
			c.mu.Lock()
			c.prompt()
			c.out.Flush()
			c.mu.Unlock()
			continue
		}
		if l == "/quit" || l == "/exit" {
			return
		}
		if strings.HasPrefix(l, "/") {
			c.cmd(l)
			continue
		}
		if c.cur == "" {
			c.printf("相手が選ばれていません。/list と /open で選んでください")
			continue
		}
		var m Message
		if err := c.call("/api/send", map[string]string{"To": c.cur, "Text": l}, &m); err != nil {
			c.printf("送信エラー: %v", err)
			continue
		}
		c.printf("%s", c.line(m))
	}
}

func (c *cli) cmd(l string) {
	f := strings.Fields(l)
	arg := strings.TrimSpace(strings.TrimPrefix(l, f[0]))
	switch f[0] {
	case "/help", "/h", "/?":
		c.printf("%s", cliHelp)
	case "/list", "/ls":
		list := c.sortedContacts()
		if len(list) == 0 {
			c.printf("  連絡先はまだありません。/add <アドレス> で追加")
		}
		for i, ct := range list {
			tag := ""
			if ct.Req {
				tag = " [リクエスト]"
			}
			nick := ct.Nick
			if nick == "" {
				nick = "（名前なし）"
			}
			c.printf("  %2d. %s  %s%s", i+1, nick, ct.Addr, tag)
		}
	case "/open", "/o":
		a := c.find(arg)
		if a == "" {
			c.printf("見つかりません: %s", arg)
			return
		}
		c.mu.Lock()
		c.cur = a
		c.mu.Unlock()
		c.printf("── %s（%s）──", c.name(a), a)
		c.history(20)
	case "/add":
		if len(f) < 2 {
			c.printf("使い方: /add <アドレス> [名前]")
			return
		}
		body := map[string]any{"Addr": f[1], "Accept": true}
		if len(f) > 2 {
			body["Nick"] = strings.Join(f[2:], " ")
		}
		var ct Contact
		if err := c.call("/api/contact", body, &ct); err != nil {
			c.printf("追加できません: %v", err)
			return
		}
		c.mu.Lock()
		c.st.Contacts[ct.Addr] = ct
		c.cur = ct.Addr
		c.mu.Unlock()
		c.printf("追加しました: %s", c.name(ct.Addr))
	case "/hist":
		n := 50
		if v, err := strconv.Atoi(arg); err == nil && v > 0 {
			n = v
		}
		if c.cur == "" {
			c.printf("先に /open してください")
			return
		}
		c.history(n)
	case "/accept", "/rename":
		if c.cur == "" {
			c.printf("先に /open してください")
			return
		}
		body := map[string]any{"Addr": c.cur, "Accept": f[0] == "/accept"}
		if f[0] == "/rename" {
			if arg == "" {
				c.printf("使い方: /rename <名前>")
				return
			}
			body["Nick"] = arg
		}
		var ct Contact
		if err := c.call("/api/contact", body, &ct); err != nil {
			c.printf("エラー: %v", err)
			return
		}
		c.mu.Lock()
		c.st.Contacts[ct.Addr] = ct
		c.mu.Unlock()
		c.printf("OK")
	case "/retry":
		c.mu.Lock()
		var ids []string
		for _, m := range c.st.Messages {
			if m.Peer == c.cur && m.Out && (m.Status == "failed" || m.Status == "queued" || m.Status == "stored") {
				ids = append(ids, m.ID)
			}
		}
		cur := c.cur
		c.mu.Unlock()
		for _, id := range ids {
			_ = c.call("/api/send", map[string]string{"To": cur, "Retry": id}, nil)
		}
		c.printf("%d件を再送中", len(ids))
	case "/me":
		_ = c.load()
		c.printf("  %s  %s  ピア %d  置き配 %v", c.st.Nick, c.st.Addr, c.st.Peers, c.st.Nostr.On)
	case "/nick":
		if arg == "" {
			c.printf("使い方: /nick <名前>")
			return
		}
		if err := c.call("/api/nick", map[string]string{"Nick": arg}, nil); err != nil {
			c.printf("エラー: %v", err)
			return
		}
		c.mu.Lock()
		c.st.Nick = arg
		c.mu.Unlock()
		c.printf("名前を %s にしました", arg)
	case "/nostr":
		if arg != "on" && arg != "off" {
			c.printf("使い方: /nostr on|off  （公開リレーに暗号化して最大7日預けます。宛先タグ・時刻・IPはリレーに見えます）")
			return
		}
		var res struct{ On bool }
		if err := c.call("/api/nostr", map[string]bool{"On": arg == "on"}, &res); err != nil {
			c.printf("エラー: %v", err)
			return
		}
		c.printf("Nostr置き配: %v", res.On)
	default:
		c.printf("不明なコマンド。/help を見てください")
	}
}
