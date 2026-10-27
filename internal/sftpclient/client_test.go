package sftpclient

import (
	"bytes"
	"crypto/ed25519"
	crand "crypto/rand"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// startSFTPServer runs an in-process SSH server whose "sftp" subsystem is backed
// by an SFTP server operating on the real OS filesystem. It returns a dialed
// *ssh.Client ready for sftpclient.New.
func startSFTPServer(t *testing.T) *ssh.Client {
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
			go serveSFTPSession(nc, cfg)
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

func serveSFTPSession(nc net.Conn, cfg *ssh.ServerConfig) {
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

func TestTouchCreatesEmptyFileAndRefusesOverwrite(t *testing.T) {
	root := filepath.ToSlash(t.TempDir())
	client := startSFTPServer(t)
	c, err := New(client)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	p := root + "/fresh.txt"
	if err := c.Touch(p); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	fi, err := c.Stat(p)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if fi.Size != 0 {
		t.Fatalf("new file size = %d, want 0", fi.Size)
	}
	if fi.IsDir {
		t.Fatal("new file must not be a directory")
	}

	// A second Touch has to fail rather than quietly truncate what is there.
	if err := c.Touch(p); err == nil {
		t.Fatal("Touch on an existing path must fail")
	}
	if _, err := c.Stat(p); err != nil {
		t.Fatalf("file disappeared after a refused Touch: %v", err)
	}
}

// TestWalkCollectsNestedFilesAndHonoursLimit covers the folder-download scan:
// every file below the root must appear exactly once with its full path, and a
// tree larger than the limit must fail rather than return a truncated list that
// would silently download only part of a folder.
func TestWalkCollectsNestedFilesAndHonoursLimit(t *testing.T) {
	root := filepath.ToSlash(t.TempDir())
	put := func(rel string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	put("top.txt")
	put("a/one.txt")
	put("a/b/two.txt")
	put("c/three.txt")

	client := startSFTPServer(t)
	c, err := New(client)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	got, err := c.Walk(root, 100)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	want := map[string]bool{
		root + "/top.txt": true, root + "/a/one.txt": true,
		root + "/a/b/two.txt": true, root + "/c/three.txt": true,
	}
	if len(got) != len(want) {
		t.Fatalf("Walk returned %d paths, want %d: %v", len(got), len(want), got)
	}
	for _, p := range got {
		if !want[p] {
			t.Fatalf("Walk returned unexpected path %q", p)
		}
	}
	// Sorted output keeps the transfer queue in a predictable order.
	for i := 1; i < len(got); i++ {
		if got[i-1] > got[i] {
			t.Fatalf("Walk output not sorted: %q before %q", got[i-1], got[i])
		}
	}

	if _, err := c.Walk(root, 2); err == nil {
		t.Fatal("Walk must fail when the tree exceeds the limit")
	}

	// An empty directory is a valid, empty result - not an error.
	empty := filepath.ToSlash(t.TempDir())
	none, err := c.Walk(empty, 10)
	if err != nil {
		t.Fatalf("Walk on empty dir: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("Walk on empty dir returned %v", none)
	}
}

func TestSFTPOperations(t *testing.T) {
	root := t.TempDir()
	rootSlash := filepath.ToSlash(root)

	// Seed a couple of files via the local FS so List has something to see.
	if err := os.WriteFile(filepath.Join(root, "alpha.txt"), []byte("aaa"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	client := startSFTPServer(t)
	c, err := New(client)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	// List
	entries, err := c.List(rootSlash)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "alpha.txt" {
		t.Fatalf("unexpected list: %+v", entries)
	}

	// Upload + progress
	localUpload := filepath.Join(t.TempDir(), "up.txt")
	content := strings.Repeat("x", 1000)
	if err := os.WriteFile(localUpload, []byte(content), 0o644); err != nil {
		t.Fatalf("write local: %v", err)
	}
	remoteUpload := rootSlash + "/up.txt"
	var lastDone int64
	var seenTotal int64
	if err := c.Upload(localUpload, remoteUpload, func(done, total int64) {
		lastDone = done
		seenTotal = total
	}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if lastDone != int64(len(content)) {
		t.Fatalf("upload progress done = %d, want %d", lastDone, len(content))
	}
	if seenTotal != int64(len(content)) {
		t.Fatalf("upload progress total = %d, want %d", seenTotal, len(content))
	}

	// Mkdir + nested file + recursive Remove
	sub := rootSlash + "/sub"
	if err := c.Mkdir(sub); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := c.Upload(localUpload, sub+"/nested.txt", nil); err != nil {
		t.Fatalf("upload nested: %v", err)
	}

	// Rename
	moved := rootSlash + "/moved.txt"
	if err := c.Rename(remoteUpload, moved); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if _, err := c.Stat(moved); err != nil {
		t.Fatalf("Stat moved: %v", err)
	}

	// Download round-trip
	localDownload := filepath.Join(t.TempDir(), "dl.txt")
	if err := c.Download(moved, localDownload, nil); err != nil {
		t.Fatalf("Download: %v", err)
	}
	gotBytes, err := os.ReadFile(localDownload)
	if err != nil {
		t.Fatalf("read downloaded: %v", err)
	}
	if string(gotBytes) != content {
		t.Fatalf("downloaded content mismatch: len=%d want=%d", len(gotBytes), len(content))
	}

	// Remove a file
	if err := c.Remove(moved); err != nil {
		t.Fatalf("Remove file: %v", err)
	}
	if _, err := c.Stat(moved); err == nil {
		t.Fatal("moved.txt should be gone")
	}

	// Remove a directory recursively
	if err := c.Remove(sub); err != nil {
		t.Fatalf("Remove dir recursive: %v", err)
	}
	if _, err := c.Stat(sub); err == nil {
		t.Fatal("sub dir should be gone")
	}
}

// TestDownloadLeavesNoPartialFile pins the cleanup the transfer queue already
// had: a download that fails must not leave a truncated local file behind that
// looks complete to the user and to any later open of that path.
func TestDownloadLeavesNoPartialFile(t *testing.T) {
	root := filepath.ToSlash(t.TempDir())
	client := startSFTPServer(t)
	c, err := New(client)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	// A remote path that does not exist must not create anything locally either.
	absent := filepath.Join(t.TempDir(), "absent.txt")
	if err := c.Download(root+"/nope.txt", absent, nil); err == nil {
		t.Fatal("Download of a missing remote file must fail")
	}
	if _, err := os.Stat(absent); !os.IsNotExist(err) {
		t.Fatalf("failed download left a local file behind: %v", err)
	}

	// A directory opens over SFTP but cannot be read as a file, so the copy
	// fails after the local file already exists - and has to be removed again.
	dir := root + "/folder"
	if err := c.Mkdir(dir); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	partial := filepath.Join(t.TempDir(), "dir-as-file.bin")
	if err := c.Download(dir, partial, nil); err != nil {
		if _, statErr := os.Stat(partial); !os.IsNotExist(statErr) {
			t.Fatalf("Download %v left a partial file behind: %v", err, statErr)
		}
	} else {
		t.Log("server read the directory as a file; the cleanup path was not exercised")
	}
}
