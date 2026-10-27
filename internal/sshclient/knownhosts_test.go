package sshclient

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"golang.org/x/crypto/ssh"
)

func mustSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	return s
}

func TestKnownHostsTOFU(t *testing.T) {
	dir := t.TempDir()
	kh := NewKnownHosts(dir)
	if err := kh.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}

	var enrolled []string
	kh.OnNewHost = func(addr string) { enrolled = append(enrolled, addr) }

	addr := "10.0.0.1:22"
	keyA := mustSigner(t).PublicKey()
	keyB := mustSigner(t).PublicKey()

	// First contact enrolls (TOFU) and returns nil.
	if err := kh.Verify(addr, nil, keyA); err != nil {
		t.Fatalf("first verify: %v", err)
	}
	if len(enrolled) != 1 {
		t.Fatalf("expected 1 enrollment, got %d", len(enrolled))
	}

	// Same key passes.
	if err := kh.Verify(addr, nil, keyA); err != nil {
		t.Fatalf("repeat verify: %v", err)
	}
	if len(enrolled) != 1 {
		t.Fatalf("expected no extra enrollment, got %d", len(enrolled))
	}

	// Different key triggers a mismatch error.
	err := kh.Verify(addr, nil, keyB)
	if err == nil {
		t.Fatal("expected mismatch error")
	}
	mismatch, ok := err.(*HostKeyMismatchError)
	if !ok {
		t.Fatalf("expected *HostKeyMismatchError, got %T", err)
	}
	if mismatch.Addr != addr {
		t.Fatalf("mismatch addr = %q, want %q", mismatch.Addr, addr)
	}

	// Persistence: a fresh store reloaded from disk keeps the enrollment.
	kh2 := NewKnownHosts(dir)
	if err := kh2.Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if err := kh2.Verify(addr, nil, keyA); err != nil {
		t.Fatalf("reloaded verify of known key: %v", err)
	}
	if err := kh2.Verify(addr, nil, keyB); err == nil {
		t.Fatal("reloaded store should still reject different key")
	}
}

func TestKnownHostsInteractiveTrust(t *testing.T) {
	dir := t.TempDir()
	kh := NewKnownHosts(dir)
	kh.Interactive = true
	if err := kh.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}

	addr := "10.0.0.5:22"
	signer := mustSigner(t)
	err := kh.Verify(addr, nil, signer.PublicKey())
	if err == nil {
		t.Fatal("Interactive mode must not auto-enroll unknown hosts")
	}
	unk, ok := err.(*UnknownHostError)
	if !ok {
		t.Fatalf("expected *UnknownHostError, got %T", err)
	}
	if unk.Addr != addr || unk.Fingerprint == "" || unk.KeyType == "" {
		t.Fatalf("incomplete UnknownHostError: %+v", unk)
	}

	fp, kt, present := kh.PendingFingerprint(addr)
	if !present || fp != unk.Fingerprint || kt != unk.KeyType {
		t.Fatalf("PendingFingerprint mismatch: fp=%q kt=%q present=%v", fp, kt, present)
	}

	// Wrong fingerprint must be rejected.
	if err := kh.Trust(addr, "SHA256:not-the-right-one"); err == nil {
		t.Fatal("Trust with wrong fingerprint should fail")
	}

	// Correct fingerprint enrolls the host.
	if err := kh.Trust(addr, fp); err != nil {
		t.Fatalf("Trust: %v", err)
	}
	if _, _, present := kh.PendingFingerprint(addr); present {
		t.Fatal("pending entry must be cleared after Trust")
	}
	if err := kh.Verify(addr, nil, signer.PublicKey()); err != nil {
		t.Fatalf("Verify after Trust should pass, got %v", err)
	}

	// Persistence: a reloaded store remembers the trusted fingerprint.
	kh2 := NewKnownHosts(dir)
	kh2.Interactive = true
	if err := kh2.Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if err := kh2.Verify(addr, nil, signer.PublicKey()); err != nil {
		t.Fatalf("reloaded Verify should pass, got %v", err)
	}
}
