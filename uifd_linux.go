//go:build linux

// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

package main

import (
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"syscall"
	"time"
)

// uiListenerFromApp takes the UI listening socket from the Android app
// (YGGCHAT_UIFD=<abstract unix socket name>, v0.19.4). The app binds
// 127.0.0.1:7780 once and keeps it for its whole life, so the port is never
// free for another app while our node restarts; see YggService.holdUiPort.
// Waits until the app has the port (another app may hold it right now).
func uiListenerFromApp(name, want string) (net.Listener, error) {
	logged := false
	for {
		ln, err := uiFDOnce(name, want)
		if err == nil {
			return ln, nil
		}
		if errors.Is(err, errUIFDWrong) {
			return nil, err
		}
		if !logged {
			log.Printf("waiting for the UI port from the app: %v", err)
			logged = true
		}
		time.Sleep(300 * time.Millisecond)
	}
}

var errUIFDWrong = errors.New("the app handed over something that is not the UI socket")

func uiFDOnce(name, want string) (net.Listener, error) {
	c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: "@" + name, Net: "unix"})
	if err != nil {
		return nil, err
	}
	defer c.Close()
	raw, err := c.SyscallConn()
	if err != nil {
		return nil, err
	}
	var cred *syscall.Ucred
	_ = raw.Control(func(fd uintptr) { cred, _ = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED) })
	if cred == nil || int(cred.Uid) != os.Getuid() {
		return nil, errors.New("UI socket server is not our app")
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf, oob := make([]byte, 16), make([]byte, syscall.CmsgSpace(4*4))
	_, oobn, flags, _, err := c.ReadMsgUnix(buf, oob)
	if err != nil {
		return nil, err
	}
	msgs, err := syscall.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return nil, err
	}
	var fds []int
	for i := range msgs {
		if got, err := syscall.ParseUnixRights(&msgs[i]); err == nil {
			fds = append(fds, got...)
		}
	}
	if len(fds) != 1 || flags&syscall.MSG_CTRUNC != 0 {
		for _, fd := range fds {
			syscall.Close(fd)
		}
		return nil, errors.New("no UI socket received yet")
	}
	ln, err := uiListenerFD(fds[0], want)
	if err != nil {
		return nil, err
	}
	_, _ = c.Write([]byte{1})
	return ln, nil
}

// uiListenerFD checks that fd is a listening TCP socket on want and wraps it.
func uiListenerFD(fd int, want string) (net.Listener, error) {
	f := os.NewFile(uintptr(fd), "ui")
	defer f.Close()
	typ, err := syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_TYPE)
	acc, err2 := syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_ACCEPTCONN)
	if err != nil || err2 != nil || typ != syscall.SOCK_STREAM || acc != 1 {
		return nil, errUIFDWrong
	}
	ln, err := net.FileListener(f)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errUIFDWrong, err)
	}
	wa, err := net.ResolveTCPAddr("tcp", want)
	ta, ok := ln.Addr().(*net.TCPAddr)
	if err != nil || !ok || !ta.IP.Equal(wa.IP) || ta.Port != wa.Port {
		ln.Close()
		return nil, fmt.Errorf("%w: bound to %v, want %s", errUIFDWrong, ln.Addr(), want)
	}
	return ln, nil
}
