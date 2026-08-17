package proxy

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/codex2api/auth"
)

// Codex 设备指纹收敛：把出站请求里携带的客户端标识改写成账号级恒定值，
// 使上游看到的「设备数 / 会话数」收敛，而不是随共享该账号的下游用户数增长。
// 档位语义见 auth/codex_fingerprint_mode.go。
//
// 收敛面严格限定在两个载体：
//   - X-Codex-Turn-Metadata 请求头里的 JSON（本项目按白名单原样透传，见
//     codexAllowedForwardHeaders，客户端真实标识由此泄漏）
//   - 请求体 client_metadata（Codex 官方路径同样原样透传）
//
// 明确不改写的部分：
//   - 出站 Session_id 头：由 resolveUpstreamSessionID 独立决定（默认隔离模式下每请求
//     随机），收敛不介入，否则会改变 prompt cache 隔离语义。注意它与 metadata 里的
//     session_id 是两套独立身份，开启收敛后两者取值不同属预期。
//   - turn_id / turn_started_at_unix_ms：客户端逐轮随机值，不标识设备或会话；
//     重算反而会让头与体、turn_id 与其时间戳彼此不一致，本身就是特征。
//   - 客户端未携带的字段：只改写已存在的键，绝不新增，避免改变 metadata 的形状。
//     该规则同样适用于请求头——出站只覆盖下游确实发过的标识头，不凭空补发。

const (
	codexTurnMetadataHeader    = "X-Codex-Turn-Metadata"
	codexInstallationIDHeader  = "X-Codex-Installation-Id"
	codexWindowIDHeader        = "X-Codex-Window-Id"
	codexClientRequestIDHeader = "X-Client-Request-Id"
	codexThreadIDHeader        = "Thread-Id"
)

// codexFingerprintIDs 是一次请求的收敛目标值。全部字段都由「账号 + 下游请求头」
// 确定性推导，因此请求头改写点和请求体改写点各自推导也必然得到同一组值，
// 不需要跨函数传递即可保证一致。
type codexFingerprintIDs struct {
	mode           string
	installationID string
	sessionID      string
	threadID       string
	windowID       string
}

// deriveStableCodexUUID 从种子确定性派生一个 UUIDv4 格式的字符串：同一种子恒定返回
// 同一值，跨进程重启也不变，因此无需落库。
//
// 这里没有复用 uuid.NewSHA1（本仓库其它确定性 ID 的做法），因为那会产出 v5 UUID，
// 版本位与真实客户端的 v4 标识不同，本身可被识别。
func deriveStableCodexUUID(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	var u uuid.UUID
	copy(u[:], sum[:16])
	u[6] = (u[6] & 0x0f) | 0x40 // version 4
	u[8] = (u[8] & 0x3f) | 0x80 // RFC 4122 variant
	return u.String()
}

// resolveCodexFingerprintIDs 按账号配置推导收敛目标值，off 档或账号缺失时返回 nil。
//
// downstreamHeaders 是下游客户端的原始请求头，用于取客户端真实会话标识来派生
// thread_id —— 这样每个真实 Codex 会话在上游对应一个独立线程，而不是全部压成一条。
func resolveCodexFingerprintIDs(account *auth.Account, downstreamHeaders http.Header) *codexFingerprintIDs {
	if account == nil {
		return nil
	}
	mode := account.EffectiveCodexFingerprintMode()
	if mode == auth.CodexFingerprintModeOff {
		return nil
	}
	accountID := account.ID()
	if accountID <= 0 {
		return nil
	}

	ids := &codexFingerprintIDs{
		mode:           mode,
		installationID: resolveConvergedInstallationID(account, accountID),
	}
	if ids.installationID == "" {
		return nil
	}
	if mode == auth.CodexFingerprintModeDevice {
		return ids
	}

	ids.sessionID = deriveStableCodexUUID(fmt.Sprintf("codex2api:codex-session-id:v1:%d", accountID))
	ids.threadID = ids.sessionID
	if mode == auth.CodexFingerprintModeSession {
		if clientSessionID := extractClientCodexSessionID(downstreamHeaders); clientSessionID != "" {
			ids.threadID = deriveStableCodexUUID(fmt.Sprintf("codex2api:codex-thread-id:v1:%d:%s", accountID, clientSessionID))
		}
	}
	ids.windowID = ids.threadID + ":0"
	return ids
}

// resolveConvergedInstallationID 返回账号级恒定的 installation_id。
// 账号自定义头里显式配置的 installation id 优先——运维借此把账号钉到一个已知的
// 真实设备标识上，且该值本来就会覆盖出站头（自定义头最后应用），这里一并采用可
// 保证请求头与 metadata 中的取值一致。
func resolveConvergedInstallationID(account *auth.Account, accountID int64) string {
	for name, value := range account.GetCustomHeaders() {
		if strings.EqualFold(strings.TrimSpace(name), codexInstallationIDHeader) {
			if pinned := strings.TrimSpace(value); pinned != "" {
				return pinned
			}
		}
	}
	return deriveStableCodexUUID(fmt.Sprintf("codex2api:codex-install-id:v1:%d", accountID))
}

// extractClientCodexSessionID 取下游客户端的原始会话标识。
// 只读请求头：请求头改写点拿不到请求体，限定在同一输入才能保证两处推导一致。
func extractClientCodexSessionID(headers http.Header) string {
	if headers == nil {
		return ""
	}
	// Codex CLI 走 HTTP/2 发的是连字符形式 session-id；下划线形式是旧写法，两者都认。
	for _, name := range []string{"Session-Id", "Session_id"} {
		if value := strings.TrimSpace(headers.Get(name)); value != "" {
			return value
		}
	}
	// 客户端未发会话头时，退回 turn metadata 里自报的会话标识。
	raw := strings.TrimSpace(headers.Get(codexTurnMetadataHeader))
	if raw == "" || !gjson.Valid(raw) {
		return ""
	}
	for _, path := range []string{"session_id", "thread_id"} {
		if value := strings.TrimSpace(gjson.Get(raw, path).String()); value != "" {
			return value
		}
	}
	return ""
}

// ApplyCodexFingerprintHeaders 按账号配置收敛出站请求头中的设备指纹。
// 必须在白名单透传（applyCodexAllowedForwardHeaders）之后调用，才能覆盖掉客户端原值；
// 也应在账号自定义头之前调用，让运维显式配置的头保持最终优先。
//
// downstreamHeaders 用于判断这次请求是否真的来自 Codex 客户端：非 Codex 下游
// （普通 OpenAI SDK 等）不会补发 x-codex-* 头，否则等于给一个本来没有 Codex 特征的
// 请求伪造出半套客户端特征，比不发更可疑。
func ApplyCodexFingerprintHeaders(outbound http.Header, account *auth.Account, downstreamHeaders http.Header) {
	if outbound == nil {
		return
	}
	ids := resolveCodexFingerprintIDs(account, downstreamHeaders)
	if ids == nil {
		return
	}

	if raw := strings.TrimSpace(outbound.Get(codexTurnMetadataHeader)); raw != "" {
		if rewritten, changed := rewriteCodexTurnMetadataJSON(raw, ids); changed {
			outbound.Set(codexTurnMetadataHeader, rewritten)
		}
	}

	// X-Client-Request-Id 按白名单原样透传（codexAllowedForwardHeaders）。实测真实
	// 客户端把它取成与自身 thread_id 相同的值，因此不处理就等于把下游用户的原始
	// 线程标识直接送到上游，与已收敛的 metadata 各说各话。只在它确实复用了客户端
	// 自报的会话/线程标识时改写；取值本就与身份无关时保持原样。
	if converged := convergedClientRequestID(outbound.Get(codexClientRequestIDHeader), ids, downstreamHeaders); converged != "" {
		outbound.Set(codexClientRequestIDHeader, converged)
	}

	// 仅对确有 Codex 引擎特征的下游请求处理标识头。
	if !EvaluateEngineFingerprint(downstreamHeaders, nil, nil) {
		return
	}
	// 只覆盖下游确实发过的标识头。实测真实 Codex CLI 只发 x-codex-window-id，
	// 从不发 x-codex-installation-id（installation_id 只存在于 turn metadata JSON 里）：
	// 无条件补发等于给请求添一个真实客户端不会有的头，与收敛目标相反。
	overrideExistingHeader(outbound, downstreamHeaders, codexInstallationIDHeader, ids.installationID)
	overrideExistingHeader(outbound, downstreamHeaders, codexWindowIDHeader, ids.windowID)
}

// overrideExistingHeader 仅在下游确实发过该头时，用收敛值覆盖出站取值。
// 判定读下游头而非出站头：这两个标识头都不在 codexAllowedForwardHeaders 里，
// 下游即使发了，到这一步出站请求上也没有。
func overrideExistingHeader(outbound, downstreamHeaders http.Header, name, value string) {
	if value == "" || downstreamHeaders == nil {
		return
	}
	if strings.TrimSpace(downstreamHeaders.Get(name)) == "" {
		return
	}
	outbound.Set(name, value)
}

// convergedClientRequestID 在 X-Client-Request-Id 复用了客户端自报身份时返回对应的
// 收敛值，否则返回空串表示不改写。device 档没有会话级收敛值，恒不改写。
func convergedClientRequestID(current string, ids *codexFingerprintIDs, downstreamHeaders http.Header) string {
	current = strings.TrimSpace(current)
	if current == "" || ids == nil || ids.threadID == "" {
		return ""
	}
	clientSessionID, clientThreadID := extractClientCodexIdentity(downstreamHeaders)
	switch {
	case clientThreadID != "" && current == clientThreadID:
		return ids.threadID
	case clientSessionID != "" && current == clientSessionID:
		return ids.sessionID
	default:
		return ""
	}
}

// extractClientCodexIdentity 取下游自报的原始会话/线程标识，用于判断别的头是否
// 重复携带了同一个值。与 extractClientCodexSessionID 分开实现：后者是收敛值的派生
// 输入，改动它会让既有账号已经稳定的 thread_id 发生漂移。
func extractClientCodexIdentity(headers http.Header) (sessionID, threadID string) {
	if headers == nil {
		return "", ""
	}
	for _, name := range []string{"Session-Id", "Session_id"} {
		if value := strings.TrimSpace(headers.Get(name)); value != "" {
			sessionID = value
			break
		}
	}
	threadID = strings.TrimSpace(headers.Get(codexThreadIDHeader))
	raw := strings.TrimSpace(headers.Get(codexTurnMetadataHeader))
	if raw == "" || !gjson.Valid(raw) {
		return sessionID, threadID
	}
	if sessionID == "" {
		sessionID = strings.TrimSpace(gjson.Get(raw, "session_id").String())
	}
	if threadID == "" {
		threadID = strings.TrimSpace(gjson.Get(raw, "thread_id").String())
	}
	return sessionID, threadID
}

// ApplyCodexFingerprintToBody 收敛请求体 client_metadata 中的设备指纹。
// 请求体没有 client_metadata 时原样返回。
func ApplyCodexFingerprintToBody(body []byte, account *auth.Account, downstreamHeaders http.Header) []byte {
	ids := resolveCodexFingerprintIDs(account, downstreamHeaders)
	if ids == nil {
		return body
	}
	if !gjson.GetBytes(body, "client_metadata").IsObject() {
		return body
	}

	body = setExistingJSONString(body, "client_metadata.x-codex-installation-id", ids.installationID)
	if ids.mode != auth.CodexFingerprintModeDevice {
		body = setExistingJSONString(body, "client_metadata.session_id", ids.sessionID)
		body = setExistingJSONString(body, "client_metadata.thread_id", ids.threadID)
		body = setExistingJSONString(body, "client_metadata.x-codex-window-id", ids.windowID)
	}

	// client_metadata 里可能内嵌一份 turn metadata JSON 字符串，同样要收敛。
	const embeddedPath = "client_metadata.x-codex-turn-metadata"
	if embedded := gjson.GetBytes(body, embeddedPath); embedded.Type == gjson.String {
		if rewritten, changed := rewriteCodexTurnMetadataJSON(embedded.String(), ids); changed {
			if updated, err := sjson.SetBytes(body, embeddedPath, rewritten); err == nil {
				body = updated
			}
		}
	}
	return body
}

// rewriteCodexTurnMetadataJSON 收敛 turn metadata JSON 里的身份字段。
// 用 sjson 原地替换而非 map 反序列化再序列化：后者会按字典序重排键、丢掉原始键序，
// 这个形状差异本身就是可识别特征。未出现的键不会被补上。
func rewriteCodexTurnMetadataJSON(raw string, ids *codexFingerprintIDs) (string, bool) {
	if ids == nil || !gjson.Valid(raw) {
		return raw, false
	}
	updates := [][2]string{{"installation_id", ids.installationID}}
	if ids.mode != auth.CodexFingerprintModeDevice {
		updates = append(updates,
			[2]string{"session_id", ids.sessionID},
			[2]string{"thread_id", ids.threadID},
			[2]string{"window_id", ids.windowID},
		)
	}

	changed := false
	for _, update := range updates {
		path, value := update[0], update[1]
		if value == "" {
			continue
		}
		existing := gjson.Get(raw, path)
		if !existing.Exists() || existing.String() == value {
			continue
		}
		updated, err := sjson.Set(raw, path, value)
		if err != nil {
			continue
		}
		raw = updated
		changed = true
	}
	if scrubbed, ok := scrubCodexWorkspaces(raw); ok {
		raw = scrubbed
		changed = true
	}
	return raw, changed
}

// scrubCodexWorkspaces 抹掉 turn metadata 里的工作区身份。
//
// workspaces 以本地绝对路径为键（含系统用户名），条目里带 git remote URL 和 commit
// hash。多人共享一个账号时，这些字段会把每个下游用户的身份原样送到上游——同一个
// installation_id 下面挂着一堆互不相干的仓库地址和用户名目录，既是隐私泄漏，也让
// 前面几项标识的收敛失去意义。
//
// 处理方式是收敛而非删除：路径键换成按原值派生的占位符（条目数与互异性都保留，
// 不改变 metadata 形状），remote URL 整体移除。其余字段原样保留。
func scrubCodexWorkspaces(raw string) (string, bool) {
	workspaces := gjson.Get(raw, "workspaces")
	if !workspaces.IsObject() {
		return raw, false
	}

	rebuilt := "{}"
	changed := false
	failed := false
	workspaces.ForEach(func(key, value gjson.Result) bool {
		entry := value.Raw
		if gjson.Get(entry, "associated_remote_urls").Exists() {
			if updated, err := sjson.Delete(entry, "associated_remote_urls"); err == nil {
				entry = updated
				changed = true
			}
		}
		originalPath := key.String()
		placeholder := originalPath
		// 已是占位符就保持原样：重试链路可能把改写过的载荷再送进来一次，
		// 二次哈希会让同一工作区在不同尝试间漂移。
		if !isPlaceholderWorkspacePath(originalPath) {
			placeholder = placeholderWorkspacePath(originalPath)
			changed = true
		}
		updated, err := sjson.SetRaw(rebuilt, placeholder, entry)
		if err != nil {
			failed = true
			return false
		}
		rebuilt = updated
		return true
	})
	if failed || !changed {
		return raw, false
	}

	updated, err := sjson.SetRaw(raw, "workspaces", rebuilt)
	if err != nil {
		return raw, false
	}
	return updated, true
}

// placeholderWorkspacePath 把本地路径映射成稳定占位符：同一路径恒定得到同一占位符，
// 不同路径互不相同，且不携带用户名或项目名。返回值不含 sjson 的路径元字符
// （. * ?），可直接作为 sjson 路径使用。
func placeholderWorkspacePath(original string) string {
	sum := sha256.Sum256([]byte("codex2api:workspace-path:v1:" + original))
	return workspacePlaceholderPrefix + fmt.Sprintf("%x", sum[:4])
}

const workspacePlaceholderPrefix = "/workspace/"

// isPlaceholderWorkspacePath 判断路径是否已经是本函数产出的占位符，用于让抹除保持幂等。
func isPlaceholderWorkspacePath(path string) bool {
	suffix, ok := strings.CutPrefix(path, workspacePlaceholderPrefix)
	if !ok || len(suffix) != 8 {
		return false
	}
	for _, r := range suffix {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// setExistingJSONString 只在目标路径已存在时替换其值，避免给客户端未携带该字段的
// 请求新增字段。
func setExistingJSONString(body []byte, path, value string) []byte {
	if value == "" {
		return body
	}
	existing := gjson.GetBytes(body, path)
	if !existing.Exists() || existing.String() == value {
		return body
	}
	updated, err := sjson.SetBytes(body, path, value)
	if err != nil {
		return body
	}
	return updated
}
