package transfer

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/pkg/sftp"
)

// TransferDirection indicates upload or download
type TransferDirection int

const (
	Download TransferDirection = iota
	Upload
)

// TransferStatus represents the state of a transfer item
type TransferStatus int

const (
	StatusPending TransferStatus = iota
	StatusInProgress
	StatusPaused
	StatusCompleted
	StatusFailed
	StatusCancelled
)

func (s TransferStatus) String() string {
	switch s {
	case StatusPending:
		return "Pending"
	case StatusInProgress:
		return "Transferring"
	case StatusPaused:
		return "Paused"
	case StatusCompleted:
		return "Completed"
	case StatusFailed:
		return "Failed"
	case StatusCancelled:
		return "Cancelled"
	}
	return "Unknown"
}

// errPaused stops a pump goroutine at the next chunk boundary when the user
// pauses a running transfer. It is deliberately not a failure: the byte counter
// stays where the copy stopped, and ResumeItem continues from that offset.
var errPaused = errors.New("paused")

// TransferItem represents a single file transfer
type TransferItem struct {
	ID            int               `json:"id"`
	FileName      string            `json:"name"`
	RemotePath    string            `json:"remote_path"`
	LocalPath     string            `json:"local_path"`
	Direction     TransferDirection `json:"direction"`
	TotalBytes    int64             `json:"total_bytes"`
	TransferBytes int64             `json:"transfer_bytes"`
	Status        TransferStatus    `json:"status"`
	Error         error             `json:"-"`
	ErrorMsg      string            `json:"error"`
	StartTime     string            `json:"start_time"`
	EndTime       string            `json:"end_time"`
	cancel        chan struct{}     `json:"-"`
	// cancelled records that the cancel channel was already closed, so a second
	// CancelItem on the same running item cannot panic on a double close.
	cancelled bool
	// running records that a pump goroutine is inside this item's copy loop. A
	// paused item must not be picked up again until that goroutine has left,
	// otherwise two writers would race on the same file offsets.
	running bool
}

// Progress returns the transfer progress as a percentage (0-100)
func (t *TransferItem) Progress() int {
	if t.TotalBytes == 0 {
		return 0
	}
	return int(t.TransferBytes * 100 / t.TotalBytes)
}

// Speed returns current transfer speed in bytes per second
func (t *TransferItem) Speed(startTimeObj time.Time) int64 {
	if startTimeObj.IsZero() || t.TransferBytes == 0 {
		return 0
	}
	elapsed := time.Since(startTimeObj).Seconds()
	if elapsed == 0 {
		return 0
	}
	return int64(float64(t.TransferBytes) / elapsed)
}

// ETA returns estimated time remaining
func (t *TransferItem) ETA(startTimeObj time.Time) string {
	speed := t.Speed(startTimeObj)
	if speed == 0 {
		return "∞"
	}
	remaining := t.TotalBytes - t.TransferBytes
	seconds := remaining / speed
	if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	} else if seconds < 3600 {
		return fmt.Sprintf("%dm %ds", seconds/60, seconds%60)
	}
	return fmt.Sprintf("%dh %dm", seconds/3600, (seconds%3600)/60)
}

// FormatSize formats bytes into human-readable string
func FormatSize(bytes int64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
	)
	switch {
	case bytes >= GB:
		return fmt.Sprintf("%.2f GB", float64(bytes)/float64(GB))
	case bytes >= MB:
		return fmt.Sprintf("%.2f MB", float64(bytes)/float64(MB))
	case bytes >= KB:
		return fmt.Sprintf("%.1f KB", float64(bytes)/float64(KB))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

// FormatSpeed formats speed in bytes/sec to human readable
func FormatSpeed(bytesPerSec int64) string {
	return FormatSize(bytesPerSec) + "/s"
}

// TransferQueue manages a queue of file transfers.
//
// The UI polls GetItems for progress; there is deliberately no change callback,
// because a hook fired from every 32 KB chunk would only be a lock round trip
// on the transfer's hot path.
type TransferQueue struct {
	mu      sync.Mutex
	items   []*TransferItem
	nextID  int
	maxConc int // max concurrent transfers
	active  int
	client  *sftp.Client
}

// NewTransferQueue creates a new transfer queue
func NewTransferQueue(client *sftp.Client, maxConcurrent int) *TransferQueue {
	return &TransferQueue{
		items:   make([]*TransferItem, 0),
		maxConc: maxConcurrent,
		client:  client,
	}
}

// SetClient updates the SFTP client
func (q *TransferQueue) SetClient(client *sftp.Client) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.client = client
}

// AddDownload adds a download to the queue
func (q *TransferQueue) AddDownload(remotePath, localPath string) *TransferItem {
	q.mu.Lock()

	q.nextID++
	item := &TransferItem{
		ID:         q.nextID,
		FileName:   filepath.Base(remotePath),
		RemotePath: remotePath,
		LocalPath:  localPath,
		Direction:  Download,
		Status:     StatusPending,
		cancel:     make(chan struct{}),
	}

	q.items = append(q.items, item)
	q.mu.Unlock()

	go q.processNext()
	return item
}

// AddUpload adds an upload to the queue
func (q *TransferQueue) AddUpload(localPath, remotePath string) *TransferItem {
	q.mu.Lock()

	q.nextID++
	item := &TransferItem{
		ID:         q.nextID,
		FileName:   filepath.Base(localPath),
		RemotePath: remotePath,
		LocalPath:  localPath,
		Direction:  Upload,
		Status:     StatusPending,
		cancel:     make(chan struct{}),
	}

	q.items = append(q.items, item)
	q.mu.Unlock()

	go q.processNext()
	return item
}

// PauseItem stops a queued or running transfer. A pending row simply waits; a
// running one keeps the bytes it already moved, so ResumeItem does not restart
// the file. A row that already finished keeps its real status.
func (q *TransferQueue) PauseItem(id int) {
	q.mu.Lock()
	for _, item := range q.items {
		if item.ID != id {
			continue
		}
		switch item.Status {
		case StatusPending, StatusInProgress:
			item.Status = StatusPaused
		}
		break
	}
	q.mu.Unlock()
}

// ResumeItem puts a paused transfer back in line. If its pump goroutine has not
// noticed the pause yet, that goroutine carries on with the handles it already
// has open; otherwise a fresh pump reopens both sides at the saved offset.
func (q *TransferQueue) ResumeItem(id int) {
	q.mu.Lock()
	restart := false
	for _, item := range q.items {
		if item.ID != id {
			continue
		}
		if item.Status == StatusPaused {
			item.Status = StatusPending
			restart = !item.running
		}
		break
	}
	q.mu.Unlock()

	// Only spawn a pump once the previous writer is gone; processNext skips rows
	// still marked running, so a resume that races a live pump is picked up by
	// that pump's own loop instead.
	if restart {
		go q.processNext()
	}
}

// isPaused asks the lock rather than the pointer: the pump and the UI poll both
// touch the same row.
func (q *TransferQueue) isPaused(item *TransferItem) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return item.Status == StatusPaused
}

// bytesDone reads how much has really been moved, under the lock.
func (q *TransferQueue) bytesDone(item *TransferItem) int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return item.TransferBytes
}

// setDone realigns the counter with the file that is actually on disk before a
// resumed copy decides where to continue.
func (q *TransferQueue) setDone(item *TransferItem, n int64) {
	q.mu.Lock()
	item.TransferBytes = n
	q.mu.Unlock()
}

// CancelItem cancels a queued or running transfer. It is idempotent, and an
// item that already finished keeps its real status - only pending and running
// transfers can be cancelled.
func (q *TransferQueue) CancelItem(id int) {
	q.mu.Lock()

	for _, item := range q.items {
		if item.ID != id {
			continue
		}
		switch item.Status {
		case StatusPending, StatusInProgress, StatusPaused:
			// Close the cancel channel for both Pending and InProgress items so
			// that a transfer goroutine which picks up a just-cancelled Pending
			// item still receives the cancellation signal immediately.
			if !item.cancelled {
				item.cancelled = true
				close(item.cancel)
			}
			item.Status = StatusCancelled
		}
		break
	}
	q.mu.Unlock()
}

// ClearCompleted removes completed/failed/cancelled items
func (q *TransferQueue) ClearCompleted() {
	q.mu.Lock()

	var remaining []*TransferItem
	for _, item := range q.items {
		if item.Status == StatusPending || item.Status == StatusInProgress || item.Status == StatusPaused {
			remaining = append(remaining, item)
		}
	}
	q.items = remaining
	q.mu.Unlock()
}

// GetItems returns a snapshot of all queue items. Each entry is a copy: the
// transfer goroutine advances the byte counters under the queue lock, so
// handing out the live pointers would let a reader catch them mid-write.
func (q *TransferQueue) GetItems() []*TransferItem {
	q.mu.Lock()
	defer q.mu.Unlock()

	result := make([]*TransferItem, 0, len(q.items))
	for _, item := range q.items {
		snapshot := *item
		result = append(result, &snapshot)
	}
	return result
}

// setTotal and addProgress publish the byte counters under the queue lock so
// the progress the UI polls can never be read as a torn value.
func (q *TransferQueue) setTotal(item *TransferItem, total int64) {
	q.mu.Lock()
	item.TotalBytes = total
	q.mu.Unlock()
}

func (q *TransferQueue) addProgress(item *TransferItem, n int64) {
	q.mu.Lock()
	item.TransferBytes += n
	q.mu.Unlock()
}

// GetStats returns queue statistics. Paused rows are counted separately: they
// are neither waiting to start nor occupying a concurrency slot.
func (q *TransferQueue) GetStats() (pending, paused, active, completed, failed int) {
	q.mu.Lock()
	defer q.mu.Unlock()

	for _, item := range q.items {
		switch item.Status {
		case StatusPending:
			pending++
		case StatusPaused:
			paused++
		case StatusInProgress:
			active++
		case StatusCompleted:
			completed++
		case StatusFailed, StatusCancelled:
			failed++
		}
	}
	return
}

func (q *TransferQueue) processNext() {
	for {
		q.mu.Lock()
		if q.client == nil || q.active >= q.maxConc {
			q.mu.Unlock()
			return
		}

		// Find next pending item. A paused row is skipped, and so is one whose
		// pump has not exited yet - its file handles belong to that goroutine.
		var nextItem *TransferItem
		for _, item := range q.items {
			if item.Status == StatusPending && !item.running {
				nextItem = item
				break
			}
		}

		if nextItem == nil {
			q.mu.Unlock()
			return
		}

		startTimeObj := time.Now()
		nextItem.Status = StatusInProgress
		nextItem.running = true
		nextItem.StartTime = startTimeObj.Format(time.RFC3339)
		q.active++
		client := q.client
		q.mu.Unlock()

		// Execute transfer
		var err error
		if nextItem.Direction == Download {
			err = q.doDownload(client, nextItem)
		} else {
			err = q.doUpload(client, nextItem)
		}

		q.finish(nextItem, err)
		// Loop back to pick up the next pending item rather than calling
		// processNext recursively. Recursive calls accumulate a goroutine per
		// completed transfer when the queue drains slowly, and can exhaust the
		// stack if many items complete back-to-back.
	}
}

// finish records a transfer's outcome and frees its concurrency slot. A
// cancelled row keeps the Cancelled status the user set: the pump returns a
// "cancelled" error precisely because it noticed that signal, and turning it
// into "Failed - error: cancelled" would report a mis-click as a broken link.
// A paused row keeps its counters and gets no end time, because it is not over.
func (q *TransferQueue) finish(item *TransferItem, err error) {
	q.mu.Lock()
	q.active--
	item.running = false
	paused := item.Status == StatusPaused || errors.Is(err, errPaused)
	switch {
	case paused:
		item.Status = StatusPaused
	case item.Status == StatusCancelled:
	case err != nil:
		item.Status = StatusFailed
		item.Error = err
		item.ErrorMsg = err.Error()
	default:
		item.Status = StatusCompleted
	}
	if !paused {
		item.EndTime = time.Now().Format(time.RFC3339)
	}
	q.mu.Unlock()
}

func (q *TransferQueue) doDownload(client *sftp.Client, item *TransferItem) (err error) {
	// Bytes already on disk: 0 for a fresh row, and where a pause stopped for a
	// resumed one. Both sides are opened around this offset.
	done := q.bytesDone(item)

	// Get remote file info for size
	info, err := client.Stat(item.RemotePath)
	if err != nil {
		return fmt.Errorf("stat remote: %w", err)
	}
	q.setTotal(item, info.Size())

	// The counter is only as good as the file it describes. If the partial was
	// deleted or shortened behind our back, continuing from its real length beats
	// stitching unrelated bytes onto the end of it.
	if fi, serr := os.Stat(item.LocalPath); serr != nil {
		done = 0
	} else if fi.Size() < done {
		done = fi.Size()
	}
	if q.bytesDone(item) != done {
		q.setDone(item, done)
	}

	// Open remote file
	src, err := client.Open(item.RemotePath)
	if err != nil {
		return fmt.Errorf("open remote: %w", err)
	}
	defer src.Close()
	if done > 0 {
		if _, err := src.Seek(done, io.SeekStart); err != nil {
			return fmt.Errorf("seek remote: %w", err)
		}
	}

	// Create local file - or continue the one a pause left behind. The offset is
	// set on the handle instead of opening with O_APPEND: the copy issues plain
	// Writes, and on a remote handle the protocol carries an explicit offset that
	// an append flag would not move.
	flags := os.O_WRONLY | os.O_CREATE
	if done == 0 {
		flags |= os.O_TRUNC
	}
	dst, err := os.OpenFile(item.LocalPath, flags, 0o666)
	if err != nil {
		return fmt.Errorf("create local: %w", err)
	}
	defer dst.Close()
	if done > 0 {
		if _, err := dst.Seek(done, io.SeekStart); err != nil {
			return fmt.Errorf("seek local: %w", err)
		}
		// Anything the partial holds beyond the counter is somebody else's bytes;
		// dropping it keeps a resumed file exactly the length of the remote one.
		if err := dst.Truncate(done); err != nil {
			return fmt.Errorf("truncate local: %w", err)
		}
	}

	// Cleanup on error. A pause is not an error and its partial file is exactly
	// what the resume needs, so only a real failure removes it.
	defer func() {
		if err != nil && !errors.Is(err, errPaused) {
			// Close file before removing
			dst.Close()
			os.Remove(item.LocalPath)
		}
	}()

	// Transfer with progress tracking
	buf := make([]byte, 32*1024)
	for {
		select {
		case <-item.cancel:
			return fmt.Errorf("cancelled")
		default:
		}
		if q.isPaused(item) {
			return errPaused
		}

		n, readErr := src.Read(buf)
		if n > 0 {
			_, writeErr := dst.Write(buf[:n])
			if writeErr != nil {
				return fmt.Errorf("write: %w", writeErr)
			}
			q.addProgress(item, int64(n))
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return fmt.Errorf("read: %w", readErr)
		}
	}
	return nil
}

func (q *TransferQueue) doUpload(client *sftp.Client, item *TransferItem) (err error) {
	// Bytes already delivered to the remote side: 0 for a fresh row, and where a
	// pause stopped for a resumed one.
	done := q.bytesDone(item)

	// Get local file info
	info, err := os.Stat(item.LocalPath)
	if err != nil {
		return fmt.Errorf("stat local: %w", err)
	}
	q.setTotal(item, info.Size())

	// The remote length is the only trustworthy offset: if that file was removed
	// or truncated meanwhile, appending at the remembered position would produce
	// content that is neither the old nor the new file.
	if done > 0 {
		if fi, serr := client.Stat(item.RemotePath); serr != nil {
			done = 0
		} else if fi.Size() < done {
			done = fi.Size()
		}
		if q.bytesDone(item) != done {
			q.setDone(item, done)
		}
	}

	// Open local file
	src, err := os.Open(item.LocalPath)
	if err != nil {
		return fmt.Errorf("open local: %w", err)
	}
	defer src.Close()
	if done > 0 {
		if _, err := src.Seek(done, io.SeekStart); err != nil {
			return fmt.Errorf("seek local: %w", err)
		}
	}

	// Create remote file - or continue the one a pause left behind, at the offset
	// the protocol writes will carry.
	flags := os.O_WRONLY | os.O_CREATE
	if done == 0 {
		flags |= os.O_TRUNC
	}
	dst, err := client.OpenFile(item.RemotePath, flags)
	if err != nil {
		return fmt.Errorf("create remote: %w", err)
	}
	defer dst.Close()
	if done > 0 {
		if _, err := dst.Seek(done, io.SeekStart); err != nil {
			return fmt.Errorf("seek remote: %w", err)
		}
		// Same rule as the download side: the counter decides where the resumed
		// copy ends, so a remote file left longer by an earlier attempt is cut back.
		if err := dst.Truncate(done); err != nil {
			return fmt.Errorf("truncate remote: %w", err)
		}
	}

	// Cleanup on error; a pause keeps its partial remote file as the resume point
	defer func() {
		if err != nil && !errors.Is(err, errPaused) {
			dst.Close()
			client.Remove(item.RemotePath)
		}
	}()

	// Transfer with progress tracking
	buf := make([]byte, 32*1024)
	for {
		select {
		case <-item.cancel:
			return fmt.Errorf("cancelled")
		default:
		}
		if q.isPaused(item) {
			return errPaused
		}

		n, readErr := src.Read(buf)
		if n > 0 {
			_, writeErr := dst.Write(buf[:n])
			if writeErr != nil {
				return fmt.Errorf("write: %w", writeErr)
			}
			q.addProgress(item, int64(n))
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return fmt.Errorf("read: %w", readErr)
		}
	}
	return nil
}
