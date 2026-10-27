package terminal

import (
	"image/color"

	"github.com/hinshun/vt10x"
)

// DefaultFGColor and DefaultBGColor are used when a cell carries the vt10x
// "default" color, i.e. whatever the terminal theme dictates.
var (
	DefaultFGColor = color.RGBA{R: 0xE0, G: 0xE0, B: 0xE0, A: 0xFF}
	DefaultBGColor = color.RGBA{R: 0x1E, G: 0x1E, B: 0x1E, A: 0xFF}
)

// ansi16 is the canonical 16-color xterm palette (indices 0-7 normal, 8-15 bright).
var ansi16 = [16]color.RGBA{
	{0x00, 0x00, 0x00, 0xFF}, // black
	{0xCD, 0x00, 0x00, 0xFF}, // red
	{0x00, 0xCD, 0x00, 0xFF}, // green
	{0xCD, 0xCD, 0x00, 0xFF}, // yellow
	{0x00, 0x00, 0xEE, 0xFF}, // blue
	{0xCD, 0x00, 0xCD, 0xFF}, // magenta
	{0x00, 0xCD, 0xCD, 0xFF}, // cyan
	{0xE5, 0xE5, 0xE5, 0xFF}, // white
	{0x7F, 0x7F, 0x7F, 0xFF}, // bright black (grey)
	{0xFF, 0x00, 0x00, 0xFF}, // bright red
	{0x00, 0xFF, 0x00, 0xFF}, // bright green
	{0xFF, 0xFF, 0x00, 0xFF}, // bright yellow
	{0x5C, 0x5C, 0xFF, 0xFF}, // bright blue
	{0xFF, 0x00, 0xFF, 0xFF}, // bright magenta
	{0x00, 0xFF, 0xFF, 0xFF}, // bright cyan
	{0xFF, 0xFF, 0xFF, 0xFF}, // bright white
}

// cubeLevels maps the 6x6x6 color cube components (indices 16-231).
var cubeLevels = [6]uint8{0x00, 0x5F, 0x87, 0xAF, 0xD7, 0xFF}

// mapColor resolves a vt10x.Color to an RGBA color. Unknown/default values fall
// back to the theme defaults.
func mapColor(c vt10x.Color) color.Color {
	switch {
	case c == vt10x.DefaultFG || c == vt10x.DefaultCursor:
		return DefaultFGColor
	case c == vt10x.DefaultBG:
		return DefaultBGColor
	case c < 16:
		return ansi16[c]
	case c < 232: // 16..231: 6x6x6 cube
		i := int(c) - 16
		r := cubeLevels[(i/36)%6]
		g := cubeLevels[(i/6)%6]
		b := cubeLevels[i%6]
		return color.RGBA{R: r, G: g, B: b, A: 0xFF}
	case c < 256: // 232..255: grayscale ramp
		v := uint8(8 + (int(c)-232)*10)
		return color.RGBA{R: v, G: v, B: v, A: 0xFF}
	default:
		return DefaultFGColor
	}
}
