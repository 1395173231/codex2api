package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

const continuousRetryKeepaliveComment = ": keepalive\n\n"

var continuousRetryKeepaliveInterval = 15 * time.Second

type continuousRetryKeepalive interface {
	Activate()
	Active() bool
	Keepalive() error
}

type continuousRetryKeepaliveContextKey struct{}

type requestContinuousRetryKeepalive struct {
	active bool
	last   time.Time
	write  func() error
	cancel context.CancelCauseFunc
}

func (k *requestContinuousRetryKeepalive) Activate() {
	if k != nil && !k.active {
		k.active = true
		// Start the heartbeat window when unlimited retry actually begins.
		// Short backoff calls then accumulate toward the same deadline instead
		// of restarting a fresh interval on every retry.
		k.last = time.Now()
	}
}

func (k *requestContinuousRetryKeepalive) Active() bool {
	return k != nil && k.active
}

func (k *requestContinuousRetryKeepalive) Keepalive() error {
	if k == nil || !k.active || k.write == nil {
		return nil
	}
	if !k.last.IsZero() && time.Since(k.last) < continuousRetryKeepaliveInterval {
		return nil
	}
	if err := k.write(); err != nil {
		if k.cancel != nil {
			k.cancel(err)
		}
		return err
	}
	k.last = time.Now()
	return nil
}

func installContinuousRetrySSEKeepalive(c *gin.Context, stream bool, contentType string) func() {
	if c == nil || c.Request == nil || c.Writer == nil || !stream {
		return func() {}
	}
	if _, ok := c.Writer.(http.Flusher); !ok {
		return func() {}
	}
	if contentType == "" {
		contentType = "text/event-stream"
	}
	original := c.Request
	requestCtx, cancel := context.WithCancelCause(original.Context())
	keepalive := &requestContinuousRetryKeepalive{write: func() error {
		setSSEStreamHeaders(c, contentType)
		if _, err := c.Writer.WriteString(continuousRetryKeepaliveComment); err != nil {
			return err
		}
		if flusher, ok := c.Writer.(http.Flusher); ok {
			flusher.Flush()
		}
		return nil
	}, cancel: cancel}
	c.Request = original.WithContext(context.WithValue(requestCtx, continuousRetryKeepaliveContextKey{}, continuousRetryKeepalive(keepalive)))
	return func() {
		cancel(nil)
		c.Request = original
	}
}

func installContinuousRetryWSKeepalive(c *gin.Context, conn *websocket.Conn) func() {
	if c == nil || c.Request == nil || conn == nil {
		return func() {}
	}
	original := c.Request
	requestCtx, cancel := context.WithCancelCause(original.Context())
	keepalive := &requestContinuousRetryKeepalive{write: func() error {
		return conn.WriteControl(websocket.PingMessage, []byte("continuous-retry"), time.Now().Add(responsesWSWriteTimeout))
	}, cancel: cancel}
	c.Request = original.WithContext(context.WithValue(requestCtx, continuousRetryKeepaliveContextKey{}, continuousRetryKeepalive(keepalive)))
	return func() {
		cancel(nil)
		c.Request = original
	}
}

func setSSEStreamHeaders(c *gin.Context, contentType string) {
	if c == nil {
		return
	}
	if contentType == "" {
		contentType = "text/event-stream"
	}
	c.Header("Content-Type", contentType)
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
}

func continuousRetryKeepaliveForContext(ctx context.Context) continuousRetryKeepalive {
	if ctx == nil {
		return nil
	}
	keepalive, _ := ctx.Value(continuousRetryKeepaliveContextKey{}).(continuousRetryKeepalive)
	return keepalive
}

func activateContinuousRetryKeepalive(ctx context.Context) {
	if keepalive := continuousRetryKeepaliveForContext(ctx); keepalive != nil {
		keepalive.Activate()
	}
}

func activateContinuousRetryKeepaliveForLimit(ctx context.Context, retryLimit int) {
	if retryLimit == -1 {
		activateContinuousRetryKeepalive(ctx)
	}
}

func continuousRetryKeepaliveActive(ctx context.Context) bool {
	if keepalive := continuousRetryKeepaliveForContext(ctx); keepalive != nil {
		return keepalive.Active()
	}
	return false
}

func continuousRetryKeepaliveDelay(keepalive continuousRetryKeepalive) time.Duration {
	if continuousRetryKeepaliveInterval <= 0 {
		return 0
	}
	requestKeepalive, ok := keepalive.(*requestContinuousRetryKeepalive)
	if !ok || requestKeepalive.last.IsZero() {
		return continuousRetryKeepaliveInterval
	}
	delay := continuousRetryKeepaliveInterval - time.Since(requestKeepalive.last)
	if delay < 0 {
		return 0
	}
	return delay
}

func continuousRetryContextError(ctx context.Context) error {
	if ctx == nil {
		return context.Canceled
	}
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	return ctx.Err()
}

func stopContinuousRetryTimer(timer *time.Timer) {
	if timer == nil || timer.Stop() {
		return
	}
	select {
	case <-timer.C:
	default:
	}
}

type continuousRetryHTTPResult struct {
	response *http.Response
	err      error
}

// executeHTTPWithContinuousRetryKeepalive keeps the downstream alive while a
// retry attempt is waiting for upstream response headers. The worker never
// touches the downstream writer; that remains owned by the handler goroutine.
func executeHTTPWithContinuousRetryKeepalive(ctx context.Context, execute func() (*http.Response, error)) (*http.Response, error) {
	keepalive := continuousRetryKeepaliveForContext(ctx)
	if execute == nil {
		return nil, errors.New("nil upstream executor")
	}
	if keepalive == nil || !keepalive.Active() || continuousRetryKeepaliveInterval <= 0 {
		return execute()
	}

	result := make(chan continuousRetryHTTPResult)
	abandoned := make(chan struct{})
	go func() {
		response, err := execute()
		select {
		case result <- continuousRetryHTTPResult{response: response, err: err}:
		case <-abandoned:
			if response != nil && response.Body != nil {
				_ = response.Body.Close()
			}
		}
	}()
	defer close(abandoned)

	for {
		timer := time.NewTimer(continuousRetryKeepaliveDelay(keepalive))
		select {
		case callResult := <-result:
			stopContinuousRetryTimer(timer)
			return callResult.response, callResult.err
		case <-timer.C:
			if err := keepalive.Keepalive(); err != nil {
				return nil, err
			}
		case <-ctx.Done():
			stopContinuousRetryTimer(timer)
			return nil, continuousRetryContextError(ctx)
		}
	}
}

type continuousRetryStreamItem[T any] struct {
	value T
	ack   chan bool
}

// readStreamWithContinuousRetryKeepalive pumps upstream reads through the
// handler goroutine. That goroutine remains the sole downstream writer and can
// therefore emit heartbeats without racing real SSE or WebSocket output.
func readStreamWithContinuousRetryKeepalive[T any](ctx context.Context, read func(func(T) bool) error, callback func(T) bool) error {
	keepalive := continuousRetryKeepaliveForContext(ctx)
	if keepalive == nil || !keepalive.Active() || continuousRetryKeepaliveInterval <= 0 {
		return read(callback)
	}

	items := make(chan continuousRetryStreamItem[T])
	done := make(chan error, 1)
	stop := make(chan struct{})
	go func() {
		done <- read(func(value T) bool {
			ack := make(chan bool, 1)
			select {
			case items <- continuousRetryStreamItem[T]{value: value, ack: ack}:
			case <-stop:
				return false
			case <-ctx.Done():
				return false
			}
			select {
			case keepReading := <-ack:
				return keepReading
			case <-stop:
				return false
			case <-ctx.Done():
				return false
			}
		})
	}()
	defer close(stop)

	for {
		timer := time.NewTimer(continuousRetryKeepaliveDelay(keepalive))
		select {
		case item := <-items:
			stopContinuousRetryTimer(timer)
			keepReading := callback(item.value)
			item.ack <- keepReading
			if !keepReading {
				return <-done
			}
		case err := <-done:
			stopContinuousRetryTimer(timer)
			return err
		case <-timer.C:
			if err := keepalive.Keepalive(); err != nil {
				return err
			}
		case <-ctx.Done():
			stopContinuousRetryTimer(timer)
			return continuousRetryContextError(ctx)
		}
	}
}

type continuousRetrySSEEvent struct {
	event string
	data  []byte
}

func readSSEStreamWithContinuousRetryKeepalive(ctx context.Context, body io.Reader, callback func(event string, data []byte) bool) error {
	return readStreamWithContinuousRetryKeepalive(ctx, func(yield func(continuousRetrySSEEvent) bool) error {
		return ReadSSEStreamWithEvent(body, func(event string, data []byte) bool {
			return yield(continuousRetrySSEEvent{event: event, data: data})
		})
	}, func(item continuousRetrySSEEvent) bool {
		return callback(item.event, item.data)
	})
}

func readRawGrokSSEFramesWithContinuousRetryKeepalive(ctx context.Context, body io.Reader, callback func(rawGrokSSEFrame) bool) error {
	return readStreamWithContinuousRetryKeepalive(ctx, func(yield func(rawGrokSSEFrame) bool) error {
		return readRawGrokSSEFrames(body, yield)
	}, callback)
}

// waitWithContinuousRetryKeepalive waits in short chunks only after a request
// has entered the unlimited retry path. It keeps the write on the handler
// goroutine, so it never races normal stream output. A failed heartbeat is a
// downstream write failure and stops the retry immediately.
func waitWithContinuousRetryKeepalive(ctx context.Context, interval time.Duration) bool {
	if interval <= 0 {
		return true
	}
	keepalive := continuousRetryKeepaliveForContext(ctx)
	if keepalive == nil || continuousRetryKeepaliveInterval <= 0 {
		return waitForRetryInterval(ctx, interval)
	}
	keepalive.Activate()
	remaining := interval
	for remaining > 0 {
		step := continuousRetryKeepaliveDelay(keepalive)
		if step <= 0 {
			if err := keepalive.Keepalive(); err != nil {
				return false
			}
			// A disabled or custom heartbeat may not advance its deadline.
			// 心跳未推进截止时间时必须实际等待，避免处理协程零延迟忙等。
			step = continuousRetryKeepaliveDelay(keepalive)
			if step <= 0 {
				step = continuousRetryKeepaliveInterval
			}
		}
		if step > remaining {
			step = remaining
		}
		if !waitForRetryInterval(ctx, step) {
			return false
		}
		remaining -= step
		if err := keepalive.Keepalive(); err != nil {
			return false
		}
	}
	return true
}

func waitForRetryInterval(ctx context.Context, interval time.Duration) bool {
	if interval <= 0 {
		return true
	}
	if ctx == nil {
		time.Sleep(interval)
		return true
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// retryKeepaliveCommitted reports that a retry heartbeat has already committed
// the streaming HTTP response. Callers must send a protocol event from that
// point on; appending a fresh JSON HTTP response would corrupt the stream.
func retryKeepaliveCommitted(c *gin.Context) bool {
	return c != nil && c.Writer != nil && c.Writer.Written()
}

func writeCommittedResponsesRetryError(c *gin.Context, message string) bool {
	if !retryKeepaliveCommitted(c) {
		return false
	}
	if c.Request != nil && c.Request.Context().Err() != nil {
		return true
	}
	payload, _ := json.Marshal(gin.H{
		"type": "response.failed",
		"response": gin.H{
			"status": "failed",
			"error":  gin.H{"message": message, "type": "upstream_error", "code": "upstream_error"},
		},
	})
	_, _ = c.Writer.WriteString("data: " + string(payload) + "\n\n")
	if flusher, ok := c.Writer.(http.Flusher); ok {
		flusher.Flush()
	}
	return true
}

func writeCommittedChatRetryError(c *gin.Context, message string) bool {
	if !retryKeepaliveCommitted(c) {
		return false
	}
	if c.Request != nil && c.Request.Context().Err() != nil {
		return true
	}
	payload, _ := json.Marshal(gin.H{
		"error": gin.H{"message": message, "type": ErrorTypeUpstreamError, "code": ErrorCodeUpstreamStreamBreak},
	})
	_, _ = c.Writer.WriteString("data: " + string(payload) + "\n\n")
	if flusher, ok := c.Writer.(http.Flusher); ok {
		flusher.Flush()
	}
	return true
}

func writeCommittedAnthropicRetryError(c *gin.Context, errorType, message string) bool {
	if !retryKeepaliveCommitted(c) {
		return false
	}
	if c.Request != nil && c.Request.Context().Err() != nil {
		return true
	}
	payload, _ := json.Marshal(gin.H{
		"type":  "error",
		"error": gin.H{"type": errorType, "message": message},
	})
	_, _ = c.Writer.WriteString("event: error\ndata: " + string(payload) + "\n\n")
	if flusher, ok := c.Writer.(http.Flusher); ok {
		flusher.Flush()
	}
	return true
}

// abortContinuousRetryCommitFailure closes a winning attempt that could not
// be replayed to the downstream. Replay/storage/write failures are local
// proxy failures, not upstream failures: retrying would only duplicate a paid
// request and could turn a broken client or filesystem into an infinite loop.
func abortContinuousRetryCommitFailure(h *Handler, account *auth.Account, resp *http.Response, attempt *continuousRetryStreamAttempt) {
	if attempt != nil {
		_ = attempt.Close()
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if h != nil && h.store != nil && account != nil {
		h.store.Release(account)
	}
	log.Printf("continuous retry replay commit failed; request aborted")
}

func continuousRetryRequestErrorMessage(err error) string {
	var structured *Error
	if errors.As(err, &structured) && structured != nil && structured.Message != "" {
		return structured.Message
	}
	return "Upstream request failed"
}
