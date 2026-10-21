package tui

import (
	"fmt"
	"math/big"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/rcondron/Morpheus-Core-Node/internal/node"
	"github.com/rcondron/Morpheus-Core-Node/internal/units"
)

// RunDashboard shows the live node view until the user quits.
func RunDashboard(status func() node.Status, logPath string) error {
	m := &dashboard{status: status, logPath: logPath}
	m.st = status()
	_, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	return err
}

type tickMsg time.Time

type dashboard struct {
	status  func() node.Status
	st      node.Status
	logPath string
	width   int
	height  int
}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *dashboard) Init() tea.Cmd { return tick() }

func (m *dashboard) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			return m, tea.Quit
		}
	case tickMsg:
		m.st = m.status()
		return m, tick()
	}
	return m, nil
}

var (
	dashTitle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "#FFFFFF", Dark: "#0D1117"}).Background(Accent).Padding(0, 1)
	section    = lipgloss.NewStyle().Bold(true).Foreground(Accent).MarginTop(1)
	label      = lipgloss.NewStyle().Foreground(Muted)
	colHeader  = lipgloss.NewStyle().Foreground(Muted).Bold(true)
	onlineDot  = Good.Render("●")
	busyDot    = Warning.Render("●")
	idleDot    = Subtle.Render("○")
	statusGood = Good
	statusBad  = Danger
)

func (m *dashboard) View() string {
	if m.width == 0 {
		return "loading…"
	}
	st := m.st
	w := m.width
	var b strings.Builder

	up := "starting"
	if !st.StartedAt.IsZero() {
		up = time.Since(st.StartedAt).Truncate(time.Second).String()
	}
	left := dashTitle.Render("MORPHEUS CORE NODE") + " " + Subtle.Render(st.Version+" · "+st.Network+" · "+st.Role)
	right := onlineDot + " online " + Subtle.Render("up "+up)
	b.WriteString(spread(left, right, w) + "\n")

	// Wallet & API.
	eth, mor, locked := parse(st.ETH), parse(st.MOR), parse(st.LockedStake)
	b.WriteString(section.Render("NODE") + "\n")
	b.WriteString(fmt.Sprintf("%s %s   %s %s   %s %s   %s %s\n",
		label.Render("wallet"), st.Address,
		label.Render("MOR"), Strong.Render(units.FormatToken(mor, 4)),
		label.Render("ETH"), Strong.Render(units.FormatToken(eth, 5)),
		label.Render("escrowed"), units.FormatToken(locked, 4)+" MOR"))
	if st.Gateway != "" {
		b.WriteString(fmt.Sprintf("%s %s   %s %d active   %s %d (%d errors)   %s %s in / %s out\n",
			label.Render("agent API"), Code.Render("http://"+st.Gateway+"/v1"),
			label.Render("keys"), st.Keys,
			label.Render("requests"), st.Totals.Requests, st.Totals.Errors,
			label.Render("tokens"), human(st.Totals.InputTokens), human(st.Totals.OutputTokens)))
	}
	if st.Provider != nil {
		b.WriteString(fmt.Sprintf("%s %s → %s   %s %d   %s %d   %s %d\n",
			label.Render("provider"), st.ProviderListen, Code.Render(st.PublicEndpoint),
			label.Render("models"), st.Provider.Models,
			label.Render("live sessions"), st.Provider.ActiveSessions,
			label.Render("served"), st.Provider.Requests))
	}
	b.WriteString(label.Render(fmt.Sprintf("rpc %s · block %d", st.RPC, st.Block)) + "\n")

	used := strings.Count(b.String(), "\n")
	remaining := m.height - used - 3

	// Sessions.
	if st.Gateway != "" || len(st.Sessions) > 0 {
		b.WriteString(section.Render("SESSIONS") + "\n")
		b.WriteString(colHeader.Render(row(w, []col{{"", 2}, {"MODEL", 26}, {"PROVIDER", 14}, {"ENDS IN", 9}, {"REQS", 6}, {"TOKENS", 9}, {"STAKE", 12}})) + "\n")
		lines := 0
		if len(st.Sessions) == 0 {
			b.WriteString(Subtle.Render("  no open sessions — they open automatically when an agent calls a model") + "\n")
			lines++
		}
		for _, s := range st.Sessions {
			if lines >= max(3, remaining/3) {
				break
			}
			dot := idleDot
			if s.Busy {
				dot = busyDot
			}
			if s.State != "active" {
				dot = Warning.Render("◐")
			}
			ends := time.Until(s.EndsAt).Truncate(time.Minute)
			endStr := ends.String()
			if ends <= 0 {
				endStr = "ended"
			}
			b.WriteString(row(w, []col{{dot, 2}, {s.Model, 26}, {ShortAddr(s.Provider), 14}, {strings.TrimSuffix(endStr, "0s"), 9},
				{fmt.Sprint(s.Requests), 6}, {human(s.TokensIn + s.TokensOut), 9}, {units.FormatToken(parse(s.Stake), 2) + " MOR", 12}}) + "\n")
			lines++
		}
		for _, p := range st.Pools {
			if p.Opening > 0 {
				b.WriteString(Warning.Render(fmt.Sprintf("  ◌ opening a session for %s…", p.Model)) + "\n")
				lines++
			} else if p.LastErr != "" && p.Sessions == 0 {
				b.WriteString(Danger.Render(truncate("  ✗ "+p.Model+": "+p.LastErr, w)) + "\n")
				lines++
			}
		}
		remaining -= lines + 2
	}

	// Requests.
	b.WriteString(section.Render("REQUESTS") + "\n")
	b.WriteString(colHeader.Render(row(w, []col{{"TIME", 9}, {"KEY", 14}, {"MODEL", 24}, {"STATUS", 7}, {"LATENCY", 9}, {"TTFT", 8}, {"TOKENS", 12}})) + "\n")
	reqs := st.Requests
	maxReq := max(2, remaining-6)
	if len(reqs) > maxReq {
		reqs = reqs[len(reqs)-maxReq:]
	}
	if len(reqs) == 0 {
		b.WriteString(Subtle.Render("  waiting for agent requests…") + "\n")
	}
	for i := len(reqs) - 1; i >= 0; i-- {
		r := reqs[i]
		status := statusGood.Render(fmt.Sprint(r.Status))
		if r.Status >= 400 || r.Error != "" {
			status = statusBad.Render(fmt.Sprint(r.Status))
		}
		b.WriteString(row(w, []col{{r.Time.Format("15:04:05"), 9}, {r.Key, 14}, {r.Model, 24}, {status, 7},
			{r.Latency.Truncate(time.Millisecond).String(), 9}, {r.TTFT.Truncate(time.Millisecond).String(), 8},
			{fmt.Sprintf("%d→%d", r.InputTokens, r.OutputTokens), 12}}) + "\n")
	}

	// Events.
	b.WriteString(section.Render("EVENTS") + "\n")
	evs := st.Events
	if len(evs) > 4 {
		evs = evs[len(evs)-4:]
	}
	for i := len(evs) - 1; i >= 0; i-- {
		e := evs[i]
		msg := e.Message
		switch e.Level {
		case "warn":
			msg = Warning.Render(msg)
		case "error":
			msg = Danger.Render(msg)
		}
		b.WriteString(truncate(Subtle.Render(e.Time.Format("15:04:05"))+"  "+msg, w+20) + "\n")
	}

	footer := Subtle.Render("q quit (closes sessions) · logs " + m.logPath)
	body := b.String()
	if pad := m.height - strings.Count(body, "\n") - 1; pad > 0 {
		body += strings.Repeat("\n", pad)
	}
	return body + footer
}

type col struct {
	text  string
	width int
}

func row(total int, cols []col) string {
	var b strings.Builder
	used := 0
	for _, c := range cols {
		if used+c.width > total {
			break
		}
		t := c.text
		vis := lipgloss.Width(t)
		if vis > c.width-1 {
			t = truncate(t, c.width-1)
			vis = lipgloss.Width(t)
		}
		b.WriteString(t + strings.Repeat(" ", max(c.width-vis, 1)))
		used += c.width
	}
	return b.String()
}

func truncate(s string, n int) string {
	if n <= 1 || lipgloss.Width(s) <= n {
		return s
	}
	r := []rune(s)
	for len(r) > 0 && lipgloss.Width(string(r)) > n-1 {
		r = r[:len(r)-1]
	}
	return string(r) + "…"
}

func spread(left, right string, width int) string {
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
}

func parse(s string) *big.Int {
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return new(big.Int)
	}
	return v
}

func human(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}
