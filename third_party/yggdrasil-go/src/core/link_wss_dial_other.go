//go:build !js

// Modified for YggChat (2026): WebSocket dialing split into link_ws*_dial_js.go /
// link_ws*_dial_other.go so the core builds for js/wasm. Licensed under the
// LGPLv3 with the Yggdrasil special exception, like the rest of this library.

package core

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/coder/websocket"
)

func (l *linkWSS) dial(ctx context.Context, url *url.URL, info linkInfo, options linkOptions) (net.Conn, error) {
	tlsconfig := l.tlsconfig.Clone()
	return l.findSuitableIP(url, func(hostname string, ip net.IP, port int) (net.Conn, error) {
		tlsconfig.ServerName = hostname
		tlsconfig.MinVersion = tls.VersionTLS12
		tlsconfig.MaxVersion = tls.VersionTLS13
		if sni := options.tlsSNI; sni != "" {
			tlsconfig.ServerName = sni
		}
		u := *url
		u.Host = net.JoinHostPort(ip.String(), fmt.Sprintf("%d", port))
		addr := &net.TCPAddr{
			IP:   ip,
			Port: port,
		}
		dialer, err := l.tcp.dialerFor(addr, info.sintf)
		if err != nil {
			return nil, err
		}
		transport := &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			Dial:                  dialer.Dial, // nolint:staticcheck
			DialContext:           dialer.DialContext,
			DisableKeepAlives:     true,
			ResponseHeaderTimeout: 5 * time.Second,
			TLSClientConfig:       tlsconfig,
			TLSHandshakeTimeout:   5 * time.Second,
		}
		defer transport.CloseIdleConnections()
		wsconn, _, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{
			HTTPClient: &http.Client{
				Transport: transport,
			},
			Subprotocols: []string{"ygg-ws"},
			Host:         hostname,
		})
		if err != nil {
			return nil, err
		}
		return &linkWSSConn{
			Conn: websocket.NetConn(ctx, wsconn, websocket.MessageBinary),
		}, nil
	})
}
