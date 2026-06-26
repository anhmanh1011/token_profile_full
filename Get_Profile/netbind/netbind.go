// Package netbind builds net/http dialers that bind outbound connections to a
// fixed local source IP (the tenant's "callout IP"). On a VPS with multiple
// public addresses (one IPv4 and one IPv6), each Get_Profile instance pins its
// Loki and token-exchange traffic to its tenant's address.
//
// Binding is applied via net.Dialer.LocalAddr, so it affects only direct
// connections — which is the deployment model here: each tenant dials Loki and
// Microsoft directly from its own VPS IP. The localhost API client deliberately
// does NOT use this, so loopback traffic is never forced onto a public address.
package netbind

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

// LocalTCPAddr parses a callout IP into a *net.TCPAddr suitable for
// net.Dialer.LocalAddr. An empty string returns (nil, nil) meaning "use the OS
// default route". A non-empty but unparseable value is an error so startup can
// surface the misconfiguration instead of silently leaking the default IP.
func LocalTCPAddr(localIP string) (*net.TCPAddr, error) {
	localIP = strings.TrimSpace(localIP)
	if localIP == "" {
		return nil, nil
	}
	ip := net.ParseIP(localIP)
	if ip == nil {
		return nil, fmt.Errorf("netbind: invalid local IP %q", localIP)
	}
	return &net.TCPAddr{IP: ip}, nil
}

// DialContext returns a net.Dialer.DialContext bound to localIP. When localIP
// is empty it dials from the OS default route. A malformed localIP degrades to
// an unbound dialer with the error returned, so the caller can log it.
func DialContext(localIP string, timeout, keepAlive time.Duration) (func(ctx context.Context, network, addr string) (net.Conn, error), error) {
	addr, err := LocalTCPAddr(localIP)
	dialer := &net.Dialer{
		Timeout:   timeout,
		KeepAlive: keepAlive,
		LocalAddr: addr,
	}
	return dialer.DialContext, err
}
