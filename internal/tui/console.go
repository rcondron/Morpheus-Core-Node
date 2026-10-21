package tui

import (
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Init prepares the terminal: ANSI processing on Windows, and an optional
// MORCORE_THEME=light|dark override for terminals whose background color
// cannot be detected automatically.
func Init() {
	EnableVT()
	switch strings.ToLower(os.Getenv("MORCORE_THEME")) {
	case "light":
		lipgloss.SetHasDarkBackground(false)
	case "dark":
		lipgloss.SetHasDarkBackground(true)
	}
}
