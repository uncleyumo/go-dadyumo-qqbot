package imgproc

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// 图片下载上限必须容得下群里正常发的大图。
//
// 2026-10-02：有人怀疑一张 2.6MB 的图是被体积上限卡掉的，机器人因此说
// 「我这儿看不见」。查证后不是——那张图成功进了上下文，机器人还答出了
// 「像哪个游戏里的」，说明它看见了内容。6MB 的上限本来就够。
//
// 但那次查证本身很费劲：fetchImages 把失败记在 Debug，生产跑 Info，
// 抓图失败在日志里完全不留痕，只能靠猜。这条测试和那次提级一起防复发。
func TestMaxBytesAllowsNormalGroupPhotos(t *testing.T) {
	// 群里常见的原图体积：2.6MB 那次实测的量级
	const wantAtLeast = 5 << 20
	if MaxBytes < wantAtLeast {
		t.Errorf("下载上限 %d 字节太小：群里 2~3MB 的手机截图/摄影原图会被整张丢掉，"+
			"模型只能收到「有人发了张图」，比图糊了糟得多（流量不值钱，压缩才值钱）",
			MaxBytes)
	}
}

// 一张超过上限的图必须被明确拒绝，而不是截断成半张。
func TestFetchRejectsOversizedImage(t *testing.T) {
	// 造一个刚好超过上限的响应：不需要是真图，内容-Type 对就行，
	// 这里验的是体积闸门本身。
	body := make([]byte, MaxBytes+1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	if _, _, err := Fetch(srv.URL, 1024); err == nil {
		t.Error("超过上限的图片应该报错，不能悄悄塞进来")
	}
}

// 刚好卡在上限下的图要能正常取回来。
func TestFetchAcceptsJustUnderLimit(t *testing.T) {
	raw := makePNG(600, 400)
	body := make([]byte, MaxBytes-len(raw))
	copy(body, raw)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	got, ct, err := Fetch(srv.URL, 1024)
	if err != nil {
		t.Fatalf("未超上限的图应该能取回: %v", err)
	}
	if len(got) == 0 || ct == "" {
		t.Error("取回来的内容不该是空的")
	}
}

// 上限写成了多少，日志里就得说多少——排查时不能靠猜。
func TestOversizeErrorMentionsTheLimit(t *testing.T) {
	body := make([]byte, MaxBytes+1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	_, _, err := Fetch(srv.URL, 1024)
	if err == nil {
		t.Fatal("应报错")
	}
	want := strconv.Itoa(MaxBytes >> 20) + " MB"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("报错里应写明实际上限 %s，否则排查时得回去翻代码，got: %v", want, err)
	}
}