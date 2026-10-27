package main

import (
	"testing"

	"gravel/internal/sessionstore"
)

func TestParseImportLine(t *testing.T) {
	cases := []struct {
		line, name, host string
	}{
		{"192.168.44.10", "", "192.168.44.10"},
		{"192.168.44.10:2222", "", "192.168.44.10:2222"},
		{"web1 192.168.44.10", "web1", "192.168.44.10"},
		{"db-prod, 10.0.0.5", "db-prod", "10.0.0.5"},
		{"jump = ssh.example.com", "jump", "ssh.example.com"},
		{"two words only here", "", "two words only here"}, // not name+host, kept whole
	}
	for _, c := range cases {
		n, h := parseImportLine(c.line)
		if n != c.name || h != c.host {
			t.Errorf("parseImportLine(%q) = (%q, %q), want (%q, %q)", c.line, n, h, c.name, c.host)
		}
	}
}

func TestImportHostsCreatesAndDedupes(t *testing.T) {
	a := NewApp()
	a.store = sessionstore.NewStore(t.TempDir())
	if err := a.store.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}

	text := "# comment\n\nweb1 192.168.44.10\nssh://192.168.44.11:2222\ndb-prod, 10.0.0.5\n"
	if n := a.ImportHosts(text); n != 3 {
		t.Fatalf("imported %d, want 3", n)
	}
	list := a.store.List()
	if len(list) != 3 {
		t.Fatalf("store holds %d profiles, want 3: %+v", len(list), list)
	}
	// Re-import with a changed port must update, not duplicate.
	if n := a.ImportHosts("web1 192.168.44.99"); n != 1 {
		t.Fatalf("re-import returned %d, want 1", n)
	}
	again := a.store.List()
	if len(again) != 3 {
		t.Fatalf("re-import duplicated: %d rows", len(again))
	}
	for _, p := range again {
		if p.Name == "web1" && p.Host != "192.168.44.99" {
			t.Fatalf("web1 not updated: %+v", p)
		}
	}
	// Port parsing: ssh://host:2222 landed as 2222.
	found := false
	for _, p := range again {
		if p.Host == "192.168.44.11" && p.Port == 2222 {
			found = true
		}
	}
	if !found {
		t.Fatalf("ssh://host:port not parsed: %+v", again)
	}
}

// TestImportHostsKeepsPortVariantsApart pins the reported case: the same host
// on 22 and 2222 must land as two rows, not collapse into one.
func TestImportHostsKeepsPortVariantsApart(t *testing.T) {
	a := NewApp()
	a.store = sessionstore.NewStore(t.TempDir())
	if err := a.store.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	text := "debiansrv\ndebiansrv:2222\n192.168.45.25\n192.168.45.25:2222\n"
	if n := a.ImportHosts(text); n != 4 {
		t.Fatalf("imported %d, want 4", n)
	}
	// Port variants get a label-style name ("host (port)"), never a
	// hostname-looking "host:port".
	want := map[string]int{"debiansrv": 22, "debiansrv (2222)": 2222, "192.168.45.25": 22, "192.168.45.25 (2222)": 2222}
	list := a.store.List()
	if len(list) != 4 {
		t.Fatalf("store holds %d profiles, want 4: %+v", len(list), list)
	}
	for _, p := range list {
		port, ok := want[p.Name]
		if !ok || p.Port != port {
			t.Fatalf("unexpected row %s (port %d)", p.Name, p.Port)
		}
	}
}
