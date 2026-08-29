package wsrelay

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/proxy"
)

var (
	errRotationCapacityExhausted = errors.New("rotation websocket capacity exhausted")
	errRotationRouteLimit        = errors.New("rotation websocket proxy route limit reached")
)

// Rotation mode intentionally remains opt-in for one release. The legacy busy
// path is kept as a rollback switch while operators measure sibling connection
// pressure and upstream continuation behavior. Set CODEX_WS_CONNECTION_MODE=time
// (or rotation/drain) to enable the new drain-and-sibling scheduler; when the
// environment variable is absent, the admin-panel runtime setting controls it.
func wsRotationModeEnabled() bool {
	if raw := strings.ToLower(strings.TrimSpace(os.Getenv("CODEX_WS_CONNECTION_MODE"))); raw != "" {
		switch raw {
		case "time", "rotation", "drain", "on", "true", "1":
			return true
		case "busy", "legacy", "off", "false", "0":
			return false
		default:
			return false
		}
	}
	return proxy.CurrentRuntimeSettings().CodexWSRotationEnabled
}

// connectionRotationAge is the point at which a live connection stops taking
// new leases. An explicit environment value wins; otherwise the hot-reloaded
// admin-panel value is used. It is clamped to the existing hard lifetime so an
// operator cannot accidentally rotate after the upstream's known age limit.
func connectionRotationAge() time.Duration {
	hardMax := connectionMaxLifetime()
	// Keep the existing hard lifetime guard in legacy busy mode, but do not let
	// the panel's configurable retirement age silently change legacy reuse.
	if !wsRotationModeEnabled() {
		return hardMax
	}
	if raw := strings.TrimSpace(os.Getenv("CODEX_WS_ROTATION_MAX_AGE")); raw != "" {
		value, err := time.ParseDuration(raw)
		if err != nil || value <= 0 {
			return hardMax
		}
		if value > hardMax {
			return hardMax
		}
		return value
	}
	if seconds := proxy.CurrentRuntimeSettings().CodexWSRotationMaxAgeSec; seconds > 0 {
		value := time.Duration(seconds) * time.Second
		if value > hardMax {
			return hardMax
		}
		return value
	}
	return hardMax
}

// wsMaxProxyRoutes bounds how many distinct effective proxy routes one logical
// connection group may use. Two is the conservative default; three is allowed
// for deployments with enough independent egress capacity.
func wsMaxProxyRoutes() int {
	return boundedRotationInt("CODEX_WS_MAX_PROXY_ROUTES", proxy.CurrentRuntimeSettings().CodexWSMaxProxyRoutes, 1, 3)
}

// wsMaxSiblingConnections bounds the number of live generations in one logical
// session/slot group. It is independent from the account's request concurrency
// cap: the latter still remains the hard physical connection limit.
func wsMaxSiblingConnections() int {
	return boundedRotationInt("CODEX_WS_MAX_SIBLINGS", proxy.CurrentRuntimeSettings().CodexWSMaxSiblings, 1, 16)
}

func boundedRotationInt(name string, fallback, min, max int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}
