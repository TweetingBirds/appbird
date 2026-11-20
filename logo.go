package main

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// The App Bird owl, drawn from the icon (assets/appbird-icon.svg). Each line of owlMask colors the
// same column of owlArt: b body, f facial disc, e eyes, y beak, d chest feathers.
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

// 24-bit colors from the logo.
var owlColors = map[byte]string{
	'b': "\x1b[1;38;2;47;128;237m",  // #2F80ED
	'f': "\x1b[1;38;2;147;189;248m", // #93BDF8
	'e': "\x1b[1;97m",               // white
	'y': "\x1b[1;38;2;247;178;59m",  // #F7B23B
	'd': "\x1b[1;38;2;29;95;192m",   // #1D5FC0
}

const ansiReset = "\x1b[0m"

// printOwl writes the owl with App Bird, the version and the tagline beside it.
func printOwl(w io.Writer, color bool) {
	text := []string{"", "App Bird", "appbird " + version, "Keep your App Store listing in your repo.", "", "", ""}
	if color {
		text[1] = owlColors['b'] + text[1] + ansiReset
		text[3] = "\x1b[2m" + text[3] + ansiReset
	}
	for i, line := range owlArt {
		var b strings.Builder
		if color {
			// One escape per run of same-colored characters.
			for j := 0; j < len(line); {
				k := j
				for k < len(line) && owlMask[i][k] == owlMask[i][j] {
					k++
				}
				if c, ok := owlColors[owlMask[i][j]]; ok {
					b.WriteString(c + line[j:k] + ansiReset)
				} else {
					b.WriteString(line[j:k])
				}
				j = k
			}
		} else {
			b.WriteString(line)
		}
		if text[i] != "" {
			b.WriteString("   " + text[i])
		}
		fmt.Fprintln(w, strings.TrimRight(b.String(), " "))
	}
}

// useColor reports whether w is a terminal that wants color (https://no-color.org).
func useColor(w io.Writer) bool {
	if _, ok := os.LookupEnv("NO_COLOR"); ok || os.Getenv("TERM") == "dumb" {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
