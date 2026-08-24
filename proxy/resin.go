package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/security"
)

// ==================== Resin 粘性代理池集成 ====================

// ResinConfig 保存 Resin 代理池连接配置
type ResinConfig struct {
	BaseURL      string // 完整基础地址，例如 http://127.0.0.1:2260/my-token
	PlatformName string // 平台标识，例如 codex2api
}

// 全局 Resin 配置（原子指针，支持热更新）
var resinCfg atomic.Pointer[ResinConfig]

// SetResinConfig 设置全局 Resin 配置；cfg 为 nil 或两项均为空时禁用 Resin。
// 调用方应先通过 ValidateResinConfig 校验配置，以便向操作者返回可处理的错误。
func SetResinConfig(cfg *ResinConfig) {
	if cfg == nil {
		resinCfg.Store(nil)
		return
	}
	if err := ValidateResinConfig(cfg); err != nil {
		resinCfg.Store(nil)
		log.Printf("[Resin] 配置无效，已禁用: %v", err)
		return
	}
	baseURL := strings.TrimSpace(cfg.BaseURL)
	platformName := strings.TrimSpace(cfg.PlatformName)
	if baseURL == "" && platformName == "" {
		resinCfg.Store(nil)
		return
	}
	resinCfg.Store(&ResinConfig{BaseURL: baseURL, PlatformName: platformName})
	// BaseURL contains the Resin access token in its path. Never write that
	// credential to logs; operators only need the proxy origin to verify the
	// active endpoint.
	log.Printf("[Resin] 已启用: platform=%s proxy=%s", platformName, redactResinURL(baseURL))
}

func redactResinURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "<invalid>"
	}
	return parsed.Scheme + "://" + parsed.Host + "/<redacted>"
}

// GetResinConfig 获取当前 Resin 配置，未配置时返回 nil
func GetResinConfig() *ResinConfig {
	return resinCfg.Load()
}

// IsResinEnabled 检查 Resin 代理池是否已启用
func IsResinEnabled() bool {
	return GetResinConfig() != nil
}

// resinMaintenanceTarget 保留历史维护请求调用约定，但不再改写目标 URL。
// Resin 以 HTTP CONNECT 正向代理承载账号身份，目标主机与 TLS 握手仍由本地
// transport 建立，从而保留 uTLS/浏览器指纹。返回 viaResin=true 仅表示调用方
// 应使用返回的池化客户端；生产请求不需要也不应注入 X-Resin-Account。
func resinMaintenanceTarget(account *auth.Account, targetURL string) (finalURL string, client *http.Client, viaResin bool) {
	if !IsResinEnabled() || account == nil {
		return targetURL, nil, false
	}
	effectiveProxyURL := EffectiveProxyURLForAccount(account, "")
	return targetURL, getCodexMaintenanceClient(account, effectiveProxyURL), true
}

// ValidateResinConfig 校验 Resin forward-proxy 配置。
// Resin 需要一个 HTTP(S) 代理端点、路径中的访问 token 与平台标识；仅其中一项存在
// 代表半配置状态，必须由调用方拒绝，避免误以为启用 Resin 后实际走了直连。
func ValidateResinConfig(cfg *ResinConfig) error {
	_, _, _, err := resinConfigParts(cfg)
	return err
}

func resinConfigParts(cfg *ResinConfig) (*url.URL, string, string, error) {
	if cfg == nil {
		return nil, "", "", nil
	}
	baseURL := strings.TrimSpace(cfg.BaseURL)
	platform := strings.TrimSpace(cfg.PlatformName)
	if baseURL == "" && platform == "" {
		return nil, "", "", nil
	}
	if baseURL == "" || platform == "" {
		return nil, "", "", fmt.Errorf("resin_url 和 resin_platform_name 必须同时填写")
	}
	parsed, err := security.ParseProxyURL(baseURL)
	if err != nil {
		return nil, "", "", fmt.Errorf("resin_url 无效: %w", err)
	}
	scheme := strings.ToLower(strings.TrimSpace(parsed.Scheme))
	if scheme != "http" && scheme != "https" {
		return nil, "", "", fmt.Errorf("resin_url 必须使用 http 或 https 代理协议")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, "", "", fmt.Errorf("resin_url 只能包含代理地址和路径 token")
	}
	token := strings.Trim(strings.TrimSpace(parsed.EscapedPath()), "/")
	if token == "" {
		return nil, "", "", fmt.Errorf("resin_url 必须包含路径 token")
	}
	return parsed, token, platform, nil
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
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return ""
	}
	parsed, token, platform, err := resinConfigParts(cfg)
	if err != nil || parsed == nil {
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

// TemporaryResinIdentity returns a deterministic, non-secret identity for a
// request that is account-bound but happens before a database account ID exists.
// The input is hashed so API keys, refresh tokens, and SSO cookies never appear
// in Resin Proxy-Auth credentials or logs. Callers should include a namespace
// describing the flow to avoid cross-flow collisions.
func TemporaryResinIdentity(namespace, stableValue string) string {
	namespace = strings.TrimSpace(namespace)
	stableValue = strings.TrimSpace(stableValue)
	if namespace == "" {
		namespace = "account"
	}
	if stableValue == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(namespace + "\x00" + stableValue))
	return "temp-" + hex.EncodeToString(sum[:16])
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
	if account == nil || account.DBID <= 0 {
		return ""
	}
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

// getResinHTTPClient 保留给旧维护调用方使用；实际走 Resin 正向 CONNECT
// 代理并复用网关同款维护 transport，而不是重写 URL 或注入身份头。
func getResinHTTPClient(account *auth.Account) *http.Client {
	if account == nil {
		return nil
	}
	return getCodexMaintenanceClient(account, EffectiveProxyURLForAccount(account, ""))
}
