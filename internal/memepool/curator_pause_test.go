package memepool

import (
	"context"
	"testing"
	"time"

	"dadyumo/internal/llm"
)

// 真实 Router 必须满足 ConfigSource。
//
// 这条是编译期契约，不是冗余：Curator 拿不到配置的其它字段，Paused() 是它
// 唯一能看见总开关的通道。哪天有人把接口换掉、或给 Router 改了方法集，
// 这里先红，而不是等到「用户关了机器人、它却每 6 小时照烧一次 token」。
var _ ConfigSource = (*llm.Router)(nil)

type fakeSrc struct{ paused bool }

func (f fakeSrc) CheapestModel() (string, string) { return "ep", "model" }
func (f fakeSrc) Paused() bool                    { return f.paused }

// TestCuratorSkipsWhenPaused 总开关关停时，优选任务不许碰模型。
//
// 这是唯一一个**没有入口**的模型调用方：它不依赖任何群消息，ticker 到点自己跑。
// brain 里那几道闸对它完全无效，所以这里必须独立测一次。
//
// router 故意传 nil：关停判定是 runOnce 的第一句，命中就直接返回，
// 根本走不到 judge → router.Chat。反过来说，**如果哪天这道闸被删掉，
// 这个用例会以 nil 解引用 panic 收场**——一个吵的失败，而不是静默烧钱。
// 用真 Router 反而测不出问题：没有可用目标时 judge 本来就会提前返回，
// 删了闸也照样绿。
func TestCuratorSkipsWhenPaused(t *testing.T) {
	t.Parallel()
	pool := New(DefaultConfig(), newMemStorage())
	pool.Add([]byte("fake-jpeg-1"), "image/jpeg", "一张图", time.Now())

	c := NewCurator(pool, nil, fakeSrc{paused: true})
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.runOnce(context.Background())
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("关停时 runOnce 应立即返回，不该有任何等待或调用")
	}
	// 池子必须原样不动：关停期间连分数都不该被写（写分数意味着这轮真跑了）
	if got := pool.Browse(time.Now()); len(got) != 1 {
		t.Errorf("关停期间池子不该被改动，期望 1 条，实际 %d 条", len(got))
	}
}
