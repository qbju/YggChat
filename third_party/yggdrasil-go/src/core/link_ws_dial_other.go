//go:build !js

package core

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"

	"github.com/coder/websocket"
)

func (l *linkWS) dial(ctx context.Context, url *url.URL, info linkInfo, options linkOptions) (net.Conn, error) {
	return l.findSuitableIP(url, func(hostname string, ip net.IP, port int) (net.Conn, error) {
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
		wsconn, _, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{
			HTTPClient: &http.Client{
				Transport: &http.Transport{
					Proxy:       http.ProxyFromEnvironment,
					Dial:        dialer.Dial,
					DialContext: dialer.DialContext,
				},
			},
			Subprotocols: []string{"ygg-ws"},
			Host:         hostname,
		})
		if err != nil {
			return nil, err
		}
		return &linkWSConn{
			Conn: websocket.NetConn(ctx, wsconn, websocket.MessageBinary),
		}, nil
	})
}
