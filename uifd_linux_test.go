//go:build linux

package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"testing"
	"time"
)

// fakeApp plays YggService: holds a TCP listener and hands its fd to whoever connects.
func fakeApp(t *testing.T, held *net.TCPListener) string {
	name := fmt.Sprintf("yggchat-ui-test-%d-%d", os.Getpid(), time.Now().UnixNano())
	srv, err := net.ListenUnix("unix", &net.UnixAddr{Name: "@" + name, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	f, err := held.File()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	go func() {
		for {
			c, err := srv.AcceptUnix()
			if err != nil {
				return
			}
			c.WriteMsgUnix([]byte{1}, syscall.UnixRights(int(f.Fd())), nil)
			b := make([]byte, 1)
			c.SetReadDeadline(time.Now().Add(2 * time.Second))
			c.Read(b)
			c.Close()
		}
	}()
	return name
}

func TestUIListenerFromApp(t *testing.T) {
	held, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 19301})
	if err != nil {
		t.Skip("port 19301 busy:", err)
	}
	defer held.Close()
	name := fakeApp(t, held)
	ln, err := uiListenerFromApp(name, "127.0.0.1:19301")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		c, err := ln.Accept()
		if err == nil {
			c.Write([]byte("ok"))
			c.Close()
		}
	}()
	c, err := net.Dial("tcp", "127.0.0.1:19301")
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 2)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(b); err != nil || string(b) != "ok" {
		t.Fatal("node did not serve on the handed-over socket", err)
	}
	c.Close()

	// The node goes down (restartSelf): the port stays taken, so no other
	// program can listen there; a client connecting meanwhile waits for the
	// next node instead of reaching someone else.
	ln.Close()
	if l2, err := net.Listen("tcp", "127.0.0.1:19301"); err == nil {
		l2.Close()
		t.Fatal("port was free while the node was down")
	}
	waiting, err := net.Dial("tcp", "127.0.0.1:19301")
	if err != nil {
		t.Fatal("connect while node down:", err)
	}
	defer waiting.Close()
	ln2, err := uiListenerFromApp(name, "127.0.0.1:19301") // the next node
	if err != nil {
		t.Fatal(err)
	}
	defer ln2.Close()
	ln2.(*net.TCPListener).SetDeadline(time.Now().Add(2 * time.Second))
	if c, err := ln2.Accept(); err != nil {
		t.Fatal("queued connection not served by the next node:", err)
	} else {
		c.Close()
	}
}

func TestUIListenerFromAppWrongSocket(t *testing.T) {
	held, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 19302})
	if err != nil {
		t.Skip("port busy")
	}
	defer held.Close()
	name := fakeApp(t, held)
	if _, err := uiListenerFromApp(name, "127.0.0.1:19303"); !errors.Is(err, errUIFDWrong) {
		t.Fatalf("socket on another port accepted: %v", err)
	}
	// Nobody serving the name yet: uiFDOnce fails without a socket (the node keeps asking).
	if _, err := uiFDOnce("yggchat-ui-nobody-"+fmt.Sprint(time.Now().UnixNano()), "127.0.0.1:19302"); err == nil {
		t.Fatal("no server, but got a socket")
	}
}
