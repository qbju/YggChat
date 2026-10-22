// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

package main

import (
	"strings"
	"sync"
	"testing"
)

func TestCLITermSafe(t *testing.T) {
	cases := []struct{ in, want string }{
		{"hello", "hello"},
		{"日本語 😀", "日本語 😀"},
		{"hi \x1b[2J\x1b]52;c;ZWNobyBwd25lZA==\x07\r\nfake: line", "hi [2J]52;c;ZWNobyBwd25lZA==\n" + cliIndent + "fake: line"},
		{"a\x1b]8;;http://evil\x1b\\b", "a]8;;http://evil\\b"},
		{"\u009b31mred\u009d0;title\u0007", "31mred0;title"},
		{"x\u202egnp.exe", "xgnp.exe"},
		{"\u2066iso\u2069 \u200fr", "iso r"},
		{"tab\there", "tab here"},
		{"del\x7fx", "delx"},
		{"line1\nline2\n", "line1\n" + cliIndent + "line2"},
		{"a\r\rb", "ab"},
		{"bad \xff utf8", "bad \ufffd utf8"},
	}
	for _, c := range cases {
		if got := termSafe(c.in); got != c.want {
			t.Errorf("termSafe(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	for _, s := range []string{"\x1b[31m\x9b\x07\x00", "x\r\ny\u202e\x1b"} {
		got := termSafe(s)
		for _, r := range got {
			if (r < 0x20 && r != '\n') || r == 0x7f || (r >= 0x80 && r <= 0x9f) || isBidiControl(r) {
				t.Errorf("termSafe(%q) kept %U", s, r)
			}
		}
	}
	// every line after a newline is indented, so it can't pose as "[time] who:"
	got := termSafe("ok\n[10/21 20:00] Bob: forged")
	if !strings.Contains(got, "\n"+cliIndent+"[10/21") {
		t.Errorf("continuation not indented: %q", got)
	}
}

func TestCLITermSafeLine(t *testing.T) {
	if got := termSafeLine("Bob\n\x1b[2Jevil\r\n"); got != "Bob [2Jevil" {
		t.Errorf("termSafeLine = %q", got)
	}
	if strings.ContainsAny(termSafeLine("a\nb\nc"), "\n") {
		t.Error("newline kept")
	}
}

func TestCLIParseInput(t *testing.T) {
	cases := []struct{ in, cmd, arg, text string }{
		{"hello", "", "", "hello"},
		{"  hello  ", "", "", "hello"},
		{"/open  Bob ", "/open", "Bob", ""},
		{"/add 200:1::2 ボブ さん", "/add", "200:1::2 ボブ さん", ""},
		{"/list", "/list", "", ""},
		{"//shrug ¯\\_(ツ)_/¯", "", "", "/shrug ¯\\_(ツ)_/¯"},
		{"//", "", "", "/"},
		{"", "", "", ""},
	}
	for _, c := range cases {
		cmd, arg, text := parseInput(c.in)
		if cmd != c.cmd || arg != c.arg || text != c.text {
			t.Errorf("parseInput(%q) = %q,%q,%q want %q,%q,%q", c.in, cmd, arg, text, c.cmd, c.arg, c.text)
		}
	}
}

func TestCLIParseHist(t *testing.T) {
	for in, want := range map[string]int{"": 50, " 5 ": 5, "100": 100} {
		if n, ok := parseHist(in); !ok || n != want {
			t.Errorf("parseHist(%q) = %d,%v", in, n, ok)
		}
	}
	for _, in := range []string{"abc", "0", "-3", "5x"} {
		if _, ok := parseHist(in); ok {
			t.Errorf("parseHist(%q) accepted", in)
		}
	}
}

func TestCLIFindContact(t *testing.T) {
	list := []cliContact{
		{Addr: "200:aaaa::1", Nick: "Alice"},
		{Addr: "200:aaab::2", Nick: "Alicia"},
		{Addr: "201:bbbb::3", Nick: "Bob"},
		{Addr: "g:abc", Nick: "テスト班", Group: true},
	}
	ok := map[string]string{
		"1": "200:aaaa::1", "3": "201:bbbb::3", "alice": "200:aaaa::1", "BOB": "201:bbbb::3",
		"201": "201:bbbb::3", "200:aaab::2": "200:aaab::2", "テスト": "g:abc", "ob": "201:bbbb::3",
	}
	for in, want := range ok {
		if got, cand := findContact(list, in); got != want {
			t.Errorf("findContact(%q) = %q (%d candidates), want %q", in, got, len(cand), want)
		}
	}
	if got, cand := findContact(list, "200"); got != "" || len(cand) != 2 {
		t.Errorf("ambiguous prefix: got %q, %d candidates", got, len(cand))
	}
	if got, cand := findContact(list, "Ali"); got != "" || len(cand) != 2 {
		t.Errorf("ambiguous nick: got %q, %d candidates", got, len(cand))
	}
	if got, cand := findContact(list, "zzz"); got != "" || len(cand) != 0 {
		t.Errorf("no match: got %q, %v", got, cand)
	}
	if got, _ := findContact(list, ""); got != "" {
		t.Error("empty arg matched")
	}
	if got, _ := findContact(list, "9"); got != "" {
		t.Error("out-of-range number matched")
	}
}

func TestCLIFlag(t *testing.T) {
	for _, a := range [][]string{{"-cli"}, {"--cli"}, {"-data", "x", "-cli=true"}} {
		if !cliFlag(a) {
			t.Errorf("cliFlag(%v) = false", a)
		}
	}
	for _, a := range [][]string{{}, {"-cli=false"}, {"--", "-cli"}, {"-client"}} {
		if cliFlag(a) {
			t.Errorf("cliFlag(%v) = true", a)
		}
	}
}

// Events writing contacts while the input side formats lines must not race
// (run with -race).
func TestCLIConcurrentEvents(t *testing.T) {
	c := &cli{st: cliState{Contacts: map[string]cliContact{}}, gdone: map[string]bool{}}
	c.out = nil
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			c.mu.Lock()
			c.st.Contacts["200::1"] = cliContact{Addr: "200::1", Nick: "B"}
			delete(c.st.Contacts, "200::2")
			c.mu.Unlock()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			_ = c.line(Message{Peer: "200::1", Text: "x"})
			_ = c.name("200::2")
			_ = c.sortedContacts()
		}
	}()
	wg.Wait()
}

func TestCLIGroupSender(t *testing.T) {
	c := &cli{st: cliState{Contacts: map[string]cliContact{
		"g:1":    {Addr: "g:1", Nick: "テスト班", Group: true},
		"200::2": {Addr: "200::2", Nick: "Bob"},
	}}}
	if l := c.line(Message{Peer: "g:1", From: "200::2", Nick: "Bob\x1b[2J", Text: "hi"}); !strings.Contains(l, " Bob[2J: hi") {
		t.Errorf("group line = %q", l)
	}
	if l := c.line(Message{Peer: "g:1", From: "200::2", Text: "hi"}); !strings.Contains(l, " Bob: hi") {
		t.Errorf("group line without nick = %q", l)
	}
	if l := c.line(Message{Peer: "g:1", Out: true, Status: "sent", Dlv: 1, Tot: 2, Text: "x"}); !strings.HasSuffix(l, "✓ 1/2") {
		t.Errorf("group status = %q", l)
	}
	if l := c.line(Message{Peer: "g:1", Sys: "ttl:300"}); !strings.Contains(l, "300秒") {
		t.Errorf("sys = %q", l)
	}
}

// A post relayed without the sender's proof shows who relayed it (as in the
// UI), and the sender's own copy replacing it later is not an edit (r2-matrix).
func TestCLIRelayedPost(t *testing.T) {
	c := &cli{st: cliState{Contacts: map[string]cliContact{
		"g:1":    {Addr: "g:1", Nick: "班", Group: true},
		"200::3": {Addr: "200::3", Nick: "Carol"},
	}}, gdone: map[string]bool{}}
	m := Message{ID: "abc1", Peer: "g:1", From: "200::2", Nick: "Bob", Text: "📷 cap", Via: "200::3", Hist: true}
	if l := c.line(m); !strings.Contains(l, "Bob（Carolが中継）: 📷 cap") {
		t.Errorf("relayed line = %q", l)
	}
	c.st.Messages = []Message{m}
	c.onEvent([]byte(`{"type":"msg_upd","data":{"id":"abc1","peer":"g:1","from":"200::2","nick":"Bob","text":"cap","via":"","hist":false,"img":"data:image/png;base64,AA=="}}`))
	x := c.st.Messages[0]
	if x.Edited || x.Via != "" || x.Hist || x.Text != "cap" || x.Img == "" {
		t.Errorf("after the own copy: %+v", x)
	}
	if l := c.line(x); strings.Contains(l, "中継") || strings.Contains(l, "編集") {
		t.Errorf("line = %q", l)
	}
	c.onEvent([]byte(`{"type":"msg_upd","data":{"id":"abc1","peer":"g:1","from":"200::2","text":"cap2","edited":true}}`))
	if x := c.st.Messages[0]; !x.Edited || x.Text != "cap2" {
		t.Errorf("edit: %+v", x)
	}
}
