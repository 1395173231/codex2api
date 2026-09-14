package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/auth"
)

const (
	openAIAuthAccountsBaseURL    = "https://auth.openai.com/api/accounts"
	personalAccessTokenWhoAmIURL = openAIAuthAccountsBaseURL + "/v1/user-auth-credential/whoami"
	patWhoAmITimeout             = 10 * time.Second
)

// patWhoAmIURLForTest 允许测试替换 whoami 端点 URL。生产代码不要赋值。
var patWhoAmIURLForTest = ""

// PersonalAccessTokenMetadata 是 /v1/user-auth-credential/whoami 返回的身份元数据结构。
type PersonalAccessTokenMetadata struct {
	Email                   *string `json:"email"`
	ChatGPTUserID           string  `json:"chatgpt_user_id"`
	ChatGPTAccountID        string  `json:"chatgpt_account_id"`
	ChatGPTPlanType         string  `json:"chatgpt_plan_type"`
	ChatGPTAccountIsFedramp bool    `json:"chatgpt_account_is_fedramp"`
}

func newPATWhoAmIClient(proxyURL string) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	dialer := &net.Dialer{Timeout: 8 * time.Second, KeepAlive: 30 * time.Second}
	transport.DialContext = dialer.DialContext
	if err := auth.ConfigureTransportProxy(transport, proxyURL, dialer); err != nil {
		return nil, fmt.Errorf("invalid proxy URL for whoami: %w", err)
	}
	return &http.Client{
		Transport: transport,
		Timeout:   patWhoAmITimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// 安全原则：绝不将带 Bearer PAT 的请求自动跟随重定向发送到其他 host
			return http.ErrUseLastResponse
		},
	}, nil
}

// QueryPersonalAccessTokenMetadata 调用 OpenAI Auth API 的 /v1/user-auth-credential/whoami
// 获取 PAT 的 workspace ID (ChatGPT-Account-Id)、用户 ID、邮箱和套餐类型。
func QueryPersonalAccessTokenMetadata(ctx context.Context, accessToken string, proxyURL string) (*PersonalAccessTokenMetadata, error) {
	token := strings.TrimSpace(accessToken)
	if token == "" {
		return nil, errors.New("access token is empty")
	}

	endpoint := personalAccessTokenWhoAmIURL
	if patWhoAmIURLForTest != "" {
		endpoint = patWhoAmIURLForTest
	}

	client, err := newPATWhoAmIClient(proxyURL)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("create whoami request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "codex2api")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("execute whoami request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("whoami returned status %d", resp.StatusCode)
	}

	var meta PersonalAccessTokenMetadata
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&meta); err != nil {
		return nil, fmt.Errorf("decode whoami response: %w", err)
	}

	meta.ChatGPTAccountID = strings.TrimSpace(meta.ChatGPTAccountID)
	meta.ChatGPTUserID = strings.TrimSpace(meta.ChatGPTUserID)
	meta.ChatGPTPlanType = strings.TrimSpace(meta.ChatGPTPlanType)

	return &meta, nil
}
