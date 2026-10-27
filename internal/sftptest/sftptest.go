// Package sftptest runs an in-process SSH server whose "sftp" subsystem is
// backed by pkg/sftp serving the real OS filesystem. Transfer and file code can
// then be tested against the same Seek/Append/rename behaviour it would meet on
// a live server, without a network.
//
// It exists so more than one package can exercise that behaviour; the harness
// inside internal/sftpclient's own tests predates it and is left as it is.
package sftptest

import (
	"bytes"
	"crypto/ed25519"
	crand "crypto/rand"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// NewClient returns an SFTP client talking to a server rooted on this machine's
// filesystem, plus a scratch directory (slash-separated) the caller may use for
// its remote-side files. Everything is closed when the test finishes.
func NewClient(t *testing.T) (client *sftp.Client, root string) {
	t.Helper()

	sshConn := dialServer(t)
	client, err := sftp.NewClient(sshConn)
	if err != nil {
		t.Fatalf("sftp client: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	return client, filepath.ToSlash(t.TempDir())
}

// dialServer starts the listening side and returns an authenticated ssh.Client.
func dialServer(t *testing.T) *ssh.Client {
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
			if c.User() == "root" && string(pass) == "pw" {
				return nil, nil
			}
			return nil, fmt.Errorf("denied")
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
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go serveSession(nc, cfg)
		}
	}()

	client, err := ssh.Dial("tcp", ln.Addr().String(), &ssh.ClientConfig{
		User:            "root",
		Auth:            []ssh.AuthMethod{ssh.Password("pw")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatalf("dial test server: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

func serveSession(nc net.Conn, cfg *ssh.ServerConfig) {
	sshConn, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		return
	}
	defer sshConn.Close()
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
					req.Reply(false, nil)
				default:
					if req.WantReply {
						req.Reply(false, nil)
					}
				}
			}
		}(requests)
	}
}
