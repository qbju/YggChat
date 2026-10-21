// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

// Helpers for the terminal chat (cli.go): making remote text safe to print,
// display widths, and putting the terminal into a no-echo, per-key mode
// through stty (no extra dependency; where stty is missing the CLI falls back
// to plain line input).
package main

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"unicode"
)

// cliIndent is put in front of continuation lines of a multi-line message so a
// peer cannot forge a line that looks like it came from someone else.
const cliIndent = "    "

// termSafe makes a remote string safe to write to a terminal: C0 controls
// (except \n), ESC, DEL, C1 controls and bidi overrides/isolates are removed,
// tabs become spaces, and every line after the first is indented.
func termSafe(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteString("\n" + cliIndent)
		case r == '\t':
			b.WriteByte(' ')
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
			// control characters, including ESC (0x1b), CR, BEL and C1 (CSI 0x9b, OSC 0x9d, ...)
		case isBidiControl(r):
		default:
			b.WriteRune(r)
		}
	}
	return strings.TrimRight(b.String(), " \n")
}

// termSafeLine is termSafe for strings shown on one line (names, previews, errors).
func termSafeLine(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(termSafe(strings.ReplaceAll(s, "\n", " ")), "\n"+cliIndent, " "))
}

func isBidiControl(r rune) bool {
	switch {
	case r >= 0x202a && r <= 0x202e: // LRE RLE PDF LRO RLO
		return true
	case r >= 0x2066 && r <= 0x2069: // LRI RLI FSI PDI
		return true
	case r == 0x200e || r == 0x200f || r == 0x061c: // LRM RLM ALM
		return true
	}
	return false
}

// runeWidth is a rough terminal column width (enough for redrawing the input line).
func runeWidth(r rune) int {
	switch {
	case r < 0x20 || r == 0x7f:
		return 0
	case unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || r == 0x200d || (r >= 0xfe00 && r <= 0xfe0f):
		return 0
	case r >= 0x1100 && r <= 0x115f, r >= 0x2e80 && r <= 0xa4cf, r >= 0xac00 && r <= 0xd7a3,
		r >= 0xf900 && r <= 0xfaff, r >= 0xfe30 && r <= 0xfe4f, r >= 0xff00 && r <= 0xff60,
		r >= 0xffe0 && r <= 0xffe6, r >= 0x1f300 && r <= 0x1f64f, r >= 0x1f900 && r <= 0x1f9ff,
		r >= 0x20000 && r <= 0x3fffd:
		return 2
	}
	return 1
}

func strWidth(s string) int {
	n := 0
	for _, r := range s {
		n += runeWidth(r)
	}
	return n
}

// isCharDev reports whether f looks like a terminal.
func isCharDev(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func stty(args ...string) (string, error) {
	cmd := exec.Command("stty", args...)
	cmd.Stdin = os.Stdin
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// ttyRaw switches stdin to per-key input without echo (signals stay on) and
// returns a function that restores the previous mode. ok is false when stdin
// is not a terminal or stty is not available.
func ttyRaw() (restore func(), ok bool) {
	if !isCharDev(os.Stdin) {
		return nil, false
	}
	saved, err := stty("-g")
	if err != nil || saved == "" {
		return nil, false
	}
	if _, err := stty("-icanon", "-echo", "min", "1", "time", "0"); err != nil {
		_, _ = stty(saved)
		return nil, false
	}
	return func() { _, _ = stty(saved) }, true
}

// ttyCols returns the terminal width (80 if unknown).
func ttyCols() int {
	if s, err := stty("size"); err == nil {
		if f := strings.Fields(s); len(f) == 2 {
			if n, err := strconv.Atoi(f[1]); err == nil && n > 0 {
				return n
			}
		}
	}
	return 80
}
