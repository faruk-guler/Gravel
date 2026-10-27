package logging

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// maxLines bounds the in-memory ring the Logs tab reads, so a long-running
// session cannot grow the buffer without limit.
const maxLines = 500

// Logger handles connection and session logging. Every line goes to the file
// (when started), to a fixed-size memory ring the UI can snapshot, and to an
// optional callback the app wires to a live "log:line" event.
type Logger struct {
	mu      sync.Mutex
	file    *os.File
	enabled bool
	lines   []string
	onLine  func(string)
}

// NewLogger creates a new logger
func NewLogger() *Logger {
	return &Logger{}
}

// Start begins logging to a file
func (l *Logger) Start(filePath string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.file != nil {
		l.file.Close()
	}

	f, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}

	l.file = f
	l.enabled = true

	// Write header
	header := fmt.Sprintf("\n=== Gravel Session Log ===\n=== Started: %s ===\n\n",
		time.Now().Format("2006-01-02 15:04:05"))
	l.file.WriteString(header)

	return nil
}

// Stop stops logging
func (l *Logger) Stop() {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.file != nil {
		l.file.WriteString(fmt.Sprintf("\n=== Session ended: %s ===\n",
			time.Now().Format("2006-01-02 15:04:05")))
		l.file.Close()
		l.file = nil
	}
	l.enabled = false
}

// Write writes a log entry. Buffering happens even before Start succeeds, so
// the Logs tab still shows everything when the file cannot be opened.
func (l *Logger) Write(text string) {
	line := fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05"), text)

	l.mu.Lock()
	l.lines = append(l.lines, line)
	if len(l.lines) > maxLines {
		l.lines = append(l.lines[:0:0], l.lines[len(l.lines)-maxLines:]...)
	}
	if l.enabled && l.file != nil {
		if _, ferr := l.file.WriteString(line + "\n"); ferr != nil {
			// Best-effort: disable file logging so we do not keep retrying
			// on every write when the disk is full or the file is gone.
			l.file = nil
			l.enabled = false
		}
	}
	fn := l.onLine
	l.mu.Unlock()

	// Callbacks run outside the lock: the UI handler may log again.
	if fn != nil {
		fn(line)
	}
}

// OnLine registers a callback invoked for every Write. It replaces any
// previous one and runs on the writing goroutine.
func (l *Logger) OnLine(fn func(string)) {
	l.mu.Lock()
	l.onLine = fn
	l.mu.Unlock()
}

// Lines returns the buffered log lines, oldest first.
func (l *Logger) Lines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}
