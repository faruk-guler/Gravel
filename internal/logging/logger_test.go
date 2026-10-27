package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestWriteBuffersBeforeStart pins the reason the ring is filled even with no
// file attached: if the config folder cannot be opened the Logs tab still has to
// show what happened, instead of an empty pane and no explanation.
func TestWriteBuffersBeforeStart(t *testing.T) {
	l := NewLogger()
	l.Write("first")
	l.Write("second")

	lines := l.Lines()
	if len(lines) != 2 {
		t.Fatalf("buffered %d lines, want 2: %v", len(lines), lines)
	}
	if !strings.HasSuffix(lines[0], "first") || !strings.HasSuffix(lines[1], "second") {
		t.Fatalf("unexpected order or format: %v", lines)
	}
	if !strings.HasPrefix(lines[0], "[") {
		t.Fatalf("lines should carry a time prefix, got %q", lines[0])
	}
}

func TestRingBufferDropsOldest(t *testing.T) {
	l := NewLogger()
	total := maxLines + 50
	for i := 0; i < total; i++ {
		l.Write(fmt.Sprintf("line-%03d", i))
	}

	lines := l.Lines()
	if len(lines) != maxLines {
		t.Fatalf("ring holds %d lines, want the cap %d", len(lines), maxLines)
	}
	if strings.Contains(strings.Join(lines, "\n"), "line-049") {
		t.Fatal("the evicted lines are still in the ring")
	}
	if last := lines[len(lines)-1]; !strings.HasSuffix(last, fmt.Sprintf("line-%03d", total-1)) {
		t.Fatalf("newest line = %q, want line-%03d", last, total-1)
	}
}

// TestLinesReturnsACopy: the UI polls Lines while a connection goroutine may be
// appending, so handing out the live slice would race the reader.
func TestLinesReturnsACopy(t *testing.T) {
	l := NewLogger()
	l.Write("one")

	snapshot := l.Lines()
	l.Write("two")

	if len(snapshot) != 1 {
		t.Fatalf("the snapshot changed after it was taken: %v", snapshot)
	}
}

// TestOnLineMayLogAgainWithoutDeadlocking proves the callback runs outside the
// logger's mutex. The app wires it to a UI event, and the UI logs feedback of
// its own; if Write held the lock across the call, that re-entry would hang the
// whole application on the first log line.
func TestOnLineMayLogAgainWithoutDeadlocking(t *testing.T) {
	l := NewLogger()
	var mu sync.Mutex
	var seen []string
	l.OnLine(func(line string) {
		mu.Lock()
		seen = append(seen, line)
		mu.Unlock()
		if strings.Contains(line, "trigger") {
			l.Write("re-entrant")
		}
	})

	done := make(chan struct{})
	go func() {
		l.Write("trigger")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Write deadlocked when the OnLine callback logged again")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("callback saw %d lines, want 2 (the write plus the re-entry): %v", len(seen), seen)
	}
	if !strings.Contains(seen[1], "re-entrant") {
		t.Fatalf("the re-entrant line never reached the callback: %v", seen)
	}
}

func TestConcurrentWritesKeepTheRingIntact(t *testing.T) {
	l := NewLogger()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				l.Write(fmt.Sprintf("g%d-%d", g, i))
			}
		}(g)
	}
	wg.Wait()

	if got := len(l.Lines()); got != maxLines {
		t.Fatalf("ring holds %d lines after 1600 writes, want the cap %d", got, maxLines)
	}
}

func TestStartWritesToFileAndStopClosesIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gravel-test.log")
	l := NewLogger()
	if err := l.Start(path); err != nil {
		t.Fatalf("Start: %v", err)
	}
	l.Write("hello file")
	l.Stop()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	s := string(data)
	for _, want := range []string{"=== Gravel Session Log ===", "hello file", "=== Session ended:"} {
		if !strings.Contains(s, want) {
			t.Fatalf("log file is missing %q:\n%s", want, s)
		}
	}

	// Stop closes the file; the Logs tab must keep collecting regardless.
	l.Write("after stop")
	lines := l.Lines()
	if len(lines) == 0 || !strings.HasSuffix(lines[len(lines)-1], "after stop") {
		t.Fatalf("buffering stopped along with the file: %v", lines)
	}
}

// TestRestartReplacesTheFile covers a second Start: the previous handle is
// closed and only the new file receives later lines.
func TestRestartReplacesTheFile(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.log")
	second := filepath.Join(dir, "second.log")

	l := NewLogger()
	if err := l.Start(first); err != nil {
		t.Fatalf("Start first: %v", err)
	}
	l.Write("to first")
	if err := l.Start(second); err != nil {
		t.Fatalf("Start second: %v", err)
	}
	l.Write("to second")
	l.Stop()

	a, err := os.ReadFile(first)
	if err != nil {
		t.Fatalf("read first: %v", err)
	}
	if strings.Contains(string(a), "to second") {
		t.Fatal("the replaced file kept receiving lines; its handle was not closed")
	}
	b, err := os.ReadFile(second)
	if err != nil {
		t.Fatalf("read second: %v", err)
	}
	if !strings.Contains(string(b), "to second") {
		t.Fatalf("the new file did not receive the line:\n%s", b)
	}
}

// TestWriteFailureKeepsTheMemoryRing covers the disk-full and vanished-file
// paths: after a write fails the logger must stop pushing at that handle, but
// the Logs tab and the live callback must still see every line.
func TestWriteFailureKeepsTheMemoryRing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gravel.log")
	l := NewLogger()
	if err := l.Start(path); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var seen []string
	l.OnLine(func(line string) { seen = append(seen, line) })

	l.Write("written while the disk works")

	// Closing the handle from under the logger makes the next write fail exactly
	// the way a full disk or a deleted file would, without needing either.
	if err := l.file.Close(); err != nil {
		t.Fatalf("close fixture: %v", err)
	}
	for i := 0; i < 3; i++ {
		l.Write(fmt.Sprintf("after the failure %d", i))
	}

	if l.enabled || l.file != nil {
		t.Errorf("a failing write must detach the file (enabled=%v file=%v), not retry it forever", l.enabled, l.file)
	}
	lines := l.Lines()
	if len(lines) != 4 {
		t.Fatalf("ring kept %d lines, want all 4: %v", len(lines), lines)
	}
	for i, want := range []string{"written while the disk works", "after the failure 0", "after the failure 1", "after the failure 2"} {
		if !strings.HasSuffix(lines[i], want) {
			t.Errorf("line %d = %q, want it to end with %q", i, lines[i], want)
		}
	}
	if len(seen) != 4 {
		t.Errorf("the live callback fired %d times, want 4 - the Logs tab would otherwise go quiet", len(seen))
	}
	l.Stop()
}
