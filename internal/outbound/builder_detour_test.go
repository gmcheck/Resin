package outbound

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	M "github.com/sagernet/sing/common/metadata"
)

// TestBuild_DetourToRegisteredNodeOutbound verifies the ref:<hash> mechanism
// end to end: a node outbound built with detour=<hash> must resolve that tag in
// the sing-box outbound manager and reach the dial destination through it.
//
// A missing registration makes the dial fail with "outbound detour not found"
// and never touches the listener, so an accepted connection proves the detour
// chain was followed.
func TestBuild_DetourToRegisteredNodeOutbound(t *testing.T) {
	b := newTestSingboxBuilder(t)
	defer b.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	accepted := make(chan struct{}, 1)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			select {
			case accepted <- struct{}{}:
			default:
			}
			_ = conn.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	// The "upstream node" is registered under its hash, exactly like
	// EnsureNodeOutbound does for ref:/platform: support.
	// The upstream must be a real proxy outbound: sing-box rejects a detour to
	// an empty direct outbound ("makes no sense"), which is also why a
	// direct-type node cannot serve as a ref:/platform: upstream.
	const upstreamHash = "a0a8ffd679b6c23ba0ddf842ca2185b0"
	upstreamRaw := json.RawMessage(fmt.Sprintf(
		`{"type":"socks","tag":"up","version":"5","server":"127.0.0.1","server_port":%d}`,
		port,
	))
	if err := b.RegisterNodeOutbound(upstreamHash, upstreamRaw); err != nil {
		t.Fatalf("register upstream node outbound: %v", err)
	}

	// Consumer outbound dials its SOCKS server through the detour. The server
	// address points at the listener, so reaching it proves the chain.
	raw := json.RawMessage(fmt.Sprintf(
		`{"type":"socks","tag":"consumer","version":"5","server":"127.0.0.1","server_port":%d,"detour":%q}`,
		port, upstreamHash,
	))
	ob, err := b.Build(raw)
	if err != nil {
		t.Fatalf("build consumer outbound: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// The SOCKS handshake against a non-SOCKS listener will fail; what matters
	// is that it reached the listener at all.
	_, dialErr := ob.DialContext(ctx, "tcp", M.ParseSocksaddrHostPortStr("1.1.1.1", "443"))

	select {
	case <-accepted:
	case <-time.After(3 * time.Second):
		t.Fatalf("detour chain not followed: listener never accepted a connection (dialErr=%v)", dialErr)
	}
	if dialErr != nil && strings.Contains(dialErr.Error(), "detour not found") {
		t.Fatalf("detour tag did not resolve: %v", dialErr)
	}
}

// TestBuild_DetourToMissingTagFails verifies the failure mode we rely on to
// detect a broken upstream reference.
func TestBuild_DetourToMissingTagFails(t *testing.T) {
	b := newTestSingboxBuilder(t)
	defer b.Close()

	raw := json.RawMessage(`{"type":"socks","tag":"consumer","version":"5","server":"127.0.0.1","server_port":1080,"detour":"ffffffffffffffffffffffffffffffff"}`)
	ob, err := b.Build(raw)
	if err != nil {
		t.Fatalf("build consumer outbound: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, dialErr := ob.DialContext(ctx, "tcp", M.ParseSocksaddrHostPortStr("1.1.1.1", "443"))
	if dialErr == nil {
		t.Fatal("expected dial to fail for an unregistered detour tag")
	}
	if !strings.Contains(dialErr.Error(), "detour not found") {
		t.Fatalf("unexpected error: %v", dialErr)
	}
}
