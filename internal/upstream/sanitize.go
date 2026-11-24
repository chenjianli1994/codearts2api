// sanitize.go 出站请求体脱敏：剥掉客户端固定指纹。
//
// 背景：Claude Code 一类 CLI 会把固定模板标记塞进 system prompt 与 tool 参数，
// 例如 x-anthropic-billing-hdr 键值段、cc_ 前缀的键值对。上游按逐字精确匹配判断
// 请求是否来自某个客户端，删掉这些标记能显著降低被判定异常的概率，且不影响语义。
// 策略：键值型标记整段删除；残留裸键名做最小缩写，破坏逐字匹配、保留可读性。
package upstream

import (
	"regexp"
	"strings"
	"sync/atomic"
)

// sanitizeFeatures 特征预检（小写化后 Contains）：普通请求全不中 → 原样返回。
var sanitizeFeatures = []string{
	"x-anthropic-billing-hdr",
	"cc_version=",
}

// hdrSegmentRe 剥离层：键值形态整段删除（键名即触发，与值无关）。
var hdrSegmentRe = regexp.MustCompile("(?i)x-anthropic-billing-hdr[ \t]*[:=][^;\\n]*;?[ \t]*")

// ccKvRe 剥离层：裸键值对，循环清理到稳定。
var ccKvRe = regexp.MustCompile("(?i)\\bcc_[a-z0-9_]+=[^;\\n]*;?[ \t]*")

// bareHdrRe 兜底层：键值段删完可能只剩裸键名（被引用、或写成示例文本），
// 裸串同样能命中逐字匹配，做最小缩写 header → hdr。
var bareHdrRe = regexp.MustCompile("(?i)x-anthropic-billing-hdr")

// 两个运行期开关，由 cmd/server 启动时按 config.json 设置。
var (
	sanitizeOutbound   atomic.Bool
	reasoningAsContent atomic.Bool
)

func init() {
	sanitizeOutbound.Store(true)   // 默认开：与参考项目一致，排查问题时才关
	reasoningAsContent.Store(true) // 默认开：正文为空时把推理当正文，避免空回复
}

// SetSanitizeOutbound 设置出站请求指纹脱敏开关。
func SetSanitizeOutbound(on bool) { sanitizeOutbound.Store(on) }

// SanitizeOutboundEnabled 当前脱敏开关状态（供测试与状态接口读取）。
func SanitizeOutboundEnabled() bool { return sanitizeOutbound.Load() }

// SetReasoningAsContent 设置「思考内容兜底为正文」开关。
func SetReasoningAsContent(on bool) { reasoningAsContent.Store(on) }

// ReasoningAsContentEnabled 当前兜底开关状态。
func ReasoningAsContentEnabled() bool { return reasoningAsContent.Load() }

// hasFingerprint 预检：小写化后 Contains，再兜一遍裸键名正则（可能大小写混合）。
func hasFingerprint(text string) bool {
	lower := strings.ToLower(text)
	for _, f := range sanitizeFeatures {
		if strings.Contains(lower, f) {
			return true
		}
	}
	return bareHdrRe.MatchString(text)
}

// SanitizeText 单段文本净化：预检不中时原样返回。
func SanitizeText(text string) string {
	if !hasFingerprint(text) {
		return text
	}
	if hdrSegmentRe.MatchString(text) {
		text = hdrSegmentRe.ReplaceAllString(text, "")
	}
	if strings.Contains(strings.ToLower(text), "cc_") {
		prev := ""
		for prev != text { // 可能连着多条裸键值，清到稳定为止
			prev = text
			text = ccKvRe.ReplaceAllString(text, "")
		}
	}
	// 兜底：键值形态已删，这里只剩裸键名，缩写后逐字匹配即失效。
	text = bareHdrRe.ReplaceAllString(text, "x-anth-billing-hdr")
	return strings.TrimSpace(text)
}

// sanitizeContent 兼容字符串与多模态数组：只动 text part，image 等不动。
func sanitizeContent(v any) (any, bool) {
	switch c := v.(type) {
	case string:
		s := SanitizeText(c)
		return s, s != c
	case []any:
		changed := false
		for _, p := range c {
			m, ok := p.(map[string]any)
			if !ok {
				continue
			}
			text, ok := m["text"].(string)
			if !ok {
				continue
			}
			if s := SanitizeText(text); s != text {
				m["text"] = s
				changed = true
			}
		}
		return c, changed
	}
	return v, false
}

// sanitizeToolCalls 净化 assistant.tool_calls[].function.arguments（字符串化 JSON）。
// 工具轮消息的 content 通常是 null，所以与 content 独立判断，否则整条会被跳过。
func sanitizeToolCalls(v any) bool {
	callList, ok := v.([]any)
	if !ok {
		return false
	}
	changed := false
	for _, c := range callList {
		call, ok := c.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := call["function"].(map[string]any)
		if !ok {
			continue
		}
		args, ok := fn["arguments"].(string)
		if !ok {
			continue
		}
		if s := SanitizeText(args); s != args {
			fn["arguments"] = s
			changed = true
		}
	}
	return changed
}

// SanitizeOutboundMessages 净化 messages 里的 content、reasoning_content 与 tool_calls。
func SanitizeOutboundMessages(messages []map[string]any) bool {
	changed := false
	for _, m := range messages {
		if c, ok := m["content"]; ok {
			if nc, ch := sanitizeContent(c); ch {
				m["content"] = nc
				changed = true
			}
		}
		if rc, ok := m["reasoning_content"].(string); ok {
			if s := SanitizeText(rc); s != rc {
				m["reasoning_content"] = s
				changed = true
			}
		}
		if tc, ok := m["tool_calls"]; ok {
			if sanitizeToolCalls(tc) {
				changed = true
			}
		}
	}
	return changed
}
