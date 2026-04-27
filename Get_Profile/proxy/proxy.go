// Package proxy parses proxy configuration and builds SOCKS5 dial functions
// for use with net/http transports. Hostnames are resolved on the proxy side
// (RFC 1928 SOCKS5 with domain ATYP), so DNS does not leak.
package proxy

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	xproxy "golang.org/x/net/proxy"
)

// DialContextFunc matches the signature of net.Dialer.DialContext.
type DialContextFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// Parse converts the legacy host:port[:user:pass] form into a normalised
// socks5h:// URL. A value already containing "://" is returned as-is so a
// future config entry can opt into a different scheme. Returns "" for
// empty/malformed input.
func Parse(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.Contains(raw, "://") {
		return raw
	}
	parts := strings.Split(raw, ":")
	switch len(parts) {
	case 2:
		return fmt.Sprintf("socks5h://%s:%s", parts[0], parts[1])
	case 4:
		return fmt.Sprintf(
			"socks5h://%s:%s@%s:%s",
			url.QueryEscape(parts[2]), url.QueryEscape(parts[3]),
			parts[0], parts[1],
		)
	}
	return ""
}

// SOCKS5DialContext returns a DialContext function that routes TCP connections
// through the SOCKS5 proxy at proxyURL. When proxyURL is empty, the returned
// function is a plain net.Dialer.DialContext with the supplied timeouts.
func SOCKS5DialContext(proxyURL string, timeout, keepAlive time.Duration) (DialContextFunc, error) {
	base := &net.Dialer{Timeout: timeout, KeepAlive: keepAlive}
	if proxyURL == "" {
		return base.DialContext, nil
	}

	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("proxy: parse url: %w", err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("proxy: empty host in %q", proxyURL)
	}

	var auth *xproxy.Auth
	if u.User != nil {
		pass, _ := u.User.Password()
		auth = &xproxy.Auth{User: u.User.Username(), Password: pass}
	}

	dialer, err := xproxy.SOCKS5("tcp", u.Host, auth, base)
	if err != nil {
		return nil, fmt.Errorf("proxy: build SOCKS5 dialer: %w", err)
	}

	ctxDialer, ok := dialer.(xproxy.ContextDialer)
	if !ok {
		// All x/net/proxy SOCKS5 dialers implement ContextDialer; this guards
		// against a future API change rather than something we expect.
		return nil, fmt.Errorf("proxy: dialer does not implement ContextDialer")
	}
	return ctxDialer.DialContext, nil
}
