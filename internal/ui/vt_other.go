//go:build !windows

package ui

// enableVT is a no-op outside Windows: every other terminal speaks ANSI.
func enableVT() bool { return true }
