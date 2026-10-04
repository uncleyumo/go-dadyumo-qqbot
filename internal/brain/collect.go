package brain

import (
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"dadyumo/internal/config"
	"dadyumo/internal/logx"
	"dadyumo/internal/memepool"
)

// errNotDataURL 提示这串东西根本不是 data URI
var errNotDataURL = errors.New("不是 data URI")

// maxCollectPerRound 单轮最多收编几张。
//
// 不设高上限是因为池子本来只有 20 个位置，而模型很容易一头热把整轮图都报上来。
// 超出上限的多余条目直接丢掉——丢的是「多收一张」，不值得为它多跑一次上传。
const maxCollectPerRound = 3

// collectMemes 把模型标记的图收进表情包池。
//
// 为什么不额外做一次视觉判断：这些图**本来就在这一轮的上下文里**
// （decide 里抓成 dataURL 喂进去了），让模型顺手报一句「第几张、是什么」
// 就是零额外成本。再单独调一次模型看同一张图，纯属重复付费。
//
// 取图用 poolSrcs 而不是 dataURLs，两者**下标一一对应但内容不同**：
// dataURLs 是模型看到的东西（动图会拆成若干张静态帧），
// poolSrcs 是同一位置的原始那一张（会动）。
// 用 dataURLs 取图的话，池子里会多一张不会动的表情包——
// 2026-10-01 那次 GIF 被压成静图的事故换个入口重演一遍，
// 而 memepool 自己的测试抓不到（它直接调 Add，不经过这里）。
func (e *Engine) collectMemes(cfg config.Config, items []Collect, dataURLs, poolSrcs []string) {
	if len(items) == 0 || len(poolSrcs) == 0 {
		return
	}
	pool := e.poolOf(cfg)
	if pool == nil {
		return
	}

	now := time.Now()
	added := 0
	for _, it := range items {
		if added >= maxCollectPerRound {
			logx.Debug("表情包收编超过单轮上限，多余的丢掉", "上限", maxCollectPerRound)
			break
		}
		// 序号越界说明模型在编。宁可少收，也不能拿一个不存在的下标去取图。
		// 拿 dataURLs 的长度做界：它是模型看到的序号空间。
		if it.I < 1 || int(it.I) > len(dataURLs) {
			logx.Warn("模型给的图片序号越界，已忽略", "序号", it.I, "本轮图片数", len(dataURLs))
			continue
		}
		data, mime, err := decodeDataURL(poolSrcs[it.I-1])
		if err != nil {
			logx.Warn("图片解不开，收编跳过", "序号", it.I, "err", err.Error())
			continue
		}
		id, err := pool.Add(data, mime, it.D, now)
		if err != nil {
			// ErrNotFound 那类不该出现（Add 只在存储失败时返回错误），
			// 一律当「这张收不进」处理，绝不能让它把整轮决策带崩。
			logx.Warn("表情包收编失败", "序号", it.I, "err", err.Error())
			continue
		}
		added++
		logx.Info("收编了一张表情包", "id", id, "描述", it.D, "池大小", pool.Len())
	}
}

// decodeDataURL 拆 "data:image/jpeg;base64,xxxx"。
//
// 走不通就返回错误而不是空切片：空切片会让 pool.Add 报 ErrEmptyData，
// 排查时看到的是「数据是空的」，跟真正的原因（格式不对）八竿子打不着。
func decodeDataURL(s string) ([]byte, string, error) {
	const prefix = "data:"
	if !strings.HasPrefix(s, prefix) {
		return nil, "", errNotDataURL
	}
	comma := strings.IndexByte(s, ',')
	if comma < 0 {
		return nil, "", errNotDataURL
	}
	meta := s[len(prefix):comma]
	mime := strings.TrimSuffix(meta, ";base64")
	if mime == "" {
		mime = "image/jpeg"
	}
	if !strings.HasSuffix(meta, ";base64") {
		// 本项目的图一律是 base64 data URI；非 base64 的原样当明文，
		// 交给 base64 解码去报错，比在这里另开一套分支更省事。
		return nil, "", errNotDataURL
	}
	data, err := base64.StdEncoding.DecodeString(s[comma+1:])
	if err != nil {
		return nil, "", err
	}
	if len(data) == 0 {
		return nil, "", memepool.ErrEmptyData
	}
	return data, mime, nil
}
