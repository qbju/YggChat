//go:build js

package core

import (
	"context"
	"net"
	"net/url"

	"github.com/coder/websocket"
)

func (l *linkWS) dial(ctx context.Context, url *url.URL, info linkInfo, options linkOptions) (net.Conn, error) {
	wsconn, _, err := websocket.Dial(ctx, url.String(), &websocket.DialOptions{Subprotocols: []string{"ygg-ws"}})
	if err != nil {
		return nil, err
	}
	return &linkWSConn{Conn: websocket.NetConn(context.Background(), wsconn, websocket.MessageBinary)}, nil
}
