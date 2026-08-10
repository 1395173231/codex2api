package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// 上游容量降载的真实序列是「event: error → event: response.failed」。可重试类
// error 帧不能算客户端输出：一旦写出置位 wroteAnyBody，随后的 response.failed
// 就进不了首包前静默换号分支。不可重试类维持原样立即转发。
func TestIsRetryableUpstreamErrorFrame(t *testing.T) {
	cases := []struct {
		name      string
		eventType string
		payload   string
		want      bool
	}{
		{"降载 server_is_overloaded", "error", `{"type":"error","error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."}}`, true},
		{"降载 slow_down", "error", `{"type":"error","error":{"code":"slow_down","message":"slow down"}}`, true},
		{"限流 rate_limit_exceeded", "error", `{"type":"error","error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"limited"}}`, true},
		{"不可重试 content_policy 原样转发", "error", `{"type":"error","error":{"type":"invalid_request_error","code":"content_policy_violation","message":"blocked"}}`, false},
		{"不可重试 invalid_request 原样转发", "error", `{"type":"error","error":{"type":"invalid_request_error","code":"invalid_request","message":"bad"}}`, false},
		{"response.failed 不归本判定管", "response.failed", `{"type":"response.failed","response":{"error":{"code":"server_is_overloaded"}}}`, false},
		{"内容事件不归本判定管", "response.output_text.delta", `{"type":"response.output_text.delta","delta":"hi"}`, false},
		{"空 payload", "error", ``, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isRetryableUpstreamErrorFrame(tc.eventType, []byte(tc.payload))
			if got != tc.want {
				t.Errorf("isRetryableUpstreamErrorFrame(%q, %s) = %v, want %v", tc.eventType, tc.payload, got, tc.want)
			}
		})
	}
}

// 只有降载码（server_is_overloaded/slow_down）被改写为客户端可重试的 server_error，
// 其余错误码（尤其 rate_limit_exceeded，客户端依赖其原码解析重试延时）必须原样保留；
// 错误消息一律不动。
func TestSanitizeCapacityShedEventForClient(t *testing.T) {
	cases := []struct {
		name        string
		eventType   string
		payload     string
		wantContain string
		wantAbsent  string
	}{
		{
			name:        "failed 事件嵌套 code 改写",
			eventType:   "response.failed",
			payload:     `{"type":"response.failed","response":{"status":"failed","error":{"code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."}}}`,
			wantContain: `"code":"server_error"`,
			wantAbsent:  "server_is_overloaded",
		},
		{
			name:        "error 帧 code 改写且消息保留",
			eventType:   "error",
			payload:     `{"type":"error","error":{"code":"slow_down","message":"slow down"}}`,
			wantContain: `"message":"slow down"`,
			wantAbsent:  `"code":"slow_down"`,
		},
		{
			name:        "rate_limit 不改写",
			eventType:   "response.failed",
			payload:     `{"type":"response.failed","response":{"error":{"code":"rate_limit_exceeded","message":"try again in 3s"}}}`,
			wantContain: `"code":"rate_limit_exceeded"`,
		},
		{
			name:        "普通 server_error 不改写",
			eventType:   "error",
			payload:     `{"type":"error","error":{"code":"server_error","message":"boom"}}`,
			wantContain: `"code":"server_error"`,
		},
		{
			name:        "非错误事件不碰",
			eventType:   "response.completed",
			payload:     `{"type":"response.completed","response":{"id":"resp_1"}}`,
			wantContain: `"id":"resp_1"`,
		},
		{
			name:        "非 JSON 不碰",
			eventType:   "error",
			payload:     `not-json`,
			wantContain: `not-json`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := string(sanitizeCapacityShedEventForClient(tc.eventType, []byte(tc.payload)))
			if !strings.Contains(out, tc.wantContain) {
				t.Errorf("sanitize(%q) = %q, want contains %q", tc.payload, out, tc.wantContain)
			}
			if tc.wantAbsent != "" && strings.Contains(out, tc.wantAbsent) {
				t.Errorf("sanitize(%q) = %q, want NOT contains %q", tc.payload, out, tc.wantAbsent)
			}
		})
	}
}

// capacityShedEpilogue 复刻 Responses 流式路径对 error/response.failed 帧的
// 抑制→拦截→defer/写出时序（与 handler.go 的三条 SSE 转发循环同构），返回是否
// 命中首包前静默换号分支。
func capacityShedEpilogue(t *testing.T, c *gin.Context, events [][]byte, attempt, maxRetries int) bool {
	t.Helper()
	c.Header("Content-Type", "text/event-stream")
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		t.Error("c.Writer 未实现 http.Flusher")
		return false
	}
	streamWriter := newStreamFlushWriter(c.Writer, flusher)
	var pending bytes.Buffer
	ttftRecorded := false
	wroteAnyBody := false
	gotTerminal := false
	abortedForHTTPError := false
	var terminalFailurePayload []byte

	for _, data := range events {
		eventType := gjson.GetBytes(data, "type").String()
		if !ttftRecorded && isFirstTokenPayload(data) {
			ttftRecorded = true
		}
		if eventType == "response.completed" || eventType == "response.failed" {
			gotTerminal = true
		}
		if eventType == "response.failed" {
			terminalFailurePayload = append([]byte(nil), data...)
		}
		if shouldSuppressRetryableResponseFailedBeforeFirstToken(eventType, terminalFailurePayload, ttftRecorded, wroteAnyBody, attempt, maxRetries, nil, nil) {
			pending.Reset()
			return true
		}
		if shouldReturnHTTPErrorForResponseFailed(eventType, ttftRecorded, wroteAnyBody, false) {
			pending.Reset()
			abortedForHTTPError = true
			break
		}
		shouldDefer := !ttftRecorded && !gotTerminal &&
			(isPreContentLifecycleEvent(eventType) || isRetryableUpstreamErrorFrame(eventType, data))
		wrote, err := writeDeferredSSEData(streamWriter, &pending, sanitizeCapacityShedEventForClient(eventType, data), shouldDefer)
		if err != nil {
			t.Errorf("writeDeferredSSEData: %v", err)
			return false
		}
		if wrote {
			wroteAnyBody = true
		}
	}
	if wroteAnyBody {
		_ = streamWriter.Flush()
	}
	if abortedForHTTPError && !wroteAnyBody {
		outcome := classifyResponseFailedOutcome(terminalFailurePayload)
		c.Header("Content-Type", "application/json; charset=utf-8")
		c.JSON(outcome.logStatusCode, gin.H{
			"error": gin.H{"message": outcome.failureMessage, "type": "upstream_error"},
		})
	}
	return false
}

// 回归用例（真实上游降载序列）：created → in_progress → error 帧 → response.failed。
func TestStreamCapacityShedWireBehavior(t *testing.T) {
	gin.SetMode(gin.TestMode)

	shedError := []byte(`{"type":"error","error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."},"sequence_number":2}`)
	shedFailed := []byte(`{"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."}},"sequence_number":3}`)
	preamble := [][]byte{
		[]byte(`{"type":"response.created","response":{"id":"resp_1"},"sequence_number":0}`),
		[]byte(`{"type":"response.in_progress","response":{"id":"resp_1"},"sequence_number":1}`),
	}

	t.Run("首包前降载且有重试额度 → 静默换号、零字节下发", func(t *testing.T) {
		var suppressed bool
		router := gin.New()
		router.GET("/stream", func(c *gin.Context) {
			suppressed = capacityShedEpilogue(t, c, append(append([][]byte{}, preamble...), shedError, shedFailed), 0, 3)
		})
		srv := httptest.NewServer(router)
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/stream")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)

		if !suppressed {
			t.Error("期望命中首包前静默换号分支")
		}
		if len(body) != 0 {
			t.Errorf("期望零字节下发, got %q", body)
		}
	})

	t.Run("首包前降载且重试耗尽 → 真实 5xx JSON 而非 200 流", func(t *testing.T) {
		router := gin.New()
		router.GET("/stream", func(c *gin.Context) {
			capacityShedEpilogue(t, c, append(append([][]byte{}, preamble...), shedError, shedFailed), 3, 3)
		})
		srv := httptest.NewServer(router)
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/stream")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)

		if resp.StatusCode != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500 (body=%q)", resp.StatusCode, body)
		}
		if !strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
			t.Errorf("Content-Type = %q, want application/json", resp.Header.Get("Content-Type"))
		}
	})

	t.Run("流中途降载 → 透传但降载码改写为可重试 server_error", func(t *testing.T) {
		events := append(append([][]byte{}, preamble...),
			[]byte(`{"type":"response.output_text.delta","delta":"partial"}`),
			shedError, shedFailed)
		router := gin.New()
		router.GET("/stream", func(c *gin.Context) {
			capacityShedEpilogue(t, c, events, 0, 3)
		})
		srv := httptest.NewServer(router)
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/stream")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		body := string(raw)

		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200", resp.StatusCode)
		}
		if !strings.Contains(body, "partial") {
			t.Errorf("body 应包含已下发的正文, got %q", body)
		}
		if !strings.Contains(body, "response.failed") {
			t.Errorf("body 应包含终止事件, got %q", body)
		}
		if !strings.Contains(body, `"code":"server_error"`) {
			t.Errorf("降载码应改写为 server_error, got %q", body)
		}
		if strings.Contains(body, "server_is_overloaded") {
			t.Errorf("不应残留 server_is_overloaded, got %q", body)
		}
		if !strings.Contains(body, "Our servers are currently overloaded") {
			t.Errorf("错误消息应原样保留, got %q", body)
		}
	})
}
