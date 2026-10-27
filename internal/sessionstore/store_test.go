package sessionstore

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func tempStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	return NewStore(dir), dir
}

func TestPutGetListRoundTrip(t *testing.T) {
	s, dir := tempStore(t)
	if err := s.Load(); err != nil {
		t.Fatalf("Load on empty: %v", err)
	}

	p, err := s.Put(Profile{Name: "web", Host: "10.0.0.1", Port: 22, User: "root", Auth: AuthPassword})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if p.ID == "" {
		t.Fatal("expected generated ID")
	}
	if _, err := s.Put(Profile{Name: "db", Host: "10.0.0.2", Port: 2222, User: "admin", Auth: AuthKey, KeyPath: `C:\keys\id_ed25519`}); err != nil {
		t.Fatalf("Put second: %v", err)
	}

	list := s.List()
	if len(list) != 2 {
		t.Fatalf("expected 2 profiles, got %d", len(list))
	}
	// Sorted by name: db before web.
	if list[0].Name != "db" || list[1].Name != "web" {
		t.Fatalf("unexpected order: %s, %s", list[0].Name, list[1].Name)
	}

	// Reload from a fresh store to prove persistence + no secret fields.
	s2 := NewStore(dir)
	if err := s2.Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	got, ok := s2.Get(p.ID)
	if !ok || got.Addr() != "10.0.0.1:22" {
		t.Fatalf("reload Get mismatch: %+v ok=%v", got, ok)
	}

	// Assert no credential VALUES are persisted. Note: the "auth" field may
	// legitimately hold the label "password" (a mode), so we only forbid
	// passphrase/secret material.
	data, err := os.ReadFile(filepath.Join(dir, "sessions.json"))
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	for _, banned := range []string{"passphrase", "secret", `"password":`} {
		if containsFold(string(data), banned) {
			t.Fatalf("sessions.json must not contain %q", banned)
		}
	}
}

// TestReorderPinsDraggedSequence proves the UI's drag-and-drop order survives a
// reload: ranked rows keep their sequence and a later-saved (unranked) profile
// lands after them by name rather than jumping back into alphabetical order.
func TestReorderPinsDraggedSequence(t *testing.T) {
	s, dir := tempStore(t)
	_ = s.Load()
	web, _ := s.Put(Profile{Name: "web", Host: "h1"})
	db, _ := s.Put(Profile{Name: "db", Host: "h2"})
	api, _ := s.Put(Profile{Name: "api", Host: "h3"})

	// Alphabetical before any drag.
	if got := names(s.List()); got[0] != "api" || got[2] != "web" {
		t.Fatalf("pre-drag order = %v, want alphabetical", got)
	}

	// Drag web to the top, then db, then api.
	if err := s.Reorder([]string{web.ID, db.ID, api.ID}); err != nil {
		t.Fatalf("Reorder: %v", err)
	}
	if got := names(s.List()); got[0] != "web" || got[1] != "db" || got[2] != "api" {
		t.Fatalf("post-drag order = %v, want web, db, api", got)
	}

	// A profile saved after the drag stays unranked and sorts below them.
	if _, err := s.Put(Profile{Name: "zeta", Host: "h4"}); err != nil {
		t.Fatalf("Put after reorder: %v", err)
	}
	if got := names(s.List()); got[0] != "web" || got[3] != "zeta" {
		t.Fatalf("new profile misplaced: %v", got)
	}

	// Reload from disk: the ranked sequence must persist.
	s2 := NewStore(dir)
	if err := s2.Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := names(s2.List()); got[0] != "web" || got[1] != "db" || got[2] != "api" {
		t.Fatalf("reloaded order = %v, want web, db, api", got)
	}
}

func names(ps []Profile) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Name
	}
	return out
}

func TestDelete(t *testing.T) {
	s, _ := tempStore(t)
	_ = s.Load()
	p, _ := s.Put(Profile{Name: "x", Host: "h"})
	if err := s.Delete(p.ID); err != nil {
		t.Fatalf("expected delete to succeed, got %v", err)
	}
	if err := s.Delete(p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound on second delete, got %v", err)
	}
	if len(s.List()) != 0 {
		t.Fatal("expected empty list after delete")
	}
}

// TestDeleteKeepsRowWhenPersistFails pins the fix for the swallowed save error:
// a profile that could not be removed from the file must stay in memory, so no
// caller can report a deletion the next start would undo.
func TestDeleteKeepsRowWhenPersistFails(t *testing.T) {
	// sessions.json would have to live inside a regular file, so every write fails.
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatalf("create blocker: %v", err)
	}
	s := NewStore(blocker)
	p, _ := s.Put(Profile{Name: "x", Host: "h"})
	if _, ok := s.Get(p.ID); !ok {
		t.Fatal("Put must keep the row in memory even when the write fails")
	}
	if err := s.Delete(p.ID); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete = %v, want a persist error", err)
	}
	if _, ok := s.Get(p.ID); !ok {
		t.Fatal("failed Delete must restore the profile it could not persist")
	}
	if len(s.List()) != 1 {
		t.Fatalf("expected the profile to survive a failed Delete, got %d rows", len(s.List()))
	}
}

// TestResolveConfigDirMovesLegacyFolder pins the rename from the old project
// name: saved servers and trusted host keys must come along, not vanish.
func TestResolveConfigDirMovesLegacyFolder(t *testing.T) {
	base := t.TempDir()
	legacy := filepath.Join(base, legacyConfigName)
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatalf("mkdir legacy: %v", err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "sessions.json"), []byte("[]"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	got, err := resolveConfigDir(base)
	if err != nil {
		t.Fatalf("resolveConfigDir: %v", err)
	}
	if want := filepath.Join(base, configDirName); got != want {
		t.Fatalf("dir = %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(got, "sessions.json")); err != nil {
		t.Fatalf("sessions.json did not survive the move: %v", err)
	}
	if dirExists(legacy) {
		t.Fatal("the legacy folder should be gone after the move")
	}
}

// TestResolveConfigDirNeverOverwritesWhenBothExist pins the case where the
// current folder already exists: the older one is left untouched so a
// half-done upgrade can always be recovered by hand.
func TestResolveConfigDirNeverOverwritesWhenBothExist(t *testing.T) {
	base := t.TempDir()
	current := filepath.Join(base, configDirName)
	legacy := filepath.Join(base, legacyConfigName)
	for _, d := range []string{current, legacy} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	legacyFile := filepath.Join(legacy, "sessions.json")
	if err := os.WriteFile(legacyFile, []byte("keep me"), 0o600); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}

	got, err := resolveConfigDir(base)
	if err != nil {
		t.Fatalf("resolveConfigDir: %v", err)
	}
	if got != current {
		t.Fatalf("dir = %q, want the existing %q", got, current)
	}
	// The legacy folder must be left untouched so a half-done upgrade can
	// always be recovered by hand.
	b, err := os.ReadFile(legacyFile)
	if err != nil || string(b) != "keep me" {
		t.Fatalf("legacy file was touched: %q err=%v", b, err)
	}
}

// TestResolveConfigDirCarriesBedrockForward covers the rename the other way
// round: a user who had Bedrock's saved servers and trusted host keys must find
// them under the Gravel folder without copying anything by hand.
func TestResolveConfigDirCarriesBedrockForward(t *testing.T) {
	base := t.TempDir()
	previous := filepath.Join(base, prevConfigName)
	if err := os.MkdirAll(previous, 0o700); err != nil {
		t.Fatalf("mkdir previous: %v", err)
	}
	if err := os.WriteFile(filepath.Join(previous, "known_hosts.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	got, err := resolveConfigDir(base)
	if err != nil {
		t.Fatalf("resolveConfigDir: %v", err)
	}
	if want := filepath.Join(base, configDirName); got != want {
		t.Fatalf("dir = %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(got, "known_hosts.json")); err != nil {
		t.Fatalf("known_hosts.json did not survive the rename: %v", err)
	}
	if dirExists(previous) {
		t.Fatal("the Bedrock folder should be gone after the move")
	}
}

// TestResolveConfigDirPrefersMostRecentName: when both earlier folders are
// still around, the newest one (Bedrock) is the one carried forward and the
// oldest stays on disk untouched.
func TestResolveConfigDirPrefersMostRecentName(t *testing.T) {
	base := t.TempDir()
	previous := filepath.Join(base, prevConfigName)
	legacy := filepath.Join(base, legacyConfigName)
	for _, d := range []string{previous, legacy} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	if err := os.WriteFile(filepath.Join(previous, "sessions.json"), []byte("from-bedrock"), 0o600); err != nil {
		t.Fatalf("seed previous: %v", err)
	}
	legacyFile := filepath.Join(legacy, "sessions.json")
	if err := os.WriteFile(legacyFile, []byte("from-genpilot"), 0o600); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}

	got, err := resolveConfigDir(base)
	if err != nil {
		t.Fatalf("resolveConfigDir: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(got, "sessions.json"))
	if err != nil {
		t.Fatalf("moved file missing: %v", err)
	}
	if string(b) != "from-bedrock" {
		t.Fatalf("moved %q, want the Bedrock folder's data", b)
	}
	kept, err := os.ReadFile(legacyFile)
	if err != nil || string(kept) != "from-genpilot" {
		t.Fatalf("the Genpilot folder must stay untouched, got %q err=%v", kept, err)
	}
}

func TestResolveConfigDirCreatesFolderWhenNeitherExists(t *testing.T) {
	base := t.TempDir()
	got, err := resolveConfigDir(base)
	if err != nil {
		t.Fatalf("resolveConfigDir: %v", err)
	}
	if want := filepath.Join(base, configDirName); got != want {
		t.Fatalf("dir = %q, want %q", got, want)
	}
	if !dirExists(got) {
		t.Fatalf("directory %q was not created", got)
	}
}

// TestAdoptWebviewProfile covers the preferences that a rename used to lose:
// while the WebView2 user-data path was unset, Wails stored localStorage under
// %APPDATA%\[BinaryName.exe], so the theme, font size and pane width lived with
// the old executable name and the new build started from defaults.
func TestAdoptWebviewProfile(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, configDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}
	old := filepath.Join(base, webviewProfileNames[0], "EBWebView", "Default")
	if err := os.MkdirAll(old, 0o700); err != nil {
		t.Fatalf("mkdir old profile: %v", err)
	}
	marker := filepath.Join(old, "preferences")
	if err := os.WriteFile(marker, []byte("{}"), 0o600); err != nil {
		t.Fatalf("seed profile: %v", err)
	}

	if !AdoptWebviewProfile(dir) {
		t.Fatal("AdoptWebviewProfile should report the move")
	}
	if _, err := os.Stat(filepath.Join(dir, "EBWebView", "Default", "preferences")); err != nil {
		t.Fatalf("profile did not land in the config folder: %v", err)
	}
	// The per-executable folder held nothing but that profile, so the empty
	// shell must not stay behind as %APPDATA%\Bedrock.exe clutter.
	if dirExists(filepath.Join(base, webviewProfileNames[0])) {
		t.Fatal("the emptied leftover folder should have been removed")
	}
	// A second run must be a no-op rather than moving a fresh profile around.
	if AdoptWebviewProfile(dir) {
		t.Fatal("an existing profile must not be adopted again")
	}
}

// TestAdoptWebviewProfileKeepsFolderWithOtherContent: only an empty leftover is
// removed. Anything else the user or an older build left in that folder stays.
func TestAdoptWebviewProfileKeepsFolderWithOtherContent(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, configDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}
	previous := filepath.Join(base, webviewProfileNames[0])
	if err := os.MkdirAll(filepath.Join(previous, "EBWebView"), 0o700); err != nil {
		t.Fatalf("mkdir profile: %v", err)
	}
	stray := filepath.Join(previous, "something-else.txt")
	if err := os.WriteFile(stray, []byte("keep"), 0o600); err != nil {
		t.Fatalf("seed stray: %v", err)
	}

	if !AdoptWebviewProfile(dir) {
		t.Fatal("AdoptWebviewProfile should report the move")
	}
	if !dirExists(previous) {
		t.Fatal("a folder that still holds files must not be deleted")
	}
	if _, err := os.ReadFile(stray); err != nil {
		t.Fatalf("the stray file was lost: %v", err)
	}
}

// TestAdoptWebviewProfileWithoutHistory: a clean install has nothing to carry
// over and must not create an empty profile directory.
func TestAdoptWebviewProfileWithoutHistory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), configDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if AdoptWebviewProfile(dir) {
		t.Fatal("nothing should be adopted when no previous profile exists")
	}
	if dirExists(filepath.Join(dir, "EBWebView")) {
		t.Fatal("a clean install must not gain an empty EBWebView folder")
	}
}

// TestAdoptWebviewProfileCleansEmptyShellAfterMigration: a completed move leaves
// the per-executable folder behind empty. That happens on every later start too
// - the profile already lives in the config folder, so nothing is moved and the
// shell still has to go, or %APPDATA% keeps a folder per name the app ever had.
func TestAdoptWebviewProfileCleansEmptyShellAfterMigration(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, configDirName)
	if err := os.MkdirAll(filepath.Join(dir, "EBWebView"), 0o700); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	shell := filepath.Join(base, webviewProfileNames[0])
	if err := os.MkdirAll(shell, 0o700); err != nil {
		t.Fatalf("mkdir shell: %v", err)
	}

	if AdoptWebviewProfile(dir) {
		t.Fatal("nothing should be reported as moved when the profile is already home")
	}
	if dirExists(shell) {
		t.Fatal("the empty leftover folder should have been removed")
	}
}

// TestPruneLogsKeepsNewest: a log file is written on every launch, so without
// pruning the config folder grows for the life of the install - and these files
// carry connection history, which is a reason to bound them rather than keep
// every one. Only the app's own per-launch logs may go.
func TestPruneLogsKeepsNewest(t *testing.T) {
	dir := t.TempDir()
	base := time.Now()
	logs := []string{
		"bedrock-2026-09-30-090000.log",
		"gravel-2026-10-01-090000.log",
		"gravel-2026-10-02-090000.log",
		"gravel-2026-10-03-090000.log",
		"gravel-2026-10-04-090000.log",
	}
	for i, name := range logs {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
		stamp := base.Add(time.Duration(i) * time.Hour)
		if err := os.Chtimes(p, stamp, stamp); err != nil {
			t.Fatalf("chtimes %s: %v", name, err)
		}
	}
	neighbours := []string{"sessions.json", "known_hosts.json", "gravel-notes.txt", "notes.log"}
	for _, name := range neighbours {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("keep"), 0o600); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	fakeDir := filepath.Join(dir, "gravel-2026-10-09-090000.log")
	if err := os.Mkdir(fakeDir, 0o700); err != nil {
		t.Fatalf("mkdir lookalike: %v", err)
	}

	removed, err := PruneLogs(dir, 2)
	if err != nil {
		t.Fatalf("PruneLogs: %v", err)
	}
	if removed != 3 {
		t.Fatalf("removed %d, want 3", removed)
	}
	for _, name := range []string{"gravel-2026-10-04-090000.log", "gravel-2026-10-03-090000.log"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("newest log %s was deleted: %v", name, err)
		}
	}
	for _, name := range []string{"gravel-2026-10-01-090000.log", "gravel-2026-10-02-090000.log", "bedrock-2026-09-30-090000.log"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Fatalf("old log %s survived the prune", name)
		}
	}
	for _, name := range neighbours {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("%s must never be touched: %v", name, err)
		}
	}
	if !dirExists(fakeDir) {
		t.Fatal("a folder named like a log must not be pruned")
	}
}

// TestPruneLogsAlwaysKeepsOne: the file this launch is writing must survive even
// if a caller asks for an impossible retention count.
func TestPruneLogsAlwaysKeepsOne(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "gravel-2026-10-04-090000.log")
	if err := os.WriteFile(current, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	removed, err := PruneLogs(dir, 0)
	if err != nil {
		t.Fatalf("PruneLogs: %v", err)
	}
	if removed != 0 {
		t.Fatalf("removed %d, want 0", removed)
	}
	if _, err := os.Stat(current); err != nil {
		t.Fatalf("the only log was deleted: %v", err)
	}
}

func TestPruneLogsOnMissingDirReports(t *testing.T) {
	if _, err := PruneLogs(filepath.Join(t.TempDir(), "nope"), 5); err == nil {
		t.Fatal("PruneLogs should report an unreadable folder instead of failing silently")
	}
}

func containsFold(hay, needle string) bool {
	return len(hay) >= len(needle) && indexOf(lower(hay), lower(needle)) >= 0
}

func lower(s string) []byte {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return b
}

func indexOf(hay, needle []byte) int {
	for i := 0; i+len(needle) <= len(hay); i++ {
		match := true
		for j := range needle {
			if hay[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
