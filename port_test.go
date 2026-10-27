package main

import (
	"testing"

	"gravel/internal/sessionstore"
)

// validPort's own bounds are already pinned in tunnel_test.go; these cover the
// two places that started calling it in this round. The rule matters behind the
// frontend forms, because an imported host, a PuTTY registry session or a
// hand-edited sessions.json never passes through that form.

// TestSaveProfileRefusesBadPort: a saved server is dialled again after a restart,
// so an out-of-range port has to be refused at save time instead of being written
// to disk to fail later in another session. (App.Connect reports its refusal
// through the terminal, which needs the Wails runtime context, so the connect
// path is guarded by the same helper but covered here only through save.)
func TestSaveProfileRefusesBadPort(t *testing.T) {
	a := NewApp()
	a.store = sessionstore.NewStore(t.TempDir())

	if err := a.SaveProfile("web", "10.0.0.1", 70000, "password", ""); err == nil {
		t.Fatal("an out-of-range port must not be saved")
	}
	if got := a.store.List(); len(got) != 0 {
		t.Fatalf("a refused save still wrote %d profile(s)", len(got))
	}

	// A blank port keeps its long-standing meaning of 22, and a real one passes.
	if err := a.SaveProfile("web", "10.0.0.1", 0, "password", ""); err != nil {
		t.Fatalf("port 0 should default to 22: %v", err)
	}
	if err := a.SaveProfile("ssh", "10.0.0.2", 2222, "password", ""); err != nil {
		t.Fatalf("valid port refused: %v", err)
	}
	ports := map[string]int{}
	for _, p := range a.store.List() {
		ports[p.Name] = p.Port
	}
	if ports["web"] != 22 || ports["ssh"] != 2222 {
		t.Fatalf("saved ports = %v, want web 22 and ssh 2222", ports)
	}
}

// TestQueuePauseResumeNeedASession keeps the two new queue commands on the same
// contract as QueueCancel: without a session they report it, they do not panic.
func TestQueuePauseResumeNeedASession(t *testing.T) {
	a := NewApp()
	if err := a.QueuePause("gone", 1); err == nil {
		t.Error("QueuePause must report a missing session")
	}
	if err := a.QueueResume("gone", 1); err == nil {
		t.Error("QueueResume must report a missing session")
	}
}
