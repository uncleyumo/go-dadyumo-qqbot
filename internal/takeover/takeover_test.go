package takeover

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestStore 在临时目录里打开一个空 Store，返回 Store 与文件路径。
func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "takeover.json")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open 不应失败: %v", err)
	}
	return s, path
}

// TestOpenMissingFileDefaultsToWindow 文件不存在时按 window 运行，且不创建文件。
// 生产上第一次启动就是这条路径，不能因为“没有文件”阻塞启动或写空文件。
func TestOpenMissingFileDefaultsToWindow(t *testing.T) {
	s, path := newTestStore(t)

	if got := s.Mode("G1"); got != ModeWindow {
		t.Fatalf("缺省模式应为 window，得到 %q", got)
	}
	if got := s.Since("G1"); !got.IsZero() {
		t.Fatalf("没有条目时 Since 应为零值，得到 %v", got)
	}
	if got := s.Groups(); got != nil {
		t.Fatalf("没有条目时 Groups 应为 nil，得到 %v", got)
	}
	if got := s.Epoch(); got != 0 {
		t.Fatalf("缺省 epoch 应为 0，得到 %d", got)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Open 不应创建文件，stat err=%v", err)
	}
}

// TestNilStoreReadsAreSafe 未注入 Store 时读方法不 panic。
func TestNilStoreReadsAreSafe(t *testing.T) {
	var s *Store
	if got := s.Mode("G1"); got != ModeWindow {
		t.Fatalf("nil Store 应按 window 处理，得到 %q", got)
	}
	if !s.Since("G1").IsZero() {
		t.Fatal("nil Store 的 Since 应为零值")
	}
	if s.Groups() != nil {
		t.Fatal("nil Store 的 Groups 应为 nil")
	}
	if s.Epoch() != 0 {
		t.Fatal("nil Store 的 Epoch 应为 0")
	}
	if err := s.SetMode("G1", ModeAlways); err == nil {
		t.Fatal("nil Store 的 SetMode 应返回错误")
	}
	if _, err := s.BumpEpoch(); err == nil {
		t.Fatal("nil Store 的 BumpEpoch 应返回错误")
	}
}

// TestStoreRoundTrip 模式、时间戳、epoch 必须能原样读回。
func TestStoreRoundTrip(t *testing.T) {
	s, path := newTestStore(t)
	before := time.Now().Unix()

	if err := s.SetMode("G1", ModeAlways); err != nil {
		t.Fatalf("SetMode always: %v", err)
	}
	if err := s.SetMode("G2", ModeOff); err != nil {
		t.Fatalf("SetMode off: %v", err)
	}
	if n, err := s.BumpEpoch(); err != nil || n != 1 {
		t.Fatalf("BumpEpoch 应为 1，得到 %d, err=%v", n, err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读回落盘文件: %v", err)
	}
	for _, key := range []string{`"version"`, `"default_mode"`, `"epoch"`, `"groups"`, `"mode_since"`} {
		if !strings.Contains(string(raw), key) {
			t.Fatalf("落盘 JSON 缺少字段 %s，原文:\n%s", key, raw)
		}
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("文件权限应为 0600，得到 %o", perm)
		}
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("重新 Open: %v", err)
	}
	if got := s2.Mode("G1"); got != ModeAlways {
		t.Fatalf("G1 读回应为 always，得到 %q", got)
	}
	if got := s2.Mode("G2"); got != ModeOff {
		t.Fatalf("G2 读回应为 off，得到 %q", got)
	}
	if got := s2.Since("G1").Unix(); got < before {
		t.Fatalf("G1 mode_since 应不早于写入前，得到 %d < %d", got, before)
	}
	if got := s2.Epoch(); got != 1 {
		t.Fatalf("epoch 读回应为 1，得到 %d", got)
	}
	if got := s2.Groups(); len(got) != 2 || got[0] != "G1" || got[1] != "G2" {
		t.Fatalf("Groups 应有序返回 [G1 G2]，得到 %v", got)
	}
}

// TestOpenCorruptFileFallsBackToWindow 坏 JSON、未知版本、未知 mode 都退回 window，不阻塞启动。
func TestOpenCorruptFileFallsBackToWindow(t *testing.T) {
	cases := []struct {
		name    string
		content string
		check   func(t *testing.T, s *Store)
	}{
		{
			name:    "完全不是 JSON",
			content: "{not json",
			check: func(t *testing.T, s *Store) {
				if got := s.Mode("G1"); got != ModeWindow {
					t.Fatalf("应为 window，得到 %q", got)
				}
				if got := s.Epoch(); got != 0 {
					t.Fatalf("epoch 应为 0，得到 %d", got)
				}
			},
		},
		{
			name:    "未知版本",
			content: `{"version":999,"default_mode":"always","epoch":7,"groups":{"G1":{"mode":"off","mode_since":123}}}`,
			check: func(t *testing.T, s *Store) {
				if got := s.Mode("G1"); got != ModeWindow {
					t.Fatalf("未知版本应按 window，得到 %q", got)
				}
				if got := s.Epoch(); got != 0 {
					t.Fatalf("未知版本 epoch 应清零，得到 %d", got)
				}
			},
		},
		{
			name:    "未知 mode",
			content: `{"version":1,"default_mode":"nonsense","epoch":2,"groups":{"G1":{"mode":"what","mode_since":123},"G2":{"mode":"always","mode_since":456}}}`,
			check: func(t *testing.T, s *Store) {
				if got := s.Mode("G3"); got != ModeWindow {
					t.Fatalf("未知 default_mode 应按 window，得到 %q", got)
				}
				if got := s.Mode("G1"); got != ModeWindow {
					t.Fatalf("未知群 mode 应按 window，得到 %q", got)
				}
				if got := s.Mode("G2"); got != ModeAlways {
					t.Fatalf("合法条目应保留 always，得到 %q", got)
				}
				if got := s.Epoch(); got != 2 {
					t.Fatalf("合法 epoch 应保留 2，得到 %d", got)
				}
			},
		},
		{
			name:    "版本字段缺失",
			content: `{"default_mode":"always","groups":{}}`,
			check: func(t *testing.T, s *Store) {
				if got := s.Mode("G1"); got != ModeWindow {
					t.Fatalf("缺 version 应按 window，得到 %q", got)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "takeover.json")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			s, err := Open(path)
			if err != nil {
				t.Fatalf("Open 不应因坏文件返回错误: %v", err)
			}
			tc.check(t, s)
		})
	}
}

// TestBadModeRejected 未知模式不能写进内存或文件。
func TestBadModeRejected(t *testing.T) {
	s, path := newTestStore(t)
	if err := s.SetMode("G1", Mode("paused")); err == nil {
		t.Fatal("未知模式应返回错误")
	}
	if err := s.SetAll(Mode("paused")); err == nil {
		t.Fatal("SetAll 未知模式应返回错误")
	}
	if got := s.Mode("G1"); got != ModeWindow {
		t.Fatalf("失败后模式应保持 window，得到 %q", got)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("被拒绝的写入不应产生文件，stat err=%v", err)
	}
}

// TestSetModeSameValueNoWrite 同值重复设置不写盘（管理端幂等要求）。
func TestSetModeSameValueNoWrite(t *testing.T) {
	s, path := newTestStore(t)
	if err := s.SetMode("G1", ModeWindow); err != nil {
		t.Fatalf("默认档同值设置: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("同值设置不应写盘，stat err=%v", err)
	}
	if err := s.SetMode("G1", ModeAlways); err != nil {
		t.Fatal(err)
	}
	info1, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := s.SetMode("G1", ModeAlways); err != nil {
		t.Fatalf("重复设置 always: %v", err)
	}
	info2, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info1.ModTime().Equal(info2.ModTime()) {
		t.Fatal("同值重复设置应跳过写盘，mtime 变了")
	}
}

// TestEpochBumpPersists epoch 每次递增并持久化。
func TestEpochBumpPersists(t *testing.T) {
	s, path := newTestStore(t)
	for want := int64(1); want <= 3; want++ {
		got, err := s.BumpEpoch()
		if err != nil {
			t.Fatalf("第 %d 次 BumpEpoch: %v", want, err)
		}
		if got != want {
			t.Fatalf("第 %d 次 BumpEpoch 应为 %d，得到 %d", want, want, got)
		}
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.Epoch(); got != 3 {
		t.Fatalf("重新打开后 epoch 应为 3，得到 %d", got)
	}
}

// TestDeleteRestoresDefault 删除条目后回落到默认 window。
func TestDeleteRestoresDefault(t *testing.T) {
	s, path := newTestStore(t)
	if err := s.SetMode("G1", ModeAlways); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("G1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := s.Mode("G1"); got != ModeWindow {
		t.Fatalf("删除后应为 window，得到 %q", got)
	}
	if got := s.Groups(); got != nil {
		t.Fatalf("删除后 Groups 应为 nil，得到 %v", got)
	}
	if err := s.Delete("G1"); err != nil {
		t.Fatalf("删除不存在的条目应返回 nil: %v", err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.Mode("G1"); got != ModeWindow {
		t.Fatalf("重新打开后应为 window，得到 %q", got)
	}
}

// TestSetAllAppliesToEveryGroup SetAll 覆盖 default_mode 和已有条目，新群也继承。
func TestSetAllAppliesToEveryGroup(t *testing.T) {
	s, path := newTestStore(t)
	if err := s.SetMode("G1", ModeOff); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMode("G2", ModeAlways); err != nil {
		t.Fatal(err)
	}
	before := time.Now().Unix()

	if err := s.SetAll(ModeAlways); err != nil {
		t.Fatalf("SetAll always: %v", err)
	}
	for _, g := range []string{"G1", "G2", "G_NEW"} {
		if got := s.Mode(g); got != ModeAlways {
			t.Fatalf("%s 应为 always，得到 %q", g, got)
		}
	}
	if got := s.Since("G1").Unix(); got < before {
		t.Fatalf("SetAll 后 G1 的 mode_since 应更新，得到 %d < %d", got, before)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.Mode("G1"); got != ModeAlways {
		t.Fatalf("重新打开后 G1 应为 always，得到 %q", got)
	}
	if got := s2.Mode("G_NEW"); got != ModeAlways {
		t.Fatalf("新群应继承 default_mode=always，得到 %q", got)
	}

	if err := s.SetAll(ModeWindow); err != nil {
		t.Fatalf("SetAll window: %v", err)
	}
	for _, g := range []string{"G1", "G2", "G_NEW"} {
		if got := s.Mode(g); got != ModeWindow {
			t.Fatalf("切回 window 后 %s 应为 window，得到 %q", g, got)
		}
	}
}

// TestWriteFailureKeepsMemory 写盘失败时内存不变，不出现“内存改了、重启丢了”。
func TestWriteFailureKeepsMemory(t *testing.T) {
	// 把文件路径指向一个目录：rename 到目录必然失败。
	path := t.TempDir()
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open 目录路径不应报错: %v", err)
	}
	if err := s.SetMode("G1", ModeAlways); err == nil {
		t.Fatal("写盘应失败并返回错误")
	}
	if got := s.Mode("G1"); got != ModeWindow {
		t.Fatalf("写失败后内存应保持 window，得到 %q", got)
	}
	if n, err := s.BumpEpoch(); err == nil || n != 0 {
		t.Fatalf("BumpEpoch 应失败并返回旧 epoch 0，得到 %d, err=%v", n, err)
	}
	if got := s.Epoch(); got != 0 {
		t.Fatalf("写失败后 epoch 应保持 0，得到 %d", got)
	}
}

// TestConcurrentReadWrite 读写并发不 panic、不丢写者、落盘可再次解析。
// 配合 -race 跑才能看出数据竞争。
func TestConcurrentReadWrite(t *testing.T) {
	s, path := newTestStore(t)
	modes := []Mode{ModeOff, ModeWindow, ModeAlways}

	var readers, writers sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = s.Mode("G1")
				_ = s.Since("G1")
				_ = s.Groups()
				_ = s.Epoch()
			}
		}()
	}
	for w := 0; w < 3; w++ {
		writers.Add(1)
		go func(w int) {
			defer writers.Done()
			for i := 0; i < 15; i++ {
				_ = s.SetMode("G1", modes[(w+i)%len(modes)])
				_ = s.SetMode("G2", modes[(w+i+1)%len(modes)])
				if i%5 == 0 {
					_, _ = s.BumpEpoch()
				}
			}
		}(w)
	}

	writers.Wait()
	close(stop)
	readers.Wait()

	// 落盘文件仍可解析，模式值都在枚举内。
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("并发写后重新 Open: %v", err)
	}
	for _, g := range []string{"G1", "G2"} {
		switch got := s2.Mode(g); got {
		case ModeOff, ModeWindow, ModeAlways:
		default:
			t.Fatalf("%s 落盘模式非法: %q", g, got)
		}
	}
	if got := s2.Epoch(); got < 1 || got > 9 {
		t.Fatalf("epoch 应在 1..9 之间，得到 %d", got)
	}
}
