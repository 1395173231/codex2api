package admin

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
)

type modelSyncRequest struct {
	SyncGrok            bool `json:"sync_grok"`
	SyncOfficialPricing bool `json:"sync_official_pricing"`
}

type grokModelSyncError struct {
	AccountID int64  `json:"account_id"`
	Error     string `json:"error"`
}

type grokModelSyncResult struct {
	Total     int                  `json:"total"`
	Updated   int                  `json:"updated"`
	Unchanged int                  `json:"unchanged"`
	Failed    int                  `json:"failed"`
	Models    []string             `json:"models"`
	Errors    []grokModelSyncError `json:"errors,omitempty"`
}

type adminModelSyncResponse struct {
	*proxy.ModelSyncResult
	Grok            *grokModelSyncResult             `json:"grok,omitempty"`
	OfficialPricing *proxy.OfficialPricingSyncResult `json:"official_pricing,omitempty"`
	PricingError    string                           `json:"pricing_error,omitempty"`
}

type grokModelFetchResult struct {
	account *auth.Account
	models  []string
	err     error
}

func normalizedModelListKey(models []string) string {
	return strings.Join(auth.NormalizeAccountModels(models), "\x00")
}

// syncGrokAccountModels performs bounded network discovery first. Only after all
// responses have returned does it persist changed whitelists one account at a
// time, keeping every SQLite write short and independent.
func (h *Handler) syncGrokAccountModels(ctx context.Context) *grokModelSyncResult {
	result := &grokModelSyncResult{}
	if h == nil || h.store == nil {
		return result
	}
	accounts := make([]*auth.Account, 0)
	for _, account := range h.store.Accounts() {
		if account != nil && account.IsGrokAPI() {
			accounts = append(accounts, account)
		}
	}
	result.Total = len(accounts)
	if len(accounts) == 0 {
		return result
	}

	modelSet := make(map[string]struct{})
	persist := func(item grokModelFetchResult) {
		accountID := int64(0)
		if item.account != nil {
			accountID = item.account.DBID
		}
		if item.err != nil {
			result.Failed++
			result.Errors = append(result.Errors, grokModelSyncError{AccountID: accountID, Error: item.err.Error()})
			return
		}
		item.models = auth.NormalizeAccountModels(item.models)
		for _, model := range item.models {
			modelSet[model] = struct{}{}
		}
		if normalizedModelListKey(item.account.GrokModels()) == normalizedModelListKey(item.models) {
			result.Unchanged++
			return
		}
		writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := h.db.UpdateCredentials(writeCtx, accountID, map[string]interface{}{"models": item.models})
		cancel()
		if err != nil {
			result.Failed++
			result.Errors = append(result.Errors, grokModelSyncError{AccountID: accountID, Error: fmt.Sprintf("保存模型白名单失败: %v", err)})
			return
		}
		h.store.ApplyAccountModels(accountID, item.models)
		result.Updated++
	}

	// Four accounts per segment: fetch the segment in parallel, then persist its
	// results sequentially before starting the next segment. Large pools therefore
	// make incremental progress without creating a goroutine or DB writer storm.
	const segmentSize = 4
	for start := 0; start < len(accounts); start += segmentSize {
		if ctx.Err() != nil {
			for _, account := range accounts[start:] {
				result.Failed++
				result.Errors = append(result.Errors, grokModelSyncError{AccountID: account.DBID, Error: ctx.Err().Error()})
			}
			break
		}
		end := start + segmentSize
		if end > len(accounts) {
			end = len(accounts)
		}
		segment := make([]grokModelFetchResult, end-start)
		var wg sync.WaitGroup
		for index := start; index < end; index++ {
			segmentIndex := index - start
			account := accounts[index]
			wg.Add(1)
			go func() {
				defer wg.Done()
				fetchCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
				models, err := proxy.FetchGrokModelIDs(fetchCtx, account, h.store.ResolveProxyForAccount(account))
				cancel()
				segment[segmentIndex] = grokModelFetchResult{account: account, models: models, err: err}
			}()
		}
		wg.Wait()
		for _, item := range segment {
			persist(item)
		}
	}
	for model := range modelSet {
		result.Models = append(result.Models, model)
	}
	sort.Strings(result.Models)
	sort.Slice(result.Errors, func(i, j int) bool { return result.Errors[i].AccountID < result.Errors[j].AccountID })
	return result
}
