package main

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// App Bird prints its owl on `appbird version` (and the unlisted `appbird hoot`). Terminals with
// 256 or 24-bit color get a pixel copy of the icon (assets/appbird-icon.svg); other terminals, pipes
// and NO_COLOR get the line owl.

// owlPixels is the icon at 24x22 pixels, drawn two pixel rows per text line with half blocks.
// . is transparent; the other letters are keys of owlPalette.
var owlPixels = []string{
	`bb....................bb`,
	`bbb..................bbb`,
	`bbbbb..............bbbbb`,
	`bbbbbbbbbbbbbbbbbbbbbbbb`,
	`bbbbbbbbbbbbbbbbbbbbbbbb`,
	`bbbbffffffbbbbffffffbbbb`,
	`bbbffffffffffffffffffbbb`,
	`bbffwwwwwffffffwwwwwffbb`,
	`bffwwwwwwwffffwwwwwwwffb`,
	`bffwwppwwwffffwwppwwwffb`,
	`bffwwpppwwffffwwpppwwffb`,
	`bffwwpppwwyyyywwpppwwffb`,
	`bbffwwwwwfyyyyfwwwwwffbb`,
	`bbbffwwwfffyyfffwwwffbbb`,
	`bbbbffffffbbbbffffffbbbb`,
	`bbbbbbbbbbbbbbbbbbbbbbbb`,
	`bbbbbbbbbbbbbbbbbbbbbbbb`,
	`bbbbbbbbdbdbbdbdbbbbbbbb`,
	`bbbbbbbbbdbbbbdbbbbbbbbb`,
	`bbbbbbbbbbbbbbbbbbbbbbbb`,
	`.bbbbbbbbbbbbbbbbbbbbbb.`,
	`..bbbbbbbbbbbbbbbbbbbb..`,
}

var owlPalette = map[byte][3]uint8{
	'b': {0x2F, 0x80, 0xED}, // body
	'f': {0x93, 0xBD, 0xF8}, // facial disc
	'w': {0xFF, 0xFF, 0xFF}, // eyes
	'p': {0x2B, 0x10, 0x20}, // pupils
	'y': {0xF7, 0xB2, 0x3B}, // beak
	'd': {0x1D, 0x5F, 0xC0}, // chest feathers
}

// The line owl. Each line of owlMask colors the same column of owlArt: b body, f facial disc,
// e eyes, y beak, d chest feathers.
var owlArt = []string{
	`  /\_________/\ `,
	` |  .--. .--.  |`,
	` | ( ()   () ) |`,
	` |  '--'V'--'  |`,
	` |             |`,
	` |    v   v    |`,
	`  \___________/ `,
}

var owlMask = []string{
	`  bbbbbbbbbbbbb `,
	` b  ffff ffff  b`,
	` b f ee   ee f b`,
	` b  ffffyffff  b`,
	` b             b`,
	` b    d   d    b`,
	`  bbbbbbbbbbbbb `,
}

// 16-color codes for the line owl, which only shows on terminals without 256 colors.
var owlArtColors = map[byte]string{
	'b': "\x1b[1;34m", 'f': "\x1b[1;94m", 'e': "\x1b[1;97m", 'y': "\x1b[1;33m", 'd': "\x1b[34m",
}

const ansiReset = "\x1b[0m"

type colorMode int

const (
	noColor colorMode = iota
	color16
	color256
	color24bit
)

// terminalColors reports what color w supports: none when it isn't a terminal, NO_COLOR is set
// (https://no-color.org) or TERM is dumb; otherwise the best of COLORTERM and TERM.
func terminalColors(w io.Writer) colorMode {
	if _, ok := os.LookupEnv("NO_COLOR"); ok || os.Getenv("TERM") == "dumb" {
		return noColor
	}
	f, ok := w.(*os.File)
	if !ok {
		return noColor
	}
	if info, err := f.Stat(); err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return noColor
	}
	switch ct := os.Getenv("COLORTERM"); {
	case ct == "truecolor" || ct == "24bit":
		return color24bit
	case strings.Contains(os.Getenv("TERM"), "256color"):
		return color256
	}
	return color16
}

// printOwl writes the owl with App Bird, the version and the tagline beside it.
func printOwl(w io.Writer, mode colorMode) {
	name, ver, tag := "App Bird", "appbird "+version, "App Store metadata, reviewed like code."
	if mode != noColor {
		name = "\x1b[1;34m" + name + ansiReset
		tag = "\x1b[2m" + tag + ansiReset
	}
	var owl []string
	var top int // owl line the text starts beside
	if mode >= color256 {
		owl, top = pixelOwl(mode), 4
	} else {
		owl, top = lineOwl(mode), 1
	}
	text := map[int]string{top: name, top + 1: ver, top + 2: tag}
	for i, line := range owl {
		if t, ok := text[i]; ok {
			line += "   " + t
		}
		fmt.Fprintln(w, strings.TrimRight(line, " "))
	}
}

func lineOwl(mode colorMode) []string {
	if mode == noColor {
		return owlArt
	}
	lines := make([]string, len(owlArt))
	for i, line := range owlArt {
		var b strings.Builder
		// One escape per run of same-colored characters.
		for j := 0; j < len(line); {
			k := j
			for k < len(line) && owlMask[i][k] == owlMask[i][j] {
				k++
			}
			if c, ok := owlArtColors[owlMask[i][j]]; ok {
				b.WriteString(c + line[j:k] + ansiReset)
			} else {
				b.WriteString(line[j:k])
			}
			j = k
		}
		lines[i] = b.String()
	}
	return lines
}

// pixelOwl draws owlPixels with upper half blocks: the foreground is the top pixel and the
// background the bottom one.
func pixelOwl(mode colorMode) []string {
	var lines []string
	for y := 0; y+1 < len(owlPixels); y += 2 {
		var b strings.Builder
		for x := 0; x < len(owlPixels[y]); x++ {
			top, topOK := owlPalette[owlPixels[y][x]]
			bot, botOK := owlPalette[owlPixels[y+1][x]]
			switch {
			case topOK && botOK:
				b.WriteString("\x1b[" + ansiColor(mode, 38, top) + ";" + ansiColor(mode, 48, bot) + "m\u2580")
			case topOK:
				b.WriteString("\x1b[" + ansiColor(mode, 38, top) + "m\u2580")
			case botOK:
				b.WriteString("\x1b[" + ansiColor(mode, 38, bot) + "m\u2584")
			default:
				b.WriteString(" ")
			}
			b.WriteString(ansiReset)
		}
		lines = append(lines, b.String())
	}
	return lines
}

// ansiColor returns the SGR parameters for c as a foreground (38) or background (48) color.
func ansiColor(mode colorMode, layer int, c [3]uint8) string {
	if mode == color24bit {
		return fmt.Sprintf("%d;2;%d;%d;%d", layer, c[0], c[1], c[2])
	}
	// Nearest color in the xterm 6x6x6 cube, whose levels are 0, 95, 135, 175, 215 and 255.
	level := func(v uint8) int {
		if v < 48 {
			return 0
		}
		if v < 115 {
			return 1
		}
		return (int(v) - 35) / 40
	}
	return fmt.Sprintf("%d;5;%d", layer, 16+36*level(c[0])+6*level(c[1])+level(c[2]))
}
