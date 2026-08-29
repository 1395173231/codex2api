package wsrelay

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
)

var (
	errRotationCapacityExhausted = errors.New("rotation websocket capacity exhausted")
	errRotationRouteLimit        = errors.New("rotation websocket proxy route limit reached")
)

// Rotation mode intentionally remains opt-in for one release. The legacy busy
// path is kept as a rollback switch while operators measure sibling connection
// pressure and upstream continuation behavior. Set CODEX_WS_CONNECTION_MODE=time
// (or rotation/drain) to enable the new drain-and-sibling scheduler.
func wsRotationModeEnabled() bool {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv("CODEX_WS_CONNECTION_MODE")))
	switch raw {
	case "time", "rotation", "drain", "on", "true", "1":
		return true
	case "busy", "legacy", "off", "false", "0":
		return false
	default:
		return false
	}
}

// connectionRotationAge is the point at which a live connection stops taking
// new leases. It is clamped to the existing hard lifetime so an operator cannot
// accidentally rotate after the upstream's known connection-age limit.
func connectionRotationAge() time.Duration {
	hardMax := connectionMaxLifetime()
	raw := strings.TrimSpace(os.Getenv("CODEX_WS_ROTATION_MAX_AGE"))
	if raw == "" {
		return hardMax
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return hardMax
	}
	if value > hardMax {
		return hardMax
	}
	return value
}

// wsMaxProxyRoutes bounds how many distinct effective proxy routes one logical
// connection group may use. Two is the conservative default; three is allowed
// for deployments with enough independent egress capacity.
func wsMaxProxyRoutes() int {
	return boundedRotationInt("CODEX_WS_MAX_PROXY_ROUTES", 2, 1, 3)
}

// wsMaxSiblingConnections bounds the number of live generations in one logical
// session/slot group. It is independent from the account's request concurrency
// cap: the latter still remains the hard physical connection limit.
func wsMaxSiblingConnections() int {
	return boundedRotationInt("CODEX_WS_MAX_SIBLINGS", 3, 1, 16)
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
