package brain

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"dadyumo/internal/config"
	"dadyumo/internal/imgproc"
	"dadyumo/internal/logx"
)

// 视频处理各环节的超时。整体预算由调用方的 ctx 控制（videoPipelineTimeout），
// 下载是最慢的一步单独给足；ffprobe / ffmpeg 都是本地操作，很快。
const (
	videoPipelineTimeout = 90 * time.Second
	videoDownloadTimeout = 45 * time.Second
	videoStepTimeout     = 20 * time.Second

	videoUserAgent = "Mozilla/5.0 (dadyumo-qqbot)"
)

// downloadFailNote 视频没下载下来时的提示词。
// 措辞刻意含糊——模型的回复不应该暴露「机器人有一套视频处理流程」这回事。
const downloadFailNote = "有人发了条视频，但你没加载出来。" +
	"有人让你分析它就自然地搪塞过去（没流量、懒得看、喊别的群友帮忙看都行），别解释原因。"

// videoResult 一个视频进上下文的最终产物：帧的 data URI + 给模型看的一句说明。
type videoResult struct {
	frames []string
	note   string
}

// hasFFmpeg 机器上有没有 ffmpeg/ffprobe。没有就整体降级成「看不了视频」：
// 机器人照常营业，只是对视频只有一句搪塞提示。每次现查（exec.LookPath 很便宜），
// 这样装完 ffmpeg 不用重启进程也能生效。
func hasFFmpeg() bool {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return false
	}
	_, err := exec.LookPath("ffprobe")
	return err == nil
}

// handleVideos 处理攒批窗口里攒到的所有视频。
//
// 一个窗口内只认真看**最后一条**——群友连发视频基本是在连着分享，
// 最后那条才是「现在正在聊的」，前面的看了也是白看。其余的只在提示词里记一笔。
// 超过 video_max_sec 的视频不抽帧不转写，只给一句搪塞提示：
// 超长视频既费流量又烧 ASR 额度，而且群友互发的视频几乎都在半分钟以内。
func handleVideos(ctx context.Context, cfg config.Config, refs []MediaRef) videoResult {
	if len(refs) == 0 {
		return videoResult{}
	}
	last := refs[len(refs)-1]
	url := last.URL
	prefix := ""
	if len(refs) > 1 {
		prefix = fmt.Sprintf("%s连发了 %d 条视频，你只认真看了最后那条。", orDefault(last.Name, "他"), len(refs))
	}

	if !hasFFmpeg() {
		return videoResult{note: prefix + "有人发了条视频，你这边看不了视频内容。" +
			"要是有人让你分析它，就自然地搪塞过去（没流量、懒得看、喊别的群友帮忙看都行），别解释原因。"}
	}

	// 下载到独立临时目录，流程结束连同抽出的帧、音轨一起清掉
	dir, err := os.MkdirTemp("", "dadyumo-video-*")
	if err != nil {
		logx.Warn("视频临时目录创建失败", "err", err.Error())
		return videoResult{note: prefix + downloadFailNote}
	}
	defer func() { _ = os.RemoveAll(dir) }()

	videoPath := filepath.Join(dir, "input.mp4")
	if err := downloadVideo(ctx, url, videoPath, cfg.Brain.VideoMaxMB); err != nil {
		logx.Warn("视频下载失败，按看不了处理", "err", err.Error())
		return videoResult{note: prefix + downloadFailNote}
	}

	duration, durOK := probeDuration(ctx, videoPath)

	// 超长视频：直接过滤。这是省钱闸门也是防滥用闸门——
	// 群友把一部电影发进来再 @ 机器人「帮我总结一下」，就该是这个提示接住它。
	if durOK && duration > float64(cfg.Brain.VideoMaxSec) {
		logx.Info("视频超长，已跳过", "秒", int(duration), "上限", cfg.Brain.VideoMaxSec)
		return videoResult{note: prefix + fmt.Sprintf(
			"有人发了条超过 %d 秒的长视频（约 %.0f 秒），你压根没点开。"+
				"要是有人让你分析它或者问你里面是什么，就自然地搪塞过去——"+
				"没流量、懒得看、或者喊别的群友帮忙看都行。别承认自己看不了，也别解释原因。",
			cfg.Brain.VideoMaxSec, duration)}
	}

	vision := hasVisionModel(cfg)
	var frames []string
	if vision && cfg.Brain.VideoFrames > 0 {
		frames = extractFrames(ctx, videoPath, dir, duration, cfg.Brain.VideoFrames, cfg.Brain.VideoFrameSide)
	}

	audioPart, nSpeech := transcribeTrack(ctx, videoPath, dir, cfg)

	durDesc := "一段"
	if durOK {
		durDesc = fmt.Sprintf("%.0f 秒", duration)
	}
	var note string
	switch {
	case vision && len(frames) > 0:
		note = prefix + fmt.Sprintf("有人发了条 %s 的视频。你看了画面里的 %d 个瞬间%s。",
			durDesc, len(frames), audioPart)
	case vision:
		note = prefix + fmt.Sprintf("有人发了条 %s 的视频，但画面没加载出来%s。"+
			"有人让你分析画面就自然搪塞过去（没流量、懒得看）。", durDesc, audioPart)
	default:
		note = prefix + fmt.Sprintf("有人发了条 %s 的视频，你看不了画面%s。"+
			"要是有人让你分析画面，就自然搪塞过去（没流量、懒得看）。", durDesc, audioPart)
	}
	if len(frames) > 0 {
		logx.Info("视频已处理", "时长", durDesc, "帧", len(frames), "转写字数", nSpeech)
	}
	return videoResult{frames: frames, note: note}
}

// downloadVideo 把视频下载到本地，maxMB 是体积上限。
// QQ 多媒体 URL 和图片一样可能是协议相对的（//multimedia...）。
func downloadVideo(ctx context.Context, url, dst string, maxMB int) error {
	if strings.HasPrefix(url, "//") {
		url = "https:" + url
	}
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return fmt.Errorf("非 HTTP 视频 URL: %s", truncate(url, 60))
	}
	dctx, cancel := context.WithTimeout(ctx, videoDownloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(dctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", videoUserAgent)
	rsp, err := (&http.Client{}).Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = rsp.Body.Close() }()
	if rsp.StatusCode != http.StatusOK {
		return fmt.Errorf("视频下载 HTTP %d", rsp.StatusCode)
	}

	limit := int64(maxMB) << 20
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	n, err := io.Copy(f, io.LimitReader(rsp.Body, limit+1))
	if err != nil {
		return err
	}
	if n > limit {
		return fmt.Errorf("视频超过 %d MB 上限", maxMB)
	}
	if n == 0 {
		return fmt.Errorf("视频内容为空")
	}
	return nil
}

// probeDuration 用 ffprobe 拿视频时长（秒）。失败不拦流程：
// 能下载下来的多半是正常小视频，探测不到就当未知继续走。
func probeDuration(ctx context.Context, path string) (float64, bool) {
	c, cancel := context.WithTimeout(ctx, videoStepTimeout)
	defer cancel()
	out, err := exec.CommandContext(c, "ffprobe",
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		path).Output()
	if err != nil {
		return 0, false
	}
	d, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}

// extractFrames 均匀抽 n 帧，压到长边 side 以内，返回 data URI 列表。
//
// 用 fps=n/duration 实现均匀采样：fps 滤镜从 0 秒开始按固定间隔出帧，
// 比如 20 秒的视频抽 2 帧（fps=0.1）就落在 0 秒和 10 秒——开头和中间，
// 对「这视频在演什么」来说够用了。ffmpeg 的错误码不能全信（警告也非零退出），
// 所以以实际产出的文件为准。
func extractFrames(ctx context.Context, videoPath, dir string, duration float64, n, side int) []string {
	rate := float64(n) / 10.0 // 探测不到时长时按 10 秒的假设抽
	if duration > 0 {
		rate = float64(n) / duration
	}
	if rate <= 0 {
		rate = 0.1
	}
	c, cancel := context.WithTimeout(ctx, videoStepTimeout)
	defer cancel()
	_ = exec.CommandContext(c, "ffmpeg",
		"-y", "-i", videoPath,
		"-vf", fmt.Sprintf("fps=%.4f,scale=%d:%d:force_original_aspect_ratio=decrease", rate, side, side),
		"-frames:v", strconv.Itoa(n),
		"-q:v", "7",
		filepath.Join(dir, "frame_%03d.jpg")).Run()

	matches, _ := filepath.Glob(filepath.Join(dir, "frame_*.jpg"))
	if len(matches) == 0 {
		return nil
	}
	sort.Strings(matches)
	if len(matches) > n {
		matches = matches[:n]
	}
	var out []string
	for _, p := range matches {
		uri, err := dataURIFromFile(p, side)
		if err != nil {
			continue
		}
		out = append(out, uri)
	}
	return out
}

// extractAudio 抽出 16kHz 单声道 wav 音轨。
// 选 wav 而不是 m4a：PCM 什么转写服务都认，两分钟也就 4MB，不值得为省这点
// 体积去赌各家的容器格式支持列表。ffmpeg 偶尔对无音轨视频报错，正常，忽略。
func extractAudio(ctx context.Context, videoPath, dir string) string {
	audioPath := filepath.Join(dir, "audio.wav")
	c, cancel := context.WithTimeout(ctx, videoStepTimeout)
	defer cancel()
	_ = exec.CommandContext(c, "ffmpeg",
		"-y", "-i", videoPath, "-vn", "-ac", "1", "-ar", "16000", audioPath).Run()
	if st, err := os.Stat(audioPath); err != nil || st.Size() == 0 {
		return ""
	}
	return audioPath
}

// transcribeTrack 抽音轨并转写。返回 (给提示词拼的半句话, 转出的字数)。
// 字数 -1 表示音轨没抽出来/转写失败（提示词用「没听清」降级），
// 0 表示确实没人说话，>0 是正常转写。失败不重试——免费额度经不起放大。
func transcribeTrack(ctx context.Context, videoPath, dir string, cfg config.Config) (string, int) {
	if cfg.ASR.Provider == "" || strings.EqualFold(cfg.ASR.Provider, "none") {
		return "，里面的声音你没去听", -1
	}
	audioPath := extractAudio(ctx, videoPath, dir)
	if audioPath == "" {
		return "，视频里的声音你没听清", -1
	}
	text, err := transcribeAudio(ctx, audioPath, cfg.ASR)
	if err != nil {
		logx.Debug("视频音轨转写失败", "err", err.Error())
		return "，视频里的声音你没听清", -1
	}
	// SenseVoice 偶尔对纯音乐/环境音返回一串标点，当没人说话处理
	if strings.Trim(text, "。，！？、,!? ") == "" {
		return "，视频里没有人说话", 0
	}
	// 超长转写先过一道便宜模型压缩（短于阈值时原样返回，不发请求）
	return "，听到里面说「" + compactTranscript(ctx, cfg.Compact, text) + "」", len([]rune(text))
}

// dataURIFromFile 本地图片文件转 data URI，超限时复用图片压缩管道兜底。
func dataURIFromFile(path string, maxSide int) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	ct := "image/jpeg"
	if maxSide > 0 {
		if out, outCT, ok := imgproc.Compress(raw, maxSide); ok {
			raw, ct = out, outCT
		}
	}
	return "data:" + ct + ";base64," + base64.StdEncoding.EncodeToString(raw), nil
}
