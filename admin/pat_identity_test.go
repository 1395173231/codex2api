package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestQueryPersonalAccessTokenMetadata_Success(t *testing.T) {
	body := `{
		"email": "user@workplace.com",
		"chatgpt_user_id": "user-123456",
		"chatgpt_account_id": "acc-workspace-uuid",
		"chatgpt_plan_type": "business",
		"chatgpt_account_is_fedramp": false
	}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer at-test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	oldURL := patWhoAmIURLForTest
	patWhoAmIURLForTest = server.URL
	defer func() { patWhoAmIURLForTest = oldURL }()

	meta, err := QueryPersonalAccessTokenMetadata(context.Background(), "at-test-token", "")
	if err != nil {
		t.Fatalf("QueryPersonalAccessTokenMetadata error: %v", err)
	}
	if meta == nil {
		t.Fatal("meta is nil")
	}
	if meta.ChatGPTAccountID != "acc-workspace-uuid" {
		t.Errorf("ChatGPTAccountID = %q, want acc-workspace-uuid", meta.ChatGPTAccountID)
	}
	if meta.ChatGPTUserID != "user-123456" {
		t.Errorf("ChatGPTUserID = %q, want user-123456", meta.ChatGPTUserID)
	}
	if meta.ChatGPTPlanType != "business" {
		t.Errorf("ChatGPTPlanType = %q, want business", meta.ChatGPTPlanType)
	}
	if meta.Email == nil || *meta.Email != "user@workplace.com" {
		t.Errorf("Email = %v, want user@workplace.com", meta.Email)
	}
}

func TestQueryPersonalAccessTokenMetadata_RejectsRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://malicious.external.com/leak", http.StatusFound)
	}))
	defer server.Close()

	oldURL := patWhoAmIURLForTest
	patWhoAmIURLForTest = server.URL
	defer func() { patWhoAmIURLForTest = oldURL }()

	_, err := QueryPersonalAccessTokenMetadata(context.Background(), "at-test-token", "")
	if err == nil {
		t.Fatal("expected error on redirect, got nil")
	}
	if !strings.Contains(err.Error(), "302") && !strings.Contains(err.Error(), "status") {
		t.Logf("got error: %v", err)
	}
}

func TestHydrateSeedWithWhoAmI(t *testing.T) {
	body := `{
		"email": "user@team.org",
		"chatgpt_user_id": "user-team-999",
		"chatgpt_account_id": "acc-team-workspace",
		"chatgpt_plan_type": "team",
		"chatgpt_account_is_fedramp": false
	}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	oldURL := patWhoAmIURLForTest
	patWhoAmIURLForTest = server.URL
	defer func() { patWhoAmIURLForTest = oldURL }()

	seed := normalizeTokenCredentialSeed(tokenCredentialSeed{
		accessToken: "at-my-pat-token",
	})
	if seed.accessTokenType != accessTokenTypeCodexAT {
		t.Fatalf("accessTokenType = %q, want %q", seed.accessTokenType, accessTokenTypeCodexAT)
	}
	if seed.workspaceID != "" {
		t.Fatalf("workspaceID before hydrate = %q, want empty", seed.workspaceID)
	}

	hydrated := hydrateSeedWithWhoAmI(context.Background(), seed, "")
	if hydrated.workspaceID != "acc-team-workspace" {
		t.Errorf("hydrated.workspaceID = %q, want acc-team-workspace", hydrated.workspaceID)
	}
	if hydrated.accountID != "acc-team-workspace" {
		t.Errorf("hydrated.accountID = %q, want acc-team-workspace", hydrated.accountID)
	}
	if hydrated.userID != "user-team-999" {
		t.Errorf("hydrated.userID = %q, want user-team-999", hydrated.userID)
	}
	if hydrated.email != "user@team.org" {
		t.Errorf("hydrated.email = %q, want user@team.org", hydrated.email)
	}
	if hydrated.planType != "team" {
		t.Errorf("hydrated.planType = %q, want team", hydrated.planType)
	}
}
