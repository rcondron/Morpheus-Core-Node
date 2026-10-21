//go:build windows

package tui

import (
	"os"

	"golang.org/x/sys/windows"
)

// EnableVT turns on ANSI escape processing for the Windows console so colors,
// spinners and the dashboard render in cmd.exe and PowerShell.
func EnableVT() {
	for _, f := range []*os.File{os.Stdout, os.Stderr} {
		h := windows.Handle(f.Fd())
		var mode uint32
		if windows.GetConsoleMode(h, &mode) == nil {
			_ = windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING)
		}
	}
	_ = windows.SetConsoleOutputCP(65001) // UTF-8 for box drawing and logo glyphs
}
