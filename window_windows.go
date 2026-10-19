// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

//go:build windows

package main

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"

	webview2 "github.com/jchv/go-webview2"
)

var (
	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleProcList = kernel32.NewProc("GetConsoleProcessList")
	procFreeConsole        = kernel32.NewProc("FreeConsole")
)

// ownConsole reports whether this process is alone on its console, i.e. it
// was started by double-click rather than from cmd/PowerShell.
func ownConsole() bool {
	var ids [4]uint32
	n, _, _ := procGetConsoleProcList.Call(uintptr(unsafe.Pointer(&ids[0])), uintptr(len(ids)))
	return n == 1
}

// openWindow shows the UI in a WebView2 window and blocks until it is closed
// (returns true). Without the WebView2 runtime it opens the default browser
// instead and returns false.
func openWindow(url, dataDir string) bool {
	runtime.LockOSThread()
	w := webview2.NewWithOptions(webview2.WebViewOptions{
		DataPath:  filepath.Join(dataDir, "webview2"),
		AutoFocus: true,
		WindowOptions: webview2.WindowOptions{
			Title: "YggChat", Width: 1000, Height: 720, Center: true,
		},
	})
	if w == nil {
		_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
		return false
	}
	if ownConsole() {
		procFreeConsole.Call() // hide the black console window
	}
	w.SetSize(360, 520, webview2.HintMin)
	w.Navigate(url)
	w.Run()
	w.Destroy()
	return true
}
