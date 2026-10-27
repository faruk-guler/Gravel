package app

import (
	"bytes"
	"crypto/ed25519"
	crand "crypto/rand"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"gravel/internal/sessionstore"
	"gravel/internal/sftpclient"
	"gravel/internal/sshclient"
)

// startEchoServer runs a minimal SSH server that authenticates by password and
// echoes the PTY byte stream, returning its listen address.
func startEchoServer(t *testing.T) string {
	t.Helper()
	return startServer(t, "")
}

// startServer runs a minimal SSH server. When exitOn is non-empty the fake
// shell closes the PTY channel as soon as it sees that text, which is how a
// remote "exit"/logout tears the session down.
func startServer(t *testing.T, exitOn string) string {
	t.Helper()
	_, hostPriv, err := ed25519.GenerateKey(crand.Reader)
	if err != nil {
		t.Fatalf("host key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if string(pass) == "pw" {
				return nil, nil
			}
			return nil, fmt.Errorf("denied")
		},
	}
	cfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go handle(nc, cfg, exitOn)
		}
	}()
	return ln.Addr().String()
}

func handle(nc net.Conn, cfg *ssh.ServerConfig, exitOn string) {
	sc, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		return
	}
	defer sc.Close()
	go ssh.DiscardRequests(reqs)
	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			newChan.Reject(ssh.UnknownChannelType, "no")
			continue
		}
		ch, requests, err := newChan.Accept()
		if err != nil {
			continue
		}
		go func(in <-chan *ssh.Request) {
			for req := range in {
				switch req.Type {
				case "pty-req", "env":
					if req.WantReply {
						req.Reply(true, nil)
					}
				case "shell":
					if req.WantReply {
						req.Reply(true, nil)
					}
					// The echo only starts for a PTY channel: a subsystem channel
					// carries SFTP frames, and writing a prompt into it would corrupt
					// the protocol.
					go echoChannel(ch, exitOn)
				case "subsystem":
					if bytes.Contains(req.Payload, []byte("sftp")) {
						req.Reply(true, nil)
						srv, err := sftp.NewServer(ch)
						if err != nil {
							return
						}
						_ = srv.Serve()
						ch.Close()
						return
					}
					if req.WantReply {
						req.Reply(false, nil)
					}
				default:
					if req.WantReply {
						req.Reply(false, nil)
					}
				}
			}
		}(requests)
	}
}

// echoChannel writes the login prompt and then reflects the stream back, which
// is enough for the terminal model tests. With exitOn set it closes the channel
// as soon as that text arrives, like a remote logout.
func echoChannel(ch ssh.Channel, exitOn string) {
	fmt.Fprint(ch, "welcome$ ")
	if exitOn == "" {
		io.Copy(ch, ch)
		ch.Close()
		return
	}
	buf := make([]byte, 256)
	var seen strings.Builder
	for {
		n, err := ch.Read(buf)
		if n > 0 {
			ch.Write(buf[:n])
			seen.Write(buf[:n])
			if strings.Contains(seen.String(), exitOn) {
				fmt.Fprint(ch, "\r\nlogout\r\n")
				ch.Close()
				return
			}
		}
		if err != nil {
			ch.Close()
			return
		}
	}
}

func newController(t *testing.T, addr string, onOutput func()) *Controller {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	var port int
	fmt.Sscanf(portStr, "%d", &port)
	prof := sessionstore.Profile{Host: host, Port: port, User: "root", Auth: sessionstore.AuthPassword}
	kh := sshclient.NewKnownHosts(t.TempDir())
	if err := kh.Load(); err != nil {
		t.Fatalf("kh load: %v", err)
	}
	return New(prof, sshclient.Credential{Password: "pw"}, kh, onOutput)
}

func TestControllerConnectAndEcho(t *testing.T) {
	addr := startEchoServer(t)

	var updates atomic.Int64
	c := newController(t, addr, func() { updates.Add(1) })
	defer c.Close()

	if err := c.Connect(80, 24); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if c.State() != StateConnected {
		t.Fatalf("state = %v, want connected", c.State())
	}

	// The prompt should render into the terminal model.
	if err := waitForText(c, "welcome$", 3*time.Second); err != nil {
		t.Fatalf("wait prompt: %v", err)
	}

	// Send a command; the fake PTY echoes it back, so it must appear on screen.
	if err := c.WriteInput([]byte("ping\r")); err != nil {
		t.Fatalf("write input: %v", err)
	}
	if err := waitForText(c, "ping", 3*time.Second); err != nil {
		t.Fatalf("wait echo: %v", err)
	}
	if updates.Load() == 0 {
		t.Fatal("expected at least one onOutput notification")
	}

	// Resize should update the snapshot dimensions.
	c.Resize(100, 30)
	snap, ok := c.Snapshot()
	if !ok {
		t.Fatal("expected snapshot")
	}
	if snap.Cols != 100 || snap.Rows != 30 {
		t.Fatalf("snapshot size = %dx%d, want 100x30", snap.Cols, snap.Rows)
	}
}

func TestControllerConnectFailure(t *testing.T) {
	addr := startEchoServer(t)
	c := newController(t, addr, nil)
	// Override the credential to force an auth failure.
	c.cred = sshclient.Credential{Password: "wrong"}
	err := c.Connect(80, 24)
	if err == nil {
		t.Fatal("expected connect error")
	}
	if c.State() != StateFailed {
		t.Fatalf("state = %v, want failed", c.State())
	}
	if c.Err() == nil {
		t.Fatal("expected recorded error")
	}
}

// TestControllerFiresExitOnLogout proves the shell ending on its own is
// announced: without it the UI keeps showing a tab whose transport is dead.
func TestControllerFiresExitOnLogout(t *testing.T) {
	addr := startServer(t, "exit")
	c := newController(t, addr, nil)
	defer c.Close()

	exited := make(chan struct{}, 1)
	c.OnExit(func() {
		select {
		case exited <- struct{}{}:
		default:
		}
	})

	if err := c.Connect(80, 24); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := waitForText(c, "welcome$", 3*time.Second); err != nil {
		t.Fatalf("wait prompt: %v", err)
	}
	if err := c.WriteInput([]byte("exit\r")); err != nil {
		t.Fatalf("write exit: %v", err)
	}

	select {
	case <-exited:
	case <-time.After(3 * time.Second):
		t.Fatal("OnExit was not called after the remote shell logged out")
	}
	if st := c.State(); st != StateDisconnected {
		t.Fatalf("state = %v, want disconnected", st)
	}
}

// TestControllerNoExitOnClose guards the other half: a shutdown the app asked
// for must not be reported as a surprise logout.
func TestControllerNoExitOnClose(t *testing.T) {
	addr := startEchoServer(t)
	c := newController(t, addr, nil)

	exited := make(chan struct{}, 1)
	c.OnExit(func() {
		select {
		case exited <- struct{}{}:
		default:
		}
	})

	if err := c.Connect(80, 24); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := waitForText(c, "welcome$", 3*time.Second); err != nil {
		t.Fatalf("wait prompt: %v", err)
	}
	c.Close()

	select {
	case <-exited:
		t.Fatal("OnExit must not fire for a shutdown requested through Close")
	case <-time.After(300 * time.Millisecond):
	}
}

// TestSFTPDoubleOpenYieldsOneSession guards the lazy SFTP handshake, which runs
// with the controller lock released so a slow server cannot block Resize or
// WriteInput. That gap is exactly where two callers can overlap: file browsing
// and a folder download both ask for the session. They must end up on the single
// installed client - a second live SFTP channel means a second set of handles,
// and only one of them is ever closed.
func TestSFTPDoubleOpenYieldsOneSession(t *testing.T) {
	addr := startEchoServer(t)
	c := newController(t, addr, nil)
	defer c.Close()

	if err := c.Connect(80, 24); err != nil {
		t.Fatalf("connect: %v", err)
	}

	const racers = 8
	start := make(chan struct{})
	type outcome struct {
		cli *sftpclient.Client
		err error
	}
	results := make(chan outcome, racers)
	for i := 0; i < racers; i++ {
		go func() {
			<-start
			cli, err := c.SFTP()
			results <- outcome{cli, err}
		}()
	}
	close(start)

	var first *sftpclient.Client
	for i := 0; i < racers; i++ {
		r := <-results
		if r.err != nil {
			t.Fatalf("SFTP(): %v", r.err)
		}
		if r.cli == nil {
			t.Fatal("SFTP() returned no client")
		}
		if first == nil {
			first = r.cli
			continue
		}
		if r.cli != first {
			t.Fatal("two callers were handed different SFTP sessions")
		}
	}

	// Once installed, later calls reuse it instead of negotiating again.
	again, err := c.SFTP()
	if err != nil || again != first {
		t.Fatalf("second SFTP() = %p/%v, want the installed session %p", again, err, first)
	}
}

func waitForText(c *Controller, substr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(c.PlainText(), substr) {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %q; screen=%q", substr, c.PlainText())
}
