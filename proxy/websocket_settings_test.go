package proxy

import (
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
)

func TestShouldUseWebsocketHonorsRuntimeForceWithoutStaticConfig(t *testing.T) {
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })

	settings := DefaultRuntimeSettings()
	settings.CodexForceWebsocket = true
	ApplyRuntimeSettings(settings)

	handler := NewHandler(nil, nil, nil, nil)
	if !handler.shouldUseWebsocketForHTTP() {
		t.Fatal("shouldUseWebsocketForHTTP() = false, want true when runtime force websocket is enabled")
	}
}

func TestShouldUseWebsocketHonorsStoreForceBeforeHTTPConfig(t *testing.T) {
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	ApplyRuntimeSettings(DefaultRuntimeSettings())

	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:          2,
		TestConcurrency:         1,
		TestModel:               "gpt-5.4",
		CodexForceWebsocket:     true,
		CodexWSSilentMaxRetries: 2,
	})
	handler := NewHandler(store, nil, &config.Config{CodexUpstreamTransport: "http"}, nil)

	if !handler.shouldUseWebsocketForHTTP() {
		t.Fatal("shouldUseWebsocketForHTTP() = false, want true when store force websocket is enabled")
	}
}

func TestShouldUseWebsocketForRemoteCompactionDefaultsToHTTP(t *testing.T) {
	handler := NewHandler(nil, nil, &config.Config{}, nil)
	if handler.shouldUseWebsocketForRemoteCompaction(true) {
		t.Fatal("remote compaction inherited forced websocket without explicit inherit setting")
	}
}

func TestShouldUseWebsocketForRemoteCompactionHonorsExplicitPolicy(t *testing.T) {
	for _, tt := range []struct {
		name      string
		transport string
		inherited bool
		want      bool
	}{
		{name: "http overrides websocket", transport: "http", inherited: true, want: false},
		{name: "inherit websocket", transport: "inherit", inherited: true, want: true},
		{name: "inherit http", transport: "inherit", inherited: false, want: false},
		{name: "ws overrides http", transport: "ws", inherited: false, want: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			handler := NewHandler(nil, nil, &config.Config{CodexRemoteCompactionTransport: tt.transport}, nil)
			if got := handler.shouldUseWebsocketForRemoteCompaction(tt.inherited); got != tt.want {
				t.Fatalf("shouldUseWebsocketForRemoteCompaction(%v) = %v, want %v", tt.inherited, got, tt.want)
			}
		})
	}
}
