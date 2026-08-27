package cache

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRedisMessageAffinityIntegration(t *testing.T) {
	addr := strings.TrimSpace(os.Getenv("CODEX_TEST_REDIS_ADDR"))
	if addr == "" {
		t.Skip("set CODEX_TEST_REDIS_ADDR to run Redis integration coverage")
	}
	tokenCache, err := NewRedis(addr, "", 0, 2)
	if err != nil {
		t.Fatalf("NewRedis: %v", err)
	}
	defer tokenCache.Close()
	backend := tokenCache.(MessageAffinityCache)
	ctx := context.Background()
	scope := fmt.Sprintf("integration:%d", time.Now().UnixNano())
	const (
		firstAccount  = int64(9007199254740992)
		secondAccount = int64(9007199254740993)
		conflictHash  = uint64(0x1111)
		secondHash    = uint64(0x2222)
	)
	for i := 0; i < 3; i++ {
		if err := backend.RecordMessageAffinities(ctx, scope, []uint64{conflictHash}, firstAccount, time.Minute); err != nil {
			t.Fatalf("record first account %d: %v", i, err)
		}
	}
	if err := backend.RecordMessageAffinities(ctx, scope, []uint64{conflictHash, secondHash}, secondAccount, time.Minute); err != nil {
		t.Fatalf("record second account: %v", err)
	}
	bindings, err := backend.GetMessageAffinities(ctx, scope, []uint64{conflictHash, secondHash})
	if err != nil {
		t.Fatalf("GetMessageAffinities: %v", err)
	}
	if got := bindings[conflictHash]; got.AccountID != firstAccount || got.Count != 2 || got.Changes != 1 {
		t.Fatalf("conflict binding = %+v, want lossless first account with one conflict", got)
	}
	if got := bindings[secondHash]; got.AccountID != secondAccount || got.Count != 1 {
		t.Fatalf("second binding = %+v, want lossless second account", got)
	}
}
