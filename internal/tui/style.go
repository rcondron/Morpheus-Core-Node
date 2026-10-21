// Package tui holds the terminal user interface: the onboarding wizard, the
// live dashboard and shared styling.
package tui

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"
)

// Palette (adaptive for light and dark terminals).
var (
	Accent  = lipgloss.AdaptiveColor{Light: "#0A9E63", Dark: "#20DC8E"}
	Accent2 = lipgloss.AdaptiveColor{Light: "#0B6E99", Dark: "#4FC3F7"}
	Muted   = lipgloss.AdaptiveColor{Light: "#6E7781", Dark: "#7D8590"}
	Warn    = lipgloss.AdaptiveColor{Light: "#9A6700", Dark: "#E3B341"}
	Bad     = lipgloss.AdaptiveColor{Light: "#CF222E", Dark: "#F85149"}
	Fg      = lipgloss.AdaptiveColor{Light: "#1F2328", Dark: "#E6EDF3"}
)

var (
	Title    = lipgloss.NewStyle().Bold(true).Foreground(Accent)
	Heading  = lipgloss.NewStyle().Bold(true).Foreground(Fg)
	Subtle   = lipgloss.NewStyle().Foreground(Muted)
	Good     = lipgloss.NewStyle().Foreground(Accent)
	Warning  = lipgloss.NewStyle().Foreground(Warn)
	Danger   = lipgloss.NewStyle().Foreground(Bad)
	Link     = lipgloss.NewStyle().Foreground(Accent2).Underline(true)
	Code     = lipgloss.NewStyle().Foreground(Accent2)
	Strong   = lipgloss.NewStyle().Bold(true)
	Box      = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(Accent).Padding(0, 2)
	WarnBox  = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(Warn).Padding(0, 2)
	CheckMk  = Good.Render("✓")
	CrossMk  = Danger.Render("✗")
	WarnMk   = Warning.Render("!")
	BulletMk = Subtle.Render("•")
)

// Banner is the startup logo.
func Banner() string {
	logo := []string{
		"█▀▄▀█ █▀█ █▀█ █▀█ █ █ █▀▀ █ █ █▀",
		"█ ▀ █ █▄█ █▀▄ █▀▀ █▀█ ██▄ █▄█ ▄█",
	}
	var b strings.Builder
	for _, l := range logo {
		b.WriteString(Title.Render(l))
		b.WriteByte('\n')
	}
	b.WriteString(Subtle.Render("core node · decentralized AI compute routing"))
	return b.String()
}

// Theme is the huh form theme in Morpheus colors.
func Theme() *huh.Theme {
	t := huh.ThemeCharm()
	cream := lipgloss.AdaptiveColor{Light: "#FFFFFF", Dark: "#0D1117"}
	t.Focused.Title = t.Focused.Title.Foreground(Accent)
	t.Focused.NoteTitle = t.Focused.NoteTitle.Foreground(Accent)
	t.Focused.SelectSelector = t.Focused.SelectSelector.Foreground(Accent)
	t.Focused.NextIndicator = t.Focused.NextIndicator.Foreground(Accent)
	t.Focused.PrevIndicator = t.Focused.PrevIndicator.Foreground(Accent)
	t.Focused.MultiSelectSelector = t.Focused.MultiSelectSelector.Foreground(Accent)
	t.Focused.SelectedOption = t.Focused.SelectedOption.Foreground(Accent)
	t.Focused.FocusedButton = t.Focused.FocusedButton.Foreground(cream).Background(Accent)
	t.Focused.Next = t.Focused.FocusedButton
	t.Focused.TextInput.Prompt = t.Focused.TextInput.Prompt.Foreground(Accent)
	t.Focused.TextInput.Cursor = t.Focused.TextInput.Cursor.Foreground(Accent)
	t.Focused.Description = t.Focused.Description.Foreground(Muted)
	t.Blurred = t.Focused
	t.Blurred.Base = t.Focused.Base.BorderStyle(lipgloss.HiddenBorder())
	t.Blurred.Card = t.Blurred.Base
	t.Blurred.NextIndicator = lipgloss.NewStyle()
	t.Blurred.PrevIndicator = lipgloss.NewStyle()
	t.Group.Title = t.Focused.Title
	t.Group.Description = t.Focused.Description
	return t
}

// Interactive reports whether stdin and stdout are terminals.
func Interactive() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

// Spin shows a spinner with msg while fn runs (plain output when not a TTY).
func Spin(msg string, fn func() error) error {
	if !term.IsTerminal(int(os.Stderr.Fd())) {
		fmt.Fprintf(os.Stderr, "%s…\n", msg)
		return fn()
	}
	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(80 * time.Millisecond)
		defer t.Stop()
		for i := 0; ; i++ {
			select {
			case <-done:
				fmt.Fprint(os.Stderr, "\r\x1b[2K")
				return
			case <-t.C:
				fmt.Fprintf(os.Stderr, "\r\x1b[2K%s %s", Good.Render(frames[i%len(frames)]), msg)
			}
		}
	}()
	err := fn()
	close(done)
	wg.Wait()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s %s\n", CrossMk, msg)
	} else {
		fmt.Fprintf(os.Stderr, "%s %s\n", CheckMk, msg)
	}
	return err
}

// ShortAddr abbreviates a hex address.
func ShortAddr(a string) string {
	if len(a) < 12 {
		return a
	}
	return a[:6] + "…" + a[len(a)-4:]
}
