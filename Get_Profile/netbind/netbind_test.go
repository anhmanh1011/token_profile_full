package netbind

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestLocalTCPAddr(t *testing.T) {
	t.Parallel()

	if addr, err := LocalTCPAddr(""); err != nil || addr != nil {
		t.Fatalf("empty: got (%v, %v), want (nil, nil)", addr, err)
	}
	if addr, err := LocalTCPAddr("  203.0.113.10 "); err != nil || addr == nil || addr.IP.String() != "203.0.113.10" {
		t.Fatalf("ipv4: got (%v, %v)", addr, err)
	}
	if addr, err := LocalTCPAddr("2001:db8::20"); err != nil || addr == nil || addr.IP.String() != "2001:db8::20" {
		t.Fatalf("ipv6: got (%v, %v)", addr, err)
	}
	if _, err := LocalTCPAddr("not-an-ip"); err == nil {
		t.Fatal("malformed: want error, got nil")
	}
}

// TestDialContextBindsSource proves the source IP is actually bound: dialing a
// loopback listener with LocalAddr 127.0.0.1 succeeds, while binding an address
// not present on the host fails at bind() time.
func TestDialContextBindsSource(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	dialLoopback, err := DialContext("127.0.0.1", 2*time.Second, 0)
	if err != nil {
		t.Fatalf("build loopback dialer: %v", err)
	}
	conn, err := dialLoopback(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("bind 127.0.0.1: unexpected dial error: %v", err)
	}
	if got := conn.LocalAddr().(*net.TCPAddr).IP.String(); got != "127.0.0.1" {
		t.Fatalf("source IP = %s, want 127.0.0.1", got)
	}
	conn.Close()

	dialBogus, err := DialContext("10.123.45.67", 2*time.Second, 0)
	if err != nil {
		t.Fatalf("build bogus dialer: %v", err)
	}
	if _, err := dialBogus(context.Background(), "tcp", ln.Addr().String()); err == nil {
		t.Fatal("bind 10.123.45.67: expected dial to fail, got nil")
	}
}
