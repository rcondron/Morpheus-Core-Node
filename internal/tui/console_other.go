//go:build !windows

package tui

// EnableVT is a no-op outside Windows; Unix terminals speak ANSI natively.
func EnableVT() {}
