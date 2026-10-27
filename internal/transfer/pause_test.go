package transfer

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pkg/sftp"

	"gravel/internal/sftptest"
)

// Pause and resume are the reason StatusPaused exists at all: a row stops at a
// chunk boundary, keeps the bytes it already moved, and the next pump reopens
// both sides at that offset. The queue bookkeeping comes first, then the
// byte-exact round trip over a real SFTP server.
//
// The end-to-end cases deliberately start with a clientless queue: AddDownload
// would otherwise hand the row to a live pump, and the staged offsets would be
// overwritten mid-copy. SetClient just before ResumeItem starts the real work.

func TestPauseAndResumePendingItem(t *testing.T) {
	q := NewTransferQueue(nil, 1)
	item := q.AddUpload("local", "remote")

	q.PauseItem(item.ID)
	if got := statusOf(t, q, item.ID); got != StatusPaused {
		t.Fatalf("after pause: %s, want Paused", got)
	}
	pending, paused, active, _, _ := q.GetStats()
	if paused != 1 || pending != 0 || active != 0 {
		t.Fatalf("stats count a paused row as pending=%d paused=%d active=%d", pending, paused, active)
	}

	q.ResumeItem(item.ID)
	if got := statusOf(t, q, item.ID); got != StatusPending {
		t.Fatalf("after resume: %s, want Pending", got)
	}
}

// TestCancellingAPausedItemIsNotRevived: the row stays Cancelled even if a stray
// Resume click arrives afterwards, and it must not be re-queued.
func TestCancellingAPausedItemIsNotRevived(t *testing.T) {
	q := NewTransferQueue(nil, 1)
	item := q.AddDownload("/remote/file", "local")

	q.PauseItem(item.ID)
	q.CancelItem(item.ID)
	q.ResumeItem(item.ID)

	if got := statusOf(t, q, item.ID); got != StatusCancelled {
		t.Fatalf("status = %s, want Cancelled", got)
	}
	select {
	case <-item.cancel:
	default:
		t.Error("a paused row must still receive the cancel signal")
	}
}

// TestFinishKeepsPausedRowOpen pins that a pause is not an outcome: no error
// text, no end time, and the concurrency slot is released so other rows run.
func TestFinishKeepsPausedRowOpen(t *testing.T) {
	q := NewTransferQueue(nil, 1)
	item := q.AddUpload("local", "remote")

	q.mu.Lock()
	item.Status = StatusInProgress
	item.running = true
	q.active++
	q.mu.Unlock()

	q.finish(item, errPaused)

	if item.Status != StatusPaused {
		t.Fatalf("status = %s, want Paused", item.Status)
	}
	if item.Error != nil || item.ErrorMsg != "" {
		t.Errorf("a pause is not a failure, got Error=%v ErrorMsg=%q", item.Error, item.ErrorMsg)
	}
	if item.EndTime != "" {
		t.Errorf("paused row got an end time: %s", item.EndTime)
	}
	if item.running {
		t.Error("paused row must release its pump so a resume can start a new one")
	}
	if _, _, active, _, _ := q.GetStats(); active != 0 {
		t.Errorf("active slots = %d, want 0", active)
	}
}

// TestPausedPumpStopsAndKeepsPartial: the copy loop leaves the file it created
// alone, because that file is the resume point - unlike a real failure.
func TestPausedPumpStopsAndKeepsPartial(t *testing.T) {
	cli, root := sftptest.NewClient(t)
	remote := root + "/source.bin"
	writeRemote(t, cli, remote, pattern(100*1024))

	q := NewTransferQueue(nil, 1)
	local := filepath.Join(t.TempDir(), "partial.bin")
	item := q.AddDownload(remote, local)
	q.PauseItem(item.ID)

	if err := q.doDownload(cli, item); !errors.Is(err, errPaused) {
		t.Fatalf("doDownload err = %v, want the paused signal", err)
	}
	fi, err := os.Stat(local)
	if err != nil {
		t.Fatalf("a pause must not delete the partial file: %v", err)
	}
	if fi.Size() != 0 || q.bytesDone(item) != 0 {
		t.Errorf("fresh pause moved bytes it should not have: file=%d counter=%d", fi.Size(), q.bytesDone(item))
	}
}

// TestResumedDownloadIsByteExact is the point of the whole feature: the counter
// and the partial file both say 64 KB arrived, so the copy continues from there
// instead of restarting - and still lands the exact content.
func TestResumedDownloadIsByteExact(t *testing.T) {
	cli, root := sftptest.NewClient(t)
	content := pattern(200 * 1024)
	remote := root + "/full.bin"
	writeRemote(t, cli, remote, content)

	local := filepath.Join(t.TempDir(), "resumed.bin")
	done := int64(64 * 1024)
	if err := os.WriteFile(local, content[:done], 0o600); err != nil {
		t.Fatalf("seed partial: %v", err)
	}

	q := NewTransferQueue(nil, 1)
	item := q.AddDownload(remote, local)
	q.PauseItem(item.ID)
	q.setDone(item, done)

	q.SetClient(cli)
	q.ResumeItem(item.ID)
	waitStatus(t, q, item.ID, StatusCompleted)

	got, err := os.ReadFile(local)
	if err != nil {
		t.Fatalf("read downloaded: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("resumed download produced %d bytes, want the exact %d-byte source", len(got), len(content))
	}
	if int64(len(got)) < done {
		t.Fatalf("output is shorter than the partial it was supposed to extend")
	}
	// Counters are read from the poll's own snapshot, the way the UI does it -
	// never off the live item the pump was writing to.
	latest := snapshotOf(t, q, item.ID)
	if latest.TotalBytes != int64(len(content)) || latest.TransferBytes != int64(len(content)) {
		t.Errorf("counters: total=%d done=%d, want both %d", latest.TotalBytes, latest.TransferBytes, len(content))
	}
}

// TestResumedUploadIsByteExact covers the other direction, where the remote file
// holds the partial.
func TestResumedUploadIsByteExact(t *testing.T) {
	cli, root := sftptest.NewClient(t)
	content := pattern(200 * 1024)
	local := filepath.Join(t.TempDir(), "upload.bin")
	if err := os.WriteFile(local, content, 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}

	remote := root + "/remote-partial.bin"
	done := int64(96 * 1024)
	writeRemote(t, cli, remote, content[:done])

	q := NewTransferQueue(nil, 1)
	item := q.AddUpload(local, remote)
	q.PauseItem(item.ID)
	q.setDone(item, done)

	q.SetClient(cli)
	q.ResumeItem(item.ID)
	waitStatus(t, q, item.ID, StatusCompleted)

	f, err := cli.Open(remote)
	if err != nil {
		t.Fatalf("open uploaded: %v", err)
	}
	defer f.Close()
	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read uploaded: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("resumed upload does not match the source (%d bytes arrived)", len(got))
	}
}

// TestResumedTransferDoesNotTrustALostPartial guards the stitch-two-files
// failure: the counter remembers 64 KB, but the partial it describes is gone, so
// the copy starts from scratch instead of writing 120 KB onto an empty file.
func TestResumedTransferDoesNotTrustALostPartial(t *testing.T) {
	cli, root := sftptest.NewClient(t)
	content := pattern(120 * 1024)
	remote := root + "/again.bin"
	writeRemote(t, cli, remote, content)

	q := NewTransferQueue(nil, 1)
	local := filepath.Join(t.TempDir(), "vanishing.bin")
	item := q.AddDownload(remote, local)
	q.PauseItem(item.ID)
	q.setDone(item, 64*1024) // the file it describes was never written

	q.SetClient(cli)
	q.ResumeItem(item.ID)
	waitStatus(t, q, item.ID, StatusCompleted)

	got, err := os.ReadFile(local)
	if err != nil {
		t.Fatalf("read downloaded: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("a lost partial gave %d bytes instead of a clean %d-byte copy", len(got), len(content))
	}
	if q.bytesDone(item) != int64(len(content)) {
		t.Errorf("counter = %d, want %d after the restart", q.bytesDone(item), len(content))
	}
}

// TestResumedTransferCutsBackAStaleTail covers the mirror case of the lost
// partial: the file on disk is LONGER than the counter claims, so the extra bytes
// belong to an earlier attempt and the resumed copy must still end at the exact
// length of the source.
func TestResumedTransferCutsBackAStaleTail(t *testing.T) {
	cli, root := sftptest.NewClient(t)
	content := pattern(200 * 1024)
	remote := root + "/tail.bin"
	writeRemote(t, cli, remote, content)

	local := filepath.Join(t.TempDir(), "overgrown.bin")
	done := int64(40 * 1024)
	stale := append(pattern(int(done)), bytes.Repeat([]byte{0xAA}, 80*1024)...)
	if err := os.WriteFile(local, stale, 0o600); err != nil {
		t.Fatalf("seed overgrown partial: %v", err)
	}

	q := NewTransferQueue(nil, 1)
	item := q.AddDownload(remote, local)
	q.PauseItem(item.ID)
	q.setDone(item, done)

	q.SetClient(cli)
	q.ResumeItem(item.ID)
	waitStatus(t, q, item.ID, StatusCompleted)

	got, err := os.ReadFile(local)
	if err != nil {
		t.Fatalf("read downloaded: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("a stale tail survived the resume: %d bytes, want the exact %d-byte source", len(got), len(content))
	}
}

func statusOf(t *testing.T, q *TransferQueue, id int) TransferStatus {
	t.Helper()
	return snapshotOf(t, q, id).Status
}

func snapshotOf(t *testing.T, q *TransferQueue, id int) *TransferItem {
	t.Helper()
	for _, it := range q.GetItems() {
		if it.ID == id {
			return it
		}
	}
	t.Fatalf("item %d is not in the queue", id)
	return nil
}

func waitStatus(t *testing.T, q *TransferQueue, id int, want TransferStatus) {
	t.Helper()
	for waited := 0; waited < 600; waited++ {
		s := statusOf(t, q, id)
		if s == want {
			return
		}
		if s == StatusFailed {
			for _, it := range q.GetItems() {
				if it.ID == id {
					t.Fatalf("transfer failed: %s", it.ErrorMsg)
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("item %d never reached %s", id, want)
}

// pattern is deterministic content, so a resume that copied the wrong range
// shows up as a byte mismatch rather than a file of merely the right length.
func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

func writeRemote(t *testing.T, cli *sftp.Client, path string, data []byte) {
	t.Helper()
	f, err := cli.Create(path)
	if err != nil {
		t.Fatalf("create remote %s: %v", path, err)
	}
	if _, err := f.Write(data); err != nil {
		t.Fatalf("write remote %s: %v", path, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close remote %s: %v", path, err)
	}
}
