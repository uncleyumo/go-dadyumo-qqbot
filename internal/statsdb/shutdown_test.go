package statsdb

import (
	"path/filepath"
	"testing"
	"time"
)

// TestWriteAfterCloseIsDropped 关库之后再写不许崩。
//
// 生产实况（2026-10-02）：Close 打的那条「统计库已关闭」日志，
// 经 logx 的 sink 回灌到 RecordEvent，落点正好在通道已关之后——
// enqueue 往关闭的通道发送，panic: send on closed channel。
// systemd 于是记 status=2/INVALIDARGUMENT，**每次 restart 都崩**，
// 而且崩在退出阶段，前面的工作全做完了，只把退出码弄脏。
//
// Close 必须幂等可重复调用：这条测试自己调两次。
func TestWriteAfterCloseIsDropped(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatalf("打开失败: %v", err)
	}
	d.RecordEvent(Event{Cat: "decision", Level: "INFO", Msg: "关库前", KV: "{}"})
	if err := d.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}

	// 关库后的三种写：都不许 panic，且必须静默丢弃。
	// 静默是硬要求——这一支如果打日志，会经 sink 再进 RecordEvent，
	// 变成自己打自己。
	d.RecordEvent(Event{Cat: "decision", Level: "INFO", Msg: "关库后", KV: "{}"})
	d.RecordCall(Call{Model: "x"})
	d.RecordTarget(TargetState{Key: "a|b"})

	if !d.closed.Load() {
		t.Error("closed 标志没置位，后续写操作仍会进队列")
	}

	// 再关一次也不能 panic：close 已关闭的通道会 panic，
	// 而 t.Cleanup 和业务代码都可能各关一次
	if err := d.Close(); err != nil {
		t.Errorf("重复 Close 应是无害的 no-op，却返回 %v", err)
	}
}

// TestCloseDropsPendingSilently 关库那一刻排队的写必须写完，且不残留。
//
// 回归另一侧：加了 closed 闸之后，Close 之前入队的不能被一起丢掉。
func TestCloseDrainsPending(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	d, err := Open(path)
	if err != nil {
		t.Fatalf("打开失败: %v", err)
	}
	for i := 0; i < 20; i++ {
		d.RecordEvent(Event{Cat: "decision", Level: "INFO", Msg: "排队", KV: "{}"})
	}
	// 不等 flushLoop，直接关：Close 内部会排空
	if err := d.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}

	d2, err := Open(path)
	if err != nil {
		t.Fatalf("重开失败: %v", err)
	}
	t.Cleanup(func() { _ = d2.Close() })
	n, err := d2.CountEvents(EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if n != 20 {
		t.Errorf("关库后排队的 20 条应全部落库，实际 %d 条", n)
	}
}

// TestCloseRaceWithEnqueue 关库与写入并发时不许崩。
//
// 竞态版本：光跑顺序调用只能证明「关完之后不再写」，
// 证明不了「正在关的同时有人在写」。这里用 -race 才算数。
func TestCloseRaceWithEnqueue(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatalf("打开失败: %v", err)
	}

	stop := make(chan struct{})
	go func() {
		for i := 0; i < 500; i++ {
			d.RecordEvent(Event{Cat: "chat", Level: "INFO", Msg: "并发", KV: "{}"})
		}
		close(stop)
	}()

	time.Sleep(2 * time.Millisecond)
	if err := d.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}
	<-stop
}