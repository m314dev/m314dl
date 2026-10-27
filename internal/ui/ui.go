// Package ui decides how m314dl talks to the terminal: whether stderr gets
// ANSI colors, and how many columns a status line takes once its escape codes
// are ignored.
package ui

import (
	"os"
	"strings"
	"unicode/utf8"

	"golang.org/x/term"
)

// Color is true when stderr is a terminal that will render ANSI colors. It
// stays false when stderr is piped, when NO_COLOR is set (no-color.org), when
// TERM is dumb, when -no-color was given, and on a Windows console that refuses
// virtual terminal mode. Piped output therefore never carries escape codes.
var Color bool

// Setup decides Color once the flags are parsed.
func Setup(noColor bool) {
	Color = !noColor && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb" &&
		term.IsTerminal(int(os.Stderr.Fd())) && enableVT()
}

// SGR codes for Paint.
const (
	Red    = "31"
	Green  = "32"
	Yellow = "33"
	Cyan   = "36"
	Dim    = "2"
)

// Paint wraps s in the color when Color is on and returns it unchanged otherwise.
func Paint(code, s string) string {
	if !Color || s == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

// Width is the number of terminal columns s occupies. Escape sequences count
// for nothing and every other rune for one column; status lines use only
// single-width characters, so there is no wide-character table here.
func Width(s string) int {
	n := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			i = skipEscape(s, i)
			continue
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
		n++
	}
	return n
}

// Truncate cuts s to at most width columns. Escape sequences survive the cut
// and a color left open at the end is closed, so a shortened line never bleeds
// its color into the rest of the terminal.
func Truncate(s string, width int) string {
	if Width(s) <= width {
		return s
	}
	var b strings.Builder
	cells, open := 0, false
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			j := skipEscape(s, i)
			b.WriteString(s[i:j])
			open = s[i:j] != "\x1b[0m"
			i = j
			continue
		}
		if cells == width {
			break
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		b.WriteRune(r)
		cells++
		i += size
	}
	if open {
		b.WriteString("\x1b[0m")
	}
	return b.String()
}

// skipEscape returns the index just past the SGR sequence starting at i. Paint
// is the only source of escapes here and its sequences all end in 'm'.
func skipEscape(s string, i int) int {
	for i < len(s) && s[i] != 'm' {
		i++
	}
	return i + 1
}
