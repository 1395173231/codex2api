package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
)

const resetCreditLimitAttemptNamespace = "reset-credit-limit-attempt"

type resetCreditLimitAttempt struct {
	StartedAt time.Time `json:"started_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

func autoResetCreditsAccountEligible(account *auth.Account) bool {
	if account == nil || atomic.LoadInt32(&account.Disabled) != 0 || atomic.LoadInt32(&account.DispatchPaused) != 0 || account.IsBanned() {
		return false
	}
	account.Mu().RLock()
	defer account.Mu().RUnlock()
	return (account.UpstreamType == "" || account.UpstreamType == "codex") &&
		isAutoResetCreditsPlan(account.PlanType) && strings.TrimSpace(account.AccessToken) != ""
}

// 仅主 rate_limit 的两个窗口参与判断。Pro 的 additional_rate_limits
// （包括 gpt-5.3-codex-spark）既不能触发消费，也不能阻止主额度恢复。
// 缺字段、普通 429 和未到 100% 的本地自动暂停不构成消费依据。
func resetCreditMainUsageState(usage *proxy.WhamUsage, now time.Time) (limited, recovered bool) {
	if usage == nil {
		return false, false
	}
	if usage.RateLimit.LimitReached {
		return true, false
	}
	known := false
	complete := true
	for _, window := range []*proxy.WhamUsageWindow{usage.RateLimit.PrimaryWindow, usage.RateLimit.SecondaryWindow} {
		if window == nil {
			complete = false
			continue
		}
		valid := !math.IsNaN(window.UsedPercent) && !math.IsInf(window.UsedPercent, 0) && window.UsedPercent >= 0 &&
			(window.ResetAt > 0 || window.ResetAfterSeconds > 0 || window.LimitWindowSeconds > 0)
		if !valid {
			complete = false
			continue
		}
		known = true
		if window.UsedPercent >= 100 {
			if window.ResetAt == 0 || window.ResetAt > now.Unix() {
				limited = true
			}
			// 到期但仍显示满额的窗口不是明确的恢复证据。
			complete = false
		}
	}
	return limited, !limited && known && complete && usage.RateLimit.Allowed
}

func (h *Handler) queryResetUsageWithRefresh(ctx context.Context, account *auth.Account) (*proxy.WhamUsage, error) {
	query := h.queryResetUsage
	if query == nil {
		query = proxy.QueryWhamUsage
	}
	usage, resp, err := query(ctx, account, h.store.ResolveProxyForAccount(account))
	status := upstreamResetStatus(resp)
	drainResetResponse(resp)
	if status == http.StatusUnauthorized {
		if refreshErr := h.refreshAccountForReset(ctx, account.DBID); refreshErr != nil {
			return nil, fmt.Errorf("usage refresh after 401: %w", refreshErr)
		}
		usage, resp, err = query(ctx, account, h.store.ResolveProxyForAccount(account))
		status = upstreamResetStatus(resp)
		drainResetResponse(resp)
	}
	if status != 0 && status != http.StatusOK {
		return nil, fmt.Errorf("reset-credit usage query returned status %d", status)
	}
	if err != nil {
		return nil, fmt.Errorf("reset-credit usage query: %w", err)
	}
	if usage == nil {
		return nil, fmt.Errorf("reset-credit usage query returned empty response")
	}
	return usage, nil
}

// 所有消费入口都记录尝试，避免手动/临期消费后，上游旧满额状态又触发自动消费。
// 必须在工作区租约内调用；未知结果也保留，直到新查询确认主额度恢复。
func (h *Handler) markResetCreditLimitAttempt(ctx context.Context, account *auth.Account) error {
	now := time.Now()
	attempt := resetCreditLimitAttempt{StartedAt: now, ExpiresAt: now.Add(autoResetCreditHandledIDRetention)}
	key := resetCreditLockKey(account)
	h.resetCreditLimitAttempts.Store(key, attempt)
	if h.cache != nil {
		raw, err := json.Marshal(attempt)
		if err != nil {
			return err
		}
		if err := h.cache.SetRuntime(ctx, resetCreditLimitAttemptNamespace, key, raw, autoResetCreditHandledIDRetention); err != nil {
			return fmt.Errorf("reserve reset-credit limit attempt: %w", err)
		}
	}
	return nil
}

func (h *Handler) loadResetCreditLimitAttempt(ctx context.Context, account *auth.Account) (resetCreditLimitAttempt, bool, error) {
	key := resetCreditLockKey(account)
	if h.cache != nil {
		raw, found, err := h.cache.GetRuntime(ctx, resetCreditLimitAttemptNamespace, key)
		if err != nil || !found {
			return resetCreditLimitAttempt{}, found, err
		}
		var attempt resetCreditLimitAttempt
		if err := json.Unmarshal(raw, &attempt); err != nil || attempt.StartedAt.IsZero() || attempt.ExpiresAt.IsZero() {
			return resetCreditLimitAttempt{}, true, fmt.Errorf("invalid reset-credit limit attempt")
		}
		return attempt, attempt.ExpiresAt.After(time.Now()), nil
	}
	if value, ok := h.resetCreditLimitAttempts.Load(key); ok {
		attempt := value.(resetCreditLimitAttempt)
		return attempt, attempt.ExpiresAt.After(time.Now()), nil
	}
	return resetCreditLimitAttempt{}, false, nil
}

func (h *Handler) resetCreditLimitAttemptPending(ctx context.Context, account *auth.Account) (bool, error) {
	_, pending, err := h.loadResetCreditLimitAttempt(ctx, account)
	return pending, err
}

// observedAt 是查询开始时间。消费前开始的慢查询不得解除后来建立的保护。
func (h *Handler) clearResetCreditLimitAttempt(ctx context.Context, account *auth.Account, observedAt time.Time) error {
	attempt, pending, err := h.loadResetCreditLimitAttempt(ctx, account)
	if err != nil || !pending || !observedAt.After(attempt.StartedAt) {
		return err
	}
	key := resetCreditLockKey(account)
	if h.cache != nil {
		if err := h.cache.DeleteRuntime(ctx, resetCreditLimitAttemptNamespace, key); err != nil {
			return fmt.Errorf("clear reset-credit limit attempt: %w", err)
		}
	}
	h.resetCreditLimitAttempts.Delete(key)
	return nil
}

// 后置 WHAM 探针确认恢复后允许未来的新一轮限额触发，但一小时冷却独立保留。
func (h *Handler) observeResetCreditRecovery(ctx context.Context, account *auth.Account, usage *proxy.WhamUsage, observedAt time.Time) {
	_, recovered := resetCreditMainUsageState(usage, time.Now())
	if !recovered || !proxy.CurrentRuntimeSettings().AutoResetCreditsOnLimitEnabled {
		return
	}
	lock := h.resetCreditLock(account)
	if !lock.TryLock() {
		return
	}
	defer lock.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	acquired, release, err := h.acquireResetCreditLease(ctx, account)
	if err != nil || !acquired {
		return
	}
	defer release()
	// 清理失败只保留保护，由下一次新鲜查询重试。
	_ = h.clearResetCreditLimitAttempt(ctx, account, observedAt)
}
