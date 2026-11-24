package pool

import (
	"testing"
	"time"
)

// 两个账号都健康、都空闲时，PickExcluding 要挑「最久没被用过」的那个。
// 上下文：华为侧一个会话释放要 ~60s，刚用过的账号更容易撞「并发会话数已达上限」，
// 所以选号不能永远挑文件里靠前的那个。
func TestPickPrefersLeastRecentlyUsed(t *testing.T) {
	a := &Account{Name: "a", maxConcurrent: 2}
	b := &Account{Name: "b", maxConcurrent: 2}
	p := &Pool{accounts: []*Account{a, b}}

	now := time.Now()
	a.lastRelease = now.Add(-30 * time.Second)
	b.lastRelease = now.Add(-90 * time.Second) // 更久没用 → 应当被选中

	got := p.PickExcluding(nil)
	if got == nil || got.Name != "b" {
		t.Fatalf("want b, got %v", got)
	}
}

// 一个账号还有空闲槽位、另一个已占满时，必须挑还有槽位的（与 LRU 无关）。
func TestPickPrefersFreeSlotOverLRU(t *testing.T) {
	a := &Account{Name: "a", maxConcurrent: 1, activeConcurrent: 1} // 占满
	b := &Account{Name: "b", maxConcurrent: 1, activeConcurrent: 0} // 有空位
	now := time.Now()
	a.lastRelease = now.Add(-90 * time.Second) // 更久没用，但已占满
	b.lastRelease = now.Add(-1 * time.Second)
	p := &Pool{accounts: []*Account{a, b}}

	got := p.PickExcluding(nil)
	if got == nil || got.Name != "b" {
		t.Fatalf("want b, got %v", got)
	}
}

// PickOtherIdle 要跳过被限流的账号，并且只在「有空槽位」时返回。
func TestPickOtherIdleSkipsExcludedAndFull(t *testing.T) {
	a := &Account{Name: "a", maxConcurrent: 2}
	b := &Account{Name: "b", maxConcurrent: 2, activeConcurrent: 2} // 满
	c := &Account{Name: "c", maxConcurrent: 2}
	p := &Pool{accounts: []*Account{a, b, c}}

	if got := p.PickOtherIdle("a"); got == nil || got.Name != "c" {
		t.Fatalf("want c, got %v", got)
	}
	if got := p.PickOtherIdle("a"); got != nil && got.Name == "a" {
		t.Fatalf("must not return excluded account")
	}
}
