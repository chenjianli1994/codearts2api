package upstream

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// 只有推理、没有正文的一段上游流：用于验证「思考内容兜底为正文」两条路径。
const reasoningOnlyStream = "event: onreasoning\ndata: {\"text\":\"thinking-text\"}\n\nevent: done\ndata: {\"error_code\":\"0\"}\n\n"

func TestAggregateBackfillsContentFromReasoning(t *testing.T) {
	SetReasoningAsContent(true)
	out, err := Aggregate(strings.NewReader(reasoningOnlyStream), "glm-5.2")
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	choices := out["choices"].([]any)
	message := choices[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != "thinking-text" {
		t.Fatalf("content should fall back to reasoning: %v", message["content"])
	}
	if message["reasoning_content"] != "thinking-text" {
		t.Fatalf("reasoning_content should stay: %v", message["reasoning_content"])
	}
}

func TestAggregateKeepsEmptyContentWhenSwitchOff(t *testing.T) {
	SetReasoningAsContent(false)
	defer SetReasoningAsContent(true)
	out, err := Aggregate(strings.NewReader(reasoningOnlyStream), "glm-5.2")
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	message := out["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != "" {
		t.Fatalf("content should stay empty: %v", message["content"])
	}
	if message["reasoning_content"] != "thinking-text" {
		t.Fatalf("reasoning_content should stay: %v", message["reasoning_content"])
	}
}

func TestStreamEmitsReasoningAsContentBeforeDone(t *testing.T) {
	SetReasoningAsContent(true)
	rec := httptest.NewRecorder()
	if err := StreamCaptureWithUsage(rec, strings.NewReader(reasoningOnlyStream), "glm-5.2", nil, nil); err != nil {
		t.Fatalf("stream: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "thinking-text") {
		t.Fatalf("stream lost reasoning text: %q", body)
	}
	if strings.Count(body, "\"content\":\"thinking-text\"") != 1 {
		t.Fatalf("expected exactly one content backfill frame: %q", body)
	}
	idxContent := strings.Index(body, "\"content\":\"thinking-text\"")
	idxDone := strings.Index(body, "[DONE]")
	if idxContent < 0 || idxDone < 0 || idxContent > idxDone {
		t.Fatalf("backfill must come before DONE: content=%d done=%d", idxContent, idxDone)
	}
}

func TestStreamSkipsBackfillWhenSwitchOff(t *testing.T) {
	SetReasoningAsContent(false)
	defer SetReasoningAsContent(true)
	rec := httptest.NewRecorder()
	if err := StreamCaptureWithUsage(rec, strings.NewReader(reasoningOnlyStream), "glm-5.2", nil, nil); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if strings.Contains(rec.Body.String(), "\"content\":") {
		t.Fatalf("switch off but content frame emitted: %q", rec.Body.String())
	}
}
