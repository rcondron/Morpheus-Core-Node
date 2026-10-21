package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/huh"

	"github.com/rcondron/Morpheus-Core-Node/internal/config"
	"github.com/rcondron/Morpheus-Core-Node/internal/logx"
	"github.com/rcondron/Morpheus-Core-Node/internal/node"
	"github.com/rcondron/Morpheus-Core-Node/internal/tui"
)

type chatMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// cmdChat is a minimal streaming chat client for trying models from the
// terminal. It uses the running node, or starts one in-process for the chat.
func cmdChat(a *App, args []string) error {
	cfg, err := a.config()
	if err != nil {
		return err
	}
	if !cfg.ConsumerEnabled() {
		return errNoConsumer
	}
	base, token := "", ""
	if c, ok := a.running(); ok {
		base, token = c.Base, c.Token
	} else {
		w, err := a.unlock(cfg)
		if err != nil {
			return err
		}
		logPath := filepath.Join(a.Home, config.LogDir, "chat.log")
		_ = os.MkdirAll(filepath.Dir(logPath), 0o700)
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		n, err := node.New(cfg, w, logx.New(logx.Options{Level: cfg.Log.Level, Format: "json", Writer: f}))
		if err != nil {
			return err
		}
		ctx, cancel := context.WithCancel(context.Background())
		ready := make(chan struct{})
		done := make(chan error, 1)
		go func() { done <- n.Run(ctx, func() { close(ready) }) }()
		var startErr error
		_ = tui.Spin("Starting an embedded node", func() error {
			select {
			case <-ready:
			case startErr = <-done:
			}
			return startErr
		})
		if startErr != nil {
			cancel()
			return startErr
		}
		defer func() {
			cancel()
			_ = tui.Spin("Closing sessions and stopping the embedded node", func() error { return <-done })
		}()
		base, token = "http://"+n.GatewayAddr, n.AdminToken
	}

	model := ""
	if len(args) > 0 {
		model = args[0]
	} else if model, err = pickModel(base, token); err != nil {
		return err
	}

	fmt.Fprintf(a.Out, "\n%s %s\n", tui.Title.Render("chat · "+model), tui.Subtle.Render("/model <name> switch · /reset clear · /exit quit"))
	fmt.Fprintln(a.Out, tui.Subtle.Render("The first message opens a marketplace session (escrowing MOR), which can take ~10–30s."))
	var history []chatMsg
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 1<<20)
	for {
		fmt.Fprint(a.Out, "\n"+tui.Good.Render("you › "))
		if !in.Scan() {
			fmt.Fprintln(a.Out)
			return nil
		}
		line := strings.TrimSpace(in.Text())
		switch {
		case line == "":
			continue
		case line == "/exit" || line == "/quit":
			return nil
		case line == "/reset":
			history = nil
			fmt.Fprintln(a.Out, tui.Subtle.Render("conversation cleared"))
			continue
		case strings.HasPrefix(line, "/model "):
			model = strings.TrimSpace(strings.TrimPrefix(line, "/model "))
			history = nil
			fmt.Fprintln(a.Out, tui.Subtle.Render("switched to "+model))
			continue
		}
		history = append(history, chatMsg{Role: "user", Content: line})
		reply, err := streamChat(a, base, token, model, history)
		if err != nil {
			fmt.Fprintf(a.Out, "\n%s %v\n", tui.CrossMk, err)
			history = history[:len(history)-1]
			continue
		}
		history = append(history, chatMsg{Role: "assistant", Content: reply})
	}
}

func pickModel(base, token string) (string, error) {
	req, _ := http.NewRequest("GET", base+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		Data []struct {
			ID       string         `json:"id"`
			Morpheus map[string]any `json:"morpheus"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if len(out.Data) == 0 {
		return "", errors.New("no models are currently offered on the marketplace")
	}
	if !tui.Interactive() {
		return out.Data[0].ID, nil
	}
	var choice string
	opts := make([]huh.Option[string], len(out.Data))
	for i, m := range out.Data {
		opts[i] = huh.NewOption(fmt.Sprintf("%s  %s", m.ID, tui.Subtle.Render(fmt.Sprintf("%v offers", m.Morpheus["bids"]))), m.ID)
	}
	err = huh.NewForm(huh.NewGroup(huh.NewSelect[string]().Title("Pick a model").Filtering(true).Height(14).Options(opts...).Value(&choice))).
		WithTheme(tui.Theme()).Run()
	if errors.Is(err, huh.ErrUserAborted) {
		return "", tui.ErrAborted
	}
	return choice, err
}

func streamChat(a *App, base, token, model string, history []chatMsg) (string, error) {
	body, _ := json.Marshal(map[string]any{"model": model, "messages": history, "stream": true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)
	go func() {
		select {
		case <-sig:
			cancel()
		case <-ctx.Done():
		}
	}()

	req, _ := http.NewRequestWithContext(ctx, "POST", base+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	var resp *http.Response
	var err error
	waiting := make(chan struct{})
	spinDone := make(chan struct{})
	var stopOnce sync.Once
	// stopSpin clears the spinner line synchronously, so nothing printed
	// afterwards can be erased by a late clear.
	stopSpin := func() {
		stopOnce.Do(func() {
			close(waiting)
			<-spinDone
		})
	}
	defer stopSpin()
	go func() {
		defer close(spinDone)
		frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
		t := time.NewTicker(100 * time.Millisecond)
		defer t.Stop()
		for i := 0; ; i++ {
			select {
			case <-waiting:
				fmt.Fprint(a.Out, "\r\x1b[2K")
				return
			case <-t.C:
				fmt.Fprintf(a.Out, "\r\x1b[2K%s %s", tui.Subtle.Render(frames[i%len(frames)]), tui.Subtle.Render("routing to a provider…"))
			}
		}
	}()
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		stopSpin()
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		stopSpin()
		data, _ := io.ReadAll(resp.Body)
		var e struct {
			Error struct{ Message string } `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error.Message != "" {
			return "", errors.New(e.Error.Message)
		}
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var reply strings.Builder
	first := true
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 4<<20)
	var usage struct{ PromptTokens, CompletionTokens int }
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok || data == "[DONE]" {
			continue
		}
		var chunk struct {
			Error   *struct{ Message string } `json:"error"`
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					Reasoning string `json:"reasoning_content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		if chunk.Error != nil {
			return reply.String(), errors.New(chunk.Error.Message)
		}
		if chunk.Usage != nil {
			usage.PromptTokens, usage.CompletionTokens = chunk.Usage.PromptTokens, chunk.Usage.CompletionTokens
		}
		for _, c := range chunk.Choices {
			text := c.Delta.Content
			if text == "" && c.Delta.Reasoning != "" {
				text = tui.Subtle.Render(c.Delta.Reasoning)
			}
			if text == "" {
				continue
			}
			if first {
				stopSpin()
				first = false
				fmt.Fprint(a.Out, tui.Title.Render("ai  › "))
			}
			fmt.Fprint(a.Out, text)
			reply.WriteString(c.Delta.Content)
		}
	}
	stopSpin()
	meta := fmt.Sprintf("%.1fs", time.Since(start).Seconds())
	if usage.CompletionTokens > 0 {
		meta += fmt.Sprintf(" · %d→%d tokens", usage.PromptTokens, usage.CompletionTokens)
	}
	if p := resp.Header.Get("X-Morpheus-Provider"); p != "" {
		meta += " · provider " + tui.ShortAddr(p)
	}
	fmt.Fprintf(a.Out, "\n%s\n", tui.Subtle.Render(meta))
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		return reply.String(), err
	}
	return reply.String(), nil
}
