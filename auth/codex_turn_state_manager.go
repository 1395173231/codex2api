package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/database"
)

type CodexTurnStateProbe func(context.Context, *Account, string, string) (string, error)
type turnStateKey struct {
	account int64
	model   string
}
type turnStateObservation struct {
	account               *Account
	model, used, observed string
}

type CodexTurnStateManager struct {
	db           *database.DB
	store        *Store
	probe        CodexTurnStateProbe
	mu           sync.RWMutex
	opMu         sync.Mutex
	config       CodexTurnStateSettings
	records      map[turnStateKey]database.CodexTurnStateRecord
	inflight     map[turnStateKey]context.CancelFunc
	generation   uint64
	wake         chan struct{}
	observations map[turnStateKey]turnStateObservation
	startOnce    sync.Once
}

func NewCodexTurnStateManager(ctx context.Context, db *database.DB, store *Store, probe CodexTurnStateProbe) (*CodexTurnStateManager, error) {
	m := &CodexTurnStateManager{db: db, store: store, probe: probe, config: DefaultCodexTurnStateSettings(),
		records: make(map[turnStateKey]database.CodexTurnStateRecord), inflight: make(map[turnStateKey]context.CancelFunc),
		wake: make(chan struct{}, 1), observations: make(map[turnStateKey]turnStateObservation)}
	if db == nil || store == nil {
		return nil, errors.New("turn-state store unavailable")
	}
	if err := m.Reload(ctx); err != nil {
		return nil, err
	}
	store.codexTurnStates.Store(m)
	for _, a := range store.Accounts() {
		a.mu.Lock()
		a.codexTurnStateManager = m
		a.mu.Unlock()
	}
	return m, nil
}

func (m *CodexTurnStateManager) Config() CodexTurnStateSettings {
	m.mu.RLock()
	defer m.mu.RUnlock()
	cfg := m.config
	cfg.Models = slices.Clone(cfg.Models)
	cfg.TargetLengths = slices.Clone(cfg.TargetLengths)
	return cfg
}

func sameTurnStateConfig(a, b CodexTurnStateSettings) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func (m *CodexTurnStateManager) publishConfig(cfg CodexTurnStateSettings) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !sameTurnStateConfig(m.config, cfg) {
		m.generation++
		for _, cancel := range m.inflight {
			cancel()
		}
		m.config = cfg
	}
}

func (m *CodexTurnStateManager) Reload(ctx context.Context) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	raw, err := m.db.LoadCodexTurnStateConfig(ctx)
	if err != nil {
		return err
	}
	cfg, err := DecodeCodexTurnStateSettings(raw)
	if err != nil {
		return errors.New("invalid persisted turn-state configuration")
	}
	cfg, err = NormalizeCodexTurnStateSettings(cfg)
	if err != nil {
		return err
	}
	rows, err := m.db.ListCodexTurnStates(ctx)
	if err != nil {
		return err
	}
	m.publishConfig(cfg)
	records := make(map[turnStateKey]database.CodexTurnStateRecord, len(rows))
	for _, rec := range rows {
		records[turnStateKey{rec.AccountID, rec.Model}] = rec
	}
	m.mu.Lock()
	m.records = records
	m.mu.Unlock()
	return nil
}

func (m *CodexTurnStateManager) SaveConfig(ctx context.Context, cfg CodexTurnStateSettings) error {
	cfg, err := NormalizeCodexTurnStateSettings(cfg)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if err := m.db.SaveCodexTurnStateConfig(ctx, string(raw)); err != nil {
		return err
	}
	m.publishConfig(cfg)
	m.Wake()
	return nil
}

func (m *CodexTurnStateManager) Wake() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *CodexTurnStateManager) Injection(a *Account, model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	cfg := m.Config()
	if !cfg.Enabled || !codexTicketAccount(a) || !slices.Contains(cfg.Models, model) {
		return ""
	}
	m.mu.RLock()
	rec := m.records[turnStateKey{a.ID(), model}]
	revoked := m.observations[turnStateKey{a.ID(), model}].used == rec.Token && rec.Token != ""
	m.mu.RUnlock()
	if !revoked && codexTicketReady(rec, a, cfg, time.Now()) {
		return rec.Token
	}
	return ""
}

func (m *CodexTurnStateManager) Statuses(id int64, a *Account) []CodexTurnStateStatus {
	cfg := m.Config()
	now := time.Now()
	models := slices.Clone(cfg.Models)
	m.mu.RLock()
	records := make(map[string]database.CodexTurnStateRecord)
	busy := make(map[string]bool)
	for key, rec := range m.records {
		if key.account == id {
			records[key.model] = rec
			if !slices.Contains(models, key.model) {
				models = append(models, key.model)
			}
		}
	}
	for key := range m.inflight {
		if key.account == id {
			busy[key.model] = true
		}
	}
	m.mu.RUnlock()
	result := make([]CodexTurnStateStatus, 0, len(models))
	for _, model := range models {
		rec := records[model]
		status := CodexTurnStateStatus{Model: model, TargetLengths: slices.Clone(cfg.TargetLengths), TokenLength: len(rec.Token),
			IssuedAt: turnStateTime(rec.IssuedAt), ExpiresAt: turnStateTime(rec.ExpiresAt), CapturedAt: turnStateTime(rec.CapturedAt),
			LastAttemptAt: turnStateTime(rec.LastAttemptAt), NextAttemptAt: turnStateTime(rec.NextAttemptAt), Attempts: rec.Attempts, LastError: rec.LastError, Status: "missing"}
		if a != nil && codexTicketReady(rec, a, cfg, now) {
			status.Ready = true
			expires := minTime(rec.ExpiresAt, rec.IssuedAt.Add(time.Duration(cfg.TTLSeconds)*time.Second))
			status.ExpiresAt = turnStateTime(expires)
			status.RemainingSeconds = max(0, int64(expires.Sub(now)/time.Second))
			status.Status = "ready"
		} else if rec.Token != "" {
			status.Status = "expired"
		} else if rec.LastError != "" {
			status.Status = "error"
		}
		if busy[model] || rec.LeaseUntil > now.Unix() {
			status.Status = "refreshing"
		}
		if reason := codexTicketHarvestPauseReason(a, model, now); reason != "" {
			status.Status = "paused"
			status.PauseReason = reason
		}
		if !cfg.Enabled || !slices.Contains(cfg.Models, model) || !codexTicketAccount(a) {
			status.Status = "disabled"
			status.Ready = false
		}
		result = append(result, status)
	}
	return result
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func (m *CodexTurnStateManager) Replace(ctx context.Context, a *Account, model, token string) error {
	model, err := NormalizeCodexTurnStateModel(model)
	if err != nil {
		return err
	}
	if a == nil || a.IsRelayStyle() {
		return errors.New("Codex account required")
	}
	cfg := m.Config()
	now := time.Now()
	rec := database.CodexTurnStateRecord{AccountID: a.ID(), Model: model, Identity: codexTicketIdentity(a)}
	token = strings.TrimSpace(token)
	if token != "" {
		issued, expires, err := validateCodexTicket(token, cfg.TargetLengths, time.Duration(cfg.TTLSeconds)*time.Second, now)
		if err != nil {
			return err
		}
		rec.Token = token
		rec.IssuedAt = issued
		rec.ExpiresAt = expires
		rec.CapturedAt = now
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	rec, err = m.db.ReplaceCodexTurnState(ctx, rec)
	if err != nil {
		return errors.New("cannot save ticket; each model requires its own distinct token")
	}
	key := turnStateKey{a.ID(), model}
	m.mu.Lock()
	if cancel := m.inflight[key]; cancel != nil {
		cancel()
	}
	m.records[key] = rec
	m.mu.Unlock()
	m.Wake()
	return nil
}

func (m *CodexTurnStateManager) RequestRefresh(ctx context.Context, a *Account, model string) error {
	model, err := NormalizeCodexTurnStateModel(model)
	if err != nil {
		return err
	}
	cfg := m.Config()
	if !cfg.Enabled || !codexTicketAccount(a) || !slices.Contains(cfg.Models, model) {
		return errors.New("enable collection and select an active Codex account/model first")
	}
	if reason := codexTicketHarvestPauseReason(a, model, time.Now()); reason != "" {
		return fmt.Errorf("turn-state collection paused: %s", reason)
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	key := turnStateKey{a.ID(), model}
	rec, err := m.db.RequestCodexTurnStateRefresh(ctx, a.ID(), model)
	if err != nil {
		return err
	}
	m.mu.Lock()
	if cancel := m.inflight[key]; cancel != nil {
		cancel()
	}
	m.records[key] = rec
	m.mu.Unlock()
	m.Wake()
	return nil
}

// Run is owned by the database background-task lifecycle. Only the bounded
// worker set performs network work; request handlers never wait for harvests.
func (m *CodexTurnStateManager) Run(ctx context.Context) {
	m.startOnce.Do(func() { m.run(ctx) })
}

func (m *CodexTurnStateManager) run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var wg sync.WaitGroup
	defer func() {
		m.mu.Lock()
		for _, cancel := range m.inflight {
			cancel()
		}
		m.mu.Unlock()
		wg.Wait()
	}()
	nextReload := time.Now().Add(5 * time.Second)
	for {
		if ctx.Err() != nil {
			return
		}
		m.mu.RLock()
		pending := make([]turnStateObservation, 0, len(m.observations))
		for _, event := range m.observations {
			pending = append(pending, event)
		}
		m.mu.RUnlock()
		for _, event := range pending {
			m.invalidateObservation(ctx, event)
		}
		m.schedule(ctx, &wg)
		select {
		case <-ctx.Done():
			return
		case <-m.wake:
		case <-ticker.C:
			if time.Now().After(nextReload) {
				loadCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				if err := m.Reload(loadCtx); err != nil && ctx.Err() == nil {
					log.Printf("[turn-state] settings/state reload failed: %v", err)
				}
				cancel()
				nextReload = time.Now().Add(5 * time.Second)
			}
		}
	}
}

func (m *CodexTurnStateManager) schedule(ctx context.Context, wg *sync.WaitGroup) {
	m.mu.RLock()
	cfg := m.config
	generation := m.generation
	m.mu.RUnlock()
	if !cfg.Enabled || m.probe == nil {
		return
	}
	type task struct {
		a     *Account
		model string
		due   time.Time
	}
	var tasks []task
	now := time.Now()
	for _, a := range m.store.Accounts() {
		if !codexTicketAccount(a) {
			continue
		}
		for _, model := range cfg.Models {
			key := turnStateKey{a.ID(), model}
			if codexTicketHarvestPauseReason(a, model, now) != "" {
				m.mu.Lock()
				if cancel := m.inflight[key]; cancel != nil {
					cancel()
				}
				m.mu.Unlock()
				continue
			}
			m.mu.RLock()
			rec := m.records[key]
			_, busy := m.inflight[key]
			m.mu.RUnlock()
			if busy || rec.LeaseUntil > now.Unix() || !turnStateDue(rec, a, cfg, now) {
				continue
			}
			tasks = append(tasks, task{a, model, rec.NextAttemptAt})
		}
	}
	sort.SliceStable(tasks, func(i, j int) bool { return tasks[i].due.Before(tasks[j].due) })
	for _, task := range tasks {
		if ctx.Err() != nil {
			return
		}
		m.mu.Lock()
		if len(m.inflight) >= cfg.Concurrency || generation != m.generation {
			m.mu.Unlock()
			return
		}
		key := turnStateKey{task.a.ID(), task.model}
		probeCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.AttemptTimeoutSeconds+10)*time.Second)
		m.inflight[key] = cancel
		m.mu.Unlock()
		wg.Add(1)
		go func(a *Account, model string) {
			defer wg.Done()
			defer cancel()
			defer func() { m.mu.Lock(); delete(m.inflight, key); m.mu.Unlock() }()
			if err := m.probeOnce(probeCtx, a, model, cfg, generation); err != nil && ctx.Err() == nil {
				log.Printf("[turn-state] account=%d model=%s persistence failed", a.ID(), model)
			}
		}(task.a, task.model)
	}
}

func (m *CodexTurnStateManager) probeOnce(ctx context.Context, a *Account, model string, cfg CodexTurnStateSettings, generation uint64) error {
	now := time.Now()
	m.opMu.Lock()
	rec, claimed, err := m.db.ClaimCodexTurnState(ctx, a.ID(), model, now, time.Duration(cfg.AttemptTimeoutSeconds+15)*time.Second)
	m.opMu.Unlock()
	if err != nil || !claimed {
		return err
	}
	claimedRecord := rec
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		_ = m.db.ReleaseCodexTurnState(releaseCtx, claimedRecord)
		m.mu.Lock()
		key := turnStateKey{a.ID(), model}
		cached := m.records[key]
		if cached.Lease == claimedRecord.Lease {
			cached.Lease = ""
			cached.LeaseUntil = 0
			m.records[key] = cached
		}
		m.mu.Unlock()
	}()
	identity := codexTicketIdentity(a)
	if rec.Identity != identity {
		rec = database.CodexTurnStateRecord{AccountID: a.ID(), Model: model, Identity: identity, Revision: rec.Revision, Lease: rec.Lease, LeaseUntil: rec.LeaseUntil}
	}
	if !turnStateDue(rec, a, cfg, now) {
		_, err = m.db.CommitCodexTurnState(ctx, rec)
		return err
	}
	rec.Attempts++
	rec.LastAttemptAt = now
	rec.RefreshRequested = false
	probeCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.AttemptTimeoutSeconds)*time.Second)
	token, probeErr := m.probe(probeCtx, a, model, cfg.HarvestProxyURL)
	cancel()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	issued, expires, validationErr := validateCodexTicket(token, cfg.TargetLengths, time.Duration(cfg.TTLSeconds)*time.Second, time.Now())
	if probeErr == nil && validationErr == nil {
		rec.Token = token
		rec.IssuedAt = issued
		rec.ExpiresAt = expires
		rec.CapturedAt = time.Now()
		rec.LastError = ""
		rec.Failures = 0
		rec.NextAttemptAt = expires.Add(-time.Duration(cfg.RefreshBeforeSeconds) * time.Second)
		if rec.NextAttemptAt.Before(time.Now()) {
			rec.NextAttemptAt = time.Now().Add(time.Duration(cfg.RetryIntervalSeconds) * time.Second)
		}
	} else {
		rec.Failures++
		delay := cfg.RetryIntervalSeconds
		if rec.Failures >= cfg.MaxAttempts {
			delay *= cfg.MaxAttempts
			rec.Failures = 0
		}
		rec.NextAttemptAt = time.Now().Add(time.Duration(delay) * time.Second)
		// Never persist raw network errors: they may embed proxy credentials.
		if probeErr != nil {
			rec.LastError = "harvest request failed"
		} else {
			rec.LastError = validationErr.Error()
		}
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.mu.RLock()
	currentGeneration := m.generation
	m.mu.RUnlock()
	if currentGeneration != generation || codexTicketIdentity(a) != identity || !codexTicketAccount(a) || codexTicketHarvestPauseReason(a, model, time.Now()) != "" {
		return nil
	}
	changed, err := m.db.CommitCodexTurnState(ctx, rec)
	if err != nil && probeErr == nil && validationErr == nil {
		// A duplicate model ticket cannot be published. Persist a bounded retry
		// while retaining this model's previous token when it is still valid.
		previous := claimedRecord
		if previous.Identity != identity {
			previous.Token = ""
			previous.IssuedAt = time.Time{}
			previous.ExpiresAt = time.Time{}
		}
		previous.Identity = identity
		previous.Attempts = rec.Attempts
		previous.LastAttemptAt = rec.LastAttemptAt
		previous.LastError = "cannot persist ticket; another model may already own this token"
		previous.NextAttemptAt = time.Now().Add(time.Duration(cfg.RetryIntervalSeconds) * time.Second)
		previous.RefreshRequested = false
		rec = previous
		changed, err = m.db.CommitCodexTurnState(ctx, rec)
	}
	if err == nil && changed {
		rec.Lease = ""
		rec.LeaseUntil = 0
		rec.Revision++
		m.mu.Lock()
		m.records[turnStateKey{a.ID(), model}] = rec
		m.mu.Unlock()
	}
	return err
}

// A 312-character observation is a configurable-policy miss, not an HTTP 312.
// It only invalidates the ticket used by that attempt, never a newer replacement.
func (m *CodexTurnStateManager) Observe(a *Account, model, used, observed string) {
	model = strings.ToLower(strings.TrimSpace(model))
	if len(observed) != 312 || used == "" {
		return
	}
	cfg := m.Config()
	if !cfg.Enabled || a == nil || slices.Contains(cfg.TargetLengths, 312) {
		return
	}
	if _, err := ParseCodexTurnStateIssuedAt(observed); err != nil {
		return
	}
	key := turnStateKey{a.ID(), model}
	m.mu.Lock()
	if m.records[key].Token == used {
		m.observations[key] = turnStateObservation{a, model, used, observed}
	}
	m.mu.Unlock()
	m.Wake()
}

func turnStateDue(rec database.CodexTurnStateRecord, a *Account, cfg CodexTurnStateSettings, now time.Time) bool {
	if rec.RefreshRequested || rec.Identity != codexTicketIdentity(a) {
		return true
	}
	// Backoff applies only to failed probes. A changed TTL/length must not wait
	// for a successful ticket's old refresh deadline.
	if rec.LastError != "" && rec.NextAttemptAt.After(now) {
		return false
	}
	if !codexTicketReady(rec, a, cfg, now) {
		return true
	}
	expires := minTime(rec.ExpiresAt, rec.IssuedAt.Add(time.Duration(cfg.TTLSeconds)*time.Second))
	return !expires.After(now.Add(time.Duration(cfg.RefreshBeforeSeconds)*time.Second)) && !rec.CapturedAt.Add(time.Duration(cfg.RetryIntervalSeconds)*time.Second).After(now)
}

func (m *CodexTurnStateManager) invalidateObservation(ctx context.Context, event turnStateObservation) {
	key := turnStateKey{event.account.ID(), event.model}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.mu.RLock()
	rec := m.records[key]
	m.mu.RUnlock()
	if rec.Token != event.used || rec.Identity != codexTicketIdentity(event.account) {
		m.mu.Lock()
		if m.observations[key].used == event.used {
			delete(m.observations, key)
		}
		m.mu.Unlock()
		return
	}
	// Compare the actual persisted token, even while a harvest holds a lease.
	updated, changed, err := m.db.InvalidateCodexTurnState(ctx, event.account.ID(), event.model, event.used)
	if err != nil {
		return
	}
	m.mu.Lock()
	if pending := m.observations[key]; pending.used == event.used {
		delete(m.observations, key)
	}
	if changed {
		if cancel := m.inflight[key]; cancel != nil {
			cancel()
		}
	}
	m.records[key] = updated
	m.mu.Unlock()
}
