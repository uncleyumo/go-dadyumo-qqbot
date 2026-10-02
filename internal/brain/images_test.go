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