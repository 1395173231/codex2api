package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/codex2api/database"
)

type CodexTurnStateSettings struct {
	Enabled               bool     `json:"enabled"`
	Models                []string `json:"models"`
	HarvestProxyURL       string   `json:"harvest_proxy_url"`
	TargetLengths         []int    `json:"target_lengths"`
	TTLSeconds            int      `json:"ttl_seconds"`
	RefreshBeforeSeconds  int      `json:"refresh_before_seconds"`
	RetryIntervalSeconds  int      `json:"retry_interval_seconds"`
	AttemptTimeoutSeconds int      `json:"attempt_timeout_seconds"`
	MaxAttempts           int      `json:"max_attempts"`
	Concurrency           int      `json:"concurrency"`
}

func DefaultCodexTurnStateSettings() CodexTurnStateSettings {
	return CodexTurnStateSettings{Models: []string{"gpt-6-astra", "gpt-5.6-sol"}, TargetLengths: []int{292, 332},
		TTLSeconds: 3600, RefreshBeforeSeconds: 300, RetryIntervalSeconds: 45, AttemptTimeoutSeconds: 25, MaxAttempts: 3, Concurrency: 2}
}

// DecodeCodexTurnStateSettings migrates the pre-array target_length and
// team_target_length fields while keeping defaults for omitted fields.
func DecodeCodexTurnStateSettings(raw string) (CodexTurnStateSettings, error) {
	cfg := DefaultCodexTurnStateSettings()
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return cfg, err
	}
	var legacy struct {
		TargetLengths    json.RawMessage `json:"target_lengths"`
		TargetLength     int             `json:"target_length"`
		TeamTargetLength int             `json:"team_target_length"`
	}
	if err := json.Unmarshal([]byte(raw), &legacy); err != nil {
		return cfg, err
	}
	if len(legacy.TargetLengths) == 0 || string(legacy.TargetLengths) == "null" {
		lengths := make([]int, 0, 2)
		if legacy.TargetLength > 0 {
			lengths = append(lengths, legacy.TargetLength)
		}
		if legacy.TeamTargetLength > 0 && !slices.Contains(lengths, legacy.TeamTargetLength) {
			lengths = append(lengths, legacy.TeamTargetLength)
		}
		if len(lengths) > 0 {
			cfg.TargetLengths = lengths
		}
	}
	return cfg, nil
}

func NormalizeCodexTurnStateModel(raw string) (string, error) {
	model := strings.ToLower(strings.TrimSpace(raw))
	if model == "" || len(model) > 128 {
		return "", errors.New("model must contain 1-128 ASCII characters")
	}
	for _, c := range model {
		if c <= ' ' || c > '~' || strings.ContainsRune("*,\\", c) {
			return "", errors.New("model must be an exact name without wildcards")
		}
	}
	return model, nil
}

func NormalizeCodexTurnStateSettings(cfg CodexTurnStateSettings) (CodexTurnStateSettings, error) {
	models := make([]string, 0, len(cfg.Models))
	for _, raw := range cfg.Models {
		model, err := NormalizeCodexTurnStateModel(raw)
		if err != nil {
			return cfg, err
		}
		if !slices.Contains(models, model) {
			models = append(models, model)
		}
	}
	if len(models) == 0 || len(models) > 32 {
		return cfg, errors.New("configure between 1 and 32 models")
	}
	cfg.Models = models
	lengths := make([]int, 0, len(cfg.TargetLengths))
	for _, length := range cfg.TargetLengths {
		if length < 100 || length > 4096 {
			return cfg, fmt.Errorf("target_lengths entries must be between %d and %d", 100, 4096)
		}
		if !slices.Contains(lengths, length) {
			lengths = append(lengths, length)
		}
	}
	if len(lengths) == 0 || len(lengths) > 8 {
		return cfg, errors.New("configure between 1 and 8 target_lengths")
	}
	cfg.TargetLengths = lengths
	for _, field := range []struct {
		name            string
		value, min, max int
	}{
		{"ttl_seconds", cfg.TTLSeconds, 180, 3600}, {"refresh_before_seconds", cfg.RefreshBeforeSeconds, 1, cfg.TTLSeconds - 1},
		{"retry_interval_seconds", cfg.RetryIntervalSeconds, 1, 3600}, {"attempt_timeout_seconds", cfg.AttemptTimeoutSeconds, 1, 120},
		{"max_attempts", cfg.MaxAttempts, 1, 10}, {"concurrency", cfg.Concurrency, 1, 16},
	} {
		if field.value < field.min || field.value > field.max {
			return cfg, fmt.Errorf("%s must be between %d and %d", field.name, field.min, field.max)
		}
	}
	cfg.HarvestProxyURL = strings.TrimSpace(cfg.HarvestProxyURL)
	if cfg.Enabled && cfg.HarvestProxyURL == "" {
		return cfg, errors.New("a dedicated harvest proxy is required")
	}
	if cfg.HarvestProxyURL != "" {
		if err := ValidateCodexHarvestProxy(cfg.HarvestProxyURL); err != nil {
			return cfg, err
		}
	}
	return cfg, nil
}

// Validation errors deliberately exclude the URL, which may contain secrets.
func ValidateCodexHarvestProxy(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("harvest proxy requires a host and no path, query or fragment")
	}
	if !slices.Contains([]string{"http", "https", "socks5", "socks5h"}, u.Scheme) {
		return errors.New("harvest proxy must use http, https, socks5 or socks5h")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return errors.New("invalid harvest proxy port")
		}
	}
	return nil
}

func MaskCodexHarvestProxy(raw string) string {
	if raw == "" || ValidateCodexHarvestProxy(raw) != nil {
		return ""
	}
	u, _ := url.Parse(raw)
	if u.User != nil {
		u.User = url.UserPassword("***", "***")
	}
	return u.String()
}

// ParseCodexTurnStateIssuedAt checks the public Fernet envelope only. Without
// the issuer's key it cannot authenticate the timestamp or decrypt the payload.
func ParseCodexTurnStateIssuedAt(token string) (time.Time, error) {
	if ValidateCodexTurnState(token) != nil || strings.TrimSpace(token) != token {
		return time.Time{}, errors.New("invalid Fernet token")
	}
	raw, err := base64.URLEncoding.Strict().DecodeString(token)
	if err != nil {
		raw, err = base64.RawURLEncoding.Strict().DecodeString(token)
	}
	if err != nil || len(raw) < 73 || raw[0] != 0x80 || (len(raw)-57)%16 != 0 {
		return time.Time{}, errors.New("invalid Fernet envelope")
	}
	seconds := binary.BigEndian.Uint64(raw[1:9])
	if seconds == 0 || seconds > 253402300799 {
		return time.Time{}, errors.New("invalid Fernet timestamp")
	}
	return time.Unix(int64(seconds), 0).UTC(), nil
}

func validateCodexTicket(token string, targets []int, ttl time.Duration, now time.Time) (time.Time, time.Time, error) {
	if !slices.Contains(targets, len(token)) {
		return time.Time{}, time.Time{}, fmt.Errorf("turn-state length %d; expected one of %v", len(token), targets)
	}
	issued, err := ParseCodexTurnStateIssuedAt(token)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if issued.After(now.Add(60 * time.Second)) {
		return time.Time{}, time.Time{}, errors.New("Fernet timestamp is in the future")
	}
	expires := issued.Add(ttl)
	if !expires.After(now) {
		return time.Time{}, time.Time{}, errors.New("turn-state ticket has expired")
	}
	return issued, expires, nil
}

func codexTicketIdentity(a *Account) string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	family, token, email := a.CredentialFamilyID, a.AccessToken, a.Email
	customAuthorization := a.CustomHeaders["Authorization"]
	for key, value := range a.CustomHeaders {
		if strings.EqualFold(key, "Authorization") {
			customAuthorization = value
		}
	}
	a.mu.RUnlock()
	workspace := a.EffectiveAccountID()
	user := ""
	if info := ParseAccessToken(token); info != nil {
		user = info.UserID
	}
	if user == "" && family == "" {
		user = token
	}
	sum := sha256.Sum256([]byte(family + "\x00" + workspace + "\x00" + user + "\x00" + email + "\x00" + customAuthorization))
	return hex.EncodeToString(sum[:])
}

func codexTicketAccount(a *Account) bool {
	if a == nil || a.IsRelayStyle() || a.IsCodexAgentIdentity() {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.DBID > 0 && a.AccessToken != "" && a.Status != StatusError && atomic.LoadInt32(&a.DispatchPaused) == 0
}

func codexTicketReady(rec database.CodexTurnStateRecord, a *Account, cfg CodexTurnStateSettings, now time.Time) bool {
	if rec.Identity != codexTicketIdentity(a) || rec.Token == "" {
		return false
	}
	_, expires, err := validateCodexTicket(rec.Token, cfg.TargetLengths, time.Duration(cfg.TTLSeconds)*time.Second, now)
	return err == nil && rec.ExpiresAt.After(now) && expires.After(now)
}

type CodexTurnStateStatus struct {
	Model            string `json:"model"`
	TokenLength      int    `json:"token_length"`
	TargetLengths    []int  `json:"target_lengths"`
	IssuedAt         string `json:"issued_at,omitempty"`
	ExpiresAt        string `json:"expires_at,omitempty"`
	CapturedAt       string `json:"captured_at,omitempty"`
	RemainingSeconds int64  `json:"remaining_seconds"`
	Ready            bool   `json:"ready"`
	Status           string `json:"status"`
	LastAttemptAt    string `json:"last_attempt_at,omitempty"`
	NextAttemptAt    string `json:"next_attempt_at,omitempty"`
	Attempts         int    `json:"attempts"`
	LastError        string `json:"last_error,omitempty"`
}

func turnStateTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
