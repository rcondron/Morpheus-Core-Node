// Package apikeys manages the API keys agents use to reach the node.
//
// Secrets are shown once at creation and stored only as SHA-256 hashes. Each
// key carries scopes, an optional model allowlist, request-rate and
// concurrency limits, a daily token quota and an optional expiry.
package apikeys

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rcondron/Morpheus-Core-Node/internal/store"
)

// Scopes.
const (
	ScopeInference = "inference" // chat, embeddings, model listing
	ScopeSessions  = "sessions"  // view/open/close marketplace sessions
	ScopeAdmin     = "admin"     // everything, including key management
)

// AllScopes lists valid scopes.
var AllScopes = []string{ScopeInference, ScopeSessions, ScopeAdmin}

// SecretPrefix starts every key, so secrets are easy to recognize and scan for.
const SecretPrefix = "sk-mor-"

var (
	ErrInvalidKey  = errors.New("invalid API key")
	ErrRevoked     = errors.New("API key has been revoked")
	ErrExpired     = errors.New("API key has expired")
	ErrNotFound    = errors.New("API key not found")
	ErrRateLimited = errors.New("rate limit exceeded")
	ErrConcurrency = errors.New("too many concurrent requests for this key")
	ErrQuota       = errors.New("daily token quota exhausted")
	ErrForbidden   = errors.New("API key lacks the required scope")
)

// Key is a stored API key (never the secret itself).
type Key struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Prefix      string     `json:"prefix"`
	Hash        string     `json:"hash"`
	Scopes      []string   `json:"scopes"`
	Models      []string   `json:"models,omitempty"`
	RPM         int        `json:"rpm,omitempty"`
	Concurrency int        `json:"concurrency,omitempty"`
	DailyTokens int64      `json:"dailyTokens,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
	ExpiresAt   *time.Time `json:"expiresAt,omitempty"`
	RevokedAt   *time.Time `json:"revokedAt,omitempty"`
	LastUsedAt  *time.Time `json:"lastUsedAt,omitempty"`
	Usage       Usage      `json:"usage"`
}

// Usage counters (lifetime, plus the current UTC day for quotas).
type Usage struct {
	Requests     int64  `json:"requests"`
	Errors       int64  `json:"errors"`
	InputTokens  int64  `json:"inputTokens"`
	OutputTokens int64  `json:"outputTokens"`
	Day          string `json:"day,omitempty"`
	DayTokens    int64  `json:"dayTokens,omitempty"`
}

// Status is a human summary of a key's state.
func (k *Key) Status(now time.Time) string {
	switch {
	case k.RevokedAt != nil:
		return "revoked"
	case k.ExpiresAt != nil && now.After(*k.ExpiresAt):
		return "expired"
	default:
		return "active"
	}
}

// HasScope reports whether the key grants scope (admin implies all).
func (k *Key) HasScope(scope string) bool {
	for _, s := range k.Scopes {
		if s == scope || s == ScopeAdmin {
			return true
		}
	}
	return false
}

// AllowsModel reports whether the key may use a model, matched by alias,
// name or id (case-insensitive). An empty allowlist allows every model.
func (k *Key) AllowsModel(names ...string) bool {
	if len(k.Models) == 0 {
		return true
	}
	for _, allowed := range k.Models {
		for _, n := range names {
			if n != "" && strings.EqualFold(strings.TrimSpace(allowed), n) {
				return true
			}
		}
	}
	return false
}

// CreateOptions configure a new key.
type CreateOptions struct {
	Name        string
	Scopes      []string
	Models      []string
	RPM         int
	Concurrency int
	DailyTokens int64
	TTL         time.Duration
}

// Store is the file-backed key registry. It is safe for concurrent use.
type Store struct {
	path string
	log  *slog.Logger

	mu        sync.Mutex
	keys      []*Key
	byHash    map[string]*Key
	runtime   map[string]*runtimeState
	dirty     bool
	modTime   time.Time
	checkedAt time.Time
}

type runtimeState struct {
	tokens   float64
	last     time.Time
	inflight int
}

type fileFormat struct {
	Version int    `json:"version"`
	Keys    []*Key `json:"keys"`
}

// Open loads (or initializes) the key store at path.
func Open(path string, log *slog.Logger) (*Store, error) {
	s := &Store{path: path, log: log, byHash: map[string]*Key{}, runtime: map[string]*runtimeState{}}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) load() error {
	var f fileFormat
	if _, err := store.ReadJSON(s.path, &f); err != nil {
		return err
	}
	s.keys = f.Keys
	s.byHash = make(map[string]*Key, len(f.Keys))
	for _, k := range f.Keys {
		s.byHash[k.Hash] = k
	}
	if st, err := os.Stat(s.path); err == nil {
		s.modTime = st.ModTime()
	}
	return nil
}

func (s *Store) saveLocked() error {
	if err := store.WriteJSON(s.path, fileFormat{Version: 1, Keys: s.keys}, 0o600); err != nil {
		return err
	}
	if st, err := os.Stat(s.path); err == nil {
		s.modTime = st.ModTime()
	}
	s.dirty = false
	return nil
}

// reloadIfChangedLocked picks up edits made by another process (the CLI
// editing keys while the node is stopped and then started, or vice versa),
// preserving in-memory usage counters.
func (s *Store) reloadIfChangedLocked() {
	st, err := os.Stat(s.path)
	if err != nil || !st.ModTime().After(s.modTime) {
		return
	}
	usage := map[string]Usage{}
	lastUsed := map[string]*time.Time{}
	for _, k := range s.keys {
		usage[k.ID], lastUsed[k.ID] = k.Usage, k.LastUsedAt
	}
	if err := s.load(); err != nil {
		s.log.Warn("could not reload API keys", "err", err)
		return
	}
	for _, k := range s.keys {
		if u, ok := usage[k.ID]; ok && u.Requests >= k.Usage.Requests {
			k.Usage, k.LastUsedAt = u, lastUsed[k.ID]
		}
	}
}

// Create mints a key and returns its secret (shown to the user exactly once).
func (s *Store) Create(o CreateOptions) (string, Key, error) {
	name := strings.TrimSpace(o.Name)
	if name == "" {
		return "", Key{}, errors.New("key name is required")
	}
	scopes, err := normalizeScopes(o.Scopes)
	if err != nil {
		return "", Key{}, err
	}
	secret, err := newSecret()
	if err != nil {
		return "", Key{}, err
	}
	id, err := randHex(5)
	if err != nil {
		return "", Key{}, err
	}
	now := time.Now().UTC()
	k := &Key{
		ID: "key_" + id, Name: name, Prefix: secret[:len(SecretPrefix)+6], Hash: hashSecret(secret),
		Scopes: scopes, Models: cleanList(o.Models), RPM: max(o.RPM, 0), Concurrency: max(o.Concurrency, 0),
		DailyTokens: max(o.DailyTokens, 0), CreatedAt: now,
	}
	if o.TTL > 0 {
		exp := now.Add(o.TTL)
		k.ExpiresAt = &exp
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadIfChangedLocked()
	for _, x := range s.keys {
		if strings.EqualFold(x.Name, name) && x.RevokedAt == nil {
			return "", Key{}, fmt.Errorf("an active key named %q already exists", name)
		}
	}
	s.keys = append(s.keys, k)
	s.byHash[k.Hash] = k
	if err := s.saveLocked(); err != nil {
		return "", Key{}, err
	}
	return secret, *k, nil
}

// List returns copies of all keys, newest first.
func (s *Store) List() []Key {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadIfChangedLocked()
	out := make([]Key, len(s.keys))
	for i, k := range s.keys {
		out[i] = *k
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// findLocked resolves a key by id, name or secret prefix, preferring active
// keys when a name was reused after a revocation.
func (s *Store) findLocked(ref string) (*Key, error) {
	ref = strings.TrimSpace(ref)
	var active, inactive []*Key
	for _, k := range s.keys {
		if k.ID == ref {
			return k, nil
		}
		if strings.EqualFold(k.Name, ref) || (len(ref) >= len(SecretPrefix)+4 && strings.HasPrefix(k.Prefix, ref)) {
			if k.RevokedAt == nil {
				active = append(active, k)
			} else {
				inactive = append(inactive, k)
			}
		}
	}
	switch {
	case len(active) == 1:
		return active[0], nil
	case len(active) > 1:
		return nil, fmt.Errorf("%q matches several keys; use the key id", ref)
	case len(inactive) > 0:
		return inactive[len(inactive)-1], nil
	}
	return nil, fmt.Errorf("%w: %s", ErrNotFound, ref)
}

// Get returns a key by id, name or prefix.
func (s *Store) Get(ref string) (Key, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadIfChangedLocked()
	k, err := s.findLocked(ref)
	if err != nil {
		return Key{}, err
	}
	return *k, nil
}

// Revoke disables a key permanently.
func (s *Store) Revoke(ref string) (Key, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadIfChangedLocked()
	k, err := s.findLocked(ref)
	if err != nil {
		return Key{}, err
	}
	if k.RevokedAt == nil {
		now := time.Now().UTC()
		k.RevokedAt = &now
		if err := s.saveLocked(); err != nil {
			return Key{}, err
		}
	}
	return *k, nil
}

// Rotate replaces a key's secret, keeping its settings and usage.
func (s *Store) Rotate(ref string) (string, Key, error) {
	secret, err := newSecret()
	if err != nil {
		return "", Key{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadIfChangedLocked()
	k, err := s.findLocked(ref)
	if err != nil {
		return "", Key{}, err
	}
	if k.RevokedAt != nil {
		return "", Key{}, ErrRevoked
	}
	delete(s.byHash, k.Hash)
	k.Hash, k.Prefix = hashSecret(secret), secret[:len(SecretPrefix)+6]
	s.byHash[k.Hash] = k
	if err := s.saveLocked(); err != nil {
		return "", Key{}, err
	}
	return secret, *k, nil
}

// Update applies limit/scope changes. Nil fields are left unchanged.
type Update struct {
	Name        *string
	Scopes      []string
	Models      *[]string
	RPM         *int
	Concurrency *int
	DailyTokens *int64
	TTL         *time.Duration
}

func (s *Store) Update(ref string, u Update) (Key, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadIfChangedLocked()
	k, err := s.findLocked(ref)
	if err != nil {
		return Key{}, err
	}
	if u.Name != nil && strings.TrimSpace(*u.Name) != "" {
		k.Name = strings.TrimSpace(*u.Name)
	}
	if u.Scopes != nil {
		sc, err := normalizeScopes(u.Scopes)
		if err != nil {
			return Key{}, err
		}
		k.Scopes = sc
	}
	if u.Models != nil {
		k.Models = cleanList(*u.Models)
	}
	if u.RPM != nil {
		k.RPM = max(*u.RPM, 0)
	}
	if u.Concurrency != nil {
		k.Concurrency = max(*u.Concurrency, 0)
	}
	if u.DailyTokens != nil {
		k.DailyTokens = max(*u.DailyTokens, 0)
	}
	if u.TTL != nil {
		if *u.TTL <= 0 {
			k.ExpiresAt = nil
		} else {
			exp := time.Now().UTC().Add(*u.TTL)
			k.ExpiresAt = &exp
		}
	}
	if err := s.saveLocked(); err != nil {
		return Key{}, err
	}
	return *k, nil
}

// Authenticate resolves a presented secret to its key.
func (s *Store) Authenticate(secret string) (Key, error) {
	secret = strings.TrimSpace(secret)
	if !strings.HasPrefix(secret, SecretPrefix) {
		return Key{}, ErrInvalidKey
	}
	h := hashSecret(secret)
	s.mu.Lock()
	defer s.mu.Unlock()
	if now := time.Now(); now.Sub(s.checkedAt) >= time.Second {
		s.checkedAt = now
		s.reloadIfChangedLocked()
	}
	k, ok := s.byHash[h]
	if !ok {
		return Key{}, ErrInvalidKey
	}
	now := time.Now()
	switch k.Status(now) {
	case "revoked":
		return Key{}, ErrRevoked
	case "expired":
		return Key{}, ErrExpired
	}
	return *k, nil
}

// LimitError carries a Retry-After hint.
type LimitError struct {
	Err        error
	RetryAfter time.Duration
}

func (e *LimitError) Error() string { return e.Err.Error() }
func (e *LimitError) Unwrap() error { return e.Err }

// Admit enforces rate, concurrency and quota limits for one request. The
// returned release must be called when the request finishes.
func (s *Store) Admit(id string) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var k *Key
	for _, x := range s.keys {
		if x.ID == id {
			k = x
			break
		}
	}
	if k == nil {
		return nil, ErrInvalidKey
	}
	rt := s.runtime[id]
	if rt == nil {
		rt = &runtimeState{tokens: float64(k.RPM), last: time.Now()}
		s.runtime[id] = rt
	}
	now := time.Now()
	if k.DailyTokens > 0 && k.Usage.Day == day(now) && k.Usage.DayTokens >= k.DailyTokens {
		tomorrow := now.UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
		return nil, &LimitError{Err: ErrQuota, RetryAfter: tomorrow.Sub(now)}
	}
	if k.Concurrency > 0 && rt.inflight >= k.Concurrency {
		return nil, &LimitError{Err: ErrConcurrency, RetryAfter: time.Second}
	}
	if k.RPM > 0 {
		rate := float64(k.RPM) / 60
		rt.tokens = minf(float64(k.RPM), rt.tokens+now.Sub(rt.last).Seconds()*rate)
		rt.last = now
		if rt.tokens < 1 {
			wait := time.Duration((1 - rt.tokens) / rate * float64(time.Second))
			return nil, &LimitError{Err: ErrRateLimited, RetryAfter: wait}
		}
		rt.tokens--
	}
	rt.inflight++
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			rt.inflight--
			s.mu.Unlock()
		})
	}, nil
}

// Record adds a finished request to a key's usage.
func (s *Store) Record(id string, inTokens, outTokens int, failed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range s.keys {
		if k.ID != id {
			continue
		}
		now := time.Now().UTC()
		k.LastUsedAt = &now
		k.Usage.Requests++
		if failed {
			k.Usage.Errors++
		}
		k.Usage.InputTokens += int64(inTokens)
		k.Usage.OutputTokens += int64(outTokens)
		if d := day(now); k.Usage.Day != d {
			k.Usage.Day, k.Usage.DayTokens = d, 0
		}
		k.Usage.DayTokens += int64(inTokens + outTokens)
		s.dirty = true
		return
	}
}

// Flush persists usage counters if they changed.
func (s *Store) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return nil
	}
	return s.saveLocked()
}

// Count returns the number of active keys.
func (s *Store) Count() (active int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for _, k := range s.keys {
		if k.Status(now) == "active" {
			active++
		}
	}
	return active
}

func day(t time.Time) string { return t.UTC().Format("2006-01-02") }

func minf(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func hashSecret(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}

const base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

func newSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	n := new(big.Int).SetBytes(b)
	base := big.NewInt(62)
	var sb strings.Builder
	mod := new(big.Int)
	for n.Sign() > 0 {
		n.DivMod(n, base, mod)
		sb.WriteByte(base62[mod.Int64()])
	}
	s := sb.String()
	for len(s) < 43 {
		s += "0"
	}
	return SecretPrefix + s, nil
}

func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func normalizeScopes(in []string) ([]string, error) {
	if len(in) == 0 {
		return []string{ScopeInference}, nil
	}
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" || seen[s] {
			continue
		}
		valid := false
		for _, v := range AllScopes {
			valid = valid || v == s
		}
		if !valid {
			return nil, fmt.Errorf("unknown scope %q (valid: %s)", s, strings.Join(AllScopes, ", "))
		}
		seen[s] = true
		out = append(out, s)
	}
	return out, nil
}

func cleanList(in []string) []string {
	var out []string
	for _, s := range in {
		for _, p := range strings.Split(s, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}
