// Package sshclient provides SSH connection establishment, authentication,
// trust-on-first-use host key verification, and interactive PTY sessions for
// Gravel.
package sshclient

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/crypto/ssh"
)

// HostKeyMismatchError is returned when a server presents a different host key
// than the one recorded during trust-on-first-use enrollment.
type HostKeyMismatchError struct {
	Addr       string
	Expected   string
	Actual     string
	ActualType string
}

func (e *HostKeyMismatchError) Error() string {
	return fmt.Sprintf("sshclient: host key mismatch for %s: recorded %s but server presented %s (%s)",
		e.Addr, e.Expected, e.Actual, e.ActualType)
}

// UnknownHostError is returned when Interactive mode is enabled and a host key
// has not been seen before. Callers should surface the fingerprint to the user
// and, on approval, invoke KnownHosts.Trust with the same fingerprint before
// retrying the connection.
type UnknownHostError struct {
	Addr        string
	Fingerprint string
	KeyType     string
}

func (e *UnknownHostError) Error() string {
	return fmt.Sprintf("sshclient: unknown host %s (fingerprint %s %s)", e.Addr, e.KeyType, e.Fingerprint)
}

// KnownHosts records server fingerprints using trust-on-first-use (TOFU): the
// first key seen for an address is accepted and stored; later connections must
// match it exactly. When Interactive is true, unknown hosts return
// *UnknownHostError and require an explicit Trust call before proceeding.
type KnownHosts struct {
	mu    sync.Mutex
	path  string
	hosts map[string]string // addr -> sha256 fingerprint
	// OnNewHost, when set, is called with the addr of a host being enrolled.
	OnNewHost func(addr string)
	// Interactive makes first-time connections return UnknownHostError instead
	// of silently enrolling the key. The UI is expected to prompt the user and
	// call Trust on approval.
	Interactive bool
	// pending captures the last-observed fingerprint per address so that the
	// Dial path can retrieve it after the ssh library has wrapped the error.
	pending map[string]*UnknownHostError
}

// NewKnownHosts returns a TOFU store backed by known_hosts.json inside dir.
func NewKnownHosts(dir string) *KnownHosts {
	return &KnownHosts{
		path:    filepath.Join(dir, "known_hosts.json"),
		hosts:   map[string]string{},
		pending: map[string]*UnknownHostError{},
	}
}

// Load reads the recorded fingerprints. A missing file is treated as empty.
func (k *KnownHosts) Load() error {
	k.mu.Lock()
	defer k.mu.Unlock()

	data, err := os.ReadFile(k.path)
	if err != nil {
		if os.IsNotExist(err) {
			k.hosts = map[string]string{}
			return nil
		}
		return err
	}
	var m map[string]string
	if len(trimWS(data)) == 0 {
		m = map[string]string{}
	} else if err := json.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("sshclient: parse %s: %w", k.path, err)
	}
	if m == nil {
		m = map[string]string{}
	}
	k.hosts = m
	return nil
}

func (k *KnownHosts) saveLocked() error {
	data, err := json.MarshalIndent(k.hosts, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(k.path), 0o700); err != nil {
		return err
	}
	tmp := k.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, k.path)
}

// Verify implements the TOFU check for the given address and remote key. It
// enrolls unknown hosts and errors on mismatch.
func (k *KnownHosts) Verify(addr string, remote net.Addr, key ssh.PublicKey) error {
	fp := ssh.FingerprintSHA256(key)

	k.mu.Lock()
	if k.pending == nil {
		k.pending = map[string]*UnknownHostError{}
	}
	stored, seen := k.hosts[addr]
	if !seen {
		if k.Interactive {
			err := &UnknownHostError{Addr: addr, Fingerprint: fp, KeyType: key.Type()}
			k.pending[addr] = err
			k.mu.Unlock()
			return err
		}
		k.hosts[addr] = fp
		saveErr := k.saveLocked()
		newCb := k.OnNewHost
		k.mu.Unlock()
		if saveErr != nil {
			return saveErr
		}
		if newCb != nil {
			newCb(addr)
		}
		return nil
	}
	k.mu.Unlock()

	if stored != fp {
		return &HostKeyMismatchError{Addr: addr, Expected: stored, Actual: fp, ActualType: key.Type()}
	}
	return nil
}

// HostKeyCallback returns an ssh.HostKeyCallback bound to this store.
func (k *KnownHosts) HostKeyCallback() ssh.HostKeyCallback {
	return func(addr string, remote net.Addr, key ssh.PublicKey) error {
		return k.Verify(addr, remote, key)
	}
}

// PendingFingerprint returns the last-observed fingerprint for addr when the
// Interactive TOFU flow is awaiting user approval, plus whether one is pending.
func (k *KnownHosts) PendingFingerprint(addr string) (fp, keyType string, ok bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.pending == nil {
		return "", "", false
	}
	err, found := k.pending[addr]
	if !found {
		return "", "", false
	}
	return err.Fingerprint, err.KeyType, true
}

// Trust records the given fingerprint for addr and clears the pending entry.
// It returns an error if the fingerprint does not match what was captured on
// the last failed connection attempt (guards against UI/user mismatch).
func (k *KnownHosts) Trust(addr, fingerprint string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.pending == nil {
		k.pending = map[string]*UnknownHostError{}
	}
	p, ok := k.pending[addr]
	if !ok || p.Fingerprint != fingerprint {
		return fmt.Errorf("sshclient: no pending fingerprint for %s (or mismatch)", addr)
	}
	k.hosts[addr] = fingerprint
	delete(k.pending, addr)
	return k.saveLocked()
}

func trimWS(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && isSpace(b[start]) {
		start++
	}
	for end > start && isSpace(b[end-1]) {
		end--
	}
	return b[start:end]
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}
