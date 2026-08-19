package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestRealtimeResponseControllerAssignsCancelToActiveWireTurn(t *testing.T) {
	controller := &realtimeResponseController{}
	controller.observeInbound(responsesWSInboundMessage{
		messageType: websocket.TextMessage,
		payload:     []byte(`{"type":"response.create"}`),
		turn:        1,
	})
	controller.observeInbound(responsesWSInboundMessage{
		messageType: websocket.TextMessage,
		payload:     []byte(`{"type":"response.create"}`),
		turn:        2,
	})
	controller.observeInbound(responsesWSInboundMessage{
		messageType: websocket.TextMessage,
		payload:     []byte(`{"type":"response.cancel"}`),
		turn:        3,
	})

	firstCtx, finishFirst := controller.begin(context.Background(), 1)
	select {
	case <-firstCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("cancel observed before dispatch did not reach the first response turn")
	}
	if !errors.Is(context.Cause(firstCtx), errRealtimeResponseCanceled) {
		t.Fatalf("first turn cancel cause = %v", context.Cause(firstCtx))
	}
	if !finishFirst() {
		t.Fatal("first response turn was not marked canceled")
	}

	secondCtx, finishSecond := controller.begin(context.Background(), 2)
	defer finishSecond()
	select {
	case <-secondCtx.Done():
		t.Fatalf("response.cancel leaked into the next queued turn: %v", context.Cause(secondCtx))
	default:
	}
}

func TestRealtimeResponseControllerCancelWithoutPendingTurnIsNoop(t *testing.T) {
	controller := &realtimeResponseController{}
	controller.observeInbound(responsesWSInboundMessage{
		messageType: websocket.TextMessage,
		payload:     []byte(`{"type":"response.cancel"}`),
		turn:        1,
	})
	controller.observeInbound(responsesWSInboundMessage{
		messageType: websocket.TextMessage,
		payload:     []byte(`{"type":"response.create"}`),
		turn:        2,
	})

	turnCtx, finishTurn := controller.begin(context.Background(), 2)
	select {
	case <-turnCtx.Done():
		t.Fatalf("response.cancel without a pending turn canceled a later response: %v", context.Cause(turnCtx))
	default:
	}
	if finishTurn() {
		t.Fatal("later response was marked canceled by an earlier no-op response.cancel")
	}
}

func TestRealtimeResponseControllerCancelBeforeCompleteRemainsCanceled(t *testing.T) {
	controller := &realtimeResponseController{}
	controller.observeInbound(responsesWSInboundMessage{
		messageType: websocket.TextMessage,
		payload:     []byte(`{"type":"response.create"}`),
		turn:        1,
	})
	turnCtx, finishTurn := controller.begin(context.Background(), 1)
	controller.observeInbound(responsesWSInboundMessage{
		messageType: websocket.TextMessage,
		payload:     []byte(`{"type":"response.cancel"}`),
		turn:        2,
	})
	controller.complete(1)

	select {
	case <-turnCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("active turn did not receive response.cancel")
	}
	if !errors.Is(context.Cause(turnCtx), errRealtimeResponseCanceled) {
		t.Fatalf("turn cancel cause = %v", context.Cause(turnCtx))
	}
	if !finishTurn() {
		t.Fatal("complete discarded an earlier canceled marker")
	}
}

func TestRealtimeResponseControllerLateCancelAfterCompleteIsNoop(t *testing.T) {
	controller := &realtimeResponseController{}
	controller.observeInbound(responsesWSInboundMessage{
		messageType: websocket.TextMessage,
		payload:     []byte(`{"type":"response.create"}`),
		turn:        1,
	})
	controller.observeInbound(responsesWSInboundMessage{
		messageType: websocket.TextMessage,
		payload:     []byte(`{"type":"response.create"}`),
		turn:        2,
	})
	firstCtx, finishFirst := controller.begin(context.Background(), 1)
	controller.complete(1)
	controller.observeInbound(responsesWSInboundMessage{
		messageType: websocket.TextMessage,
		payload:     []byte(`{"type":"response.cancel"}`),
		turn:        3,
	})

	select {
	case <-firstCtx.Done():
		t.Fatalf("late response.cancel canceled a completed turn: %v", context.Cause(firstCtx))
	default:
	}
	if finishFirst() {
		t.Fatal("completed turn was marked canceled by a late response.cancel")
	}

	secondCtx, finishSecond := controller.begin(context.Background(), 2)
	defer finishSecond()
	select {
	case <-secondCtx.Done():
		t.Fatalf("late response.cancel skipped into the next queued turn: %v", context.Cause(secondCtx))
	default:
	}
}

func TestRealtimeResponseCancelSkipsUpstreamDrainDelay(t *testing.T) {
	turnCtx, cancelTurn := context.WithCancelCause(context.Background())
	upstreamCtx, cancelUpstream := newDrainableUpstreamContext(turnCtx, time.Hour)
	defer cancelUpstream()

	cancelTurn(errRealtimeResponseCanceled)
	select {
	case <-upstreamCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("Realtime response cancellation waited for the upstream drain timeout")
	}
}

func TestRealtimeResponseCancelStopsUnlimitedRetryAndKeepsSocket(t *testing.T) {
	gin.SetMode(gin.TestMode)

	previousExec := WebsocketExecuteFunc
	previousSettings := CurrentRuntimeSettings()
	t.Cleanup(func() {
		WebsocketExecuteFunc = previousExec
		ApplyRuntimeSettings(previousSettings)
	})
	nextSettings := previousSettings
	nextSettings.CodexWSSilentRetry = false
	nextSettings.CodexWSHideErrors = false
	nextSettings.CodexWSSilentRetries = 0
	nextSettings.ContinuousRetryPolicy = database.ContinuousRetryPolicy{
		Enabled:    true,
		Categories: []string{database.ContinuousRetryCategoryTransport},
	}
	ApplyRuntimeSettings(nextSettings)

	var calls atomic.Int64
	firstAttempt := make(chan struct{}, 1)
	requestBodies := make(chan []byte, 2)
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		requestBodies <- append([]byte(nil), requestBody...)
		if calls.Add(1) == 1 {
			firstAttempt <- struct{}{}
			return nil, errors.New("connection reset by peer")
		}
		sse := `data: {"type":"response.output_text.delta","delta":"ok"}` + "\n\n" +
			`data: {"type":"response.completed","response":{"id":"resp_after_cancel","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(sse)),
		}, nil
	}

	handler, store := newRetryTestHandler(t)
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "test-token", PlanType: "pro", AccountID: "test-account"})
	store.SetRetryIntervalMS(60_000)
	store.SetTransportRetryPolicy("sticky")

	router := gin.New()
	handler.RegisterRoutes(router)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/realtime?model=gpt-5.4"
	conn, response, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		if response != nil {
			t.Fatalf("dial Realtime websocket: %v status=%d", err, response.StatusCode)
		}
		t.Fatalf("dial Realtime websocket: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	readEvent := func(timeout time.Duration) []byte {
		t.Helper()
		_ = conn.SetReadDeadline(time.Now().Add(timeout))
		_, payload, readErr := conn.ReadMessage()
		if readErr != nil {
			t.Fatalf("read Realtime event: %v", readErr)
		}
		return payload
	}
	if event := readEvent(time.Second); gjson.GetBytes(event, "type").String() != "session.created" {
		t.Fatalf("initial Realtime event = %s", event)
	}

	writeItem := func(text string) {
		t.Helper()
		payload := `{"type":"conversation.item.create","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"` + text + `"}]}}`
		if writeErr := conn.WriteMessage(websocket.TextMessage, []byte(payload)); writeErr != nil {
			t.Fatalf("write conversation item: %v", writeErr)
		}
	}

	writeItem("cancel this")
	if event := readEvent(time.Second); gjson.GetBytes(event, "type").String() != "conversation.item.created" {
		t.Fatalf("first item acknowledgement = %s", event)
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create"}`)); err != nil {
		t.Fatalf("write first response.create: %v", err)
	}
	select {
	case <-firstAttempt:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the first upstream attempt")
	}
	select {
	case <-requestBodies:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the first upstream body")
	}

	cancelStarted := time.Now()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.cancel"}`)); err != nil {
		t.Fatalf("write response.cancel: %v", err)
	}
	writeItem("continue here")
	if event := readEvent(2 * time.Second); gjson.GetBytes(event, "type").String() != "conversation.item.created" {
		t.Fatalf("post-cancel item acknowledgement = %s", event)
	}
	if elapsed := time.Since(cancelStarted); elapsed > time.Second {
		t.Fatalf("response.cancel took too long to release the retry wait: %v", elapsed)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream retried after response.cancel: calls=%d", got)
	}

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create"}`)); err != nil {
		t.Fatalf("write second response.create: %v", err)
	}
	select {
	case body := <-requestBodies:
		if got := gjson.GetBytes(body, "input.0.content.0.text").String(); got != "continue here" {
			t.Fatalf("post-cancel upstream input = %q body=%s", got, body)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the post-cancel upstream request")
	}

	for {
		event := readEvent(2 * time.Second)
		if eventType := gjson.GetBytes(event, "type").String(); eventType == "response.done" {
			break
		} else if eventType == "error" {
			t.Fatalf("post-cancel turn returned an error: %s", event)
		}
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream call count = %d, want 2", got)
	}
}
