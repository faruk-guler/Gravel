package sshclient

import (
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"gravel/internal/sessionstore"
)

// Credential holds secret inputs supplied at connect time. These values are
// never persisted to disk by Gravel.
type Credential struct {
	Password   string // used for AuthPassword
	Passphrase string // used to decrypt an encrypted private key
}

// Dial establishes an SSH client for the profile using the given credential and
// TOFU known-hosts store.
func Dial(p sessionstore.Profile, cred Credential, kh *KnownHosts) (*ssh.Client, error) {
	auth, err := authMethods(p, cred)
	if err != nil {
		return nil, err
	}
	if kh == nil {
		return nil, fmt.Errorf("sshclient: known-hosts store is required (no insecure fallback)")
	}
	cfg := &ssh.ClientConfig{
		User:            p.User,
		Auth:            auth,
		HostKeyCallback: kh.HostKeyCallback(),
		Timeout:         15 * time.Second,
	}
	client, err := ssh.Dial("tcp", p.Addr(), cfg)
	if err != nil {
		// Interactive TOFU: the callback recorded a pending fingerprint; surface
		// it as *UnknownHostError so UIs can prompt the user rather than trying
		// to parse the ssh library's wrapped handshake error text.
		if fp, kt, ok := kh.PendingFingerprint(p.Addr()); ok {
			return nil, &UnknownHostError{Addr: p.Addr(), Fingerprint: fp, KeyType: kt}
		}
		return nil, fmt.Errorf("sshclient: dial %s: %w", p.Addr(), err)
	}
	return client, nil
}

// AuthFailed reports whether err means the server rejected the supplied
// credentials, as opposed to a network, host-key or protocol problem. Only this
// case is worth asking the user for another password.
//
// x/crypto/ssh builds this error with fmt.Errorf and exports no sentinel value
// or error type, so matching its message is the only handle available. The
// TestAuthFailed* tests dial a real server to pin that wording, so a dependency
// update cannot silently turn the retry loop into a dead branch.
func AuthFailed(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "unable to authenticate")
}

// authMethods builds the SSH auth method list for a profile + credential pair.
func authMethods(p sessionstore.Profile, cred Credential) ([]ssh.AuthMethod, error) {
	switch p.Auth {
	case sessionstore.AuthKey:
		if p.KeyPath == "" {
			return nil, fmt.Errorf("sshclient: key auth requires a key file path")
		}
		pem, err := os.ReadFile(p.KeyPath)
		if err != nil {
			return nil, fmt.Errorf("sshclient: read key %s: %w", p.KeyPath, err)
		}
		var signer ssh.Signer
		if len(cred.Passphrase) > 0 {
			signer, err = ssh.ParsePrivateKeyWithPassphrase(pem, []byte(cred.Passphrase))
		} else {
			signer, err = ssh.ParsePrivateKey(pem)
		}
		if err != nil {
			return nil, fmt.Errorf("sshclient: parse key %s: %w", p.KeyPath, err)
		}
		return []ssh.AuthMethod{ssh.PublicKeys(signer)}, nil
	case sessionstore.AuthPassword, "":
		return []ssh.AuthMethod{ssh.Password(cred.Password)}, nil
	default:
		return nil, fmt.Errorf("sshclient: unknown auth kind %q", p.Auth)
	}
}
