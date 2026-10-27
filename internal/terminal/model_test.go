package terminal

import (
	"image/color"
	"strings"
	"testing"
)

func cellAt(t *testing.T, term *Terminal, x, y int) Cell {
	t.Helper()
	s := term.Snapshot()
	if y*s.Cols+x >= len(s.Cells) {
		t.Fatalf("cell (%d,%d) out of range (cols=%d rows=%d)", x, y, s.Cols, s.Rows)
	}
	return s.Cells[y*s.Cols+x]
}

func TestPlainTextWrite(t *testing.T) {
	tr := New(80, 24)
	if _, err := tr.Write([]byte("hello world\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := cellAt(t, tr, 0, 0).Char; got != 'h' {
		t.Fatalf("cell(0,0) = %q, want 'h'", got)
	}
	if !strings.Contains(tr.PlainText(), "hello world") {
		t.Fatalf("PlainText missing text: %q", tr.PlainText())
	}
}

func TestANSIColorAndBold(t *testing.T) {
	tr := New(80, 24)
	// Red foreground, then bold green, then reset.
	seq := "\x1b[31mR\x1b[0m\x1b[1;32mG\x1b[0m\x1b[44mB\x1b[0m"
	if _, err := tr.Write([]byte(seq)); err != nil {
		t.Fatalf("write: %v", err)
	}

	red := cellAt(t, tr, 0, 0)
	if red.Char != 'R' {
		t.Fatalf("cell0 char = %q, want R", red.Char)
	}
	if want := ansi16[1]; toRGBA(red.FG) != want {
		t.Fatalf("red FG = %v, want %v", toRGBA(red.FG), want)
	}

	green := cellAt(t, tr, 1, 0)
	if green.Char != 'G' || !green.Bold {
		t.Fatalf("cell1 = %+v, want bold G", green)
	}
	if want := ansi16[2+8]; toRGBA(green.FG) != want {
		// bold green (FG 2) brightens to index 10
		t.Fatalf("bold green FG = %v, want %v", toRGBA(green.FG), want)
	}

	blueBg := cellAt(t, tr, 2, 0)
	if blueBg.Char != 'B' {
		t.Fatalf("cell2 char = %q, want B", blueBg.Char)
	}
	if want := ansi16[4]; toRGBA(blueBg.BG) != want {
		t.Fatalf("blue BG = %v, want %v", toRGBA(blueBg.BG), want)
	}
}

func TestCursorPositioning(t *testing.T) {
	tr := New(80, 24)
	// Move to row 10, column 5 (1-based) then print X.
	if _, err := tr.Write([]byte("\x1b[10;5HX")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := cellAt(t, tr, 4, 9).Char; got != 'X' {
		t.Fatalf("cell(4,9) = %q, want X", got)
	}
}

func TestResize(t *testing.T) {
	tr := New(80, 24)
	tr.Resize(120, 40)
	cols, rows := tr.Size()
	if cols != 120 || rows != 40 {
		t.Fatalf("size = %dx%d, want 120x40", cols, rows)
	}
	s := tr.Snapshot()
	if len(s.Cells) != 120*40 {
		t.Fatalf("cells = %d, want %d", len(s.Cells), 120*40)
	}
}

func TestCubeAndGrayColors(t *testing.T) {
	// Spot-check palette math without touching vt10x internals.
	if got := mapColor(16); toRGBA(got) != (color.RGBA{0, 0, 0, 0xFF}) {
		t.Fatalf("cube base 16 = %v", toRGBA(got))
	}
	// index 231 == full white corner of cube (5,5,5)
	if got := mapColor(231); toRGBA(got) != (color.RGBA{0xFF, 0xFF, 0xFF, 0xFF}) {
		t.Fatalf("cube 231 = %v", toRGBA(got))
	}
	// 232 is darkest gray (8), 255 lightest (238)
	if got := mapColor(232); toRGBA(got).R != 8 {
		t.Fatalf("gray 232 R = %d, want 8", toRGBA(got).R)
	}
	if got := mapColor(255); toRGBA(got).R != 238 {
		t.Fatalf("gray 255 R = %d, want 238", toRGBA(got).R)
	}
}

func toRGBA(c color.Color) color.RGBA {
	r, g, b, a := c.RGBA()
	return color.RGBA{uint8(r >> 8), uint8(g >> 8), uint8(b >> 8), uint8(a >> 8)}
}
