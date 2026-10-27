package sshclient

import (
	"crypto/ed25519"
	crand "crypto/rand"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"gravel/internal/sessionstore"
)

const testPassword = "s3cr3t-pw"

// startTestServer runs an in-process SSH server that authenticates by password
// and echoes anything sent to its PTY shell. It returns the listen address.
func startTestServer(t *testing.T) string {
	t.Helper()

	_, hostPriv, err := ed25519.GenerateKey(crand.Reader)
	if err != nil {
		t.Fatalf("host key: %v", err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatalf("host signer: %v", err)
	}

	cfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if c.User() == "root" && string(pass) == testPassword {
				return nil, nil
			}
			return nil, fmt.Errorf("auth denied")
		},
	}
	cfg.AddHostKey(hostSigner)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			nConn, err := ln.Accept()
			if err != nil {
				return // listener closed
			}
			go handleConn(nConn, cfg)
		}
	}()
	return ln.Addr().String()
}

func handleConn(nConn net.Conn, cfg *ssh.ServerConfig) {
	sshConn, chans, reqs, err := ssh.NewServerConn(nConn, cfg)
	if err != nil {
		return
	}
	defer sshConn.Close()
	go ssh.DiscardRequests(reqs)

	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			newChan.Reject(ssh.UnknownChannelType, "only sessions supported")
			continue
		}
		ch, requests, err := newChan.Accept()
		if err != nil {
			continue
		}
		go func(in <-chan *ssh.Request) {
			for req := range in {
				switch req.Type {
				case "pty-req", "shell", "env":
					if req.WantReply {
						req.Reply(true, nil)
					}
				default:
					if req.WantReply {
						req.Reply(false, nil)
					}
				}
			}
		}(requests)
		go func() {
			// Prompt once, then echo the byte stream back.
			fmt.Fprint(ch, "gravel-test$ ")
			io.Copy(ch, ch)
			ch.Close()
		}()
	}
}

// TestAuthFailedClassifiesRealServerRejection pins the wording that the retry
// loop depends on: the error must come from an actual handshake against a
// server that denied the password, not from a hand-written string.
func TestAuthFailedClassifiesRealServerRejection(t *testing.T) {
	addr := startTestServer(t)
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	var port int
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		t.Fatalf("parse port: %v", err)
	}

	kh := NewKnownHosts(t.TempDir())
	if err := kh.Load(); err != nil {
		t.Fatalf("kh load: %v", err)
	}
	prof := sessionstore.Profile{Host: host, Port: port, User: "root", Auth: sessionstore.AuthPassword}

	if _, err := Dial(prof, Credential{Password: "not-the-password"}, kh); err == nil {
		t.Fatal("expected the wrong password to be rejected")
	} else if !AuthFailed(err) {
		t.Fatalf("AuthFailed = false for a rejected password; err=%v", err)
	}

	// The same server must not classify a good password as an auth failure.
	if client, err := Dial(prof, Credential{Password: testPassword}, kh); err != nil {
		t.Fatalf("dial with correct password: %v", err)
	} else {
		_ = client.Close()
	}
}

// TestAuthFailedIgnoresTransportErrors keeps a refused connection from being
// mistaken for a bad credential, which would prompt the user for nothing.
func TestAuthFailedIgnoresTransportErrors(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	var port int
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		t.Fatalf("parse port: %v", err)
	}
	ln.Close() // nothing is listening any more

	kh := NewKnownHosts(t.TempDir())
	if err := kh.Load(); err != nil {
		t.Fatalf("kh load: %v", err)
	}
	prof := sessionstore.Profile{Host: host, Port: port, User: "root", Auth: sessionstore.AuthPassword}

	if _, err := Dial(prof, Credential{Password: testPassword}, kh); err == nil {
		t.Fatal("expected a connection failure")
	} else if AuthFailed(err) {
		t.Fatalf("AuthFailed = true for a transport error; err=%v", err)
	}

	if AuthFailed(nil) {
		t.Fatal("AuthFailed(nil) must be false")
	}
	if !AuthFailed(fmt.Errorf("sshclient: dial x: ssh: unable to authenticate, attempted methods [none password]")) {
		t.Fatal("AuthFailed must match the library's rejection message")
	}
}

func TestDialAndShellEcho(t *testing.T) {
	addr := startTestServer(t)
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	var port int
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		t.Fatalf("parse port: %v", err)
	}

	// A wrong password must fail authentication.
	badProf := sessionstore.Profile{Host: host, Port: port, User: "root", Auth: sessionstore.AuthPassword}
	khBad := NewKnownHosts(t.TempDir())
	khBad.Load()
	if _, err := Dial(badProf, Credential{Password: "wrong"}, khBad); err == nil {
		t.Fatal("expected auth failure with wrong password")
	}

	dir := t.TempDir()
	kh := NewKnownHosts(dir)
	if err := kh.Load(); err != nil {
		t.Fatalf("kh load: %v", err)
	}
	prof := sessionstore.Profile{Host: host, Port: port, User: "root", Auth: sessionstore.AuthPassword}

	client, err := Dial(prof, Credential{Password: testPassword}, kh)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	shell, err := StartShell(client, "xterm", 80, 24)
	if err != nil {
		t.Fatalf("start shell: %v", err)
	}
	defer shell.Close()

	if err := shell.Resize(100, 30); err != nil {
		t.Fatalf("resize: %v", err)
	}

	payload := "hello-gravel\r"
	if _, err := shell.WriteInput([]byte(payload)); err != nil {
		t.Fatalf("write input: %v", err)
	}

	// Read echoed output with a deadline so a hang fails the test clearly.
	type result struct {
		s   string
		err error
	}
	done := make(chan result, 1)
	go func() {
		buf := make([]byte, 256)
		var acc strings.Builder
		for {
			n, err := shell.Output().Read(buf)
			acc.Write(buf[:n])
			if strings.Contains(acc.String(), "hello-gravel") {
				done <- result{acc.String(), nil}
				return
			}
			if err != nil {
				done <- result{acc.String(), err}
				return
			}
		}
	}()

	select {
	case r := <-done:
		if r.err != nil && r.err != io.EOF {
			t.Fatalf("read output: %v", r.err)
		}
		if !strings.Contains(r.s, "hello-gravel") {
			t.Fatalf("expected echoed output to contain payload, got %q", r.s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for echoed PTY output")
	}

	// The TOFU store should have enrolled the test host.
	khReload := NewKnownHosts(dir)
	if err := khReload.Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(khReload.hosts) != 1 {
		t.Fatalf("expected 1 enrolled host, got %d", len(khReload.hosts))
	}
}
