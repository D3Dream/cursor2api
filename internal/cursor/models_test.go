package cursor

import (
	"context"
	"testing"
	"time"
)

// Peek 对过期缓存必须返回 nil（视为无缓存、校验放行），
// 不能用 stale 列表把已恢复可用的模型 400 掉。
func TestModelCachePeekExpiry(t *testing.T) {
	c := NewModelCache(time.Minute)
	c.models = []UsableModel{{ModelID: "m1"}}
	c.fetched = time.Now()
	if got := c.Peek(); len(got) != 1 {
		t.Fatalf("新鲜缓存 Peek = %v, want 1 model", got)
	}
	c.fetched = time.Now().Add(-2 * time.Minute)
	if got := c.Peek(); got != nil {
		t.Fatalf("过期缓存 Peek = %v, want nil（过期即无缓存）", got)
	}
}

// Usable 必须同时认 ModelID 与别名。
func TestUsable(t *testing.T) {
	models := []UsableModel{
		{ModelID: "claude-sonnet-5", Aliases: []string{"sonnet", "claude-sonnet-4-5"}},
		{ModelID: "default"},
	}
	for _, id := range []string{"claude-sonnet-5", "sonnet", "claude-sonnet-4-5", "default"} {
		if !Usable(models, id) {
			t.Errorf("Usable(%q) = false, want true", id)
		}
	}
	if Usable(models, "gpt-x") {
		t.Error("Usable(gpt-x) = true, want false")
	}
}

// 负缓存：拉取失败后在窗口期内 Get 不得重复发 RPC（故障期每请求都打上游会放大故障）。
func TestModelCacheNegativeCache(t *testing.T) {
	c := NewModelCache(time.Minute)
	c.models = []UsableModel{{ModelID: "m1"}}
	c.fetched = time.Now().Add(-2 * time.Minute) // 过期
	c.failedAt = time.Now()                      // 刚失败过
	// client 传 nil：若真的发起 fetch 会立即 panic/报错——
	// 负缓存命中时根本不触碰 client
	got := c.Get(context.Background(), nil)
	if len(got) != 1 || got[0].ModelID != "m1" {
		t.Fatalf("负缓存窗口内 Get = %v, want 旧缓存兜底", got)
	}
	// 窗口期外：会尝试 fetch（nil client 会 panic，用 recover 验证确实走到了 fetch）
	c.failedAt = time.Now().Add(-2 * time.Minute)
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("窗口期外 Get 未触发 fetch（应触碰 nil client 而 panic）")
			}
		}()
		c.Get(context.Background(), nil)
	}()
}
