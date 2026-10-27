// Package sessionstore persists Gravel server profiles and known host keys
// to local JSON files. No credentials (passwords/passphrases) are ever written
// to disk; profiles only store connection metadata and key file paths.
package sessionstore

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// AuthKind identifies how a profile authenticates to a server.
type AuthKind string

const (
	// AuthPassword authenticates with a user-typed password (never stored).
	AuthPassword AuthKind = "password"
	// AuthKey authenticates with a private key file plus optional passphrase.
	AuthKey AuthKind = "key"
)

// Profile is a saved server connection. It intentionally contains no secret
// material: the password and key passphrase are supplied at connect time only.
// User is filled in for live sessions from the terminal login prompt; saved
// profiles leave it empty because no login name is stored.
type Profile struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Host    string   `json:"host"`
	Port    int      `json:"port"`
	User    string   `json:"user,omitempty"`
	Auth    AuthKind `json:"auth"`
	KeyPath string   `json:"key_path,omitempty"`
	// Order positions the profile in the UI list after the user dragged rows
	// into place. Zero means "never ranked": those entries follow the ranked
	// ones in name order, so a store that was never reordered keeps showing
	// the old alphabetical display.
	Order int `json:"order,omitempty"`
}

// Addr returns the "host:port" dial target for the profile.
func (p Profile) Addr() string {
	port := p.Port
	if port == 0 {
		port = 22
	}
	return fmt.Sprintf("%s:%d", p.Host, port)
}

// NewID returns a random hex identifier for a new profile.
func NewID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Store manages profiles on disk, guarded by a mutex for concurrent access.
type Store struct {
	mu       sync.Mutex
	path     string
	profiles map[string]Profile
}

// NewStore returns a Store backed by sessions.json inside dir.
func NewStore(dir string) *Store {
	return &Store{
		path:     filepath.Join(dir, "sessions.json"),
		profiles: map[string]Profile{},
	}
}

// Names of the config folder. The product has been renamed twice: it started
// as Genpilot, shipped as Bedrock, and is now Gravel. Each older folder is
// carried forward once so a rename never costs saved servers or trusted keys.
const (
	configDirName    = "Gravel"
	prevConfigName   = "Bedrock"
	legacyConfigName = "Genpilot"
)

// DefaultDir returns the OS-specific config directory for Gravel, creating it
// if needed. On Windows this resolves under %APPDATA%.
//
// An existing folder from an earlier name is carried over once. If the move
// cannot be made - a file is held by another running instance, say - the old
// path stays in use rather than presenting the user with empty settings.
func DefaultDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return resolveConfigDir(base)
}

// resolveConfigDir picks the Gravel folder under base, moving an older Bedrock
// or Genpilot folder forward in that order of preference. Split out from
// DefaultDir so the move can be tested against a temporary directory on any
// platform.
func resolveConfigDir(base string) (string, error) {
	dir := filepath.Join(base, configDirName)
	if dirExists(dir) {
		return ensureDir(dir)
	}
	for _, previous := range []string{prevConfigName, legacyConfigName} {
		old := filepath.Join(base, previous)
		if !dirExists(old) {
			continue
		}
		if err := os.Rename(old, dir); err == nil {
			return ensureDir(dir)
		}
		// Half of an upgrade that cannot be finished must not be broken; keep
		// using the folder that is clearly still in use.
		return ensureDir(old)
	}
	return ensureDir(dir)
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// webviewProfileNames are the folders older builds of the app left their
// WebView2 profile in. While the path was unset, Wails stored it under
// %APPDATA%\[BinaryName.exe], so localStorage - the theme, font size, pane width
// - was keyed to the executable's name and every rename orphaned it silently.
// The profile now lives in the config folder above, which does migrate.
var webviewProfileNames = []string{"Bedrock.exe", "Genpilot.exe"}

// AdoptWebviewProfile moves a previous per-executable WebView2 profile into dir
// so stored preferences survive a rename, and reports whether it moved one. It
// is a no-op on the data when the config folder already has a profile, and a
// profile held open by a running instance is left exactly where it is:
// half-copying a leveldb store is worse than starting from defaults.
//
// Whatever previous folder is left behind is removed only if it is empty.
// os.Remove fails on a directory that still holds anything, so an abandoned
// profile that was never migrated - and anything else the user kept there -
// cannot be destroyed by this, while the empty shells a completed move leaves
// behind do not pile up in %APPDATA%.
func AdoptWebviewProfile(dir string) bool {
	target := filepath.Join(dir, "EBWebView")
	parent := filepath.Dir(dir)
	moved := false
	for _, previous := range webviewProfileNames {
		folder := filepath.Join(parent, previous)
		if !dirExists(folder) {
			continue
		}
		if !moved && !dirExists(target) {
			if old := filepath.Join(folder, "EBWebView"); dirExists(old) {
				if err := os.Rename(old, target); err != nil {
					return false
				}
				moved = true
			}
		}
		_ = os.Remove(folder)
	}
	return moved
}

// logPrefixes are the per-launch log files the app has written, under its
// current name and the names it shipped as. One file is created per start, so
// without pruning the folder grows for the lifetime of the install.
var logPrefixes = []string{"gravel-", "bedrock-"}

// PruneLogs deletes per-launch log files beyond the newest keep and reports how
// many went. Only plain files directly inside dir whose name starts with one of
// logPrefixes and ends in .log are considered, so sessions.json, the known-hosts
// file and the WebView2 profile are never touched. keep below 1 is raised to 1:
// there is always at least the file this launch is writing.
func PruneLogs(dir string, keep int) (int, error) {
	if keep < 1 {
		keep = 1
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	type logFile struct {
		path    string
		modTime time.Time
	}
	var found []logFile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".log") || !hasAnyPrefix(name, logPrefixes) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		found = append(found, logFile{path: filepath.Join(dir, name), modTime: info.ModTime()})
	}
	if len(found) <= keep {
		return 0, nil
	}
	// Newest first, then delete the tail.
	sort.Slice(found, func(i, j int) bool { return found[i].modTime.After(found[j].modTime) })
	removed := 0
	for _, f := range found[keep:] {
		if err := os.Remove(f.path); err == nil {
			removed++
		}
	}
	return removed, nil
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func ensureDir(p string) (string, error) {
	if err := os.MkdirAll(p, 0o700); err != nil {
		return "", err
	}
	return p, nil
}

// Load reads profiles from disk. A missing file is treated as an empty store.
func (s *Store) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.profiles = map[string]Profile{}
			return nil
		}
		return err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		s.profiles = map[string]Profile{}
		return nil
	}
	var list []Profile
	if err := json.Unmarshal(data, &list); err != nil {
		return fmt.Errorf("sessionstore: parse %s: %w", s.path, err)
	}
	m := make(map[string]Profile, len(list))
	for _, p := range list {
		if p.ID == "" {
			p.ID = NewID()
		}
		m[p.ID] = p
	}
	s.profiles = m
	return nil
}

// save writes the current profiles atomically (temp file + rename). The caller
// must hold s.mu.
func (s *Store) save() error {
	list := s.sortedLocked()
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Put inserts or updates a profile (assigning an ID when empty) and persists.
// An update keeps the row's drag-and-drop rank; a new profile stays unranked
// and sorts after the arranged rows by name.
func (s *Store) Put(p Profile) (Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if p.ID == "" {
		p.ID = NewID()
	}
	if p.Auth == "" {
		p.Auth = AuthPassword
	}
	if old, exists := s.profiles[p.ID]; exists && p.Order == 0 {
		p.Order = old.Order
	}
	s.profiles[p.ID] = p
	if err := s.save(); err != nil {
		return p, err
	}
	return p, nil
}

// PutNamed inserts or updates a profile keyed by its name, so saving the same
// session name twice updates the existing row instead of piling up duplicates.
// The name is matched case-insensitively and trimmed; a blank name is rejected
// because it is the key the list and Load rely on.
func (s *Store) PutNamed(p Profile) (Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" {
		return p, fmt.Errorf("sessionstore: profile name is required")
	}
	if p.Auth == "" {
		p.Auth = AuthPassword
	}
	// Reuse the ID of an existing profile with the same name (any case).
	for id, existing := range s.profiles {
		if strings.EqualFold(strings.TrimSpace(existing.Name), p.Name) {
			p.ID = id
			p.Order = existing.Order
			s.profiles[p.ID] = p
			if err := s.save(); err != nil {
				return p, err
			}
			return p, nil
		}
	}
	if p.ID == "" {
		p.ID = NewID()
	}
	s.profiles[p.ID] = p
	if err := s.save(); err != nil {
		return p, err
	}
	return p, nil
}

// Reorder ranks profiles to match the UI's drag-and-drop sequence: the ID at
// position i becomes the (i+1)-th row and every listed profile leaves the
// "never ranked" state. It is persisted like every other mutation.
func (s *Store) Reorder(ids []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, id := range ids {
		if p, ok := s.profiles[id]; ok {
			p.Order = i + 1
			s.profiles[id] = p
		}
	}
	return s.save()
}

// Get returns a profile by ID.
func (s *Store) Get(id string) (Profile, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.profiles[id]
	return p, ok
}

// ErrNotFound is returned when an operation names a profile that is not stored.
var ErrNotFound = errors.New("sessionstore: no such profile")

// Delete removes a profile and persists it. It returns ErrNotFound when the ID
// is unknown. A failed write is returned as an error and the row is put back in
// memory: a profile that vanishes here but survives in the file would come back
// on the next start, so reporting the delete as successful would promise a
// removal that never happened.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.profiles[id]
	if !ok {
		return ErrNotFound
	}
	delete(s.profiles, id)
	if err := s.save(); err != nil {
		s.profiles[id] = old
		return err
	}
	return nil
}

// List returns all profiles sorted by name for stable UI display.
func (s *Store) List() []Profile {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sortedLocked()
}

func (s *Store) sortedLocked() []Profile {
	list := make([]Profile, 0, len(s.profiles))
	for _, p := range s.profiles {
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool {
		a, b := list[i], list[j]
		// Dragged rows lead in their assigned sequence; never-ranked rows
		// follow alphabetically, which is all a fresh store ever has.
		if (a.Order == 0) != (b.Order == 0) {
			return b.Order == 0
		}
		if a.Order != b.Order {
			return a.Order < b.Order
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.ID < b.ID
	})
	return list
}
