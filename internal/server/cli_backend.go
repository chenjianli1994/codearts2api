package server

// CLI 后端（codearts serve）
//
// 官方对 snap-access 裸 chat 调用加了传输层加密，直调已用模型可能拿到
// TM.000010051「您当前无权调用该模型」；官方 CLI（codearts serve）内部走加密
// 通道，能调用全部已开通模型。因此把 config.json 里 cli_backend.models 列出的
// 模型转给 CLI，其余模型继续走原来的直连路径。
//
// 代价（移植自 fkfbnv/codearts2api-hardened 的实测结论）：CLI 是 agent 内核，
// 每次请求都带约 1.4 万 token 的 system prompt，费用更高；且只产出纯文本，
// 不返回 OpenAI tool_calls。所以本后端默认关闭，需要时再开。

import (
	"context"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"codearts2api/internal/cli"
)

// isCLIModelName 只按配置判断模型是否属于 CLI 后端（与 CLI 客户端是否就绪无关）。
func (h *Handler) isCLIModelName(model string) bool {
	key := strings.ToLower(strings.TrimSpace(model))
	if key == "" {
		key = strings.ToLower(h.cfg.DefaultModel)
	}
	_, ok := h.cfg.CLIModels[key]
	return ok
}

// cliModelFor 判断该模型是否应由 CLI 后端处理，并返回 CLI 侧 modelID。
func (h *Handler) cliModelFor(model string) (string, bool) {
	if h.cfg.CLI == nil || len(h.cfg.CLIModels) == 0 {
		return "", false
	}
	key := strings.ToLower(strings.TrimSpace(model))
	if key == "" {
		key = strings.ToLower(h.cfg.DefaultModel)
	}
	id, ok := h.cfg.CLIModels[key]
	if !ok {
		return "", false
	}
	return id, true
}

// cliModelExtras 返回 CLI 后端独有、直连目录里没有的模型条目。
func (h *Handler) cliModelExtras(entries []map[string]any) []map[string]any {
	if len(h.cfg.CLIModels) == 0 {
		return nil
	}
	have := make(map[string]bool, len(entries))
	for _, e := range entries {
		if id, ok := e["id"].(string); ok {
			have[strings.ToLower(id)] = true
		}
	}
	extra := make([]string, 0, len(h.cfg.CLIModels))
	for id := range h.cfg.CLIModels {
		if !have[id] {
			extra = append(extra, id)
		}
	}
	if len(extra) == 0 {
		return nil
	}
	sort.Strings(extra)
	out := make([]map[string]any, 0, len(extra))
	for _, id := range extra {
		out = append(out, map[string]any{
			"id": id, "object": "model", "owned_by": "codearts-cli",
		})
	}
	return out
}

// chatViaCLI 把 OpenAI 请求转成 codearts serve 的 session/message 调用。
//
// 说明：CLI 是 agent 内核，回复是纯文本（不返回 tool_calls）。带工具时按官方对
// DSML 模型的做法，把工具 schema 注入 prompt、要求输出 ```tool_call 代码块，
// 再由 toolResponse 解析成标准 tool_calls。
func (h *Handler) chatViaCLI(w http.ResponseWriter, r *http.Request, req *chatRequest, cliModel, affinity string) {
	convID := conversationIDFor(req, affinity)
	prompt, images := promptAndImages(req.Messages)

	toolsOn := len(req.Tools) > 0 && req.ToolChoice.Mode != "none"
	if toolsOn {
		wrapped := toolPrompt(prompt, req.Tools, req.ToolChoice)
		// 工具规则是前置的：图片 Offset 是按未包装的 prompt 算的，要整体后移，
		// 否则增量过滤会误判图片是否属于本轮。
		shift := len(wrapped) - len(prompt)
		for i := range images {
			images[i].Offset += shift
		}
		prompt = wrapped
	}

	timeout := 300 * time.Second
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	res, err := h.cfg.CLI.Chat(ctx, cli.ChatRequest{
		ConversationID: convID,
		Provider:       h.cfg.CLIProvider,
		Model:          cliModel,
		Agent:          h.cfg.CLIAgent,
		Prompt:         prompt,
		Images:         images,
	})
	if err != nil {
		log.Printf("cli chat failed model=%s: %v", cliModel, err)
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "codearts cli: "+truncateMsg(err.Error(), 300))
		return
	}

	chatID := convID
	if !validChatID(chatID) {
		chatID = randHex(32)
	}
	w.Header().Set("X-Codearts-Chat-Id", chatID)

	content := res.Text
	finish := "stop"
	var calls []openAIToolCall
	if toolsOn {
		if c, rest, found := toolResponse(res.Text); found {
			kept, dropped := filterKnownCalls(c, req.Tools)
			if len(dropped) > 0 {
				log.Printf("cli dropped unknown tool names model=%s: %v", cliModel, dropped)
			}
			if len(kept) > 0 {
				calls = kept
				content = rest
				finish = "tool_calls"
				assignCallIDs(calls)
			} else {
				// 全是模型自己发明的工具名：不回 tool_calls（否则客户端执行失败
				// 并会死循环），把剥掉调用块后的文本当普通回复。
				content = strings.TrimSpace(rest)
			}
		}
	}
	if req.Stream {
		if len(calls) > 0 {
			streamCLIToolCalls(w, req.Model, calls)
			return
		}
		streamCLIText(w, req.Model, content)
		return
	}
	writeJSON(w, http.StatusOK, buildCompletion(req.Model, "", content, calls, finish, chatID, usageEstimateText(prompt, res.Text)))
}
