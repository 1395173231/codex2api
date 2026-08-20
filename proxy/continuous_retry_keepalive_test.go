package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

type recordingContinuousRetryKeepalive struct {
	active bool
	writes int
	err    error
}

func (k *recordingContinuousRetryKeepalive) Activate() { k.active = true }
func (k *recordingContinuousRetryKeepalive) Active() bool {
	return k.active
}
func (k *recordingContinuousRetryKeepalive) Keepalive() error {
	k.writes++
	return k.err
}

func contextWithContinuousRetryKeepalive(keepalive continuousRetryKeepalive) context.Context {
	return context.WithValue(context.Background(), continuousRetryKeepaliveContextKey{}, keepalive)
}

func TestRequestContinuousRetryKeepaliveAccumulatesShortWaits(t *testing.T) {
	previousInterval := continuousRetryKeepaliveInterval
	continuousRetryKeepaliveInterval = time.Minute
	t.Cleanup(func() { continuousRetryKeepaliveInterval = previousInterval })

	writes := 0
	keepalive := &requestContinuousRetryKeepalive{write: func() error {
		writes++
		return nil
	}}
	keepalive.Activate()
	if keepalive.last.IsZero() {
		t.Fatal("activation did not start the heartbeat window")
	}
	if err := keepalive.Keepalive(); err != nil {
		t.Fatalf("early heartbeat check: %v", err)
	}
	if writes != 0 {
		t.Fatalf("heartbeat wrote immediately after activation: %d", writes)
	}

	keepalive.last = time.Now().Add(-continuousRetryKeepaliveInterval)
	if err := keepalive.Keepalive(); err != nil {
		t.Fatalf("due heartbeat: %v", err)
	}
	if writes != 1 {
		t.Fatalf("due heartbeat writes = %d, want 1", writes)
	}
}

func TestWaitWithContinuousRetryKeepaliveChecksAfterShortWait(t *testing.T) {
	previousInterval := continuousRetryKeepaliveInterval
	continuousRetryKeepaliveInterval = time.Second
	t.Cleanup(func() { continuousRetryKeepaliveInterval = previousInterval })

	keepalive := &recordingContinuousRetryKeepalive{}
	ctx := contextWithContinuousRetryKeepalive(keepalive)
	if !waitWithContinuousRetryKeepalive(ctx, time.Millisecond) {
		t.Fatal("short unlimited retry wait failed")
	}
	if !keepalive.active || keepalive.writes != 1 {
		t.Fatalf("short wait heartbeat state = active:%v writes:%d, want true/1", keepalive.active, keepalive.writes)
	}

	keepalive.err = errors.New("downstream closed")
	if waitWithContinuousRetryKeepalive(ctx, time.Millisecond) {
		t.Fatal("heartbeat write failure did not stop retry wait")
	}
}

func TestWaitWithContinuousRetryKeepaliveHonorsExistingHeartbeatDeadline(t *testing.T) {
	previousInterval := continuousRetryKeepaliveInterval
	continuousRetryKeepaliveInterval = time.Second
	t.Cleanup(func() { continuousRetryKeepaliveInterval = previousInterval })

	firstWrite := make(chan time.Duration, 1)
	started := time.Now()
	keepalive := &requestContinuousRetryKeepalive{
		active: true,
		last:   time.Now().Add(-990 * time.Millisecond),
		write: func() error {
			select {
			case firstWrite <- time.Since(started):
			default:
			}
			return nil
		},
	}
	ctx := context.WithValue(context.Background(), continuousRetryKeepaliveContextKey{}, continuousRetryKeepalive(keepalive))
	if !waitWithContinuousRetryKeepalive(ctx, 200*time.Millisecond) {
		t.Fatal("unlimited retry wait failed")
	}
	select {
	case elapsed := <-firstWrite:
		if elapsed >= 150*time.Millisecond {
			t.Fatalf("first heartbeat fired after %v, want it near the existing deadline", elapsed)
		}
	default:
		t.Fatal("existing heartbeat deadline was skipped")
	}
}

func TestFiniteRetryWaitDoesNotActivateKeepalive(t *testing.T) {
	h, store := newRetryTestHandler(t)
	store.SetRetryIntervalMS(1)
	keepalive := &recordingContinuousRetryKeepalive{}
	ctx := contextWithContinuousRetryKeepalive(keepalive)

	if !h.waitBeforeRetryWithBudget(ctx, 1, 1) {
		t.Fatal("finite retry wait failed")
	}
	if keepalive.active || keepalive.writes != 0 {
		t.Fatalf("finite retry used heartbeat: active=%v writes=%d", keepalive.active, keepalive.writes)
	}
}

func TestActivateContinuousRetryKeepaliveForLimitOnlyActivatesUnlimited(t *testing.T) {
	keepalive := &recordingContinuousRetryKeepalive{}
	ctx := contextWithContinuousRetryKeepalive(keepalive)
	activateContinuousRetryKeepaliveForLimit(ctx, 3)
	if keepalive.active {
		t.Fatal("finite retry limit activated the continuous heartbeat")
	}
	activateContinuousRetryKeepaliveForLimit(ctx, -1)
	if !keepalive.active {
		t.Fatal("unlimited retry limit did not activate the continuous heartbeat")
	}
}

func TestContinuousRetryKeepaliveFailureCancelsRequest(t *testing.T) {
	previousInterval := continuousRetryKeepaliveInterval
	continuousRetryKeepaliveInterval = time.Millisecond
	t.Cleanup(func() { continuousRetryKeepaliveInterval = previousInterval })

	ctx, cancel := context.WithCancelCause(context.Background())
	wantErr := errors.New("downstream closed")
	keepalive := &requestContinuousRetryKeepalive{
		active: true,
		last:   time.Now().Add(-continuousRetryKeepaliveInterval),
		write:  func() error { return wantErr },
		cancel: cancel,
	}
	if err := keepalive.Keepalive(); !errors.Is(err, wantErr) {
		t.Fatalf("Keepalive error = %v, want %v", err, wantErr)
	}
	if !errors.Is(context.Cause(ctx), wantErr) {
		t.Fatalf("request cause = %v, want %v", context.Cause(ctx), wantErr)
	}
}

func TestExecuteHTTPWithContinuousRetryKeepaliveWhileWaitingForHeaders(t *testing.T) {
	previousInterval := continuousRetryKeepaliveInterval
	continuousRetryKeepaliveInterval = time.Millisecond
	t.Cleanup(func() { continuousRetryKeepaliveInterval = previousInterval })

	release := make(chan struct{})
	keepalive := &requestContinuousRetryKeepalive{
		active: true,
		last:   time.Now(),
		write: func() error {
			select {
			case <-release:
			default:
				close(release)
			}
			return nil
		},
	}
	ctx := context.WithValue(context.Background(), continuousRetryKeepaliveContextKey{}, continuousRetryKeepalive(keepalive))
	response, err := executeHTTPWithContinuousRetryKeepalive(ctx, func() (*http.Response, error) {
		<-release
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	})
	if err != nil {
		t.Fatalf("execute HTTP: %v", err)
	}
	if response == nil || response.StatusCode != http.StatusOK {
		t.Fatalf("response = %#v", response)
	}
}

func TestReadSSEStreamWithContinuousRetryKeepaliveWhileWaitingForFrame(t *testing.T) {
	previousInterval := continuousRetryKeepaliveInterval
	continuousRetryKeepaliveInterval = time.Millisecond
	t.Cleanup(func() { continuousRetryKeepaliveInterval = previousInterval })

	release := make(chan struct{})
	keepalive := &requestContinuousRetryKeepalive{
		active: true,
		last:   time.Now(),
		write: func() error {
			select {
			case <-release:
			default:
				close(release)
			}
			return nil
		},
	}
	ctx := context.WithValue(context.Background(), continuousRetryKeepaliveContextKey{}, continuousRetryKeepalive(keepalive))
	reader, writer := io.Pipe()
	t.Cleanup(func() { _ = reader.Close() })
	go func() {
		<-release
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.completed\"}\n\n")
		_ = writer.Close()
	}()
	got := ""
	err := readSSEStreamWithContinuousRetryKeepalive(ctx, reader, func(_ string, data []byte) bool {
		got = string(data)
		return false
	})
	if err != nil {
		t.Fatalf("read SSE: %v", err)
	}
	if got != `{"type":"response.completed"}` {
		t.Fatalf("event = %q", got)
	}
}

func TestContinuousRetrySSEKeepaliveAndCommittedErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	stop := installContinuousRetrySSEKeepalive(c, true, "text/event-stream")
	defer stop()

	keepalive, ok := continuousRetryKeepaliveForContext(c.Request.Context()).(*requestContinuousRetryKeepalive)
	if !ok {
		t.Fatal("SSE heartbeat was not installed")
	}
	keepalive.Activate()
	keepalive.last = time.Time{}
	if err := keepalive.Keepalive(); err != nil {
		t.Fatalf("write SSE heartbeat: %v", err)
	}
	if !strings.Contains(recorder.Body.String(), continuousRetryKeepaliveComment) {
		t.Fatalf("SSE heartbeat body = %q", recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}

	if !writeCommittedResponsesRetryError(c, "upstream failed") {
		t.Fatal("committed Responses error was not written as SSE")
	}
	if body := recorder.Body.String(); !strings.Contains(body, `"type":"response.failed"`) || !strings.Contains(body, "upstream failed") {
		t.Fatalf("committed Responses SSE error = %q", body)
	}
}

func TestResolvePreContentRetryErrorCandidate(t *testing.T) {
	candidate := []byte(`{"type":"error","error":{"status_code":403,"code":"forbidden"}}`)
	payload, promoted := resolvePreContentRetryErrorCandidate(nil, candidate, false, false, false, nil, nil, nil)
	if !promoted || string(payload) != string(candidate) {
		t.Fatalf("standalone error was not promoted: promoted=%v payload=%s", promoted, payload)
	}

	terminal := []byte(`{"type":"response.failed"}`)
	payload, promoted = resolvePreContentRetryErrorCandidate(terminal, candidate, false, false, true, nil, nil, nil)
	if promoted || string(payload) != string(terminal) {
		t.Fatalf("terminal response.failed did not win: promoted=%v payload=%s", promoted, payload)
	}
	payload, promoted = resolvePreContentRetryErrorCandidate(nil, candidate, false, false, false, errors.New("unexpected EOF"), nil, nil)
	if !promoted || string(payload) != string(candidate) {
		t.Fatalf("structured error did not take precedence over read failure: promoted=%v payload=%s", promoted, payload)
	}

	for _, tc := range []struct {
		name         string
		contentSeen  bool
		wroteAnyBody bool
		gotTerminal  bool
		ctxErr       error
		writeErr     error
	}{
		{name: "content seen", contentSeen: true},
		{name: "body written", wroteAnyBody: true},
		{name: "successful terminal", gotTerminal: true},
		{name: "client canceled", ctxErr: context.Canceled},
		{name: "downstream failed", writeErr: errors.New("broken pipe")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload, promoted := resolvePreContentRetryErrorCandidate(nil, candidate, tc.contentSeen, tc.wroteAnyBody, tc.gotTerminal, nil, tc.ctxErr, tc.writeErr)
			if promoted || len(payload) != 0 {
				t.Fatalf("unsafe candidate promotion: promoted=%v payload=%s", promoted, payload)
			}
		})
	}
}
