package wsrelay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gorilla/websocket"
)

func enableRotationForTest(t *testing.T) {
	t.Helper()
	t.Setenv("CODEX_WS_CONNECTION_MODE", "rotation")
	t.Setenv("CODEX_WS_MAX_PROXY_ROUTES", "2")
	t.Setenv("CODEX_WS_MAX_SIBLINGS", "3")
}

func rotationTestServer(t *testing.T) string {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http")
}

func TestRotationModeCreatesSiblingInsteadOfBusyWait(t *testing.T) {
	enableRotationForTest(t)
	manager := NewManager()
	t.Cleanup(manager.Stop)
	manager.probeFunc = func(*WsConnection) bool { return true }

	account := &auth.Account{DBID: 42, DynamicConcurrencyLimit: 2}
	wsURL := rotationTestServer(t)
	group := "rotation-session"
	key := manager.poolKey(account.ID(), wsURL, group, "")
	session := NewSession(account.ID(), manager)
	session.SetConnected(true)
	busy := &WsConnection{session: session, URL: wsURL, PoolKey: key, groupKey: group}
	busy.SetState(StateConnected)
	busy.Touch()
	manager.connections.Store(key, busy)
	manager.sessions.Store(key, session)
	pending := session.AddPendingRequest(group)
	t.Cleanup(func() { session.RemovePendingRequest(pending.RequestID) })

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got, pr, err := manager.AcquireConnection(ctx, account, wsURL, group, http.Header{}, "")
	if err != nil {
		t.Fatalf("AcquireConnection() error = %v", err)
	}
	if got == busy {
		t.Fatal("rotation mode reused the busy connection")
	}
	if pr == nil {
		t.Fatal("rotation mode did not reserve a sibling request")
	}
	if !strings.Contains(got.PoolKey, "#rot-") {
		t.Fatalf("sibling pool key = %q, want rotation suffix", got.PoolKey)
	}
	got.session.RemovePendingRequest(pr.RequestID)
	manager.DiscardConnection(got)
}

func TestRotationSiblingLimitDoesNotRejectSessionRequest(t *testing.T) {
	enableRotationForTest(t)
	t.Setenv("CODEX_WS_MAX_SIBLINGS", "3")
	manager := NewManager()
	t.Cleanup(manager.Stop)
	manager.probeFunc = func(*WsConnection) bool { return true }

	account := &auth.Account{DBID: 42, DynamicConcurrencyLimit: 4}
	wsURL := rotationTestServer(t)
	group := "session-visible-in-old-limit-error"
	for i := 0; i < 3; i++ {
		poolKey := manager.poolKey(account.ID(), wsURL, group+"#existing-"+string(rune('a'+i)), "")
		session := NewSession(account.ID(), manager)
		session.SetConnected(true)
		wc := &WsConnection{session: session, URL: wsURL, PoolKey: poolKey, groupKey: group}
		wc.SetState(StateConnected)
		wc.Touch()
		manager.connections.Store(poolKey, wc)
		manager.sessions.Store(poolKey, session)
		pending := session.AddPendingRequest(group)
		t.Cleanup(func() {
			session.RemovePendingRequest(pending.RequestID)
			manager.DiscardConnection(wc)
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got, pending, err := manager.AcquireConnection(ctx, account, wsURL, group, http.Header{}, "")
	if err != nil {
		t.Fatalf("AcquireConnection() rejected a request at the soft sibling target: %v", err)
	}
	if got == nil || pending == nil {
		t.Fatal("AcquireConnection() did not create the temporary fourth sibling")
	}
	if connections := manager.rotationGroupConnections(account.ID(), wsURL, group); len(connections) != 4 {
		t.Fatalf("live siblings during request = %d, want 4", len(connections))
	}

	got.session.RemovePendingRequest(pending.RequestID)
	manager.ReleaseConnection(got)
	if connections := manager.rotationGroupConnections(account.ID(), wsURL, group); len(connections) != 3 {
		t.Fatalf("live siblings after release = %d, want retention target 3", len(connections))
	}
}

func TestRotationAgeDrainsThenCloses(t *testing.T) {
	enableRotationForTest(t)
	t.Setenv("CODEX_WS_ROTATION_MAX_AGE", "1ms")
	manager := NewManager()
	t.Cleanup(manager.Stop)

	account := &auth.Account{DBID: 42, DynamicConcurrencyLimit: 2}
	wsURL := "wss://example.test/responses"
	group := "drain-session"
	key := manager.poolKey(account.ID(), wsURL, group, "")
	session := NewSession(account.ID(), manager)
	session.SetConnected(true)
	wc := &WsConnection{session: session, URL: wsURL, PoolKey: key, groupKey: group}
	wc.SetState(StateConnected)
	wc.createdAt = time.Now().Add(-time.Second).UnixNano()
	wc.Touch()
	session.SetOnPendingEmpty(func() { manager.onSessionPendingEmpty(wc) })
	manager.connections.Store(key, wc)
	manager.sessions.Store(key, session)
	pending := session.AddPendingRequest(group)

	manager.evictExpired()
	if !wc.IsDraining() {
		t.Fatal("over-age connection was not marked draining")
	}
	if !wc.IsConnected() {
		t.Fatal("draining connection was closed before its request completed")
	}
	if _, ok := manager.connections.Load(key); !ok {
		t.Fatal("draining connection must remain tracked while pending")
	}

	session.RemovePendingRequest(pending.RequestID)
	if wc.IsConnected() {
		t.Fatal("draining connection did not close after the last pending request")
	}
	if _, ok := manager.connections.Load(key); ok {
		t.Fatal("drained connection remained in the active map")
	}
}

func TestRotationStatelessUsesRotatedSiblingNotOneShotFallback(t *testing.T) {
	enableRotationForTest(t)
	manager := NewManager()
	t.Cleanup(manager.Stop)
	manager.probeFunc = func(*WsConnection) bool { return true }

	account := &auth.Account{DBID: 42, DynamicConcurrencyLimit: 2}
	wsURL := rotationTestServer(t)
	slot := "cache-key#0"
	key := manager.poolKey(account.ID(), wsURL, slot, "")
	session := NewSession(account.ID(), manager)
	session.SetConnected(true)
	busy := &WsConnection{session: session, URL: wsURL, PoolKey: key, groupKey: slot}
	busy.SetState(StateConnected)
	busy.Touch()
	manager.connections.Store(key, busy)
	manager.sessions.Store(key, session)
	pending := session.AddPendingRequest(slot)
	t.Cleanup(func() { session.RemovePendingRequest(pending.RequestID) })

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got, pr, usedKey, err := manager.AcquireReusableConnection(ctx, account, wsURL, "cache-key", "stateless-fallback", 1, http.Header{}, "")
	if err != nil {
		t.Fatalf("AcquireReusableConnection() error = %v", err)
	}
	if got == busy || pr == nil {
		t.Fatal("rotation mode did not allocate a sibling stateless connection")
	}
	if !strings.Contains(usedKey, "#rot-") {
		t.Fatalf("used key = %q, want rotated sibling key", usedKey)
	}
	if strings.Contains(got.session.ID, "stateless-") {
		t.Fatalf("rotated persistent sibling became one-shot session: %q", got.session.ID)
	}
	got.session.RemovePendingRequest(pr.RequestID)
	manager.DiscardConnection(got)
}

func TestRotationProxyCandidatesUseSelectorAndLimit(t *testing.T) {
	enableRotationForTest(t)
	t.Setenv("CODEX_WS_MAX_PROXY_ROUTES", "2")
	manager := NewManager()
	t.Cleanup(manager.Stop)
	manager.SetProxySelector(func(*auth.Account, int) []string {
		return []string{"http://proxy-a:1", "http://proxy-b:2", "http://proxy-c:3"}
	})
	got := manager.proxyCandidates(&auth.Account{DBID: 1}, "")
	if len(got) != 2 || got[0] != "http://proxy-a:1" || got[1] != "http://proxy-b:2" {
		t.Fatalf("proxy candidates = %v, want first two selector routes", got)
	}
}

func TestPersistedRotationSettingsOverrideEnvironment(t *testing.T) {
	previous := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(previous) })
	t.Setenv("CODEX_WS_CONNECTION_MODE", "busy")
	t.Setenv("CODEX_WS_ROTATION_MAX_AGE", "1ms")
	t.Setenv("CODEX_WS_MAX_SIBLINGS", "3")
	t.Setenv("CODEX_WS_MAX_PROXY_ROUTES", "1")
	proxy.ApplyRuntimeSettingsFromSystem(&database.SystemSettings{
		CodexWSRotationEnabled:   true,
		CodexWSRotationMaxAgeSec: 600,
		CodexWSMaxSiblings:       5,
		CodexWSMaxProxyRoutes:    3,
	})
	if !wsRotationModeEnabled() {
		t.Fatal("persisted admin setting should override legacy mode environment variable")
	}
	if got := connectionRotationAge(); got != 10*time.Minute {
		t.Fatalf("persisted rotation age = %s, want 10m", got)
	}
	if got := wsMaxSiblingConnections(); got != 5 {
		t.Fatalf("persisted sibling limit = %d, want 5", got)
	}
	if got := wsMaxProxyRoutes(); got != 3 {
		t.Fatalf("persisted route limit = %d, want 3", got)
	}
}

func TestApplyRotationEnvironmentDefaultsSeedsNewSettings(t *testing.T) {
	t.Setenv("CODEX_WS_CONNECTION_MODE", "rotation")
	t.Setenv("CODEX_WS_ROTATION_MAX_AGE", "10m")
	t.Setenv("CODEX_WS_MAX_SIBLINGS", "5")
	t.Setenv("CODEX_WS_MAX_PROXY_ROUTES", "3")
	settings := &database.SystemSettings{}
	ApplyRotationEnvironmentDefaults(settings)
	if !settings.CodexWSRotationEnabled || settings.CodexWSRotationMaxAgeSec != 600 || settings.CodexWSMaxSiblings != 5 || settings.CodexWSMaxProxyRoutes != 3 {
		t.Fatalf("seeded rotation settings = enabled:%t age:%d siblings:%d routes:%d", settings.CodexWSRotationEnabled, settings.CodexWSRotationMaxAgeSec, settings.CodexWSMaxSiblings, settings.CodexWSMaxProxyRoutes)
	}
}

func TestRotationModeReadsRuntimeSettingsWhenEnvUnset(t *testing.T) {
	previous := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(previous) })
	t.Setenv("CODEX_WS_CONNECTION_MODE", "")
	t.Setenv("CODEX_WS_ROTATION_MAX_AGE", "")
	t.Setenv("CODEX_WS_MAX_SIBLINGS", "")
	t.Setenv("CODEX_WS_MAX_PROXY_ROUTES", "")
	next := proxy.DefaultRuntimeSettings()
	next.CodexWSRotationEnabled = true
	next.CodexWSRotationMaxAgeSec = 600
	next.CodexWSMaxSiblings = 5
	next.CodexWSMaxProxyRoutes = 3
	next.CodexWSRotationSettingsAuthoritative = true
	proxy.ApplyRuntimeSettings(next)
	if !wsRotationModeEnabled() {
		t.Fatal("rotation mode should follow runtime setting when env is unset")
	}
	if got := connectionRotationAge(); got != 10*time.Minute {
		t.Fatalf("runtime rotation age = %s, want 10m", got)
	}
	if got := wsMaxSiblingConnections(); got != 5 {
		t.Fatalf("runtime sibling limit = %d, want 5", got)
	}
	if got := wsMaxProxyRoutes(); got != 3 {
		t.Fatalf("runtime route limit = %d, want 3", got)
	}
}

func TestRotationModeSettingsStayIndependentFromLegacyBusySettings(t *testing.T) {
	setBusyRuntimeSettings(t, func(s *proxy.RuntimeSettings) {
		s.CodexWSBusyOverflow = false
	})
	t.Setenv("CODEX_WS_CONNECTION_MODE", "rotation")
	if !wsRotationModeEnabled() {
		t.Fatal("rotation mode should be enabled by CODEX_WS_CONNECTION_MODE=rotation")
	}
}

func TestRotationAgeClampsToConnectionHardLifetime(t *testing.T) {
	t.Setenv("CODEX_WS_CONNECTION_MODE", "rotation")
	t.Setenv("CODEX_WS_ROTATION_MAX_AGE", "999h")
	if got, want := connectionRotationAge(), connectionMaxLifetime(); got != want {
		t.Fatalf("rotation age = %s, want hard lifetime %s", got, want)
	}
	t.Setenv("CODEX_WS_ROTATION_MAX_AGE", "invalid")
	if got, want := connectionRotationAge(), connectionMaxLifetime(); got != want {
		t.Fatalf("invalid rotation age = %s, want hard lifetime %s", got, want)
	}
}

func TestDrainingConnectionCannotReuse(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)
	account := &auth.Account{DBID: 1}
	wc, _ := newTestSlotConnection(manager, account, "wss://example.test/responses", "drain-slot")
	if !wc.MarkDraining() {
		t.Fatal("MarkDraining returned false")
	}
	if canReuseConnection(wc) {
		t.Fatal("draining connection must not be reusable")
	}
	if !wc.IsConnected() {
		t.Fatal("marking draining must not close the socket")
	}
}

func TestRotationProxyRouteLimitIsAccountWide(t *testing.T) {
	enableRotationForTest(t)
	t.Setenv("CODEX_WS_MAX_PROXY_ROUTES", "2")
	manager := NewManager()
	t.Cleanup(manager.Stop)
	manager.SetProxySelector(func(*auth.Account, int) []string {
		return []string{"http://proxy-a:1", "http://proxy-b:2", "http://proxy-c:3"}
	})
	account := &auth.Account{DBID: 7}
	wsURL := "wss://example.test/responses"
	for i, route := range []string{"http://proxy-a:1", "http://proxy-b:2"} {
		poolKey := manager.poolKey(account.ID(), wsURL, "group-"+string(rune('a'+i)), route)
		session := NewSession(account.ID(), manager)
		session.SetConnected(true)
		wc := &WsConnection{session: session, URL: wsURL, PoolKey: poolKey, groupKey: "group-" + string(rune('a'+i)), proxyURL: route}
		wc.SetState(StateConnected)
		manager.connections.Store(poolKey, wc)
		manager.sessions.Store(poolKey, session)
	}
	got := manager.limitRotationRoutesToAccount(account.ID(), wsURL, []string{"http://proxy-a:1", "http://proxy-b:2", "http://proxy-c:3"})
	if len(got) != 2 || got[0] != "http://proxy-a:1" || got[1] != "http://proxy-b:2" {
		t.Fatalf("filtered routes = %v, want existing two routes", got)
	}
}

func TestDrainingAllowsAlreadyReservedLeaseToCommit(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)
	account := &auth.Account{DBID: 1}
	wc, _ := newTestSlotConnection(manager, account, "wss://example.test/responses", "reserved")
	if err := wc.BeginReadLease("reserved-request"); err != nil {
		t.Fatalf("BeginReadLease: %v", err)
	}
	if !wc.MarkDraining() {
		t.Fatal("MarkDraining returned false")
	}
	if _, _, err := wc.beginReadLeaseWrite(websocket.TextMessage); err != nil {
		t.Fatalf("already reserved lease was rejected after draining: %v", err)
	}
}

func TestPingIdleConnectionsSkipsDraining(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)
	wc := addConnectedConn(t, manager, 1, "draining-idle")
	if !wc.MarkDraining() {
		t.Fatal("MarkDraining returned false")
	}
	pinged, failed := manager.PingIdleConnections()
	if pinged != 0 || failed != 0 {
		t.Fatalf("draining keepalive = pinged:%d failed:%d, want zero", pinged, failed)
	}
}

func TestRotationPendingRouteReservationEnforcesDistinctLimit(t *testing.T) {
	enableRotationForTest(t)
	t.Setenv("CODEX_WS_MAX_PROXY_ROUTES", "2")
	manager := NewManager()
	t.Cleanup(manager.Stop)
	if !manager.reserveRotationRoute(1, "wss://example.test/responses", "route-a") {
		t.Fatal("first route reservation failed")
	}
	if !manager.reserveRotationRoute(1, "wss://example.test/responses", "route-b") {
		t.Fatal("second distinct route reservation failed")
	}
	if manager.reserveRotationRoute(1, "wss://example.test/responses", "route-c") {
		t.Fatal("third distinct route reservation exceeded limit")
	}
	manager.releaseRotationRoute(1, "wss://example.test/responses", "route-a")
	if !manager.reserveRotationRoute(1, "wss://example.test/responses", "route-c") {
		t.Fatal("route reservation did not recover after release")
	}
	manager.releaseRotationRoute(1, "wss://example.test/responses", "route-b")
	manager.releaseRotationRoute(1, "wss://example.test/responses", "route-c")
}

func TestRotationProxySelectorCanFailClosed(t *testing.T) {
	enableRotationForTest(t)
	manager := NewManager()
	t.Cleanup(manager.Stop)
	manager.SetProxySelector(func(*auth.Account, int) []string { return nil })
	if got := manager.proxyCandidates(&auth.Account{DBID: 1}, ""); got != nil {
		t.Fatalf("proxy candidates = %v, want nil for fail-closed selector", got)
	}
}

func TestRotationModeReusesIdleSiblingAcrossAllowedRoute(t *testing.T) {
	enableRotationForTest(t)
	manager := NewManager()
	t.Cleanup(manager.Stop)
	manager.probeFunc = func(*WsConnection) bool { return true }
	manager.SetProxySelector(func(*auth.Account, int) []string {
		return []string{"http://proxy-a:1", "http://proxy-b:2"}
	})
	account := &auth.Account{DBID: 42, DynamicConcurrencyLimit: 2}
	wsURL := "wss://example.test/responses"
	group := "shared-group"
	poolKey := manager.poolKey(account.ID(), wsURL, group, "http://proxy-b:2")
	session := NewSession(account.ID(), manager)
	session.SetConnected(true)
	wc := &WsConnection{session: session, URL: wsURL, PoolKey: poolKey, groupKey: group, proxyURL: "http://proxy-b:2"}
	wc.SetState(StateConnected)
	wc.Touch()
	manager.connections.Store(poolKey, wc)
	manager.sessions.Store(poolKey, session)

	got, pr, err := manager.AcquireConnection(context.Background(), account, wsURL, group, http.Header{}, "")
	if err != nil || got != wc || pr == nil {
		t.Fatalf("acquire = (%p,%v,%v), want idle proxy-b sibling", got, pr, err)
	}
	wc.session.RemovePendingRequest(pr.RequestID)
}
