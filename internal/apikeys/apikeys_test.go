package apikeys

import (
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "apikeys.json")
	s, err := Open(path, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return s, path
}

func TestCreateAuthenticateRevoke(t *testing.T) {
	s, path := newStore(t)
	secret, key, err := s.Create(CreateOptions{Name: "research-agent", Models: []string{"llama-3.3-70b, qwen3"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, SecretPrefix) || len(secret) < 45 {
		t.Fatalf("weak secret %q", secret)
	}
	if strings.Contains(key.Hash, secret) || key.Prefix != secret[:len(key.Prefix)] {
		t.Fatal("prefix/hash wrong")
	}
	got, err := s.Authenticate(secret)
	if err != nil || got.ID != key.ID {
		t.Fatalf("authenticate: %v", err)
	}
	if !got.HasScope(ScopeInference) || got.HasScope(ScopeAdmin) {
		t.Fatal("default scopes wrong")
	}
	if !got.AllowsModel("QWEN3") || got.AllowsModel("mistral") {
		t.Fatal("model allowlist wrong")
	}
	if _, err := s.Authenticate(secret + "x"); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("expected invalid key, got %v", err)
	}
	if _, _, err := s.Create(CreateOptions{Name: "Research-Agent"}); err == nil {
		t.Fatal("duplicate active name accepted")
	}

	// A second process (the CLI) sees the key and can revoke it.
	other, err := Open(path, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Revoke("research-agent"); err != nil {
		t.Fatal(err)
	}
	touch(t, path)
	time.Sleep(1100 * time.Millisecond) // reload checks are throttled to once a second
	if _, err := s.Authenticate(secret); !errors.Is(err, ErrRevoked) {
		t.Fatalf("revocation not picked up: %v", err)
	}
}

func touch(t *testing.T, path string) {
	t.Helper()
	future := time.Now().Add(2 * time.Second)
	if err := osChtimes(path, future); err != nil {
		t.Fatal(err)
	}
}

func TestRotateKeepsSettings(t *testing.T) {
	s, _ := newStore(t)
	old, k, _ := s.Create(CreateOptions{Name: "bot", RPM: 30, Scopes: []string{"inference", "sessions"}})
	fresh, k2, err := s.Rotate(k.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh == old || k2.RPM != 30 || len(k2.Scopes) != 2 {
		t.Fatal("rotate lost settings")
	}
	if _, err := s.Authenticate(old); err == nil {
		t.Fatal("old secret still works")
	}
	if _, err := s.Authenticate(fresh); err != nil {
		t.Fatal(err)
	}
}

func TestExpiry(t *testing.T) {
	s, _ := newStore(t)
	secret, _, _ := s.Create(CreateOptions{Name: "temp", TTL: time.Millisecond})
	time.Sleep(5 * time.Millisecond)
	if _, err := s.Authenticate(secret); !errors.Is(err, ErrExpired) {
		t.Fatalf("expected expiry, got %v", err)
	}
}

func TestRateLimit(t *testing.T) {
	s, _ := newStore(t)
	_, k, _ := s.Create(CreateOptions{Name: "limited", RPM: 2})
	for i := 0; i < 2; i++ {
		rel, err := s.Admit(k.ID)
		if err != nil {
			t.Fatalf("request %d rejected: %v", i, err)
		}
		rel()
	}
	_, err := s.Admit(k.ID)
	var le *LimitError
	if !errors.As(err, &le) || !errors.Is(err, ErrRateLimited) || le.RetryAfter <= 0 {
		t.Fatalf("expected rate limit with retry-after, got %v", err)
	}
}

func TestConcurrencyLimit(t *testing.T) {
	s, _ := newStore(t)
	_, k, _ := s.Create(CreateOptions{Name: "conc", Concurrency: 2})
	r1, _ := s.Admit(k.ID)
	r2, _ := s.Admit(k.ID)
	if _, err := s.Admit(k.ID); !errors.Is(err, ErrConcurrency) {
		t.Fatalf("expected concurrency limit, got %v", err)
	}
	r1()
	r1() // releasing twice must not over-release
	r3, err := s.Admit(k.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Admit(k.ID); !errors.Is(err, ErrConcurrency) {
		t.Fatal("double release leaked a slot")
	}
	r2()
	r3()
}

func TestDailyQuota(t *testing.T) {
	s, _ := newStore(t)
	_, k, _ := s.Create(CreateOptions{Name: "quota", DailyTokens: 100})
	s.Record(k.ID, 60, 50, false)
	if _, err := s.Admit(k.ID); !errors.Is(err, ErrQuota) {
		t.Fatalf("expected quota error, got %v", err)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(k.ID)
	if got.Usage.InputTokens != 60 || got.Usage.OutputTokens != 50 || got.Usage.Requests != 1 {
		t.Fatalf("usage not recorded: %+v", got.Usage)
	}
}

func TestConcurrentAdmitIsRaceFree(t *testing.T) {
	s, _ := newStore(t)
	_, k, _ := s.Create(CreateOptions{Name: "busy"})
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rel, err := s.Admit(k.ID)
			if err == nil {
				s.Record(k.ID, 1, 1, false)
				rel()
			}
		}()
	}
	wg.Wait()
	got, _ := s.Get(k.ID)
	if got.Usage.Requests != 50 {
		t.Fatalf("requests = %d", got.Usage.Requests)
	}
}

func TestInvalidScope(t *testing.T) {
	s, _ := newStore(t)
	if _, _, err := s.Create(CreateOptions{Name: "x", Scopes: []string{"root"}}); err == nil {
		t.Fatal("invalid scope accepted")
	}
}
