package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

const subscriptionUpgradeTestWorkspaceID = "288c5d93-a113-4ed3-b6a9-08b6a4d35417"

type fakeSubscriptionUpgradeUpstream struct {
	readResult   *proxy.ChatGPTSubscription
	readErr      error
	readResults  []*proxy.ChatGPTSubscription
	readErrors   []error
	readCount    int
	quoteResult  *proxy.SubscriptionUpgradeQuote
	submitResult *proxy.SubscriptionUpgradeSubmitResult
	submitErr    error
	submitCount  int
}

func (f *fakeSubscriptionUpgradeUpstream) Read(context.Context, proxy.SubscriptionUpgradeCredentials) (*proxy.ChatGPTSubscription, error) {
	if f.readCount < len(f.readResults) || f.readCount < len(f.readErrors) {
		index := f.readCount
		f.readCount++
		var result *proxy.ChatGPTSubscription
		var err error
		if index < len(f.readResults) {
			result = f.readResults[index]
		}
		if index < len(f.readErrors) {
			err = f.readErrors[index]
		}
		return result, err
	}
	return f.readResult, f.readErr
}

func (f *fakeSubscriptionUpgradeUpstream) Quote(context.Context, proxy.SubscriptionUpgradeCredentials, string, string) (*proxy.SubscriptionUpgradeQuote, error) {
	return f.quoteResult, nil
}

func TestSubscriptionUpgradeTokenInvalidationRequiresReauthenticationWithoutSecondPaidPost(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "codex2api.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	defer db.Close()
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2})
	account := &auth.Account{DBID: 20, AccessToken: "test-at", AccountID: subscriptionUpgradeTestWorkspaceID, PlanType: "prolite"}
	store.AddAccount(account)
	prolite := &proxy.ChatGPTSubscription{PlanType: "prolite", BillingCurrency: "PHP"}
	fake := &fakeSubscriptionUpgradeUpstream{
		readResults: []*proxy.ChatGPTSubscription{prolite, prolite, nil},
		readErrors:  []error{nil, nil, &proxy.SubscriptionUpstreamHTTPError{StatusCode: http.StatusUnauthorized, Body: "invalidated"}},
		quoteResult: &proxy.SubscriptionUpgradeQuote{
			Currency: "PHP", AmountDueMinor: 345196, RecurringAmountMinor: 999000,
		},
		submitResult: &proxy.SubscriptionUpgradeSubmitResult{Status: "succeeded"},
	}
	handler := &Handler{
		store:                      store,
		db:                         db,
		subscriptionUpgradeEnabled: true,
		subscriptionUpgradeQuotes:  make(map[string]subscriptionUpgradeQuoteRecord),
		subscriptionUpgradeClientFactory: func(*auth.Account, string) subscriptionUpgradeUpstream {
			return fake
		},
	}

	quoteRecorder := httptest.NewRecorder()
	quoteContext, _ := gin.CreateTestContext(quoteRecorder)
	quoteContext.Params = gin.Params{{Key: "id", Value: "20"}}
	quoteContext.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/20/subscription/upgrade-quotes", strings.NewReader(`{"target_plan":"chatgptpro","currency":"PHP"}`))
	handler.CreateSubscriptionUpgradeQuote(quoteContext)
	if strings.Contains(quoteRecorder.Body.String(), `"silent_reauthorization_available":true`) {
		t.Fatal("quote must not claim silent reauthorization without a Web Session")
	}
	var quoteResponse struct {
		QuoteID string `json:"quote_id"`
	}
	if err := json.Unmarshal(quoteRecorder.Body.Bytes(), &quoteResponse); err != nil || quoteResponse.QuoteID == "" {
		t.Fatalf("quote response = %s, err=%v", quoteRecorder.Body.String(), err)
	}

	body := `{"quote_id":"` + quoteResponse.QuoteID + `","currency":"PHP","max_amount_minor":350000,"confirmed":true}`
	invoke := func() *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Params = gin.Params{{Key: "id", Value: "20"}}
		ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/20/subscription/upgrades", strings.NewReader(body))
		ctx.Request.Header.Set("Idempotency-Key", "upgrade-once")
		handler.CreateSubscriptionUpgrade(ctx)
		return recorder
	}
	first := invoke()
	if first.Code != http.StatusAccepted || !strings.Contains(first.Body.String(), "verification_requires_reauthentication") {
		t.Fatalf("first response status=%d body=%s", first.Code, first.Body.String())
	}
	second := invoke()
	if second.Code != http.StatusOK || !strings.Contains(second.Body.String(), "verification_requires_reauthentication") {
		t.Fatalf("idempotent response status=%d body=%s", second.Code, second.Body.String())
	}
	if fake.submitCount != 1 {
		t.Fatalf("paid POST count = %d, want exactly one", fake.submitCount)
	}
}

func TestSubscriptionUpgradeUsesStoredWebSessionOnlyForReadOnlyRecovery(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "codex2api.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	defer db.Close()
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2})
	account := &auth.Account{
		DBID: 20, AccessToken: "old-at", SessionToken: "independent-web-session",
		AccountID: subscriptionUpgradeTestWorkspaceID, PlanType: "prolite",
	}
	store.AddAccount(account)
	prolite := &proxy.ChatGPTSubscription{PlanType: "prolite", BillingCurrency: "PHP"}
	fake := &fakeSubscriptionUpgradeUpstream{
		readResults:  []*proxy.ChatGPTSubscription{prolite, prolite, nil, {PlanType: "pro"}},
		readErrors:   []error{nil, nil, &proxy.SubscriptionUpstreamHTTPError{StatusCode: http.StatusUnauthorized}, nil},
		quoteResult:  &proxy.SubscriptionUpgradeQuote{Currency: "PHP", AmountDueMinor: 345196, RecurringAmountMinor: 999000},
		submitResult: &proxy.SubscriptionUpgradeSubmitResult{Status: "succeeded"},
	}
	refreshCount := 0
	handler := &Handler{
		store:                      store,
		db:                         db,
		subscriptionUpgradeEnabled: true,
		subscriptionUpgradeQuotes:  make(map[string]subscriptionUpgradeQuoteRecord),
		subscriptionUpgradeClientFactory: func(*auth.Account, string) subscriptionUpgradeUpstream {
			return fake
		},
		refreshAccount: func(context.Context, int64) error {
			refreshCount++
			account.AccessToken = "new-at"
			return nil
		},
	}

	quoteRecorder := httptest.NewRecorder()
	quoteContext, _ := gin.CreateTestContext(quoteRecorder)
	quoteContext.Params = gin.Params{{Key: "id", Value: "20"}}
	quoteContext.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/20/subscription/upgrade-quotes", strings.NewReader(`{"target_plan":"chatgptpro","currency":"PHP"}`))
	handler.CreateSubscriptionUpgradeQuote(quoteContext)
	if !strings.Contains(quoteRecorder.Body.String(), `"silent_reauthorization_available":true`) {
		t.Fatalf("quote does not report stored Web Session readiness: %s", quoteRecorder.Body.String())
	}
	var quoteResponse struct {
		QuoteID string `json:"quote_id"`
	}
	_ = json.Unmarshal(quoteRecorder.Body.Bytes(), &quoteResponse)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: "20"}}
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/20/subscription/upgrades", strings.NewReader(`{"quote_id":"`+quoteResponse.QuoteID+`","currency":"PHP","max_amount_minor":350000,"confirmed":true}`))
	ctx.Request.Header.Set("Idempotency-Key", "upgrade-with-web-session")
	handler.CreateSubscriptionUpgrade(ctx)

	if recorder.Code != http.StatusAccepted || !strings.Contains(recorder.Body.String(), `"status":"succeeded"`) {
		t.Fatalf("response status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if refreshCount != 1 || fake.submitCount != 1 {
		t.Fatalf("refresh count=%d submit count=%d, want 1 and 1", refreshCount, fake.submitCount)
	}
}

func TestSubscriptionUpgradeFeatureIsDisabledByDefault(t *testing.T) {
	t.Setenv("CODEX2API_SUBSCRIPTION_UPGRADES_ENABLED", "")
	if subscriptionUpgradeFeatureEnabled() {
		t.Fatal("subscription upgrade feature must default to disabled")
	}
}

func (f *fakeSubscriptionUpgradeUpstream) Submit(context.Context, proxy.SubscriptionUpgradeCredentials, proxy.SubscriptionUpgradeSubmission) (*proxy.SubscriptionUpgradeSubmitResult, error) {
	f.submitCount++
	return f.submitResult, f.submitErr
}

func TestSubscriptionUpgradeRejectsFreshQuoteAboveConfirmedCapWithoutSubmitting(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "codex2api.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	defer db.Close()
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2})
	account := &auth.Account{DBID: 20, AccessToken: "test-at", AccountID: subscriptionUpgradeTestWorkspaceID, PlanType: "prolite"}
	store.AddAccount(account)
	fake := &fakeSubscriptionUpgradeUpstream{
		readResult: &proxy.ChatGPTSubscription{PlanType: "prolite", BillingCurrency: "PHP"},
		quoteResult: &proxy.SubscriptionUpgradeQuote{
			Currency: "PHP", AmountDueMinor: 345196, RecurringAmountMinor: 999000,
		},
	}
	handler := &Handler{
		store:                      store,
		db:                         db,
		subscriptionUpgradeEnabled: true,
		subscriptionUpgradeQuotes:  make(map[string]subscriptionUpgradeQuoteRecord),
		subscriptionUpgradeClientFactory: func(*auth.Account, string) subscriptionUpgradeUpstream {
			return fake
		},
	}

	quoteRecorder := httptest.NewRecorder()
	quoteContext, _ := gin.CreateTestContext(quoteRecorder)
	quoteContext.Params = gin.Params{{Key: "id", Value: "20"}}
	quoteContext.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/20/subscription/upgrade-quotes", strings.NewReader(`{"target_plan":"chatgptpro","currency":"PHP"}`))
	handler.CreateSubscriptionUpgradeQuote(quoteContext)
	if quoteRecorder.Code != http.StatusOK {
		t.Fatalf("quote status = %d, body=%s", quoteRecorder.Code, quoteRecorder.Body.String())
	}
	var quoteResponse struct {
		QuoteID string `json:"quote_id"`
	}
	if err := json.Unmarshal(quoteRecorder.Body.Bytes(), &quoteResponse); err != nil || quoteResponse.QuoteID == "" {
		t.Fatalf("decode quote response: %v, body=%s", err, quoteRecorder.Body.String())
	}

	upgradeRecorder := httptest.NewRecorder()
	upgradeContext, _ := gin.CreateTestContext(upgradeRecorder)
	upgradeContext.Params = gin.Params{{Key: "id", Value: "20"}}
	upgradeContext.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/20/subscription/upgrades", strings.NewReader(`{
		"quote_id":"`+quoteResponse.QuoteID+`",
		"currency":"PHP",
		"max_amount_minor":300000,
		"confirmed":true
	}`))
	upgradeContext.Request.Header.Set("Idempotency-Key", "upgrade-once")
	handler.CreateSubscriptionUpgrade(upgradeContext)

	if upgradeRecorder.Code != http.StatusConflict {
		t.Fatalf("upgrade status = %d, body=%s", upgradeRecorder.Code, upgradeRecorder.Body.String())
	}
	if fake.submitCount != 0 {
		t.Fatalf("submit count = %d, want 0", fake.submitCount)
	}
}
