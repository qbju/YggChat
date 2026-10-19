//go:build !linux

// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

package main

import "net"

func (v *vpnState) accept(c *net.UnixConn) { c.Close() }
