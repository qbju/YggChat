// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

//go:build !windows

package main

// openWindow: only Windows gets a native app window; elsewhere use a browser.
func openWindow(url, dataDir string) bool { return false }
