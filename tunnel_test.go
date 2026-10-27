package main

import (
	"context"
	"errors"
	"net"
	"runtime/debug"
	"sync"
	"testing"
	"time"

	"gravel/internal/app"
	"gravel/internal/sessionstore"
	"gravel/internal/sshclient"
)

// Stopped tunnels hold a nil listener. These tests guard two regressions:
// CloseSession / shutdown / CloseTunnel calling Close on that nil interface and
// panicking, and Stop deleting the definition so the user had to retype it.

func newTunnelApp(t *testing.T) (*App, *tunnel, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	port := ln.Addr().(*net.TCPAddr).Port
	a := NewApp()
	tr := &tunnel{ID: "t1", SessionID: "s1", Name: "demo", LocalPort: port, RemoteHost: "db.internal", RemotePort: 5432, listener: ln}
	a.tunnels[tr.ID] = tr
	return a, tr, port
}

func TestStopTunnelKeepsTheDefinition(t *testing.T) {
	a, tr, _ := newTunnelApp(t)

	if !tr.running() {
		t.Fatal("precondition: tunnel should start out running")
	}
	if err := a.StopTunnel(tr.ID); err != nil {
		t.Fatalf("StopTunnel: %v", err)
	}
	if _, ok := a.tunnels[tr.ID]; !ok {
		t.Fatal("StopTunnel must keep the definition so the row survives")
	}
	if tr.running() {
		t.Fatal("tunnel should report stopped")
	}
	if err := a.StopTunnel(tr.ID); err != nil {
		t.Fatalf("second StopTunnel: %v", err)
	}

	list := a.ListTunnels()
	if len(list) != 1 {
		t.Fatalf("ListTunnels returned %d entries, want 1", len(list))
	}
	if list[0].Running {
		t.Fatal("ListTunnels should report the tunnel as stopped")
	}
	if list[0].LocalPort != tr.LocalPort || list[0].RemoteHost != "db.internal" || list[0].RemotePort != 5432 {
		t.Fatalf("stopped tunnel lost its parameters: %+v", list[0])
	}
}

func TestCloseTunnelOnStoppedTunnelDoesNotPanic(t *testing.T) {
	a, tr, _ := newTunnelApp(t)
	if err := a.StopTunnel(tr.ID); err != nil {
		t.Fatalf("StopTunnel: %v", err)
	}
	if err := a.CloseTunnel(tr.ID); err != nil {
		t.Fatalf("CloseTunnel on a stopped tunnel: %v", err)
	}
	if _, ok := a.tunnels[tr.ID]; ok {
		t.Fatal("CloseTunnel must remove the definition")
	}
	if err := a.CloseTunnel(tr.ID); err == nil {
		t.Fatal("CloseTunnel on a missing id should report an error")
	}
}

func TestCloseSessionClosesStoppedTunnels(t *testing.T) {
	a, tr, _ := newTunnelApp(t)
	if err := a.StopTunnel(tr.ID); err != nil {
		t.Fatalf("StopTunnel: %v", err)
	}
	sid := tr.SessionID
	// CloseSession calls Close on the controller, so the fixture needs a real
	// one; every session created by dialAndOpen has one.
	ctrl := app.New(sessionstore.Profile{}, sshclient.Credential{}, nil, nil)
	a.sessions[sid] = &appSession{ID: sid, Ctrl: ctrl}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("CloseSession panicked on a stopped tunnel: %v\n%s", r, debug.Stack())
		}
	}()
	a.CloseSession(sid)
	if len(a.ListTunnels()) != 0 {
		t.Fatal("closing the session should drop its tunnel definitions")
	}
}

func TestStopAllTunnelsKeepsDefinitions(t *testing.T) {
	a, tr, _ := newTunnelApp(t)
	if stopped := a.StopAllTunnels(tr.SessionID); stopped != 1 {
		t.Fatalf("StopAllTunnels stopped %d, want 1", stopped)
	}
	if len(a.ListTunnels()) != 1 {
		t.Fatal("StopAllTunnels must keep the definitions")
	}
	if again := a.StopAllTunnels(tr.SessionID); again != 0 {
		t.Fatalf("second StopAllTunnels = %d, want 0", again)
	}
	if n := a.StopAllTunnels("nope"); n != 0 {
		t.Fatalf("StopAllTunnels for unknown session = %d, want 0", n)
	}
}

func TestStartAllTunnelsWithoutSessionIsNoop(t *testing.T) {
	a, tr, _ := newTunnelApp(t)
	if err := a.StopTunnel(tr.ID); err != nil {
		t.Fatalf("StopTunnel: %v", err)
	}
	// The session behind the definition does not exist, so nothing can rebind.
	// The call must report zero started rather than panic, and keep the row.
	if n := a.StartAllTunnels(tr.SessionID); n != 0 {
		t.Fatalf("StartAllTunnels with no live session = %d, want 0", n)
	}
	if _, ok := a.tunnels[tr.ID]; !ok {
		t.Fatal("a failed start must still keep the definition")
	}
}

// TestUpdateTunnelRejectsBadPortWithoutBreakingAWorkingForward pins the order of
// operations in UpdateTunnel: an invalid edit has to fail while the forward that
// is carrying traffic stays up. Validating only after StopTunnel used to leave
// the user with a stopped tunnel, changed settings and no replacement.
func TestUpdateTunnelRejectsBadPortWithoutBreakingAWorkingForward(t *testing.T) {
	a, tr, port := newTunnelApp(t)

	cases := []struct {
		desc   string
		local  int
		remote int
	}{
		{"zero local port", 0, tr.RemotePort},
		{"local port above the range", 70000, tr.RemotePort},
		{"negative local port", -1, tr.RemotePort},
		{"zero remote port on a local forward", port, 0},
	}
	for _, c := range cases {
		if _, err := a.UpdateTunnel(tr.ID, "demo", c.local, tr.RemoteHost, c.remote, "local"); err == nil {
			t.Fatalf("%s: UpdateTunnel should report an error", c.desc)
		}
		if !tr.running() {
			t.Fatalf("%s: a rejected edit stopped the working forward", c.desc)
		}
		if tr.LocalPort != port || tr.RemotePort != 5432 {
			t.Fatalf("%s: a rejected edit rewrote the definition: %+v", c.desc, tr)
		}
	}
}

// blockingDialer never answers: it returns only once the caller's context is
// done, which is how a target that swallows packets rather than refusing them
// behaves behind the SSH link.
type blockingDialer struct {
	entered chan struct{}
	once    sync.Once
}

func (d *blockingDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	d.once.Do(func() { close(d.entered) })
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestDialBoundedGivesUpOnAStuckTarget: both -L and -D forwards can be bound to
// the LAN, so every accepted client that aims at a black-holed destination used
// to park a goroutine - and an SSH channel request - for the lifetime of the
// session.
func TestDialBoundedGivesUpOnAStuckTarget(t *testing.T) {
	old := tunnelDialTimeout
	tunnelDialTimeout = 80 * time.Millisecond
	defer func() { tunnelDialTimeout = old }()

	d := &blockingDialer{entered: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := dialBounded(d, "tcp", "203.0.113.1:22")
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("dialBounded returned %v, want a deadline error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dialBounded never gave up on the stuck target")
	}
	select {
	case <-d.entered:
	default:
		t.Fatal("the dial was never attempted")
	}
}

// TestDialBoundedPassesThroughAFastTarget keeps the other side of that contract:
// the helper must add a budget, not behaviour - the target's own error still
// reaches the caller untouched.
func TestDialBoundedPassesThroughAFastTarget(t *testing.T) {
	want := errors.New("connection refused by the target")
	d := funcDialer{fn: func(ctx context.Context, network, addr string) (net.Conn, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("dialBounded must put a deadline on the context")
		}
		if network != "tcp" || addr != "db.internal:5432" {
			t.Errorf("dial args = %q %q, want tcp db.internal:5432", network, addr)
		}
		return nil, want
	}}

	if _, err := dialBounded(d, "tcp", "db.internal:5432"); !errors.Is(err, want) {
		t.Fatalf("dialBounded error = %v, want the target's own %v", err, want)
	}
}

// funcDialer adapts a plain function to the sshDialer interface.
type funcDialer struct {
	fn func(ctx context.Context, network, addr string) (net.Conn, error)
}

func (d funcDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return d.fn(ctx, network, addr)
}

func TestValidPortAcceptsRealPorts(t *testing.T) {
	for _, p := range []int{1, 22, 1080, 65535} {
		if err := validPort("local", p); err != nil {
			t.Fatalf("validPort(%d) = %v, want nil", p, err)
		}
	}
	for _, p := range []int{-1, 0, 65536} {
		if err := validPort("local", p); err == nil {
			t.Fatalf("validPort(%d) = nil, want an error", p)
		}
	}
}

func TestUpdateTunnelUnknownIDFails(t *testing.T) {
	a, _, _ := newTunnelApp(t)
	if _, err := a.UpdateTunnel("missing", "x", 8080, "h", 80, "local"); err == nil {
		t.Fatal("editing a tunnel that does not exist should fail")
	}
}

func TestTunnelAddressesDerivedFromParts(t *testing.T) {
	tr := &tunnel{LocalPort: 3131, RemoteHost: "192.168.45.44", RemotePort: 75}
	if got, want := tr.localAddr(), "127.0.0.1:3131"; got != want {
		t.Fatalf("localAddr() = %q, want %q", got, want)
	}
	if info := tunnelInfo(tr); info.Bind != "local" {
		t.Fatalf("default bind = %q, want local", info.Bind)
	}
	// A LAN-bound tunnel listens on every interface and says so in its row.
	lan := &tunnel{LocalPort: 1080, bindAll: true, dynamic: true}
	if got, want := lan.localAddr(), "0.0.0.0:1080"; got != want {
		t.Fatalf("LAN localAddr() = %q, want %q", got, want)
	}
	if info := tunnelInfo(lan); info.Bind != "lan" {
		t.Fatalf("lan bind = %q, want lan", info.Bind)
	}
	if got, want := lan.remoteAddr(), ""; got != want {
		t.Fatalf("dynamic remoteAddr() = %q, want %q", got, want)
	}
	if got, want := tr.remoteAddr(), "192.168.45.44:75"; got != want {
		t.Fatalf("remoteAddr() = %q, want %q", got, want)
	}
	info := tunnelInfo(tr)
	if info.LocalPort != 3131 || info.RemoteHost != "192.168.45.44" || info.RemotePort != 75 {
		t.Fatalf("TunnelInfo lost the parts: %+v", info)
	}
	if info.Type != tunnelTypeLocal {
		t.Fatalf("TunnelInfo.Type = %q, want %q", info.Type, tunnelTypeLocal)
	}
	if info.Running {
		t.Fatal("a tunnel with no listener must not report running")
	}
}
