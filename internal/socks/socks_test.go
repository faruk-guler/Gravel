package socks

import (
	"io"
	"net"
	"strconv"
	"testing"
	"time"
)

// TestServeRelaysConnect walks a client through the full handshake against an
// in-memory Serve and proves bytes flow to the dialled target and back.
func TestServeRelaysConnect(t *testing.T) {
	// Echo target Serve will dial through the injected Dial.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_, _ = io.Copy(c, c)
		_ = c.Close()
	}()

	client, server := net.Pipe()
	defer client.Close()
	errc := make(chan error, 1)
	go func() { errc <- Serve(server, net.Dial) }()

	host, port := splitAddr(t, ln.Addr().String())

	// Greeting: offer no-auth, expect acceptance.
	if _, err := client.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		t.Fatalf("greeting: %v", err)
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(client, resp); err != nil {
		t.Fatalf("greeting reply: %v", err)
	}
	if resp[0] != 0x05 || resp[1] != 0x00 {
		t.Fatalf("greeting reply = %v, want [5 0]", resp)
	}

	// CONNECT request with an IPv4 target.
	req := []byte{0x05, 0x01, 0x00, 0x01, 127, 0, 0, 1, 0, 0}
	p := uint16(port)
	req[8], req[9] = byte(p>>8), byte(p)
	_ = host
	if _, err := client.Write(req); err != nil {
		t.Fatalf("connect: %v", err)
	}
	replyBuf := make([]byte, 10)
	if _, err := io.ReadFull(client, replyBuf); err != nil {
		t.Fatalf("connect reply: %v", err)
	}
	if replyBuf[1] != 0x00 {
		t.Fatalf("connect status = %d, want granted", replyBuf[1])
	}

	// Relay: what we send must come back from the echo target.
	_ = client.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := client.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(client, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "ping" {
		t.Fatalf("relay = %q, want ping", got)
	}
	_ = client.Close()
	if err := <-errc; err != nil {
		t.Fatalf("Serve returned error: %v", err)
	}
}

// TestServeRejectsAuthOnlyClient checks a client that offers only a non-zero
// auth method gets 0xFF and the connection is dropped.
func TestServeRejectsAuthOnlyClient(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	go func() { _ = Serve(server, net.Dial) }()

	if _, err := client.Write([]byte{0x05, 0x01, 0x02}); err != nil {
		t.Fatalf("greeting: %v", err)
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(client, resp); err != nil {
		t.Fatalf("reply: %v", err)
	}
	if resp[0] != 0x05 || resp[1] != 0xff {
		t.Fatalf("reply = %v, want [5 255]", resp)
	}
}

// TestServeDropsStalledClient guards the -D tunnel against a peer that connects
// and never speaks. The listener can be bound to the LAN, so an accepted socket
// stuck in the handshake would otherwise hold a goroutine - and the SSH session
// behind it - open for as long as the client pleases.
func TestServeDropsStalledClient(t *testing.T) {
	old := handshakeTimeout
	handshakeTimeout = 40 * time.Millisecond
	defer func() { handshakeTimeout = old }()

	client, server := net.Pipe()
	defer client.Close()

	done := make(chan error, 1)
	go func() { done <- Serve(server, net.Dial) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Serve should report the abandoned handshake, not return cleanly")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve kept waiting for a client that never sent a greeting")
	}
}

// TestSlowDialIsNotCutOffByTheHandshakeDeadline: the guard is there for clients
// that never speak, not for targets that are slow behind the SSH link. Clearing
// the deadline once the request is parsed is what stops a successful CONNECT
// from being discarded - and its freshly dialled connection closed - mid-reply.
func TestSlowDialIsNotCutOffByTheHandshakeDeadline(t *testing.T) {
	old := handshakeTimeout
	handshakeTimeout = 50 * time.Millisecond
	defer func() { handshakeTimeout = old }()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_, _ = io.Copy(c, c)
		_ = c.Close()
	}()
	_, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("port: %v", err)
	}

	slowDial := func(network, addr string) (net.Conn, error) {
		time.Sleep(4 * handshakeTimeout) // clearly past the handshake budget
		return net.Dial(network, addr)
	}

	client, server := net.Pipe()
	defer client.Close()
	go func() { _ = Serve(server, slowDial) }()

	if _, err := client.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		t.Fatalf("greeting: %v", err)
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(client, resp); err != nil {
		t.Fatalf("greeting reply: %v", err)
	}
	req := []byte{0x05, 0x01, 0x00, 0x01, 127, 0, 0, 1, 0, 0}
	req[8], req[9] = byte(port>>8), byte(port)
	if _, err := client.Write(req); err != nil {
		t.Fatalf("connect: %v", err)
	}
	_ = client.SetDeadline(time.Now().Add(5 * time.Second))
	replyBuf := make([]byte, 10)
	if _, err := io.ReadFull(client, replyBuf); err != nil {
		t.Fatalf("a slow but valid dial was cut off: %v", err)
	}
	if replyBuf[1] != 0x00 {
		t.Fatalf("connect status = %d, want granted", replyBuf[1])
	}
}

func splitAddr(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split %s: %v", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("port %s: %v", portStr, err)
	}
	return host, port
}
