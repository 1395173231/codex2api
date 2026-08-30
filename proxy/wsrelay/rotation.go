package wsrelay

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
)

var (
	errRotationCapacityExhausted = errors.New("rotation websocket capacity exhausted")
	errRotationRouteLimit        = errors.New("rotation websocket proxy route limit reached")
)

// Persisted admin-panel settings are authoritative. Environment variables remain
// a first-run/embedding fallback so existing deployments can seed a new database
// without making later page edits appear to succeed while a stale env value wins.
func wsRotationModeEnabled() bool {
	settings := proxy.CurrentRuntimeSettings()
	if settings.CodexWSRotationSettingsAuthoritative {
		return settings.CodexWSRotationEnabled
	}
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
	return settings.CodexWSRotationEnabled
}

// connectionRotationAge is the point at which a live connection stops taking
// new leases. The hot-reloaded admin-panel value wins after settings have been
// persisted; the environment is only a first-run fallback. The result is clamped
// to the existing hard lifetime.
func connectionRotationAge() time.Duration {
	hardMax := connectionMaxLifetime()
	// Keep the existing hard lifetime guard in legacy busy mode, but do not let
	// the panel's configurable retirement age silently change legacy reuse.
	if !wsRotationModeEnabled() {
		return hardMax
	}
	settings := proxy.CurrentRuntimeSettings()
	if !settings.CodexWSRotationSettingsAuthoritative {
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
	}
	if seconds := settings.CodexWSRotationMaxAgeSec; seconds > 0 {
		value := time.Duration(seconds) * time.Second
		if value > hardMax {
			return hardMax
		}
		return value
	}
	return hardMax
}

// ApplyRotationEnvironmentDefaults seeds a brand-new SystemSettings row from
// the legacy rotation environment variables. Callers must not use it for an
// existing row: persisted/admin values intentionally have higher priority.
func ApplyRotationEnvironmentDefaults(settings *database.SystemSettings) {
	if settings == nil {
		return
	}
	if raw := strings.ToLower(strings.TrimSpace(os.Getenv("CODEX_WS_CONNECTION_MODE"))); raw != "" {
		switch raw {
		case "time", "rotation", "drain", "on", "true", "1":
			settings.CodexWSRotationEnabled = true
		case "busy", "legacy", "off", "false", "0":
			settings.CodexWSRotationEnabled = false
		}
	}
	if raw := strings.TrimSpace(os.Getenv("CODEX_WS_ROTATION_MAX_AGE")); raw != "" {
		value, err := time.ParseDuration(raw)
		if err == nil && value > 0 {
			if hardMax := connectionMaxLifetime(); value > hardMax {
				value = hardMax
			}
			seconds := int(value / time.Second)
			if seconds < 1 {
				seconds = 1
			}
			settings.CodexWSRotationMaxAgeSec = database.NormalizeCodexWSRotationMaxAgeSec(seconds)
		}
	}
	if raw := strings.TrimSpace(os.Getenv("CODEX_WS_MAX_SIBLINGS")); raw != "" {
		if value, err := strconv.Atoi(raw); err == nil {
			settings.CodexWSMaxSiblings = database.NormalizeCodexWSMaxSiblings(value)
		}
	}
	if raw := strings.TrimSpace(os.Getenv("CODEX_WS_MAX_PROXY_ROUTES")); raw != "" {
		if value, err := strconv.Atoi(raw); err == nil {
			settings.CodexWSMaxProxyRoutes = database.NormalizeCodexWSMaxProxyRoutes(value)
		}
	}
}

// wsMaxProxyRoutes bounds how many distinct effective proxy routes one logical
// connection group may use. Two is the conservative default; three is allowed
// for deployments with enough independent egress capacity.
func wsMaxProxyRoutes() int {
	settings := proxy.CurrentRuntimeSettings()
	if settings.CodexWSRotationSettingsAuthoritative {
		return settings.CodexWSMaxProxyRoutes
	}
	return boundedRotationInt("CODEX_WS_MAX_PROXY_ROUTES", settings.CodexWSMaxProxyRoutes, 1, 3)
}

// wsMaxSiblingConnections is the number of idle/live siblings retained in one
// logical session/slot group after requests finish. Active demand may
// temporarily exceed this soft pool target; the account's dynamic concurrency
// limit remains the hard physical connection limit and lets the outer scheduler
// switch accounts when no connection capacity is available.
func wsMaxSiblingConnections() int {
	settings := proxy.CurrentRuntimeSettings()
	if settings.CodexWSRotationSettingsAuthoritative {
		return settings.CodexWSMaxSiblings
	}
	return boundedRotationInt("CODEX_WS_MAX_SIBLINGS", settings.CodexWSMaxSiblings, 1, 16)
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
