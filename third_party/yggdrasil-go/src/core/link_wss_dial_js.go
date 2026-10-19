//go:build js

// Modified for YggChat (2026): WebSocket dialing split into link_ws*_dial_js.go /
// link_ws*_dial_other.go so the core builds for js/wasm. Licensed under the
// LGPLv3 with the Yggdrasil special exception, like the rest of this library.

package core

import (
	"context"
	"net"
	"net/url"

	"github.com/coder/websocket"
)

func (l *linkWSS) dial(ctx context.Context, url *url.URL, info linkInfo, options linkOptions) (net.Conn, error) {
	wsconn, _, err := websocket.Dial(ctx, url.String(), &websocket.DialOptions{Subprotocols: []string{"ygg-ws"}})
	if err != nil {
		return nil, err
	}
	return &linkWSSConn{Conn: websocket.NetConn(context.Background(), wsconn, websocket.MessageBinary)}, nil
}
