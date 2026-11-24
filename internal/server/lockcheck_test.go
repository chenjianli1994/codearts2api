package server

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestNoNestedConvLock 防止再次把「已持有 convMu」的临界区里调进会二次加锁的函数。
//
// 背景（B45 真实事故）：storeChat 已经拿了 convMu，却又调 setStickyAccount，
// 而后者内部再 Lock 一次同一个 Mutex —— Go 的 sync.Mutex 不可重入，于是请求拿到
// 上游响应后永久挂住，访问日志一行都写不出来；面板侧只看到 120s 超时。
// 关掉会话粘性就恢复正常，极难从现象反推，所以留一条静态守卫。
func TestNoNestedConvLock(t *testing.T) {
	raw, err := os.ReadFile("handler.go")
	if err != nil {
		t.Fatalf("读 handler.go 失败：%v", err)
	}
	src := string(raw)
	// 抓「Lock 到 Unlock 之间」的每一段临界区
	block := regexp.MustCompile(`(?s)h\.convMu\.Lock\(\)(.*?)h\.convMu\.Unlock\(\)`)
	blocks := block.FindAllStringSubmatch(src, -1)
	if len(blocks) == 0 {
		t.Fatal("没扫到任何 convMu 临界区：正则或代码结构变了，这条守卫会空转")
	}
	for _, m := range blocks {
		body := m[1]
		for _, banned := range []string{"setStickyAccount(", "stickyAccount(", "forgetSticky("} {
			if strings.Contains(body, banned) {
				t.Fatalf("临界区里调了 %s —— 这些函数内部会再 Lock 一次 convMu，会自死锁；"+
					"应当在持锁时直接写 convAcct 字段", banned)
			}
		}
	}
}
