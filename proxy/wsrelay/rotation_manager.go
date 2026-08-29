package wsrelay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/codex2api/auth"
)

// rotationGroupLockKey serializes acquisition within one logical session/slot
// while allowing unrelated sessions to proceed independently.
func rotationGroupLockKey(accountID int64, wsURL, groupKey string) string {
	return fmt.Sprintf("ws-rotation-group|%d|%s|%s", accountID, wsURL, groupKey)
}

func normalizeRotationGroupKey(key string) string {
	key = strings.TrimSpace(key)
	for _, marker := range []string{"#rot-", busyOverflowKeyInfix} {
		if idx := strings.Index(key, marker); idx >= 0 {
			suffix := key[idx+len(marker):]
			if suffix != "" {
				allDigits := true
				for _, r := range suffix {
					if r < '0' || r > '9' {
						allDigits = false
						break
					}
				}
				if allDigits {
					return key[:idx]
				}
			}
		}
	}
	return key
}

func rotationConnectionGroup(wc *WsConnection) string {
	if wc == nil {
		return ""
	}
	if key := strings.TrimSpace(wc.groupKey); key != "" {
		return key
	}
	if wc.session != nil {
		return strings.TrimSpace(wc.session.ID)
	}
	return ""
}

func rotationConnectionMatches(wc *WsConnection, accountID int64, wsURL, groupKey string) bool {
	if wc == nil || wc.session == nil || wc.session.AccountID != accountID {
		return false
	}
	if wsURL != "" && wc.URL != wsURL {
		return false
	}
	return rotationConnectionGroup(wc) == strings.TrimSpace(groupKey)
}

func (m *Manager) rotationGroupConnections(accountID int64, wsURL, groupKey string) []*WsConnection {
	if m == nil {
		return nil
	}
	connections := make([]*WsConnection, 0, wsMaxSiblingConnections())
	m.connections.Range(func(_, value any) bool {
		wc, ok := value.(*WsConnection)
		if !ok || !rotationConnectionMatches(wc, accountID, wsURL, groupKey) {
			return true
		}
		if !wc.IsConnected() {
			// A failed reader can race acquisition before the cleanup ticker.
			// Remove it now so a dead base key cannot block the first sibling.
			m.DiscardConnection(wc)
			return true
		}
		connections = append(connections, wc)
		return true
	})
	return connections
}

func rotationRouteRank(routes []string) map[string]int {
	ranks := make(map[string]int, len(routes))
	for i, route := range routes {
		if _, exists := ranks[route]; !exists {
			ranks[route] = i
		}
	}
	return ranks
}

func rotationRouteAllowed(route string, routes []string) bool {
	for _, candidate := range routes {
		if candidate == route {
			return true
		}
	}
	return false
}

// tryAcquireRotatingIdle probes and leases an existing active sibling. The
// caller owns the logical-group lock; key locks still protect concurrent legacy
// callers that may be operating on the same pool entry during rollout.
func (m *Manager) tryAcquireRotatingIdle(
	ctx context.Context,
	account *auth.Account,
	wsURL string,
	groupKey string,
	sessionKey string,
	routes []string,
) (*WsConnection, *PendingRequest, bool) {
	if account == nil {
		return nil, nil, false
	}
	rank := rotationRouteRank(routes)
	candidates := m.rotationGroupConnections(account.ID(), wsURL, groupKey)
	sort.SliceStable(candidates, func(i, j int) bool {
		ri, rj := rank[candidates[i].proxyURL], rank[candidates[j].proxyURL]
		if ri != rj {
			return ri < rj
		}
		// Prefer newer sockets: an older idle sibling is more likely to have
		// accumulated stale NAT/LB state and will be retired by cleanup soon.
		if candidates[i].createdAt != candidates[j].createdAt {
			return candidates[i].createdAt > candidates[j].createdAt
		}
		return candidates[i].lastUsed.Load() > candidates[j].lastUsed.Load()
	})

	for _, wc := range candidates {
		if ctx != nil {
			select {
			case <-ctx.Done():
				return nil, nil, false
			default:
			}
		}
		if wc.IsDraining() || wc.IsOverAge() {
			m.retireConnection(wc)
			continue
		}
		if !rotationRouteAllowed(wc.proxyURL, routes) || !canReuseConnection(wc) {
			continue
		}
		lock := m.keyLock(wc.PoolKey)
		lock.Lock()
		if current, ok := m.connections.Load(wc.PoolKey); !ok || current != wc || wc.IsDraining() || !canReuseConnection(wc) {
			lock.Unlock()
			continue
		}
		if !m.probe(wc) {
			m.DiscardConnection(wc)
			lock.Unlock()
			continue
		}
		accountLock := m.accountLock(account.ID())
		accountLock.Lock()
		current, exists := m.connections.Load(wc.PoolKey)
		if !exists || current != wc || wc.IsDraining() || !canReuseConnection(wc) {
			accountLock.Unlock()
			lock.Unlock()
			continue
		}
		pr, leaseErr := m.addPendingAndBeginReadLease(wc, sessionKey)
		if leaseErr == nil {
			wc.account = account
			wc.Touch()
			m.trimIdleAccountConnections(account.ID(), accountConnectionLimit(account), wc)
			accountLock.Unlock()
			lock.Unlock()
			return wc, pr, true
		}
		m.DiscardConnection(wc)
		accountLock.Unlock()
		lock.Unlock()
	}
	return nil, nil, false
}

func (m *Manager) rotationRouteUsage(accountID int64, wsURL string, groupPrefix string, routes []string) map[string]int {
	usage := make(map[string]int, len(routes))
	for _, route := range routes {
		usage[route] = 0
	}
	if m == nil {
		return usage
	}
	m.connections.Range(func(_, value any) bool {
		wc, ok := value.(*WsConnection)
		if !ok || wc == nil || !wc.IsConnected() || wc.session == nil || wc.session.AccountID != accountID || wc.URL != wsURL {
			return true
		}
		group := rotationConnectionGroup(wc)
		if groupPrefix != "" && group != groupPrefix && !strings.HasPrefix(group, groupPrefix+"#") {
			return true
		}
		if _, exists := usage[wc.proxyURL]; exists {
			usage[wc.proxyURL]++
		}
		return true
	})
	return usage
}

func rotationRouteReservationKey(accountID int64, wsURL string) string {
	return fmt.Sprintf("%d|%s", accountID, wsURL)
}

// reserveRotationRoute atomically reserves a route while a handshake is in
// flight. Existing routes may have any number of siblings; only a new distinct
// route consumes the account-wide route budget.
func (m *Manager) reserveRotationRoute(accountID int64, wsURL, route string) bool {
	if m == nil {
		return false
	}
	key := rotationRouteReservationKey(accountID, wsURL)
	m.rotationRouteMu.Lock()
	defer m.rotationRouteMu.Unlock()
	used := make(map[string]struct{})
	m.connections.Range(func(_, value any) bool {
		wc, ok := value.(*WsConnection)
		if !ok || wc == nil || !wc.IsConnected() || wc.session == nil || wc.session.AccountID != accountID || wc.URL != wsURL {
			return true
		}
		used[wc.proxyURL] = struct{}{}
		return true
	})
	pending := m.rotationPendingRoutes[key]
	if pending == nil {
		pending = make(map[string]int)
		if m.rotationPendingRoutes == nil {
			m.rotationPendingRoutes = make(map[string]map[string]int)
		}
		m.rotationPendingRoutes[key] = pending
	}
	if _, alreadyUsed := used[route]; !alreadyUsed && pending[route] == 0 {
		distinctPending := 0
		for candidate, count := range pending {
			if count > 0 {
				if _, exists := used[candidate]; !exists {
					distinctPending++
				}
			}
		}
		if len(used)+distinctPending >= wsMaxProxyRoutes() {
			return false
		}
	}
	pending[route]++
	return true
}

func (m *Manager) releaseRotationRoute(accountID int64, wsURL, route string) {
	if m == nil {
		return
	}
	key := rotationRouteReservationKey(accountID, wsURL)
	m.rotationRouteMu.Lock()
	defer m.rotationRouteMu.Unlock()
	pending := m.rotationPendingRoutes[key]
	if pending == nil {
		return
	}
	if count := pending[route]; count <= 1 {
		delete(pending, route)
	} else {
		pending[route] = count - 1
	}
	if len(pending) == 0 {
		delete(m.rotationPendingRoutes, key)
	}
}

func (m *Manager) limitRotationRoutesToAccount(accountID int64, wsURL string, routes []string) []string {
	if len(routes) <= 1 || m == nil {
		return routes
	}
	used := make(map[string]struct{}, len(routes))
	m.connections.Range(func(_, value any) bool {
		wc, ok := value.(*WsConnection)
		if !ok || wc == nil || !wc.IsConnected() || wc.session == nil || wc.session.AccountID != accountID || wc.URL != wsURL {
			return true
		}
		used[wc.proxyURL] = struct{}{}
		return true
	})
	if len(used) < wsMaxProxyRoutes() {
		return routes
	}
	filtered := make([]string, 0, len(routes))
	for _, route := range routes {
		if _, exists := used[route]; exists {
			filtered = append(filtered, route)
		}
	}
	if len(filtered) == 0 {
		// A route configuration can change while an existing healthy sibling is
		// still draining/idle. Keep that already-used route eligible rather than
		// turning a healthy connection into an immediate acquire outage.
		usedRoutes := make([]string, 0, len(used))
		for route := range used {
			usedRoutes = append(usedRoutes, route)
		}
		sort.Strings(usedRoutes)
		for _, route := range usedRoutes {
			filtered = append(filtered, route)
			if len(filtered) >= wsMaxProxyRoutes() {
				break
			}
		}
	}
	return filtered
}

func chooseRotationRoute(routes []string, usage map[string]int) string {
	if len(routes) == 0 {
		return ""
	}
	best := routes[0]
	bestCount := usage[best]
	for _, route := range routes[1:] {
		if count := usage[route]; count < bestCount {
			best, bestCount = route, count
		}
	}
	return best
}

func (m *Manager) nextRotationPoolSessionKey(base string) string {
	seq := m.siblingSequence.Add(1)
	return fmt.Sprintf("%s#rot-%d", base, seq)
}

// createRotationLease creates a new sibling connection while preserving the
// logical session ID sent to the upstream. poolSessionKey is unique, so an old
// draining generation can remain in the map until its last request completes.
func (m *Manager) createRotationLease(
	ctx context.Context,
	account *auth.Account,
	wsURL string,
	sessionID string,
	groupKey string,
	poolSessionKey string,
	headers http.Header,
	proxyURL string,
) (*WsConnection, *PendingRequest, error) {
	if account == nil {
		return nil, nil, fmt.Errorf("rotation websocket acquire: nil account")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	key := m.poolKey(account.ID(), wsURL, poolSessionKey, proxyURL)
	accountLock := m.accountLock(account.ID())
	accountLock.Lock()
	if _, exists := m.connections.Load(key); exists {
		accountLock.Unlock()
		return nil, nil, fmt.Errorf("rotation websocket sibling key already exists")
	}
	if !m.reserveRotationRoute(account.ID(), wsURL, proxyURL) {
		accountLock.Unlock()
		return nil, nil, fmt.Errorf("%w: account websocket proxy route limit reached", errRotationRouteLimit)
	}
	if !m.reserveAccountConnectionCapacity(account.ID(), accountConnectionLimit(account), key) {
		m.releaseRotationRoute(account.ID(), wsURL, proxyURL)
		accountLock.Unlock()
		return nil, nil, fmt.Errorf("%w: account websocket connection capacity exhausted", errRotationCapacityExhausted)
	}
	accountLock.Unlock()

	wc, err := m.createConnectionForRoute(ctx, account, wsURL, sessionID, poolSessionKey, groupKey, headers, proxyURL)
	if err != nil {
		m.releaseAccountConnectionCapacity(account.ID())
		m.releaseRotationRoute(account.ID(), wsURL, proxyURL)
		return nil, nil, err
	}
	if actual, loaded := m.connections.LoadOrStore(key, wc); loaded {
		m.releaseAccountConnectionCapacity(account.ID())
		m.releaseRotationRoute(account.ID(), wsURL, proxyURL)
		m.DiscardConnection(wc)
		_ = actual
		return nil, nil, fmt.Errorf("rotation websocket sibling key raced with another connection")
	}
	if m.afterConnectionStored != nil {
		m.afterConnectionStored(wc)
	}
	pr, leaseErr := m.addPendingAndBeginReadLease(wc, sessionID)
	if leaseErr == nil {
		if earlyErr := wc.waitForEarlyReadFailure(ctx, newConnectionReadFailureGrace); earlyErr != nil {
			wc.session.RemovePendingRequest(pr.RequestID)
			leaseErr = earlyErr
		}
	}
	m.releaseAccountConnectionCapacity(account.ID())
	m.releaseRotationRoute(account.ID(), wsURL, proxyURL)
	if leaseErr != nil {
		m.DiscardConnection(wc)
		return nil, nil, leaseErr
	}
	if fn := m.getOnConnected(); fn != nil {
		fn(account.ID(), wc.session)
	}
	return wc, pr, nil
}

func (m *Manager) acquireConnectionWithRotation(
	ctx context.Context,
	account *auth.Account,
	wsURL string,
	sessionKey string,
	headers http.Header,
	proxyOverride string,
) (*WsConnection, *PendingRequest, error) {
	if account == nil {
		return nil, nil, fmt.Errorf("rotation websocket acquire: nil account")
	}
	groupKey := strings.TrimSpace(sessionKey)
	routes := m.proxyCandidates(account, proxyOverride)
	routes = m.limitRotationRoutesToAccount(account.ID(), wsURL, routes)
	if len(routes) == 0 {
		return nil, nil, fmt.Errorf("%w: account websocket proxy route limit reached", errRotationRouteLimit)
	}
	lock := m.keyLock(rotationGroupLockKey(account.ID(), wsURL, groupKey))
	lock.Lock()
	defer lock.Unlock()

	if wc, pr, ok := m.tryAcquireRotatingIdle(ctx, account, wsURL, groupKey, sessionKey, routes); ok {
		return wc, pr, nil
	}

	connections := m.rotationGroupConnections(account.ID(), wsURL, groupKey)
	for _, wc := range connections {
		if wc.IsConnected() && wc.IsOverAge() {
			m.retireConnection(wc)
		}
	}
	connections = m.rotationGroupConnections(account.ID(), wsURL, groupKey)
	if len(connections) >= wsMaxSiblingConnections() {
		return nil, nil, fmt.Errorf("websocket session %q has %d busy/draining siblings (limit %d)", sessionKey, len(connections), wsMaxSiblingConnections())
	}

	usage := make(map[string]int, len(routes))
	for _, route := range routes {
		usage[route] = 0
	}
	for _, wc := range connections {
		if _, exists := usage[wc.proxyURL]; exists {
			usage[wc.proxyURL]++
		}
	}
	route := chooseRotationRoute(routes, usage)
	poolSessionKey := strings.TrimSpace(sessionKey)
	if len(connections) > 0 {
		poolSessionKey = m.nextRotationPoolSessionKey(poolSessionKey)
	}
	wc, pr, err := m.createRotationLease(ctx, account, wsURL, sessionKey, groupKey, poolSessionKey, headers, route)
	if err != nil {
		return nil, nil, err
	}
	return wc, pr, nil
}

func (m *Manager) acquireReusableConnectionWithRotation(
	ctx context.Context,
	account *auth.Account,
	wsURL string,
	baseKey string,
	fallbackKey string,
	slots int,
	headers http.Header,
	proxyOverride string,
) (*WsConnection, *PendingRequest, string, error) {
	if account == nil {
		return nil, nil, "", fmt.Errorf("rotation websocket acquire: nil account")
	}
	if slots < 1 {
		slots = 1
	}
	if accountLimit := accountConnectionLimit(account); slots > accountLimit {
		slots = accountLimit
	}
	routes := m.proxyCandidates(account, proxyOverride)
	routes = m.limitRotationRoutesToAccount(account.ID(), wsURL, routes)
	if len(routes) == 0 {
		return nil, nil, "", fmt.Errorf("%w: account websocket proxy route limit reached", errRotationRouteLimit)
	}

	// First pass: inspect every configured slot before creating anything. This
	// prevents a busy slot-0 from spawning a new generation while slot-1 already
	// has an idle sibling available.
	for i := 0; i < slots; i++ {
		slotKey := fmt.Sprintf("%s#%d", baseKey, i)
		groupLock := m.keyLock(rotationGroupLockKey(account.ID(), wsURL, slotKey))
		groupLock.Lock()
		wc, pr, ok := m.tryAcquireRotatingIdle(ctx, account, wsURL, slotKey, slotKey, routes)
		groupLock.Unlock()
		if ok {
			return wc, pr, slotKey, nil
		}
	}

	// Second pass: no idle slot exists, so allocate a rotated sibling on the
	// least-used slot/route. A bounded sibling cap avoids recreating the old
	// per-request one-shot fallback under sustained load.
	for i := 0; i < slots; i++ {
		slotKey := fmt.Sprintf("%s#%d", baseKey, i)
		groupLock := m.keyLock(rotationGroupLockKey(account.ID(), wsURL, slotKey))
		groupLock.Lock()
		connections := m.rotationGroupConnections(account.ID(), wsURL, slotKey)
		for _, wc := range connections {
			if wc.IsConnected() && wc.IsOverAge() {
				m.retireConnection(wc)
			}
		}
		connections = m.rotationGroupConnections(account.ID(), wsURL, slotKey)
		if len(connections) >= wsMaxSiblingConnections() {
			groupLock.Unlock()
			continue
		}
		usage := m.rotationRouteUsage(account.ID(), wsURL, baseKey, routes)
		route := chooseRotationRoute(routes, usage)
		poolSessionKey := slotKey
		if len(connections) > 0 {
			poolSessionKey = m.nextRotationPoolSessionKey(slotKey)
		}
		wc, pr, err := m.createRotationLease(ctx, account, wsURL, slotKey, slotKey, poolSessionKey, headers, route)
		groupLock.Unlock()
		if err != nil {
			// Capacity or route reservations can be consumed by a sibling slot
			// between the two passes; keep scanning before returning an error.
			if errors.Is(err, errRotationCapacityExhausted) || errors.Is(err, errRotationRouteLimit) {
				continue
			}
			return nil, nil, "", err
		}
		return wc, pr, poolSessionKey, nil
	}

	// Rotation mode deliberately avoids per-request one-shot fallback. A
	// one-shot connection defeats the sibling pool and increases handshake
	// churn; callers can still retry another account when the account is at
	// physical capacity.
	return nil, nil, "", fmt.Errorf("all websocket sibling slots for %q are busy (fallback %q disabled in rotation mode)", baseKey, fallbackKey)
}
