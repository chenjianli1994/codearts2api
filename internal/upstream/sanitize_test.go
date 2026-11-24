package upstream

import (
	"strings"
	"testing"
)

const hdrSample = "hello\nx-anthropic-billing-hdr: abc123; keep"

func TestSanitizeTextStripsHeaderSegment(t *testing.T) {
	got := SanitizeText(hdrSample)
	if strings.Contains(got, "abc123") {
		t.Fatalf("header segment kept: %q", got)
	}
	if !strings.Contains(got, "keep") {
		t.Fatalf("unrelated text lost: %q", got)
	}
}

func TestSanitizeTextStripsBareKeyName(t *testing.T) {
	got := SanitizeText("echo x-anthropic-billing-hdr done")
	if strings.Contains(strings.ToLower(got), "x-anthropic-billing-hdr") {
		t.Fatalf("bare key name kept: %q", got)
	}
	if !strings.Contains(got, "done") {
		t.Fatalf("unrelated text lost: %q", got)
	}
}

func TestSanitizeTextKeepsCleanText(t *testing.T) {
	in := "just a normal user message"
	if got := SanitizeText(in); got != in {
		t.Fatalf("clean text changed: %q", got)
	}
}

func TestSanitizeOutboundMessagesCoversAllFields(t *testing.T) {
	args := "{\"note\":\"x-anthropic-billing-hdr: zz\"}"
	msgs := []map[string]any{
		{"role": "system", "content": "x-anthropic-billing-hdr: zzz; hi"},
		{"role": "assistant", "content": nil, "reasoning_content": "x-anthropic-billing-hdr: r"},
		{"role": "assistant", "content": nil, "tool_calls": []any{
			map[string]any{"function": map[string]any{"arguments": args}},
		}},
		{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "x-anthropic-billing-hdr: v"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:x"}},
		}},
	}
	if !SanitizeOutboundMessages(msgs) {
		t.Fatal("expected changes")
	}
	if s, _ := msgs[0]["content"].(string); strings.Contains(s, "zzz") {
		t.Fatalf("system content not sanitized: %q", s)
	}
	if s, _ := msgs[1]["reasoning_content"].(string); strings.Contains(s, ": r") {
		t.Fatalf("reasoning not sanitized: %q", s)
	}
	calls := msgs[2]["tool_calls"].([]any)
	fn := calls[0].(map[string]any)["function"].(map[string]any)
	if s, _ := fn["arguments"].(string); strings.Contains(s, "zz") {
		t.Fatalf("tool arguments not sanitized: %q", s)
	}
	parts := msgs[3]["content"].([]any)
	text := parts[0].(map[string]any)["text"].(string)
	if strings.Contains(text, ": v") {
		t.Fatalf("multimodal text part not sanitized: %q", text)
	}
	if _, ok := parts[1].(map[string]any)["image_url"]; !ok {
		t.Fatal("image part must stay untouched")
	}
}

func TestSanitizeOutboundMessagesNoopOnClean(t *testing.T) {
	msgs := []map[string]any{{"role": "user", "content": "plain"}}
	if SanitizeOutboundMessages(msgs) {
		t.Fatal("clean messages reported as changed")
	}
}

func TestSwitchesDefaultOnAndTogglable(t *testing.T) {
	if !SanitizeOutboundEnabled() {
		t.Fatal("sanitize should default to on")
	}
	SetSanitizeOutbound(false)
	if SanitizeOutboundEnabled() {
		t.Fatal("setter not applied")
	}
	SetSanitizeOutbound(true)

	if !ReasoningAsContentEnabled() {
		t.Fatal("reasoning backfill should default to on")
	}
	SetReasoningAsContent(false)
	if ReasoningAsContentEnabled() {
		t.Fatal("setter not applied")
	}
	SetReasoningAsContent(true)
}
