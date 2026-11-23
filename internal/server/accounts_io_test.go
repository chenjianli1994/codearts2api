package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/pool"
)

func newAccountsIOHandler(t *testing.T, dir string, as ...*auth.Auth) *Handler {
	t.Helper()
	p, err := pool.New(as, pool.Config{}, "")
	if err != nil {
		t.Fatalf("pool.New: %v", err)
	}
	return NewHandler(Config{Pool: p, APIKey: "test-key", AuthDir: dir})
}

func ioRequest(h *Handler, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func sampleAuth(uid, name, token, refresh string) *auth.Auth {
	return auth.New(uid, name, "domain-"+uid, token, "ak-"+uid, "sk-"+uid,
		time.Now().Add(2*time.Hour).Format(time.RFC3339), refresh, "cv-"+uid,
		map[string]string{"kty": "EC", "d": "secret-" + uid})
}

// 导出默认必须带齐 refresh_token / AK / SK / DPoP 私钥，否则换个机器导入就会掉登录。
func TestExportAccountsIncludesSecrets(t *testing.T) {
	h := newAccountsIOHandler(t, "",
		sampleAuth("u1", "张三", "tok-1", "rt-1"),
		sampleAuth("u2", "李四", "tok-2", "rt-2"),
	)
	rec := ioRequest(h, http.MethodGet, "/admin/api/accounts/export", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") || !strings.Contains(cd, ".json") {
		t.Fatalf("Content-Disposition 应为附件下载: %q", cd)
	}
	var env accountsExport
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("导出不是合法 JSON: %v", err)
	}
	if env.Version != accountsExportVersion || env.Count != 2 || len(env.Accounts) != 2 || env.SecretsMasked {
		t.Fatalf("信封字段不对: %+v", env)
	}
	byUID := map[string]*auth.Auth{}
	for _, a := range env.Accounts {
		byUID[a.UserID] = a
	}
	got := byUID["u1"]
	if got == nil {
		t.Fatal("缺少 u1")
	}
	if got.RefreshToken != "rt-1" || got.AccessKeyID != "ak-u1" || got.SecretAccessKey != "sk-u1" || got.CloudDragonTok != "tok-1" {
		t.Fatalf("密钥字段缺失: %+v", got)
	}
	if got.DPoPPrivateKey["d"] != "secret-u1" {
		t.Fatalf("DPoP 私钥应随导出带上: %+v", got.DPoPPrivateKey)
	}
}

// ?mask=1 用于截图/求助：身份与过期时间保留，凭据全抹。
func TestExportAccountsMasked(t *testing.T) {
	h := newAccountsIOHandler(t, "", sampleAuth("u1", "张三", "tok-1", "rt-1"))
	rec := ioRequest(h, http.MethodGet, "/admin/api/accounts/export?mask=1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var env accountsExport
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if !env.SecretsMasked || env.Count != 1 {
		t.Fatalf("脱敏标记不对: %+v", env)
	}
	a := env.Accounts[0]
	if a.UserID != "u1" || a.UserName != "张三" {
		t.Fatalf("身份字段应保留: %+v", a)
	}
	if a.CloudDragonTok != "" || a.AccessKeyID != "" || a.SecretAccessKey != "" || a.RefreshToken != "" || a.CodeVerifier != "" || len(a.DPoPPrivateKey) != 0 {
		t.Fatalf("脱敏后仍有凭据: %+v", a)
	}
}

func TestExportAccountsEmptyPool(t *testing.T) {
	h := newAccountsIOHandler(t, "")
	rec := ioRequest(h, http.MethodGet, "/admin/api/accounts/export", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	var env accountsExport
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Count != 0 || env.Accounts == nil {
		t.Fatalf("空池应返回 accounts: [] 而不是 null: %+v", env)
	}
}

// 导入：同 uid 覆盖、新 uid 新建，落盘后账号池立即对齐。
func TestImportAccountsCreatesAndOverwrites(t *testing.T) {
	dir := t.TempDir()
	old := sampleAuth("u1", "旧昵称", "old-token", "old-rt")
	if err := auth.SaveNew(dir, old); err != nil {
		t.Fatal(err)
	}
	loaded, err := auth.LoadDir(dir)
	if err != nil || len(loaded) != 1 {
		t.Fatalf("准备 auths 失败: %v len=%d", err, len(loaded))
	}
	h := newAccountsIOHandler(t, dir, loaded...)

	payload := `{"service":"codearts2api","version":1,"accounts":[
		{"user_id":"u1","user_name":"新昵称","domain_id":"d1","cloud_dragon_token":"new-token",
		 "access_key_id":"ak-new","secret_access_key":"sk-new","expiration":"2030-01-01T00:00:00Z",
		 "refresh_token":"new-rt","code_verifier":"cv-new"},
		{"user_id":"u2","user_name":"新账号","domain_id":"d2","cloud_dragon_token":"tok-2",
		 "refresh_token":"rt-2"}
	]}`
	rec := ioRequest(h, http.MethodPost, "/admin/api/accounts/import", payload)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK      bool `json:"ok"`
		Created int  `json:"created"`
		Updated int  `json:"updated"`
		Skipped int  `json:"skipped"`
		Loaded  int  `json:"loaded"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Created != 1 || resp.Updated != 1 || resp.Skipped != 0 || resp.Loaded != 2 {
		t.Fatalf("导入统计不对: %s", rec.Body.String())
	}

	// 文件层
	if _, err := os.Stat(filepath.Join(dir, "codearts-u2.json")); err != nil {
		t.Fatalf("u2 未落盘: %v", err)
	}
	onDisk, err := auth.LoadDir(dir)
	if err != nil || len(onDisk) != 2 {
		t.Fatalf("重载 auths 数量不对: %v len=%d", err, len(onDisk))
	}
	// 账号池层：覆盖后的凭据要立刻生效
	acct := h.cfg.Pool.Get("u1")
	if acct == nil {
		t.Fatal("池里没有 u1")
	}
	if acct.Auth.Refresh() != "new-rt" || acct.UserName != "新昵称" {
		t.Fatalf("u1 未被覆盖: refresh=%s name=%s", acct.Auth.Refresh(), acct.UserName)
	}
	if h.cfg.Pool.Get("u2") == nil {
		t.Fatal("池里没有新增的 u2")
	}
}

// 裸数组与单条对象都要能吃，方便手工喂文件。
func TestImportAccountsAcceptsBareArrayAndSingleObject(t *testing.T) {
	dir := t.TempDir()
	h := newAccountsIOHandler(t, dir)

	arr := `[{"user_id":"a1","user_name":"A","cloud_dragon_token":"t1","refresh_token":"r1"},
	         {"user_id":"a2","user_name":"B","cloud_dragon_token":"t2","refresh_token":"r2"}]`
	rec := ioRequest(h, http.MethodPost, "/admin/api/accounts/import", arr)
	if rec.Code != http.StatusOK {
		t.Fatalf("数组导入失败: status=%d body=%s", rec.Code, rec.Body.String())
	}

	one := `{"user_id":"a3","user_name":"C","cloud_dragon_token":"t3","refresh_token":"r3"}`
	rec = ioRequest(h, http.MethodPost, "/admin/api/accounts/import", one)
	if rec.Code != http.StatusOK {
		t.Fatalf("单对象导入失败: status=%d body=%s", rec.Code, rec.Body.String())
	}
	auths, err := auth.LoadDir(dir)
	if err != nil || len(auths) != 3 {
		t.Fatalf("应有 3 个账号文件: %v len=%d", err, len(auths))
	}
}

// 缺 token 的条目跳过但要报告；全坏则 ok=false。
func TestImportAccountsSkipsIncompleteEntries(t *testing.T) {
	dir := t.TempDir()
	h := newAccountsIOHandler(t, dir)
	payload := `{"accounts":[
		{"user_id":"good","user_name":"G","cloud_dragon_token":"tok","refresh_token":"rt"},
		{"user_id":"no-token","user_name":"N","refresh_token":"rt"},
		{"user_name":"no-uid","cloud_dragon_token":"tok"}
	]}`
	rec := ioRequest(h, http.MethodPost, "/admin/api/accounts/import", payload)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK      bool           `json:"ok"`
		Created int            `json:"created"`
		Skipped int            `json:"skipped"`
		Results []importResult `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Created != 1 || resp.Skipped != 2 {
		t.Fatalf("统计不对: %s", rec.Body.String())
	}
	skippedUID := ""
	for _, r := range resp.Results {
		if r.Action == "skipped" && r.UID == "no-token" {
			skippedUID = r.UID
		}
	}
	if skippedUID == "" {
		t.Fatalf("缺少 no-token 的跳过记录: %s", rec.Body.String())
	}
}

func TestImportAccountsRejectsBadPayload(t *testing.T) {
	dir := t.TempDir()
	h := newAccountsIOHandler(t, dir)
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"空请求体", "", http.StatusBadRequest},
		{"非法 JSON", "{not-json", http.StatusBadRequest},
		{"空数组", "[]", http.StatusBadRequest},
		{"空 accounts", `{"accounts":[]}`, http.StatusBadRequest},
		{"标量", `"just-a-string"`, http.StatusBadRequest},
	} {
		rec := ioRequest(h, http.MethodPost, "/admin/api/accounts/import", tc.body)
		if rec.Code != tc.status {
			t.Fatalf("%s: status=%d want=%d body=%s", tc.name, rec.Code, tc.status, rec.Body.String())
		}
	}
}

// 导出→导入闭环：脱敏导出不能用来恢复；完整导出必须能原样恢复。
func TestExportImportRoundTrip(t *testing.T) {
	srcDir := t.TempDir()
	src := newAccountsIOHandler(t, srcDir, sampleAuth("u1", "张三", "tok-1", "rt-1"))
	rec := ioRequest(src, http.MethodGet, "/admin/api/accounts/export", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("导出失败: %s", rec.Body.String())
	}
	payload := rec.Body.String()

	dstDir := t.TempDir()
	dst := newAccountsIOHandler(t, dstDir)
	rec = ioRequest(dst, http.MethodPost, "/admin/api/accounts/import", payload)
	if rec.Code != http.StatusOK {
		t.Fatalf("导入失败: status=%d body=%s", rec.Code, rec.Body.String())
	}
	acct := dst.cfg.Pool.Get("u1")
	if acct == nil {
		t.Fatal("导入后池里没有 u1")
	}
	token, ak, _ := acct.Auth.Credentials()
	if acct.Auth.Refresh() != "rt-1" || token != "tok-1" || ak != "ak-u1" {
		t.Fatalf("恢复的凭据不对: token=%s ak=%s refresh=%s", token, ak, acct.Auth.Refresh())
	}
}
