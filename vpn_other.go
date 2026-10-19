//go:build !linux

package main

import "net"

func (v *vpnState) accept(c *net.UnixConn) { c.Close() }
