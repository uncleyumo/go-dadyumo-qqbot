// Package takeover 管理人工接管的群级模式（off / window / always）及其持久化。
//
// 本包刻意不依赖 qqapi / brain / admin：brain 通过 AnchorSource 窄接口拿锚点快照，
// 业务依赖留在 main 的适配器里。模式落盘在 data/takeover.json，不进 config.Config。
package takeover

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	"dadyumo/internal/logx"
)

// Mode 是一档接管模式。
type Mode string

const (
	// ModeOff 关闭人工接管：机器人照常自动聊天，管理端不能手动发言。
	ModeOff Mode = "off"
	// ModeWindow 窗口打开时接管：有存活租约才暂停 AI，默认档。
	ModeWindow Mode = "window"
	// ModeAlways 接管常开：AI 一直暂停，直到模式被手动改掉。
	ModeAlways Mode = "always"
)

// fileVersion 是当前能识别的文件版本，不认识的版本按默认 window 运行。
const fileVersion = 1

// errNoStore 表示在 nil Store 上调用了写方法。
var errNoStore = errors.New("takeover: store 未初始化")

// valid 判断模式是否是三档之一；落盘和写接口都先过它，防止垃圾值进文件。
func (m Mode) valid() bool {
	return m == ModeOff || m == ModeWindow || m == ModeAlways
}

// Store 是接管模式的进程内副本，磁盘文件是它的持久化。
//
// mu 护内存，saveMu 只串行化写者：fsync 时不持 mu，AI 闸门的 Mode() 只拿短读锁。
type Store struct {
	mu     sync.RWMutex
	saveMu sync.Mutex
	path   string
	data   fileData
}

// fileData 是 takeover.json 的完整结构。
type fileData struct {
	Version     int                   `json:"version"`
	DefaultMode Mode                  `json:"default_mode"`
	Epoch       int64                 `json:"epoch"`
	Groups      map[string]groupEntry `json:"groups"`
}

// groupEntry 是单个群的模式与生效时刻。
type groupEntry struct {
	Mode      Mode  `json:"mode"`
	ModeSince int64 `json:"mode_since"`
}

// defaultData 返回缺省状态：window 档、epoch 0、无群条目。
func defaultData() fileData {
	return fileData{
		Version:     fileVersion,
		DefaultMode: ModeWindow,
		Epoch:       0,
		Groups:      map[string]groupEntry{},
	}
}

// clone 深拷贝一份数据，供写者在锁外拿快照。
func (fd fileData) clone() fileData {
	out := fd
	out.Groups = make(map[string]groupEntry, len(fd.Groups))
	for g, e := range fd.Groups {
		out.Groups[g] = e
	}
	return out
}

// Open 读取模式文件；缺省、损坏、版本不认识时都按 window 运行，不阻塞启动。
//
// 文件不存在时不会创建：第一次写入才落盘，避免只读启动动文件系统。
// 返回的 error 恒为 nil，保留签名是为了 main 的装配形态与将来可能的升级路径。
func Open(path string) (*Store, error) {
	s := &Store{path: path, data: defaultData()}
	if path == "" {
		logx.Warn("接管模式文件路径为空，按默认 window 运行")
		return s, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			logx.Warn("接管模式文件不存在，按默认 window 运行", "path", path)
		} else {
			logx.Warn("接管模式文件读取失败，按默认 window 运行", "path", path, "err", err.Error())
		}
		return s, nil
	}
	s.data = parseData(path, b)
	return s, nil
}

// parseData 解析文件内容，任何异常都退回默认 window 并记 Warn。
func parseData(path string, b []byte) fileData {
	var fd fileData
	if err := json.Unmarshal(b, &fd); err != nil {
		logx.Warn("接管模式文件无法解析，按默认 window 运行", "path", path, "err", err.Error())
		return defaultData()
	}
	if fd.Version != fileVersion {
		logx.Warn("接管模式文件版本不认识，按默认 window 运行",
			"path", path, "文件版本", fd.Version, "支持版本", fileVersion)
		return defaultData()
	}
	if fd.Groups == nil {
		fd.Groups = map[string]groupEntry{}
	}
	if fd.Epoch < 0 {
		fd.Epoch = 0
	}
	if !fd.DefaultMode.valid() {
		logx.Warn("接管模式文件的 default_mode 不认识，按 window 处理",
			"path", path, "mode", string(fd.DefaultMode))
		fd.DefaultMode = ModeWindow
	}
	for g, e := range fd.Groups {
		if g == "" {
			logx.Warn("接管模式文件里有空群 id，已丢弃", "path", path)
			delete(fd.Groups, g)
			continue
		}
		if !e.Mode.valid() {
			logx.Warn("接管模式文件里有不认识的群模式，按 window 处理",
				"path", path, "group", g, "mode", string(e.Mode))
			e.Mode = ModeWindow
		}
		if e.ModeSince < 0 {
			e.ModeSince = 0
		}
		fd.Groups[g] = e
	}
	return fd
}

// Mode 返回该群当前生效的模式；没有条目时用 default_mode（缺省 window）。
// nil-safe：未注入 Store 时按 window 处理。
func (s *Store) Mode(groupID string) Mode {
	if s == nil {
		return ModeWindow
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if e, ok := s.data.Groups[groupID]; ok && e.Mode.valid() {
		return e.Mode
	}
	if s.data.DefaultMode.valid() {
		return s.data.DefaultMode
	}
	return ModeWindow
}

// Since 返回该群当前模式生效的时刻；没有条目或时间戳缺失时返回零值。
// nil-safe。
func (s *Store) Since(groupID string) time.Time {
	if s == nil {
		return time.Time{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.data.Groups[groupID]
	if !ok || e.ModeSince <= 0 {
		return time.Time{}
	}
	return time.Unix(e.ModeSince, 0)
}

// Groups 返回文件里有显式条目的群 id，已排序；没有条目返回 nil。
// nil-safe。
func (s *Store) Groups() []string {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.data.Groups) == 0 {
		return nil
	}
	out := make([]string, 0, len(s.data.Groups))
	for g := range s.data.Groups {
		out = append(out, g)
	}
	sort.Strings(out)
	return out
}

// Epoch 返回当前 epoch；模式或租约被整体重置时由 BumpEpoch 递增。
// nil-safe。
func (s *Store) Epoch() int64 {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.Epoch
}

// SetMode 设置单个群的模式并落盘。
//
// 同值重复设置不写盘（管理端幂等要求）；写盘失败返回 error，内存不变。
func (s *Store) SetMode(groupID string, m Mode) error {
	if groupID == "" {
		return errors.New("takeover: groupID 不能为空")
	}
	if !m.valid() {
		return fmt.Errorf("takeover: 未知模式 %q", string(m))
	}
	now := time.Now().Unix()
	return s.apply(func(fd *fileData) bool {
		if e, ok := fd.Groups[groupID]; ok {
			if e.Mode == m {
				return false
			}
			e.Mode = m
			e.ModeSince = now
			fd.Groups[groupID] = e
			return true
		}
		if fd.DefaultMode == m {
			// 没有条目、默认档已经是它：生效模式没变，不制造冗余条目。
			return false
		}
		fd.Groups[groupID] = groupEntry{Mode: m, ModeSince: now}
		return true
	})
}

// SetAll 把 default_mode 和所有已有群条目都改成 m。
//
// 已有条目一起改写而不是清空：这样 Since 对每个群都给出本次切换的时刻，
// always 的 ai_paused_since 不会变成零值；没出现过的新群仍继承 default_mode。
func (s *Store) SetAll(m Mode) error {
	if !m.valid() {
		return fmt.Errorf("takeover: 未知模式 %q", string(m))
	}
	now := time.Now().Unix()
	return s.apply(func(fd *fileData) bool {
		changed := fd.DefaultMode != m
		fd.DefaultMode = m
		for g, e := range fd.Groups {
			if e.Mode == m {
				continue
			}
			e.Mode = m
			e.ModeSince = now
			fd.Groups[g] = e
			changed = true
		}
		return changed
	})
}

// Delete 删除单个群的条目，之后它回落到 default_mode。条目不存在时不写盘。
func (s *Store) Delete(groupID string) error {
	return s.apply(func(fd *fileData) bool {
		if _, ok := fd.Groups[groupID]; !ok {
			return false
		}
		delete(fd.Groups, groupID)
		return true
	})
}

// BumpEpoch 递增 epoch 并落盘，返回新值；写盘失败时内存不变并返回旧值。
func (s *Store) BumpEpoch() (int64, error) {
	if s == nil {
		return 0, errNoStore
	}
	s.saveMu.Lock()
	defer s.saveMu.Unlock()

	s.mu.RLock()
	snap := s.data.clone()
	s.mu.RUnlock()

	snap.Epoch++
	if err := s.write(snap); err != nil {
		return s.Epoch(), err
	}
	s.mu.Lock()
	s.data = snap
	s.mu.Unlock()
	return snap.Epoch, nil
}

// apply 在 saveMu 下对快照执行 mutate，落盘成功后才替换内存。
//
// 顺序固定：saveMu 串行化写者 → 短读锁取快照 → 锁外写临时文件 + fsync + rename
// → 成功后才改内存。mutate 返回 false 表示无需写盘。
func (s *Store) apply(mutate func(*fileData) bool) error {
	if s == nil {
		return errNoStore
	}
	s.saveMu.Lock()
	defer s.saveMu.Unlock()

	s.mu.RLock()
	snap := s.data.clone()
	s.mu.RUnlock()

	if !mutate(&snap) {
		return nil
	}
	if err := s.write(snap); err != nil {
		return err
	}
	s.mu.Lock()
	s.data = snap
	s.mu.Unlock()
	return nil
}

// write 原子写入模式文件：临时文件 + fsync + rename，权限 0600。
// Windows 上目录不能 fsync，跳过父目录同步（与 memory 包一致）。
func (s *Store) write(fd fileData) error {
	b, err := json.MarshalIndent(fd, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".takeover-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	// 必须 fsync：rename 只保证目录项，不保证文件内容落盘。
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if runtime.GOOS != "windows" {
		syncDir(dir)
	}
	return nil
}

// syncDir fsync 父目录；失败只记日志，不让一次目录同步把写操作判死。
func syncDir(dir string) {
	f, err := os.Open(dir)
	if err != nil {
		return
	}
	if err := f.Sync(); err != nil {
		logx.Warn("接管模式目录 fsync 失败", "dir", dir, "err", err.Error())
	}
	_ = f.Close()
}

// AnchorSnapshot 是一条用户消息锚点的只读副本，供管理端展示与发送前预检。
type AnchorSnapshot struct {
	MsgID        string `json:"msg_id"`
	SenderOpenID string `json:"sender_openid"`
	SenderName   string `json:"sender_name"`
	Used         int    `json:"used"`
	MaxUse       int    `json:"max_use"`
	Alive        bool   `json:"alive"`      // 仍在 5 分钟 TTL 且 used < max_use
	ExpiresAt    int64  `json:"expires_at"` // unix 秒
}

// AnchorSource 由 main 用 qqapi.Client 适配实现，brain/admin 只依赖这个窄接口。
//
// 本包不 import qqapi：锚点池真正发消息仍在 qqapi.Client.SendGroupTo 的锁内
// PickAndReserve 重新选点，快照只是展示与预检，额度以实际占用为准。
type AnchorSource interface {
	// PickAnchor 按群挑一条挂靠锚点；preferSender 非空时优先此人的最新锚点。
	// matched=false 表示退回了群里最新那条（不是他要的人）。
	PickAnchor(groupOpenID, preferSender string) (a AnchorSnapshot, matched, ok bool)
	// ListAnchors 返回池里保留的全部锚点（最新在前，含过期/用满）与其中存活条数。
	ListAnchors(groupOpenID string) (all []AnchorSnapshot, alive int)
	// AnchorGroups 返回锚点池里有记录的群（含已无存活锚点的群），供 EndAll 用。
	AnchorGroups() []string
}
