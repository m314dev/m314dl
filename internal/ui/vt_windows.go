package ui

import (
	"os"

	"golang.org/x/sys/windows"
)

// enableVT turns on virtual terminal processing for stderr so the console
// interprets ANSI colors. Windows 10 and later support it; an older console
// refuses, and colors stay off rather than printing escape codes as text.
func enableVT() bool {
	h := windows.Handle(os.Stderr.Fd())
	var mode uint32
	if windows.GetConsoleMode(h, &mode) != nil {
		return false
	}
	if mode&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING != 0 {
		return true
	}
	return windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING) == nil
}
