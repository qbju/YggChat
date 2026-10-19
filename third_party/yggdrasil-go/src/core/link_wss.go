package core

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/url"

	"github.com/Arceliar/phony"
)

type linkWSS struct {
	phony.Inbox
	*links
	tlsconfig *tls.Config
}

type linkWSSConn struct {
	net.Conn
}

func (l *links) newLinkWSS() *linkWSS {
	lwss := &linkWSS{
		links:     l,
		tlsconfig: l.core.config.tls.Clone(),
	}
	return lwss
}

func (l *linkWSS) listen(ctx context.Context, url *url.URL, _ string) (net.Listener, error) {
	return nil, fmt.Errorf("WSS listener not supported, use WS listener behind reverse proxy instead")
}
