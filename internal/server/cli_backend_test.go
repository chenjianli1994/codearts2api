package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/cli"
	"codearts2api/internal/pool"
)

// fakeServe 模拟 codearts serve 的两个必要接口：建会话与发消息。
func fakeServe(t *testing.T, reply string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/session":
			io.WriteString(w, `{"id":"sess-1"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/session/sess-1/message":
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), `"modelID":"cli-model-1"`) {
				t.Errorf("message body missing cli model id: %s", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"info":  map[string]any{"modelID": "cli-model-1", "providerID": "huaweicloud-maas"},
				"parts": []map[string]any{{"type": "text", "text": reply}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
}

func cliHandler(t *testing.T, serveURL, reply string) *Handler {
	t.Helper()
	a := auth.New("cli-account", "tester", "domain", "token", "ak", "sk",
		time.Now().Add(time.Hour).Format(time.RFC3339), "", "")
	p, err := pool.New([]*auth.Auth{a}, pool.Config{MaxConcurrent: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	return NewHandler(Config{
		Pool:         p,
		DefaultModel: "glm-5.2",
		MaxRotate:    1,
		CLI:          cli.New(serveURL, 5*time.Second),
		CLIProvider:  "huaweicloud-maas",
		CLIAgent:     "build",
		CLIModels:    map[string]string{"test-cli-model": "cli-model-1"},
		CLIEnabled:   true,
	})
}

// CLI 后端：命中 mapping 的模型走 codearts serve，直连路径完全不参与。
func TestChatCompletionsRoutesCLIModel(t *testing.T) {
	up := fakeServe(t, "hello from cli")
	defer up.Close()
	h := cliHandler(t, up.URL, "hello from cli")

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"test-cli-model","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
				Role    string `json:"role"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Model != "test-cli-model" {
		t.Fatalf("model=%q", body.Model)
	}
	if len(body.Choices) != 1 || body.Choices[0].Message.Content != "hello from cli" {
		t.Fatalf("unexpected choices: %s", rec.Body.String())
	}
	if body.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish_reason=%q", body.Choices[0].FinishReason)
	}
	if rec.Header().Get("X-Codearts-Chat-Id") == "" {
		t.Fatal("missing chat id header")
	}
}

// 流式：CLI 是纯文本内核，按 OpenAI SSE 逐块吐出并以 [DONE] 收尾。
func TestChatCompletionsCLIModelStream(t *testing.T) {
	up := fakeServe(t, "streamed text")
	defer up.Close()
	h := cliHandler(t, up.URL, "streamed text")

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"test-cli-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	if !strings.Contains(out, "streamed text") {
		t.Fatalf("stream missing text: %s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("stream missing DONE: %s", out)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type=%q", ct)
	}
}

// 未配置 CLI（CLI 为 nil）时，即便模型名在 mapping 里也必须回落直连，
// 不能把请求丢进空指针。
func TestCLIModelFallsBackWhenBackendMissing(t *testing.T) {
	h := NewHandler(Config{
		DefaultModel: "glm-5.2",
		MaxRotate:    1,
		CLIModels:    map[string]string{"test-cli-model": "cli-model-1"},
		CLIEnabled:   true,
	})
	if _, ok := h.cliModelFor("test-cli-model"); ok {
		t.Fatal("cliModelFor must return false when CLI client is nil")
	}
	if !h.isCLIModelName("TEST-CLI-MODEL") {
		t.Fatal("isCLIModelName must be case-insensitive and independent of readiness")
	}
}

// /v1/models 要把 CLI 独有模型补进列表，并标成 owned_by=codearts-cli。
func TestCLIModelExtrasAppended(t *testing.T) {
	h := NewHandler(Config{
		DefaultModel: "glm-5.2",
		CLIModels:    map[string]string{"only-in-cli": "m1", "GLM-5.2": "m2"},
	})
	entries := []map[string]any{{"id": "GLM-5.2"}, {"id": "glm-5.3-flash"}}
	got := h.cliModelExtras(entries)
	if len(got) != 1 {
		t.Fatalf("want 1 extra, got %v", got)
	}
	if got[0]["id"] != "only-in-cli" || got[0]["owned_by"] != "codearts-cli" {
		t.Fatalf("unexpected extra entry: %v", got[0])
	}
}
