//go:build !linux

// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

package main

import (
	"errors"
	"net"
)

func uiListenerFromApp(name, want string) (net.Listener, error) {
	return nil, errors.New("YGGCHAT_UIFD is only supported on Android/Linux")
}
