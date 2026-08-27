package auth

import (
	"context"
	"hash/fnv"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/codex2api/cache"
)

const (
	maxMessageAffinityHashes       = 64
	messageAffinityCacheTimeout    = 300 * time.Millisecond
	messageAffinityMinAbsoluteHits = 2
	messageAffinitySingleMinCount  = 3
)

type freshAffinityCandidate struct {
	acc               *Account
	schedulerPriority int64
	tierPriority      int
	limit             int64
	weight            uint64
}

func messageAffinityTTL() time.Duration {
	raw := strings.TrimSpace(os.Getenv("CODEX_MESSAGE_AFFINITY_TTL"))
	if raw == "" {
		return cache.DefaultMessageAffinityTTL
	}
	if duration, err := time.ParseDuration(raw); err == nil && duration > 0 {
		return duration
	}
	if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	return cache.DefaultMessageAffinityTTL
}

func messageAffinityScope(apiKeyID int64) string {
	if apiKeyID <= 0 {
		return ""
	}
	return "api-key:" + strconv.FormatInt(apiKeyID, 10)
}

func normalizeMessageAffinityHashes(hashes []uint64) []uint64 {
	if len(hashes) == 0 {
		return nil
	}
	limit := len(hashes)
	if limit > maxMessageAffinityHashes {
		limit = maxMessageAffinityHashes
	}
	normalized := make([]uint64, 0, limit)
	seen := make(map[uint64]struct{}, limit)
	for _, hash := range hashes {
		if hash == 0 {
			continue
		}
		if _, ok := seen[hash]; ok {
			continue
		}
		seen[hash] = struct{}{}
		normalized = append(normalized, hash)
		if len(normalized) == limit {
			break
		}
	}
	return normalized
}

// RecordMessageAffinity reinforces successful message fingerprints for the
// selected account. It is deliberately best-effort: exact affinity remains the
// source of truth and cache failures must not affect a completed response.
func (s *Store) RecordMessageAffinity(apiKeyID int64, hashes []uint64, account *Account) {
	if s == nil || account == nil || account.DBID == 0 {
		return
	}
	backend, ok := s.tokenCache.(cache.MessageAffinityCache)
	if !ok || backend == nil {
		return
	}
	scope := messageAffinityScope(apiKeyID)
	if scope == "" {
		return
	}
	hashes = normalizeMessageAffinityHashes(hashes)
	if len(hashes) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), messageAffinityCacheTimeout)
	defer cancel()
	if err := backend.RecordMessageAffinities(ctx, scope, hashes, account.DBID, messageAffinityTTL()); err != nil {
		log.Printf("写入消息散列账号亲和失败: account=%d err=%v", account.DBID, err)
	}
}

func (s *Store) getMessageAffinities(apiKeyID int64, hashes []uint64) (map[uint64]cache.MessageAffinityBinding, bool) {
	if s == nil {
		return nil, false
	}
	backend, ok := s.tokenCache.(cache.MessageAffinityCache)
	if !ok || backend == nil {
		return nil, false
	}
	scope := messageAffinityScope(apiKeyID)
	if scope == "" {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), messageAffinityCacheTimeout)
	defer cancel()
	bindings, err := backend.GetMessageAffinities(ctx, scope, hashes)
	if err != nil {
		log.Printf("读取消息散列账号亲和失败: %v", err)
		return nil, false
	}
	return bindings, len(bindings) > 0
}

func messageAffinityRequiredHits(hashCount int) int {
	if hashCount <= 1 {
		return 1
	}
	required := messageAffinityMinAbsoluteHits
	if hashCount >= 9 && hashCount/3 > required {
		required = hashCount / 3
	}
	return required
}

func (s *Store) nextAccountForFreshSessionWithDispatch(key string, apiKeyID int64, hashes []uint64, exclude map[int64]bool, filter AccountFilter, policy DispatchPolicy) *Account {
	if account := s.nextAccountForMessageAffinityWithDispatch(key, apiKeyID, hashes, exclude, filter, policy); account != nil {
		return account
	}
	return s.nextAccountForFreshAffinityWithDispatch(key, apiKeyID, exclude, filter, policy)
}

func (s *Store) nextAccountForMessageAffinityWithDispatch(key string, apiKeyID int64, hashes []uint64, exclude map[int64]bool, filter AccountFilter, policy DispatchPolicy) *Account {
	hashes = normalizeMessageAffinityHashes(hashes)
	if len(hashes) == 0 {
		return nil
	}
	layer := s.freshAffinityCandidateLayer(key, apiKeyID, exclude, filter, policy)
	if len(layer) == 0 {
		return nil
	}
	bindings, ok := s.getMessageAffinities(apiKeyID, hashes)
	if !ok {
		return nil
	}

	type scoredCandidate struct {
		freshAffinityCandidate
		hits        int
		singleCount int
	}
	scores := make(map[int64]*scoredCandidate, len(layer))
	for _, candidate := range layer {
		copy := candidate
		scores[candidate.acc.DBID] = &scoredCandidate{freshAffinityCandidate: copy}
	}
	for _, hash := range hashes {
		binding, exists := bindings[hash]
		if !exists || binding.AccountID == 0 || binding.Count <= 0 || binding.Stop {
			continue
		}
		candidate := scores[binding.AccountID]
		if candidate == nil {
			continue
		}
		candidate.hits++
		if len(hashes) == 1 {
			candidate.singleCount = binding.Count
		}
	}

	requiredHits := messageAffinityRequiredHits(len(hashes))
	ranked := make([]*scoredCandidate, 0, len(scores))
	for _, candidate := range scores {
		if candidate.hits < requiredHits {
			continue
		}
		if len(hashes) == 1 && candidate.singleCount < messageAffinitySingleMinCount {
			continue
		}
		ranked = append(ranked, candidate)
	}
	if len(ranked) == 0 {
		return nil
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].hits != ranked[j].hits {
			return ranked[i].hits > ranked[j].hits
		}
		if ranked[i].weight != ranked[j].weight {
			return ranked[i].weight > ranked[j].weight
		}
		return ranked[i].acc.DBID < ranked[j].acc.DBID
	})
	for _, candidate := range ranked {
		if s.accountHasBlockingCachedCooldown(candidate.acc, policy) {
			continue
		}
		if s.tryAcquireAccount(candidate.acc, candidate.limit, true) {
			return candidate.acc
		}
	}
	return nil
}

func (s *Store) freshAffinityCandidateLayer(key string, apiKeyID int64, exclude map[int64]bool, filter AccountFilter, policy DispatchPolicy) []freshAffinityCandidate {
	if s == nil {
		return nil
	}
	filter = s.withUsableEgressFilter(filter)
	maxConcurrency := atomic.LoadInt64(&s.maxConcurrency)
	accounts := s.Accounts()
	candidates := make([]freshAffinityCandidate, 0, len(accounts))
	for _, account := range accounts {
		if account == nil || (exclude != nil && exclude[account.DBID]) {
			continue
		}
		if !account.dispatchableForPolicy(policy) || !s.accountAllowedForAPIKey(account, apiKeyID) {
			continue
		}
		if filter != nil && !filter(account) {
			continue
		}
		load := accountOccupiedRequests(account)
		tier, _, _, limit := account.schedulerSnapshotForPolicy(maxConcurrency, policy)
		if limit <= 0 || load >= limit {
			continue
		}
		hasher := fnv.New64a()
		_, _ = hasher.Write([]byte(key))
		_, _ = hasher.Write([]byte{':'})
		_, _ = hasher.Write([]byte(strconv.FormatInt(account.DBID, 10)))
		candidates = append(candidates, freshAffinityCandidate{
			acc:               account,
			schedulerPriority: account.schedulerPriority(),
			tierPriority:      tierPriority(tier),
			limit:             limit,
			weight:            hasher.Sum64(),
		})
	}
	if len(candidates) == 0 {
		return nil
	}
	bestSchedulerPriority := candidates[0].schedulerPriority
	bestTierPriority := candidates[0].tierPriority
	for _, candidate := range candidates[1:] {
		if candidate.schedulerPriority > bestSchedulerPriority ||
			(candidate.schedulerPriority == bestSchedulerPriority && candidate.tierPriority > bestTierPriority) {
			bestSchedulerPriority = candidate.schedulerPriority
			bestTierPriority = candidate.tierPriority
		}
	}
	layer := make([]freshAffinityCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.schedulerPriority == bestSchedulerPriority && candidate.tierPriority == bestTierPriority {
			layer = append(layer, candidate)
		}
	}
	return layer
}
