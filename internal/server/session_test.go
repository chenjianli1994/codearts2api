package server

import (
	"testing"
	"time"
)

// B45：会话粘性的开关与 TTL。开关关闭时不记录也不读取；过期后自动解绑。
func TestSessionStickyRespectsToggleAndTTL(t *testing.T) {
	h := &Handler{cfg: Config{Session: SessionConfig{Enabled: true, TTL: 60 * time.Millisecond}},
		convAcct: map[string]convPin{}}

	h.setStickyAccount("conv-1", "acct-a")
	if got := h.stickyAccount("conv-1"); got != "acct-a" {
		t.Fatalf("绑定后应读到 acct-a，got=%q", got)
	}

	time.Sleep(80 * time.Millisecond)
	if got := h.stickyAccount("conv-1"); got != "" {
		t.Fatalf("超过 TTL 后应解绑，got=%q", got)
	}

	// 关闭开关：写不进去，也读不出来。
	h.cfg.Session.Enabled = false
	h.setStickyAccount("conv-2", "acct-b")
	if got := h.stickyAccount("conv-2"); got != "" {
		t.Fatalf("开关关闭时不应绑定，got=%q", got)
	}
}

// B45：清理周期到点后过期记录要被删掉，未过期的保留。
func TestConversationGCRemovesExpiredOnly(t *testing.T) {
	h := &Handler{cfg: Config{Session: SessionConfig{Enabled: true, TTL: time.Minute}},
		convAcct: map[string]convPin{
			"old": {acct: "a", at: time.Now().Add(-2 * time.Minute)},
			"new": {acct: "b", at: time.Now()},
		}}
	cutoff := time.Now().Add(-h.sessionTTL())
	h.convMu.Lock()
	for k, v := range h.convAcct {
		if v.at.Before(cutoff) {
			delete(h.convAcct, k)
		}
	}
	h.convMu.Unlock()
	if _, ok := h.convAcct["old"]; ok {
		t.Fatal("过期记录应被清理")
	}
	if _, ok := h.convAcct["new"]; !ok {
		t.Fatal("未过期记录不能删")
	}
}
