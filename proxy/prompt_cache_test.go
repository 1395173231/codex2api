package proxy

import (
	"os"
	"testing"

	"github.com/tidwall/gjson"
)

func TestApplyPromptCacheRetention_Default24h(t *testing.T) {
	// 确保环境变量未设置，走代码默认 24h
	t.Setenv("CODEX_PROMPT_CACHE_RETENTION", "x") // 占位以触发 t 的 env 还原机制
	os.Unsetenv("CODEX_PROMPT_CACHE_RETENTION")
	body := []byte(`{"model":"gpt-5.5"}`)
	got := ApplyPromptCacheRetention(body)
	if v := gjson.GetBytes(got, "prompt_cache_retention").String(); v != "24h" {
		t.Fatalf("prompt_cache_retention = %q, want 24h", v)
	}
}

func TestApplyPromptCacheRetention_PreservesClientValue(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","prompt_cache_retention":"in_memory"}`)
	got := ApplyPromptCacheRetention(body)
	if v := gjson.GetBytes(got, "prompt_cache_retention").String(); v != "in_memory" {
		t.Fatalf("prompt_cache_retention = %q, want in_memory (client value preserved)", v)
	}
}

func TestApplyPromptCacheRetention_EnvOverride(t *testing.T) {
	t.Setenv("CODEX_PROMPT_CACHE_RETENTION", "1h")
	body := []byte(`{"model":"gpt-5.5"}`)
	got := ApplyPromptCacheRetention(body)
	if v := gjson.GetBytes(got, "prompt_cache_retention").String(); v != "1h" {
		t.Fatalf("prompt_cache_retention = %q, want 1h (env override)", v)
	}
}

func TestApplyPromptCacheRetention_EmptyEnvStripsField(t *testing.T) {
	// 显式置空 → 恢复旧行为: 删除该字段
	t.Setenv("CODEX_PROMPT_CACHE_RETENTION", "")
	body := []byte(`{"model":"gpt-5.5","prompt_cache_retention":"24h"}`)
	got := ApplyPromptCacheRetention(body)
	if gjson.GetBytes(got, "prompt_cache_retention").Exists() {
		t.Fatalf("prompt_cache_retention should be stripped when env set empty, got %q",
			gjson.GetBytes(got, "prompt_cache_retention").String())
	}
}
