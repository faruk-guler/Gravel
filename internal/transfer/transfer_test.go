package transfer

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestQueueState(t *testing.T) {
	// Initialize with nil client to test queue logic only
	q := NewTransferQueue(nil, 2)

	if len(q.GetItems()) != 0 {
		t.Error("Queue should be empty")
	}

	// Add an item
	item := q.AddUpload("local", "remote")

	// Since client is nil, processNext should return early or fail gracefully without processing
	// In the current implementation, processNext checks `q.client == nil` and returns.
	// So the item should remain in StatusPending.
	if item.Status != StatusPending {
		t.Errorf("Expected pending, got %s", item.Status)
	}

	if len(q.GetItems()) != 1 {
		t.Errorf("Expected 1 item, got %d", len(q.GetItems()))
	}

	// Test cancellation
	q.CancelItem(item.ID)
	// CancelItem finds the item and sets status to Cancelled
	if item.Status != StatusCancelled {
		t.Errorf("Expected cancelled, got %s", item.Status)
	}
}

// These regression tests guard the queue's cancel path and its progress
// bookkeeping, both of which the UI touches concurrently: the Logs/queue poll
// reads every item while a transfer goroutine is still writing its counters.

// TestCancelRunningItemIsIdempotent guards the double close. A running item's
// cancel channel is closed to signal the pump, so a second click - or the poll
// re-rendering the row and the user clicking again - must not close it twice.
func TestCancelRunningItemIsIdempotent(t *testing.T) {
	q := NewTransferQueue(nil, 1)
	item := q.AddUpload("local", "remote")

	q.mu.Lock()
	item.Status = StatusInProgress
	q.mu.Unlock()

	q.CancelItem(item.ID)
	q.CancelItem(item.ID) // must not panic

	select {
	case <-item.cancel:
	default:
		t.Error("running item was not signalled to stop")
	}
}

// TestCancellingAFinishedItemKeepsItsStatus: a row that already completed must
// not be rewritten to "Cancelled" by a late click on a stale listing.
func TestCancellingAFinishedItemKeepsItsStatus(t *testing.T) {
	q := NewTransferQueue(nil, 1)
	item := q.AddUpload("local", "remote")

	q.mu.Lock()
	item.Status = StatusCompleted
	q.mu.Unlock()

	q.CancelItem(item.ID)

	if item.Status != StatusCompleted {
		t.Errorf("completed item was overwritten with %s", item.Status)
	}
}

// TestCancelledTransferIsNotReportedFailed: the pump returns a "cancelled"
// error as soon as it sees the signal. That is the user's own action, so the
// row has to keep reading Cancelled instead of an alarming error line.
func TestCancelledTransferIsNotReportedFailed(t *testing.T) {
	q := NewTransferQueue(nil, 1)
	item := q.AddDownload("/remote/file", "local")

	q.mu.Lock()
	item.Status = StatusInProgress
	q.mu.Unlock()

	q.CancelItem(item.ID)
	q.finish(item, errors.New("cancelled"))

	if item.Status != StatusCancelled {
		t.Fatalf("status = %s, want Cancelled", item.Status)
	}
	if item.ErrorMsg != "" {
		t.Errorf("a cancel is not a failure, but ErrorMsg = %q", item.ErrorMsg)
	}
	if item.EndTime == "" {
		t.Error("finish should still stamp the end time")
	}
}

// TestFailedTransferStillReportsTheError keeps the other side of that switch.
func TestFailedTransferStillReportsTheError(t *testing.T) {
	q := NewTransferQueue(nil, 1)
	item := q.AddUpload("local", "remote")

	q.mu.Lock()
	item.Status = StatusInProgress
	q.mu.Unlock()

	q.finish(item, errors.New("read: connection reset"))

	if item.Status != StatusFailed {
		t.Fatalf("status = %s, want Failed", item.Status)
	}
	if item.ErrorMsg == "" {
		t.Error("a real failure must carry its message to the queue row")
	}
}

// TestGetItemsReturnsSnapshots: handing out the live pointers meant a poll
// could read an item while its transfer goroutine was mid-update. Callers get
// copies, so later mutations cannot rewrite an already rendered row.
func TestGetItemsReturnsSnapshots(t *testing.T) {
	q := NewTransferQueue(nil, 1)
	item := q.AddUpload("local", "remote")

	snap := q.GetItems()[0]
	if snap == item {
		t.Fatal("GetItems must not hand out the live item")
	}
	q.setTotal(item, 1000)
	q.addProgress(item, 250)

	if snap.TotalBytes != 0 || snap.TransferBytes != 0 {
		t.Errorf("snapshot changed after it was taken: %+v", snap)
	}
	latest := q.GetItems()[0]
	if latest.Progress() != 25 {
		t.Errorf("progress = %d, want 25", latest.Progress())
	}
}

// TestConcurrentProgressAndPolling is the shape of the real workload: the UI
// polls every item while a transfer advances its counters. Run under -race it
// fails if either side ever touches an item outside the queue lock.
func TestConcurrentProgressAndPolling(t *testing.T) {
	q := NewTransferQueue(nil, 1)
	item := q.AddUpload("local", "remote")
	q.setTotal(item, 32*1024*1024)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			q.addProgress(item, 4096)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			for _, it := range q.GetItems() {
				_ = it.Progress()
				_ = it.Speed(time.Time{})
				_ = it.ETA(time.Time{})
			}
		}
	}()
	wg.Wait()

	if got := q.GetItems()[0].TransferBytes; got != 2000*4096 {
		t.Errorf("transferred %d bytes, want %d", got, 2000*4096)
	}
}
