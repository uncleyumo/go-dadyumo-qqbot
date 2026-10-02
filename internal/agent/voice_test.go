package agent

import (
	"testing"

	"dadyumo/internal/webhook"
)

// TestVoiceRefs 只有「平台没给转写文本、但留了音频地址」的语音才进兜底列表：
// 给了 asr_refer_text 的已经拼进正文，给了文本又转一遍是白烧 ASR。
func TestVoiceRefs(t *testing.T) {
	atts := []*webhook.Attachment{
		{ContentType: "voice", VoiceWavURL: "https://x/v1.wav"},                     // 该兜底
		{ContentType: "voice", VoiceWavURL: "https://x/v2.wav", ASRReferText: "你好"}, // 平台已转写
		{ContentType: "voice"},                              // 啥都没有
		{ContentType: "image/jpeg", URL: "https://x/i.jpg"}, // 非语音
		nil, // 平台偶尔给空指针，不许崩
	}
	refs := voiceRefs(atts, "openid-1", "张三")
	if len(refs) != 1 {
		t.Fatalf("应只挑出 1 条待兜底语音，got %d", len(refs))
	}
	if refs[0].Name != "张三" || refs[0].URL != "https://x/v1.wav" {
		t.Errorf("语音引用内容不对: %+v", refs[0])
	}
}
