package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"codearts2api/internal/auth"
)

// accountsExportVersion 导出文件格式版本；导入同时兼容无 version 的裸数组。
const accountsExportVersion = 1

// maxImportBytes 单次导入的请求体上限（4 MB，够放几十个账号）。
const maxImportBytes = 4 << 20

// accountsExport 账号导出文件。字段与 auths/codearts-*.json 对齐，
// 差别只是外面套了一层信封，便于导入时识别来源与版本。
type accountsExport struct {
	Service       string       `json:"service"`
	Version       int          `json:"version"`
	ExportedAt    string       `json:"exported_at"`
	SecretsMasked bool         `json:"secrets_masked,omitempty"`
	Count         int          `json:"count"`
	Accounts      []*auth.Auth `json:"accounts"`
}

// maskAuth 返回脱敏副本：保留身份与过期时间，抹掉能直接接管账号的凭据。
// 逐字段构造而不整体拷贝，避免复制 Auth 里的 sync.Mutex。
func maskAuth(a *auth.Auth) *auth.Auth {
	if a == nil {
		return nil
	}
	return &auth.Auth{
		UserID:     a.UserID,
		UserName:   a.UserName,
		DomainID:   a.DomainID,
		Expiration: a.Expiration,
		UpdatedAt:  a.UpdatedAt,
		ClientID:   a.ClientID,
	}
}

// adminExportAccounts 导出账号为 JSON 文件。
//
// 默认含完整凭据（refresh_token / AK / SK / DPoP 私钥），可直接在另一台机器导入
// 恢复；带 ?mask=1 时脱敏，只留 user_id / user_name / expiration，用于截图或求助。
func (h *Handler) adminExportAccounts(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Pool == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "message": "账号池未初始化"})
		return
	}
	masked := false
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get("mask"))) {
	case "1", "true", "yes":
		masked = true
	}

	accs := h.cfg.Pool.Accounts()
	out := accountsExport{
		Service:       "codearts2api",
		Version:       accountsExportVersion,
		ExportedAt:    time.Now().Format(time.RFC3339),
		SecretsMasked: masked,
		Accounts:      make([]*auth.Auth, 0, len(accs)),
	}
	for _, acct := range accs {
		if acct == nil || acct.Auth == nil {
			continue
		}
		if masked {
			out.Accounts = append(out.Accounts, maskAuth(acct.Auth))
			continue
		}
		out.Accounts = append(out.Accounts, acct.Auth)
	}
	out.Count = len(out.Accounts)

	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "message": "序列化失败: " + err.Error()})
		return
	}
	prefix := "codearts-accounts-"
	if masked {
		prefix = "codearts-accounts-masked-"
	}
	name := prefix + time.Now().Format("20060102-150405") + ".json"
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

// importResult 单条导入结果。
type importResult struct {
	UID     string `json:"uid"`
	Action  string `json:"action"` // created / updated / skipped
	Message string `json:"message,omitempty"`
}

// adminImportAccounts 从 JSON 导入账号，写入 auths 目录后重载账号池。
//
// 支持三种请求体：导出信封 {"accounts":[...]}、裸数组 [...]、单条对象 {...}。
// 以 user_id 为键合并：已存在则覆盖（refresh_token 会被新文件替换），不存在则新建。
func (h *Handler) adminImportAccounts(w http.ResponseWriter, r *http.Request) {
	dir := strings.TrimSpace(h.cfg.AuthDir)
	if dir == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "auth_dir 未配置，无法写入账号文件"})
		return
	}
	if h.cfg.Pool == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "message": "账号池未初始化"})
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxImportBytes))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "读取请求体失败: " + err.Error()})
		return
	}
	entries, err := parseImportPayload(raw)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "解析失败: " + err.Error()})
		return
	}
	if len(entries) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "accounts 为空"})
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "message": "创建 auths 目录失败: " + err.Error()})
		return
	}

	seen := make(map[string]bool, len(entries))
	results := make([]importResult, 0, len(entries))
	created, updated, skipped := 0, 0, 0
	for i, a := range entries {
		res := importResult{}
		switch {
		case a == nil:
			res.Message = fmt.Sprintf("第 %d 条为空对象", i+1)
		case strings.TrimSpace(a.UserID) == "":
			res.Message = fmt.Sprintf("第 %d 条缺少 user_id", i+1)
		case seen[a.UserID]:
			res.UID = a.UserID
			res.Message = "同一批次内重复，已忽略"
		case strings.TrimSpace(a.CloudDragonTok) == "":
			res.UID = a.UserID
			res.Message = "缺少 cloud_dragon_token，导入后无法直接使用"
		}
		if res.Message != "" {
			res.Action = "skipped"
			skipped++
			results = append(results, res)
			continue
		}

		seen[a.UserID] = true
		path := filepath.Join(dir, a.FileName())
		_, statErr := os.Stat(path)
		existed := statErr == nil
		if a.UpdatedAt == 0 {
			a.UpdatedAt = time.Now().Unix()
		}
		if err := auth.SaveNew(dir, a); err != nil {
			results = append(results, importResult{UID: a.UserID, Action: "skipped", Message: "写入失败: " + err.Error()})
			skipped++
			continue
		}
		if existed {
			updated++
			results = append(results, importResult{UID: a.UserID, Action: "updated"})
			continue
		}
		created++
		results = append(results, importResult{UID: a.UserID, Action: "created"})
	}

	// 落盘完成后按目录全量对齐账号池，避免半新半旧。
	reloaded := 0
	if auths, loadErr := auth.LoadDir(dir); loadErr == nil {
		h.cfg.Pool.SyncToDir(auths)
		reloaded = len(auths)
	}

	msg := fmt.Sprintf("导入完成: 新建 %d / 覆盖 %d / 跳过 %d", created, updated, skipped)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       created+updated > 0,
		"message":  msg,
		"created":  created,
		"updated":  updated,
		"skipped":  skipped,
		"loaded":   reloaded,
		"results":  results,
		"accounts": h.cfg.Pool.List(),
	})
}

// parseImportPayload 兼容导出信封、裸数组、单条对象三种形态。
func parseImportPayload(raw []byte) ([]*auth.Auth, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("请求体为空")
	}
	switch trimmed[0] {
	case '[':
		return parseImportArray(trimmed)
	case '{':
		var env struct {
			Accounts json.RawMessage `json:"accounts"`
		}
		if err := json.Unmarshal(trimmed, &env); err != nil {
			return nil, err
		}
		if len(bytes.TrimSpace(env.Accounts)) > 0 && string(bytes.TrimSpace(env.Accounts)) != "null" {
			return parseImportArray(env.Accounts)
		}
		// 单条账号对象
		var one auth.Auth
		if err := json.Unmarshal(trimmed, &one); err != nil {
			return nil, err
		}
		return []*auth.Auth{&one}, nil
	default:
		return nil, fmt.Errorf("无法识别的 JSON 结构（应为导出文件、数组或单个账号对象）")
	}
}

func parseImportArray(raw []byte) ([]*auth.Auth, error) {
	var list []*auth.Auth
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, err
	}
	return list, nil
}
