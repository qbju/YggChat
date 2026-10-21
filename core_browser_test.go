//go:build !windows

package main

import (
	"runtime"
	"testing"
)

func TestBrowserCmd(t *testing.T) {
	env := func(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }
	u := "http://127.0.0.1:7780/?k=x"
	switch runtime.GOOS {
	case "linux":
		if c := browserCmd(u, env(map[string]string{"DISPLAY": ":0"})); len(c) != 2 || c[0] != "xdg-open" || c[1] != u {
			t.Fatal(c)
		}
		if c := browserCmd(u, env(map[string]string{"WAYLAND_DISPLAY": "wayland-0"})); c == nil {
			t.Fatal("wayland")
		}
		if browserCmd(u, env(nil)) != nil {
			t.Fatal("headless opened")
		}
	}
	for _, kv := range []map[string]string{
		{"DISPLAY": ":0", "SSH_CONNECTION": "1 2 3 4"},
		{"DISPLAY": ":0", "YGGCHAT_SUPERVISED": "1"},
		{"DISPLAY": ":0", "YGGCHAT_NO_OPEN": "1"},
	} {
		if browserCmd(u, env(kv)) != nil {
			t.Fatalf("opened with %v", kv)
		}
	}
}
