package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
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

// Every probe owns its transport. Providers rotate dynamic egress at connection
// boundaries; sharing a keep-alive or HTTP/2 session would defeat that policy.
func newCodexHarvestTransport(proxyURL string) (*http.Transport, error) {
	if err := auth.ValidateCodexHarvestProxy(proxyURL); err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DisableKeepAlives = true
	transport.ForceAttemptHTTP2 = false
	transport.TLSNextProto = make(map[string]func(string, *tls.Conn) http.RoundTripper)
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}}
	// The configured attempt context bounds headers and the optional body scan.
	transport.ResponseHeaderTimeout = 0
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: -1}
	transport.DialContext = dialer.DialContext
	if err := auth.ConfigureTransportProxy(transport, proxyURL, dialer); err != nil {
		return nil, errors.New("invalid harvest proxy transport")
	}
	return transport, nil
}

// HarvestCodexTurnState deliberately bypasses Resin and the normal account
// transport. Its only route is the administrator's dedicated harvest proxy.
func HarvestCodexTurnState(ctx context.Context, account *auth.Account, model, proxyURL string) (string, error) {
	return harvestCodexTurnStateAt(ctx, account, model, proxyURL, CodexBaseURL+"/responses")
}

func harvestCodexTurnStateAt(ctx context.Context, account *auth.Account, model, proxyURL, endpoint string) (string, error) {
	if account == nil || account.IsRelayStyle() || account.GetAccessToken() == "" {
		return "", errors.New("Codex account access token required")
	}
	transport, err := newCodexHarvestTransport(proxyURL)
	if err != nil {
		return "", err
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	body, err := json.Marshal(map[string]any{"model": model, "store": false, "stream": true, "instructions": "Reply with exactly: pong",
		"input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "ping"}}}}})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", errors.New("cannot create harvest request")
	}
	applyCodexRequestHeaders(req, account, account.GetAccessToken(), NewUpstreamSessionUUID(), "", nil, nil)
	req.Header.Del(codexTurnStateHeader)
	req.Header.Del("X-Resin-Account")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	req.Close = true
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", errors.New("harvest proxy request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("harvest HTTP %d", resp.StatusCode)
	}
	if state := observedCodexTurnState(resp.Header.Get(codexTurnStateHeader)); state != "" {
		return state, nil
	}
	// Some transports surface the state as response.metadata/current_turn_state.
	// Bound the scan and stop as soon as a ticket is observed.
	scanner := bufio.NewScanner(io.LimitReader(resp.Body, 64<<10))
	scanner.Buffer(make([]byte, 4096), 64<<10)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if state := codexTurnStateFromFrame([]byte(line)); state != "" {
			return state, nil
		}
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	return "", errors.New("response did not contain a turn-state ticket")
}
