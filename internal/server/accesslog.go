package server

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"codearts2api/internal/pool"
)

// accessLogger 把每个对话请求写成一行访问日志。
//
// 为什么要在上游做：面板的「本日调用 / 最近调用」读的是 request_logs.account，
// 而这一列的唯一来源是**上游的对话日志行**——上游只回 X-Codearts-Chat-Id，/status
// 里也没有调用计数，面板无从得知「这次请求用的是哪个账号」。归属只能由真正选号的
// 一方产出，所以日志写在这里。
//
// 行格式与面板 server/services/accountlog.py::_CHAT_LINE 严格对齐：
//
//	| #12 | 14:58:13 | deepseek-v4.1-flash | stream | 200 | 昵称(3a3a19b1) | TTFB=312ms tok=120 total=20.1s |
//
// 账号段必须是「昵称(uid8)」：面板按括号里的 8 位 uid 前缀把这一行归到账号上，
// 少了括号或截错长度，界面就还是 0。
type accessLogger struct {
	mu      sync.Mutex
	file    *os.File
	written int64
	seq     atomic.Uint64
}

// maxAccessLogBytes 单个访问日志文件的上限。超过就滚动成 `.1`（只留一个历史文件）。
const maxAccessLogBytes = 8 << 20

func newAccessLogger() *accessLogger { return &accessLogger{} }

// accessLogPath 默认 data/server.out.log（相对进程工作目录；service.ps1 就是以
// 项目根为工作目录起进程的，所以这个文件落在 <上游根>/data/ 下）。
// 可用 CODEARTS_ACCESS_LOG 覆盖。
func accessLogPath() string {
	if p := os.Getenv("CODEARTS_ACCESS_LOG"); p != "" {
		return p
	}
	return filepath.Join("data", "server.out.log")
}

// write 追加一行。写日志本身失败只打一条 stderr，绝不影响正在进行的请求。
func (l *accessLogger) write(model string, stream bool, status int, acct *pool.Account, ttfb, total time.Duration, tokens int) {
	kind := "nostream"
	if stream {
		kind = "stream"
	}
	line := fmt.Sprintf("| #%d | %s | %s | %s | %d | %s | TTFB=%dms tok=%d total=%.1fs |",
		l.seq.Add(1), time.Now().Format("15:04:05"), model, kind, status,
		accountLabel(acct), ttfb.Milliseconds(), tokens, total.Seconds())

	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.ensureOpen(); err != nil {
		log.Printf("access log: %v", err)
		return
	}
	n, err := l.file.WriteString(line + "\n")
	l.written += int64(n)
	if err != nil {
		log.Printf("access log: write: %v", err)
		return
	}
	if l.written >= maxAccessLogBytes {
		l.rotate()
	}
}

// ensureOpen 懒打开（第一次写才建目录/文件），失败下一条再试。
func (l *accessLogger) ensureOpen() error {
	if l.file != nil {
		return nil
	}
	path := accessLogPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	l.file = f
	l.written = 0
	if st, serr := f.Stat(); serr == nil {
		l.written = st.Size() // 追加写要接着已有大小算
	}
	return nil
}

// rotate 关掉当前文件、改名成 `.1`（覆盖旧的），下次写会重新打开。
func (l *accessLogger) rotate() {
	path := accessLogPath()
	if l.file != nil {
		_ = l.file.Close()
		l.file = nil
	}
	_ = os.Remove(path + ".1")
	if err := os.Rename(path, path+".1"); err != nil {
		log.Printf("access log: rotate %s: %v", path, err)
	}
	l.written = 0
}

// accountLabel 拼「昵称(uid8)」。昵称为空时用 uid8 顶上，绝不输出空串——
// 面板会把空账号段整行跳过。
func accountLabel(acct *pool.Account) string {
	if acct == nil {
		return "-"
	}
	uid := acct.UID
	if uid == "" {
		uid = acct.Name
	}
	short := uid
	if len(short) > 8 {
		short = short[:8]
	}
	nick := acct.UserName
	if nick == "" {
		nick = short
	}
	if short == "" {
		return nick
	}
	return fmt.Sprintf("%s(%s)", nick, short)
}

// usageTokens 取 total_tokens。上游自己估的给 int，透传上游 JSON 时可能是
// float64 或 json.Number，三种都认，认不出就当 0（面板不靠这个数做账）。
func usageTokens(u map[string]any) int {
	switch v := u["total_tokens"].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return int(n)
		}
	}
	return 0
}

// logAccess 是 Handler 上的薄封装：把起点换算成总耗时，调用方只报状态与 token 数。
func (h *Handler) logAccess(model string, stream bool, status int, acct *pool.Account, start time.Time, ttfb time.Duration, tokens int) {
	if h.access == nil {
		return
	}
	h.access.write(model, stream, status, acct, ttfb, time.Since(start), tokens)
}
