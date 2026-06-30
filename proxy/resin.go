package proxy

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
)

// ==================== Resin 粘性代理池集成 ====================

// ResinConfig 保存 Resin 代理池连接配置
type ResinConfig struct {
	BaseURL      string // 完整基础地址，例如 http://127.0.0.1:2260/my-token
	PlatformName string // 平台标识，例如 codex2api
}

// 全局 Resin 配置（原子指针，支持热更新）
var resinCfg atomic.Pointer[ResinConfig]

// SetResinConfig 设置全局 Resin 配置；cfg 为 nil 或 BaseURL 为空时禁用 Resin
func SetResinConfig(cfg *ResinConfig) {
	if cfg != nil && strings.TrimSpace(cfg.BaseURL) != "" && strings.TrimSpace(cfg.PlatformName) != "" {
		resinCfg.Store(cfg)
		log.Printf("[Resin] 已启用: platform=%s url=%s", cfg.PlatformName, cfg.BaseURL)
	} else {
		resinCfg.Store(nil)
	}
}

// GetResinConfig 获取当前 Resin 配置，未配置时返回 nil
func GetResinConfig() *ResinConfig {
	return resinCfg.Load()
}

// IsResinEnabled 检查 Resin 代理池是否已启用
func IsResinEnabled() bool {
	return GetResinConfig() != nil
}

// ==================== 正向代理 URL 构建 ====================

// BuildForwardProxyURL 将当前 Resin 配置转换为 HTTP 正向代理 URL。
//
// Resin 正向代理认证格式为:
//
//	username = <Platform>.<Account>
//	password = <resin_url path 中的 token>
//
// 例如:
//
//	resin_url=http://127.0.0.1:2260/my-token, platform=codex2api, account=123
//	-> http://codex2api.123:my-token@127.0.0.1:2260
func BuildForwardProxyURL(accountID string) string {
	return BuildForwardProxyURLFromConfig(GetResinConfig(), accountID)
}

// BuildForwardProxyURLFromConfig 是 BuildForwardProxyURL 的可测试纯函数变体。
func BuildForwardProxyURLFromConfig(cfg *ResinConfig, accountID string) string {
	if cfg == nil {
		return ""
	}
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return ""
	}
	parsed, err := url.Parse(strings.TrimSpace(cfg.BaseURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	token := strings.Trim(strings.TrimSpace(parsed.EscapedPath()), "/")
	if token == "" {
		return ""
	}
	platform := strings.TrimSpace(cfg.PlatformName)
	if platform == "" {
		return ""
	}

	proxyURL := &url.URL{
		Scheme: parsed.Scheme,
		Host:   parsed.Host,
		User:   url.UserPassword(platform+"."+accountID, token),
	}
	return proxyURL.String()
}

// EffectiveProxyURLForAccount 返回账号请求的有效代理。
// Resin 启用且账号标识非空时，Resin 正向代理优先于传统 per-account/global proxy。
func EffectiveProxyURLForAccount(account *auth.Account, fallbackProxyURL string) string {
	if IsResinEnabled() && account != nil {
		if resinProxyURL := BuildForwardProxyURL(ResinAccountID(account)); resinProxyURL != "" {
			return resinProxyURL
		}
	}
	return strings.TrimSpace(fallbackProxyURL)
}

// EffectiveProxyURLForIdentity 返回临时身份或非 Account 对象请求的有效代理。
func EffectiveProxyURLForIdentity(accountID, fallbackProxyURL string) string {
	if IsResinEnabled() {
		if resinProxyURL := BuildForwardProxyURL(accountID); resinProxyURL != "" {
			return resinProxyURL
		}
	}
	return strings.TrimSpace(fallbackProxyURL)
}

// ==================== 反向代理 URL 构建 ====================

// BuildReverseProxyURL 将目标 URL 转换为 Resin 反向代理 URL
// 例如: https://chatgpt.com/backend-api/codex/responses
//
//	→ http://127.0.0.1:2260/my-token/codex2api/https/chatgpt.com/backend-api/codex/responses
func BuildReverseProxyURL(targetURL string) string {
	cfg := GetResinConfig()
	if cfg == nil {
		return targetURL
	}
	parsed, err := url.Parse(targetURL)
	if err != nil {
		return targetURL
	}
	// <resin_base>/<platform>/<protocol>/<host><path+query>
	base := strings.TrimRight(cfg.BaseURL, "/")
	return fmt.Sprintf("%s/%s/%s/%s%s",
		base,
		cfg.PlatformName,
		parsed.Scheme,
		parsed.Host,
		parsed.RequestURI(),
	)
}

// BuildWebSocketURL 将目标 WSS URL 转换为 Resin WS 反向代理 URL
// 例如: wss://chatgpt.com/backend-api/codex/responses
//
//	→ ws://127.0.0.1:2260/my-token/codex2api/https/chatgpt.com/backend-api/codex/responses
//
// Resin 约定: 客户端到 Resin 只支持 ws://；路径中 protocol 填 http/https 对应目标 ws/wss
func BuildWebSocketURL(targetURL string) string {
	cfg := GetResinConfig()
	if cfg == nil {
		return targetURL
	}
	parsed, err := url.Parse(targetURL)
	if err != nil {
		return targetURL
	}
	resinParsed, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return targetURL
	}

	// wss → https, ws → http（Resin 路径中的 protocol 字段）
	protocol := "https"
	if parsed.Scheme == "ws" || parsed.Scheme == "http" {
		protocol = "http"
	}

	return fmt.Sprintf("ws://%s%s/%s/%s/%s%s",
		resinParsed.Host,
		resinParsed.Path,
		cfg.PlatformName,
		protocol,
		parsed.Host,
		parsed.RequestURI(),
	)
}

// ==================== 账号标识 ====================

// ResinAccountID 返回账号在 Resin 中的稳定标识（DBID 转字符串）
func ResinAccountID(account *auth.Account) string {
	return fmt.Sprintf("%d", account.DBID)
}

// ==================== 租约继承 ====================

// InheritLease 将临时身份的 IP 租约继承给正式账号身份
// 用于 OAuth 场景：授权阶段使用临时标识，账号创建后切换为 DBID
func InheritLease(tempAccount, newAccount string) {
	cfg := GetResinConfig()
	if cfg == nil {
		return
	}

	inheritURL := fmt.Sprintf("%s/api/v1/%s/actions/inherit-lease",
		strings.TrimRight(cfg.BaseURL, "/"),
		cfg.PlatformName,
	)

	body := fmt.Sprintf(`{"parent_account":%q,"new_account":%q}`, tempAccount, newAccount)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, inheritURL, bytes.NewBufferString(body))
	if err != nil {
		log.Printf("[Resin] 构建 inherit-lease 请求失败: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("[Resin] inherit-lease 请求失败: %v", err)
		return
	}
	resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("[Resin] inherit-lease 返回非成功状态: %d (temp=%s new=%s)", resp.StatusCode, tempAccount, newAccount)
	} else {
		log.Printf("[Resin] inherit-lease 成功: %s → %s", tempAccount, newAccount)
	}
}

// ==================== Resin 连接池 ====================

// getResinHTTPClient 返回走 Resin 反代的标准 HTTP 客户端（无 uTLS）
// 池键按 accountID 隔离，复用底层 TCP 连接
func getResinHTTPClient(account *auth.Account) *http.Client {
	key := fmt.Sprintf("resin|%d", account.ID())

	if v, ok := clientPool.Load(key); ok {
		entry := v.(*poolEntry)
		entry.touch()
		return entry.client
	}

	transport := &http.Transport{
		MaxIdleConns:        10,
		MaxIdleConnsPerHost: 5,
		MaxConnsPerHost:     10,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}

	entry := &poolEntry{
		client: &http.Client{
			Transport: transport,
			Timeout:   0, // 流式响应不设超时
		},
	}
	entry.touch()

	if v, loaded := clientPool.LoadOrStore(key, entry); loaded {
		e := v.(*poolEntry)
		e.touch()
		return e.client
	}
	return entry.client
}
