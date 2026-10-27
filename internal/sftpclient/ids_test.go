package sftpclient

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseIDLine(t *testing.T) {
	cases := []struct {
		line     string
		wantID   string
		wantName string
		ok       bool
	}{
		{"root:x:0:0:root:/root:/bin/bash", "0", "root", true},
		{"farukda:x:1000:1000:Faruk,,,:/home/farukda:/bin/bash", "1000", "farukda", true},
		{"  spaced:x:42:1:notes:/home/spaced:/bin/sh", "42", "spaced", true},
		// A group file has fewer fields; the first three still line up.
		{"docker:x:999:", "999", "docker", true},
		{"users:x:100:farukda,root", "100", "users", true},
		// Anything that is not a plain account line must be skipped, not mapped.
		{"", "", "", false},
		{"# comment", "", "", false},
		{"nameonly:x", "", "", false},
		{"nonsense", "", "", false},
		{":::x", "", "", false},
		{"+username:::::::::", "", "", false},
		{"-NISexclusion:x:1001:1001", "", "", false},
		{"bad:x:notanumber:/home", "", "", false},
		{"neg:x:-5:0", "", "", false},
	}
	for _, tc := range cases {
		id, name, ok := parseIDLine(tc.line)
		if ok != tc.ok {
			t.Fatalf("parseIDLine(%q) ok = %v, want %v", tc.line, ok, tc.ok)
		}
		if !ok {
			continue
		}
		if id != tc.wantID || name != tc.wantName {
			t.Fatalf("parseIDLine(%q) = (%q,%q), want (%q,%q)", tc.line, id, name, tc.wantID, tc.wantName)
		}
	}
}

func TestLookupNameFallsBackToNumericID(t *testing.T) {
	table := map[string]string{"0": "root", "1000": "farukda"}
	if got := lookupName(table, 1000); got != "farukda" {
		t.Fatalf("lookupName = %q, want farukda", got)
	}
	// An LDAP/NIS-only account is absent from /etc/passwd and must still render.
	if got := lookupName(table, 4444); got != "4444" {
		t.Fatalf("unknown id = %q, want the decimal fallback 4444", got)
	}
	// A server without readable account files leaves every id numeric.
	if got := lookupName(map[string]string{}, 0); got != "0" {
		t.Fatalf("empty table = %q, want 0", got)
	}
}

// TestListPopulatesOwnerGroupAndMode covers the listing path end to end: the
// columns the file pane now shows must never come back empty, even when the
// account files cannot be read (the fallback is the decimal id).
func TestListPopulatesOwnerGroupAndMode(t *testing.T) {
	root := filepath.ToSlash(t.TempDir())
	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	client := startSFTPServer(t)
	c, err := New(client)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	entries, err := c.List(root)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if e.Owner == "" {
		t.Fatal("Owner must fall back to the numeric id, not be empty")
	}
	if e.Group == "" {
		t.Fatal("Group must fall back to the numeric id, not be empty")
	}
	if e.Mode.String() == "" {
		t.Fatal("Mode must be rendered for the Access column")
	}
	if e.ModTime.IsZero() {
		t.Fatal("ModTime must be carried through for the Last modified column")
	}
}
