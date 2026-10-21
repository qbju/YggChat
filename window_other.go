// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

//go:build !windows

package main

import (
	"os"
	"os/exec"
	"runtime"
)

// openWindow: only Windows gets a native app window; elsewhere use a browser.
func openWindow(url, dataDir string) bool { return false }

// browserCmd is the command that opens url in the default browser on this
// desktop, or nil when there is no desktop to open it on (server, SSH,
// Android service, a restart of ourselves).
func browserCmd(url string, getenv func(string) string) []string {
	if getenv("YGGCHAT_SUPERVISED") == "1" || getenv("YGGCHAT_NO_OPEN") == "1" {
		return nil
	}
	if getenv("SSH_CONNECTION") != "" || getenv("SSH_TTY") != "" {
		return nil // the browser would open on the remote machine's screen, if any
	}
	switch runtime.GOOS {
	case "darwin":
		return []string{"open", url}
	case "linux", "freebsd", "openbsd", "netbsd", "dragonfly":
		if getenv("DISPLAY") == "" && getenv("WAYLAND_DISPLAY") == "" {
			return nil // console / headless
		}
		return []string{"xdg-open", url}
	}
	return nil
}

// openBrowser opens the UI in the default browser (macOS/Linux desktops).
// The URL is printed as well, so nothing is lost when it can't.
func openBrowser(url string) {
	c := browserCmd(url, os.Getenv)
	if c == nil {
		return
	}
	if _, err := exec.LookPath(c[0]); err != nil {
		return
	}
	cmd := exec.Command(c[0], c[1:]...)
	cmd.Stdout, cmd.Stderr = nil, nil
	if cmd.Start() == nil {
		go cmd.Wait() // reap it
	}
}
