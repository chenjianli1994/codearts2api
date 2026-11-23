package server

import (
	_ "embed"
	"net/http"
	"strings"
)

//go:embed panel.html
var panelHTMLRaw string

func (h *Handler) servePanel(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/admin" && r.URL.Path != "/panel" && r.URL.Path != "/panel/" {
		http.NotFound(w, r)
		return
	}
	html := panelHTMLRaw
	html = strings.ReplaceAll(html, "__SERVICE_NAME__", "codearts2api")
	html = strings.ReplaceAll(html, "__SERVICE_TITLE__", "CodeArts2API")
	html = strings.ReplaceAll(html, "__LOGO__", "CA")
	html = strings.ReplaceAll(html, "__ACCENT__", "#0284c7")
	// 不再把「签到」文案改写成「保活」：CodeArts 确实有签到/新手活动接口
	// （/v1/ops/delivery → claim → confirm），改写会让面板上看不到签到入口，
	// 还会让人误以为那个按钮在刷 token。
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(html))
}
