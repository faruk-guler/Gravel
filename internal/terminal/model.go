package terminal

import (
	"image/color"
	"strings"
	"sync"

	"github.com/hinshun/vt10x"
)

// vt10x attribute bits (mirrored from the package's unexported constants).
const (
	attrReverse   = 1 << 0
	attrUnderline = 1 << 1
	attrBold      = 1 << 2
)

// Cell is a single terminal glyph with resolved presentation attributes.
type Cell struct {
	Char      rune
	FG, BG    color.Color
	Bold      bool
	Underline bool
}

// Snapshot is an immutable rendering frame of the terminal at a point in time.
type Snapshot struct {
	Cols, Rows int
	Cells      []Cell // row-major, len == Cols*Rows
	CursorX    int
	CursorY    int
	CursorVis  bool
	Title      string
}

// Terminal wraps a vt10x state machine and exposes thread-safe writes and
// render snapshots. A single internal mutex serializes all access to the
// underlying emulator.
type Terminal struct {
	mu   sync.Mutex
	vt   vt10x.Terminal
	cols int
	rows int
}

// New returns a Terminal sized cols x rows (defaults to 80x24 when non-positive).
func New(cols, rows int) *Terminal {
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	return &Terminal{
		vt:   vt10x.New(vt10x.WithSize(cols, rows)),
		cols: cols,
		rows: rows,
	}
}

// Write feeds raw bytes from the PTY into the emulator.
func (t *Terminal) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.vt.Write(p)
}

// Resize changes the emulator dimensions.
func (t *Terminal) Resize(cols, rows int) {
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cols, t.rows = cols, rows
	t.vt.Resize(cols, rows)
}

// Size returns the current dimensions.
func (t *Terminal) Size() (cols, rows int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.vt.Size()
}

// Snapshot renders the current screen into a copy for the UI layer.
func (t *Terminal) Snapshot() Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()

	cols, rows := t.vt.Size()
	cells := make([]Cell, cols*rows)
	for y := 0; y < rows; y++ {
		for x := 0; x < cols; x++ {
			g := t.vt.Cell(x, y)
			cells[y*cols+x] = Cell{
				Char:      normalizeRune(g.Char),
				FG:        mapColor(g.FG),
				BG:        mapColor(g.BG),
				Bold:      g.Mode&attrBold != 0,
				Underline: g.Mode&attrUnderline != 0,
			}
		}
	}
	cur := t.vt.Cursor()
	return Snapshot{
		Cols:      cols,
		Rows:      rows,
		Cells:     cells,
		CursorX:   cur.X,
		CursorY:   cur.Y,
		CursorVis: t.vt.CursorVisible(),
		Title:     t.vt.Title(),
	}
}

// PlainText returns the visible screen as text (used for clipboard selection
// and headless assertions).
func (t *Terminal) PlainText() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.vt.String()
}

func normalizeRune(r rune) rune {
	if r == 0 {
		return ' '
	}
	return r
}

// Lines returns the visible screen rows as strings with trailing spaces trimmed
// and trailing blank rows removed. Useful for text rendering (CLI) and tests.
func (s Snapshot) Lines() []string {
	out := make([]string, 0, s.Rows)
	for y := 0; y < s.Rows; y++ {
		var b strings.Builder
		for x := 0; x < s.Cols; x++ {
			b.WriteRune(s.Cells[y*s.Cols+x].Char)
		}
		out = append(out, strings.TrimRight(b.String(), " "))
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}
