package server

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// 授权页必须开在「无痕容器」里：portal 靠浏览器 cookie 判定身份，同一容器里
// 已有会话时会直接拿回原账号的 code，导致加不了第二个号。网页 JS 没有能力
// 强制无痕窗口，只能由本机服务进程直接把浏览器拉起来 —— 面板的「打开授权
// 链接」因此走 /admin/api/oauth/open，而不是 window.open。

// browserLauncher 一个可用的无痕启动器。
type browserLauncher struct {
	Label string // 展示名，如 "Chrome (无痕)"
	Exe   string
	Args  []string // URL 追加在末尾
}

// allowedAuthHosts 允许服务端代开的授权域名后缀（防止被当成任意 URL 打开器）。
var allowedAuthHosts = []string{"huaweicloud.com"}

var (
	launcherOnce sync.Once
	launchers    []browserLauncher
)

func windowsBrowserCandidates() []browserLauncher {
	pf := os.Getenv("ProgramFiles")
	pf86 := os.Getenv("ProgramFiles(x86)")
	local := os.Getenv("LOCALAPPDATA")
	chrome := []string{
		filepath.Join(pf, `Google\Chrome\Application\chrome.exe`),
		filepath.Join(pf86, `Google\Chrome\Application\chrome.exe`),
		filepath.Join(local, `Google\Chrome\Application\chrome.exe`),
	}
	edge := []string{
		filepath.Join(pf, `Microsoft\Edge\Application\msedge.exe`),
		filepath.Join(pf86, `Microsoft\Edge\Application\msedge.exe`),
		filepath.Join(local, `Microsoft\Edge\Application\msedge.exe`),
	}
	out := make([]browserLauncher, 0, 4)
	for _, exe := range chrome {
		if exe != "" && fileExists(exe) {
			out = append(out, browserLauncher{Label: "Chrome（无痕）", Exe: exe, Args: []string{"--incognito", "--new-window"}})
			break
		}
	}
	for _, exe := range edge {
		if exe != "" && fileExists(exe) {
			out = append(out, browserLauncher{Label: "Edge（InPrivate）", Exe: exe, Args: []string{"--inprivate", "--new-window"}})
			break
		}
	}
	// Brave / Vivaldi 与 Chrome 同源，参数一致；有就用，没有就算。
	brave := filepath.Join(pf, `BraveSoftware\Brave-Browser\Application\brave.exe`)
	if exe := brave; fileExists(exe) {
		out = append(out, browserLauncher{Label: "Brave（无痕）", Exe: exe, Args: []string{"--incognito", "--new-window"}})
	}
	return out
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// discoverLaunchers 只在 Windows 上探浏览器；其他平台返回空（调用方回落到
// 系统默认打开器）。
func discoverLaunchers() []browserLauncher {
	launcherOnce.Do(func() {
		if runtime.GOOS != "windows" {
			return
		}
		launchers = windowsBrowserCandidates()
	})
	return launchers
}

// validateAuthURL 只放行华为云 portal 的授权链接。
func validateAuthURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("授权链接不合法: %w", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("授权链接协议必须是 https，实际 %q", u.Scheme)
	}
	host := strings.ToLower(u.Hostname())
	for _, suffix := range allowedAuthHosts {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return nil
		}
	}
	return fmt.Errorf("授权链接域名不在白名单: %q", host)
}

// openIncognito 用本机无痕浏览器打开授权链接，返回实际使用的启动器描述。
// 找不到无痕浏览器时回落到系统默认处理程序（会复用已有会话，返回值里注明）。
//
// 实现要点：每次授权都配一个**独立临时 profile**（--user-data-dir）。
// 只用 --incognito 不够 —— 同一个 Chrome 实例的所有无痕窗口共享 cookie，
// 连续加第二个、第三个号时会互相污染，又回到「登录回原账号」。
func openIncognito(rawURL string) (string, error) {
	if err := validateAuthURL(rawURL); err != nil {
		return "", err
	}
	cleanupOldProfiles()
	profile := isolateProfile()
	var errs []string
	for _, l := range discoverLaunchers() {
		args := append([]string{}, l.Args...)
		args = append(args,
			"--user-data-dir="+profile,
			"--no-first-run",
			"--no-default-browser-check",
			rawURL,
		)
		cmd := exec.Command(l.Exe, args...)
		if err := cmd.Start(); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", l.Label, err))
			continue
		}
		go func(c *exec.Cmd) { _ = c.Wait() }(cmd) // 不阻塞请求，回收子进程
		return l.Label + " · 独立 profile", nil
	}
	if runtime.GOOS == "windows" {
		cmd := exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", rawURL)
		if err := cmd.Start(); err == nil {
			go func(c *exec.Cmd) { _ = c.Wait() }(cmd)
			return "系统默认浏览器（非无痕，可能复用已有登录态）", nil
		} else {
			errs = append(errs, "rundll32: "+err.Error())
		}
	}
	if len(errs) == 0 {
		return "", fmt.Errorf("本机没有找到可用的浏览器")
	}
	return "", fmt.Errorf("打开浏览器失败: %s", strings.Join(errs, "; "))
}

const profilePrefix = "codearts2api-oauth-"

// isolateProfile 生成一次性的浏览器配置目录（不存在则由浏览器创建）。
func isolateProfile() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return filepath.Join(os.TempDir(), profilePrefix+hex.EncodeToString(b))
}

// cleanupOldProfiles 清掉两小时前的临时 profile，避免临时目录无限膨胀。
// 正在运行的浏览器会占着目录，删除失败直接忽略。
func cleanupOldProfiles() {
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		return
	}
	deadline := time.Now().Add(-2 * time.Hour)
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), profilePrefix) {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(deadline) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(os.TempDir(), e.Name()))
	}
}
