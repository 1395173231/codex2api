package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

const (
	compactionProvenanceCacheNamespace = "compaction-provenance"
	compactionAffinityTTLEnv           = "CODEX_COMPACTION_AFFINITY_TTL"
	nativeCodexCompactionDomain        = "codex:openai"
	defaultCompactionProvenanceTTL     = 7 * 24 * time.Hour
	compactionProvenanceRecordVersion  = 1
)

var errConflictingCompactionProvenance = errors.New("conflicting compaction provenance domains")

type compactionProvenanceRecord struct {
	Version             int       `json:"version"`
	AccountID           int64     `json:"account_id"`
	CompatibilityDomain string    `json:"compatibility_domain"`
	CreatedAt           time.Time `json:"created_at"`
	LastSeenAt          time.Time `json:"last_seen_at"`
}

type compactionAffinityResolution struct {
	Known               bool
	CompatibilityDomain string
	PreferredAccountID  int64
}

func compactionContentDigest(encryptedContent string) string {
	sum := sha256.Sum256([]byte(encryptedContent))
	return hex.EncodeToString(sum[:])
}

func compactionProvenanceTTL() time.Duration {
	raw := strings.TrimSpace(os.Getenv(compactionAffinityTTLEnv))
	if raw == "" {
		return defaultCompactionProvenanceTTL
	}
	ttl, err := time.ParseDuration(raw)
	if err != nil || ttl <= 0 {
		return defaultCompactionProvenanceTTL
	}
	return ttl
}

func canonicalCompactionBaseURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.RawQuery = ""
	parsed.Fragment = ""
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed.String()
}

func accountCompactionDomain(account *auth.Account) string {
	if account == nil {
		return ""
	}
	if account.IsOpenAIResponsesAPI() {
		baseURL, _ := account.OpenAIResponsesCredentials()
		if normalized, err := auth.NormalizeOpenAIResponsesBaseURL(baseURL); err == nil {
			baseURL = normalized
		}
		if canonical := canonicalCompactionBaseURL(baseURL); canonical != "" {
			return "responses:" + canonical
		}
		return ""
	}
	if account.IsGrokAPI() {
		baseURL, _ := account.GrokCredentials()
		if canonical := canonicalCompactionBaseURL(baseURL); canonical != "" {
			return "grok:" + canonical
		}
		return ""
	}
	return nativeCodexCompactionDomain
}

func decodeCompactionProvenanceRecord(raw json.RawMessage) (compactionProvenanceRecord, error) {
	var record compactionProvenanceRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return compactionProvenanceRecord{}, err
	}
	if record.Version != compactionProvenanceRecordVersion || record.AccountID <= 0 || strings.TrimSpace(record.CompatibilityDomain) == "" {
		return compactionProvenanceRecord{}, errors.New("invalid compaction provenance record")
	}
	return record, nil
}

func (h *Handler) recordCompactionProvenance(ctx context.Context, account *auth.Account, encryptedContent string) error {
	if h == nil || h.cache == nil || account == nil {
		return nil
	}
	encryptedContent = strings.TrimSpace(encryptedContent)
	domain := accountCompactionDomain(account)
	if encryptedContent == "" || domain == "" || account.ID() <= 0 {
		return nil
	}
	now := time.Now().UTC()
	record := compactionProvenanceRecord{
		Version:             compactionProvenanceRecordVersion,
		AccountID:           account.ID(),
		CompatibilityDomain: domain,
		CreatedAt:           now,
		LastSeenAt:          now,
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return h.cache.SetRuntime(ctx, compactionProvenanceCacheNamespace, compactionContentDigest(encryptedContent), raw, compactionProvenanceTTL())
}

func requestCompactionEncryptedContents(body []byte) []string {
	input := gjson.GetBytes(body, "input")
	if !input.Exists() {
		return nil
	}
	contents := make([]string, 0, 1)
	inspect := func(item gjson.Result) {
		if !gjsonResultIsCompactionHistory(item) {
			return
		}
		if encrypted := strings.TrimSpace(item.Get("encrypted_content").String()); encrypted != "" {
			contents = append(contents, encrypted)
		}
	}
	if input.IsArray() {
		input.ForEach(func(_, item gjson.Result) bool {
			inspect(item)
			return true
		})
	} else {
		inspect(input)
	}
	return contents
}

func (h *Handler) resolveCompactionAffinity(ctx context.Context, body []byte) (compactionAffinityResolution, error) {
	if h == nil || h.cache == nil {
		return compactionAffinityResolution{}, nil
	}
	contents := requestCompactionEncryptedContents(body)
	if len(contents) == 0 {
		return compactionAffinityResolution{}, nil
	}

	var resolution compactionAffinityResolution
	for _, encryptedContent := range contents {
		digest := compactionContentDigest(encryptedContent)
		raw, ok, err := h.cache.GetRuntime(ctx, compactionProvenanceCacheNamespace, digest)
		if err != nil {
			return compactionAffinityResolution{}, fmt.Errorf("read compaction provenance: %w", err)
		}
		if !ok {
			continue
		}
		record, err := decodeCompactionProvenanceRecord(raw)
		if err != nil {
			_ = h.cache.DeleteRuntime(ctx, compactionProvenanceCacheNamespace, digest)
			continue
		}
		if resolution.Known && resolution.CompatibilityDomain != record.CompatibilityDomain {
			return compactionAffinityResolution{}, errConflictingCompactionProvenance
		}
		if !resolution.Known {
			resolution = compactionAffinityResolution{
				Known:               true,
				CompatibilityDomain: record.CompatibilityDomain,
				PreferredAccountID:  record.AccountID,
			}
		}

		record.LastSeenAt = time.Now().UTC()
		refreshed, marshalErr := json.Marshal(record)
		if marshalErr == nil {
			_ = h.cache.SetRuntime(ctx, compactionProvenanceCacheNamespace, digest, refreshed, compactionProvenanceTTL())
		}
	}
	return resolution, nil
}

func compactionDomainFilter(domain string, next auth.AccountFilter) auth.AccountFilter {
	domain = strings.TrimSpace(domain)
	return func(account *auth.Account) bool {
		if account == nil || accountCompactionDomain(account) != domain {
			return false
		}
		return next == nil || next(account)
	}
}
