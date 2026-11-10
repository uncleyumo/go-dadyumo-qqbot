package brain

import "testing"

// 这几个测试原本和压缩逻辑放在一起（images_test.go），
// 压缩部分搬到 internal/imgproc 之后，剩下的图片选择逻辑留在这里。

func TestPickImagesSpamTakesFirstAndLast(t *testing.T) {
	st := &groupState{}
	urls := []string{"1", "2", "3", "4", "5", "6"}
	st.addImages("u1", "阿强", urls)
	got, note, _ := st.pickImages()
	if len(got) != 2 || got[0] != "1" || got[1] != "6" {
		t.Fatalf("刷屏应只取首尾两张, got %#v", got)
	}
	if note == "" {
		t.Fatal("刷屏应给出提示")
	}
	t.Logf("刷屏提示: %s", note)
}

func TestPickImagesNormalKeepsAll(t *testing.T) {
	st := &groupState{}
	st.addImages("u1", "阿强", []string{"1", "2", "3"})
	got, note, _ := st.pickImages()
	if len(got) != 3 || note != "" {
		t.Fatalf("未刷屏应全保留且无提示, got %#v note=%q", got, note)
	}
}

func TestPickImagesDedupAndSeparateSenders(t *testing.T) {
	st := &groupState{}
	st.addImages("u1", "阿强", []string{"a", "a", "b"})
	st.addImages("u2", "小美", []string{"c"})
	got, _, _ := st.pickImages()
	if len(got) != 3 {
		t.Fatalf("应去重并分人统计, got %#v", got)
	}
}

// capImages 必须把「给模型看的图」截到上游硬上限，并且 poolSrcs 跟着截，
// 否则模型说「第 3 张有梗」时池子会存进另一张的原图。
func TestCapImagesKeepsAlignment(t *testing.T) {
	urls := []string{"u1", "u2", "u3", "u4", "u5", "u6", "u7"}
	srcs := []string{"s1", "s2", "s3", "s4", "s5", "s6", "s7"}

	gotURLs, gotSrcs := capImages(urls, srcs)
	if len(gotURLs) != maxImagesPerRequest || len(gotSrcs) != maxImagesPerRequest {
		t.Fatalf("应截到 %d 张，实际 urls=%d srcs=%d", maxImagesPerRequest, len(gotURLs), len(gotSrcs))
	}
	for i := range gotURLs {
		if gotURLs[i] != urls[i] || gotSrcs[i] != srcs[i] {
			t.Fatalf("第 %d 张错位: url=%q src=%q", i, gotURLs[i], gotSrcs[i])
		}
	}

	// 不超限时原样返回，不做无谓的复制或截断
	short := []string{"a", "b"}
	gotURLs, gotSrcs = capImages(short, short)
	if len(gotURLs) != 2 || len(gotSrcs) != 2 || gotURLs[1] != "b" {
		t.Fatalf("未超限时应原样返回，实际 %#v / %#v", gotURLs, gotSrcs)
	}
}
