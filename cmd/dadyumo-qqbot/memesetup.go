package main

import (
	"context"
	"path/filepath"
	"time"

	"dadyumo/internal/config"
	"dadyumo/internal/llm"
	"dadyumo/internal/logx"
	"dadyumo/internal/memepool"
)

// memeFlushInterval 表情包池元数据落盘周期
const memeFlushInterval = 2 * time.Minute

// setupMemePool 按配置装配表情包池与优选任务。
//
// 三种返回情形，调用方只需看 err 和 pool 是否为 nil：
//   - 功能关闭：      (nil, nil, "", nil)
//   - 装配失败：      (nil, nil, "", err) —— 调用方只打一条 Warn，文字路径照跑
//   - 正常：          (pool, curator, 落盘路径, nil)
//
// 失败绝不 panic 也绝不 os.Exit：图片只是个锦上添花的东西，
// 为了它让整个机器人起不来是本末倒置。
func setupMemePool(
	ctx context.Context,
	router *llm.Router,
	cfg config.Config,
) (*memepool.Pool, *memepool.Curator, string, error) {
	if !cfg.MemePool.Enabled {
		return nil, nil, "", nil
	}
	if ctx.Err() != nil {
		return nil, nil, "", ctx.Err()
	}

	pc := memepool.DefaultConfig()
	if cfg.MemePool.MaxPool > 0 {
		pc.MaxPool = cfg.MemePool.MaxPool
	}
	if d := cfg.MemePool.MaxResidencyDays; d > 0 {
		pc.MaxResidency = time.Duration(d) * 24 * time.Hour
	}

	st, err := memepool.NewS3(memepool.S3Config{
		Endpoint:  cfg.MemePool.MinIO.Endpoint,
		Region:    cfg.MemePool.MinIO.Region,
		Bucket:    cfg.MemePool.MinIO.Bucket,
		AccessKey: cfg.MemePool.MinIO.AccessKey,
		SecretKey: cfg.MemePool.MinIO.SecretKey,
	})
	if err != nil {
		return nil, nil, "", err
	}

	pool := memepool.New(pc, st)

	name := cfg.MemePool.MinIO.StateFile
	if name == "" {
		name = "memes.json"
	}
	poolPath := filepath.Join(cfg.Storage.DataDir, name)
	if err := pool.Load(poolPath); err != nil {
		// 恢复失败不是致命的：池子本来就是「攒着玩」的，
		// 空池启动后模型照样会用纯文本回答。
		logx.Warn("表情包池恢复失败，本次从空池开始", "path", poolPath, "err", err.Error())
	}

	// router 自己实现了 CheapestModel：优选打分只吃描述文本，
	// 派最低档的非多模态模型去干这粗活就行。
	cur := memepool.NewCurator(pool, router, router)
	cur.SetInterval(time.Duration(cfg.MemePool.OptIntervalHours) * time.Hour)
	return pool, cur, poolPath, nil
}

// memeSaveLoop 定期把池子元数据落盘。
// 图片本体在 MinIO 上不动，这里只存 id/描述/用量/评分。
func memeSaveLoop(ctx context.Context, pool *memepool.Pool, path string) {
	ticker := time.NewTicker(memeFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := pool.Save(path); err != nil {
				logx.Warn("表情包池落盘失败", "err", err.Error())
			}
		}
	}
}
