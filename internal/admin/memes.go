package admin

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"dadyumo/internal/logx"
)

// 表情包池的管理端接口。
//
// 只做两件事：**看**（按当前好感度排序的列表）与**删**（手动清一张）。
// 刻意不做「改」——描述是模型写的，人改了下次优选又会被覆盖回去，
// 给一个「改了但会被悄悄改掉」的输入框是纯粹的误导。

// handleMemeList 返回池内表情包，按此刻好感度排序（= 模型会看到的顺序）。
//
// 不返回图片直链：管理端走 https，而 MinIO 通常只有 http，
// 浏览器会当混合内容直接拦掉（右键打开却能用，因为那是顶层导航不受这条管）。
// 预览改走同源的 /api/meme/img 代理，见 handleMemeImage。
func (s *Server) handleMemeList(w http.ResponseWriter, r *http.Request) {
	if s.memes == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": false, "memes": []any{}})
		return
	}
	ranked := s.memes.Ranked(time.Now())
	items := make([]map[string]any, 0, len(ranked))
	for _, m := range ranked {
		items = append(items, map[string]any{
			"id": m.ID, "descr": m.Descr, "uses": m.Uses, "quality": m.Quality,
			"affection": m.Affection, "rank": m.Rank, "expires_in": m.ExpiresIn,
			"added_at": m.AddedAt, "last_used": m.LastUsed,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "enabled": true, "max_pool": s.memes.MaxPool(), "memes": items,
	})
}

// handleMemeImage 把池里的图原样吐出来给管理端预览。
//
// 为什么不让浏览器直接去拉 MinIO：管理端是 https，MinIO 是 http，
// 混合内容一律被拦；给 MinIO 套 TLS 又得动反向代理和证书。
// 同源代理一步到位，还顺带让「桶要不要开 Public」这件事跟预览彻底解耦。
func (s *Server) handleMemeImage(w http.ResponseWriter, r *http.Request) {
	if s.memes == nil {
		http.Error(w, "表情包池未启用", http.StatusBadRequest)
		return
	}
	id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err != nil || id == 0 {
		http.Error(w, "缺少 id", http.StatusBadRequest)
		return
	}
	data, mime, err := s.memes.Data(id)
	if err != nil {
		http.Error(w, "取图失败", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", mime)
	// 预览图会随手动删改变化，但同一张图的字节是内容寻址的、不会变，
	// 所以可以放心让它在浏览器里留一天。
	w.Header().Set("Cache-Control", "private, max-age=86400")
	_, _ = w.Write(data)
}

// handleMemeDelete 手动删掉一张（连同 MinIO 上的对象）。
func (s *Server) handleMemeDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "msg": "method not allowed"})
		return
	}
	if s.memes == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "msg": "表情包池未启用"})
		return
	}
	var body struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "msg": "缺少 id"})
		return
	}
	descr := ""
	if m, ok := s.memes.Get(body.ID); ok {
		descr = m.Descr
	}
	if err := s.memes.Remove(body.ID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "msg": err.Error()})
		return
	}
	s.persistMemes()
	logx.Info("管理端删除表情包", "id", body.ID, "描述", descr, "剩余", s.memes.Len())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
