// Command lever-login-forward is the guest half of lever's remote-access
// login path: a TCP forwarder from a guest loopback port to the host.
//
// It exists for one reason. scion validates the OIDC issuer URL at hub
// STARTUP and refuses to start unless it is https or http on
// localhost/127.0.0.1 — so the hub can only be pointed at a provider it
// believes is local. lever's provider is not local to the guest: it runs
// host-side, inside the `lever remote serve` process, because that is where
// the authorization codes must be minted (an in-process call the jail cannot
// reach). This forwarder is what makes both true at once: it listens on the
// guest's loopback at the provider's port and carries the bytes to the host.
//
// It carries NO logic beyond forwarding. It does not parse HTTP, does not
// know what OIDC is, holds no secret, and makes no decision about any byte it
// copies. The one thing it refuses is a non-loopback listen address, which is
// a fail-closed guard on its own configuration rather than a decision about
// traffic: bound to a routable address it would publish the provider's
// endpoints to the guest's network instead of its loopback.
//
// What it necessarily does expose: agent containers reach guest loopback
// (lever configures pasta --map-host-loopback 169.254.1.2 so they can reach
// the VM-loopback hub), so an agent can reach the provider's discovery,
// /token and /userinfo endpoints through this forwarder. That is the accepted
// residual of this design and it is bounded: none of those three endpoints
// yields anything without an authorization code, and no endpoint anywhere
// mints one.
//
// Built and installed by internal/backend/guest.EnsureHubLogin, which
// cross-compiles this file for the guest's architecture. Stdlib only, on
// purpose: it is compiled from a copy of its own source at apply time.
package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"time"
)

// dialTimeout bounds reaching the host. The host side is a loopback listener
// one hop away through the VM's host interface; anything slower than this is a
// failure, not latency.
const dialTimeout = 10 * time.Second

func main() {
	listen := flag.String("listen", "", "guest loopback address to listen on, e.g. 127.0.0.1:8446")
	target := flag.String("target", "", "host address to forward to, e.g. host.orb.internal:8446")
	flag.Parse()

	if *listen == "" || *target == "" {
		fail("both -listen and -target are required")
	}
	host, _, err := net.SplitHostPort(*listen)
	if err != nil {
		fail("bad -listen address %q: %v", *listen, err)
	}
	// Fail closed off loopback: see the package comment.
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		fail("-listen must be a loopback address, got %q", *listen)
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fail("listen on %s: %v", *listen, err)
	}
	fmt.Fprintf(os.Stderr, "lever-login-forward: %s -> %s\n", *listen, *target)
	for {
		c, err := ln.Accept()
		if err != nil {
			fail("accept: %v", err)
		}
		go forward(c, *target)
	}
}

// forward copies bytes both ways between an accepted connection and a fresh
// connection to the target, and ends when both directions are done.
func forward(down net.Conn, target string) {
	defer func() { _ = down.Close() }()
	up, err := net.DialTimeout("tcp", target, dialTimeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lever-login-forward: dial %s: %v\n", target, err)
		return
	}
	defer func() { _ = up.Close() }()

	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		// Half-close so the far end sees the end of the request body instead
		// of waiting for a connection close that only comes later.
		if tc, ok := dst.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		done <- struct{}{}
	}
	go pipe(up, down)
	go pipe(down, up)
	<-done
	<-done
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "lever-login-forward: "+format+"\n", args...)
	os.Exit(1)
}
