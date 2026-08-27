package proxy

import (
	"fmt"
	"strings"
	"testing"
)

func TestNormalizeMessageAffinityText(t *testing.T) {
	left := normalizeMessageAffinityText("  HELLO\tworld\nthis is a stable prompt  ")
	right := normalizeMessageAffinityText("hello world this is a stable prompt")
	if left == "" || left != right {
		t.Fatalf("normalized texts differ: left=%q right=%q", left, right)
	}
	if got := normalizeMessageAffinityText("too short"); got != "" {
		t.Fatalf("short text normalized to %q, want empty", got)
	}
	tooLong := strings.Repeat("a", maxMessageAffinityBytes+64)
	if got := normalizeMessageAffinityText(tooLong); len(got) != maxMessageAffinityBytes {
		t.Fatalf("normalized length = %d, want %d", len(got), maxMessageAffinityBytes)
	}
}

func TestDeriveMessageAffinityHashesMessageShapes(t *testing.T) {
	chat := []byte(`{
		"messages": [
			{"role":"system","content":"this system instruction is deliberately long and must be ignored"},
			{"role":"user","content":"The first sufficiently long user message belongs to this conversation."},
			{"role":"assistant","content":"The first sufficiently long user message belongs to this conversation."},
			{"role":"user","content":[{"type":"text","text":"A second sufficiently long user message is represented as an array."}]},
			{"role":"user","content":"  the FIRST sufficiently long USER message belongs to this conversation.  "}
		]
	}`)
	responses := []byte(`{
		"input": [
			{"type":"message","role":"user","content":[{"type":"input_text","text":"The first sufficiently long user message belongs to this conversation."}]},
			{"type":"function_call_output","text":"This long tool output must never participate in affinity voting."},
			{"type":"input_text","text":"A second sufficiently long user message is represented as an array."}
		]
	}`)
	anthropic := []byte(`{
		"messages": [
			{"role":"user","content":[{"type":"text","text":"The first sufficiently long user message belongs to this conversation."}]},
			{"role":"assistant","content":[{"type":"text","text":"This assistant message is long but must be ignored by affinity."}]},
			{"role":"user","content":[{"type":"text","text":"A second sufficiently long user message is represented as an array."}]}
		]
	}`)

	want := deriveMessageAffinityHashes(chat)
	if len(want) != 2 {
		t.Fatalf("chat hashes = %v, want 2 unique user hashes", want)
	}
	for name, body := range map[string][]byte{"responses": responses, "anthropic": anthropic} {
		t.Run(name, func(t *testing.T) {
			got := deriveMessageAffinityHashes(body)
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("hashes = %v, want %v", got, want)
			}
		})
	}
}

func TestDeriveMessageAffinityHashesCapsDistinctMessages(t *testing.T) {
	var body strings.Builder
	body.WriteString(`{"messages":[`)
	for i := 0; i < maxMessageAffinityHashes+10; i++ {
		if i > 0 {
			body.WriteByte(',')
		}
		fmt.Fprintf(&body, `{"role":"user","content":"message number %03d has enough stable alphanumeric content"}`, i)
	}
	body.WriteString(`]}`)
	if got := len(deriveMessageAffinityHashes([]byte(body.String()))); got != maxMessageAffinityHashes {
		t.Fatalf("hash count = %d, want cap %d", got, maxMessageAffinityHashes)
	}
}

func TestResolveRequestSessionIdentityCarriesMessageHashes(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","input":"This request contains a sufficiently long user message for affinity."}`)
	identity := resolveRequestSessionIdentity(nil, body)
	if len(identity.messageHashes) != 1 {
		t.Fatalf("message hash count = %d, want 1", len(identity.messageHashes))
	}
}
