package brain

import (
	"testing"
	"time"
)

func TestImpulseAtMeIsHigh(t *testing.T) {
	got := ComputeImpulse(ImpulseInput{AtMe: true})
	if got.Score < 0.85 {
		t.Fatalf("被 @ 时冲动值应接近 1，实际 %.2f", got.Score)
	}
}

func TestImpulseNothingRelevantIsZero(t *testing.T) {
	got := ComputeImpulse(ImpulseInput{})
	if got.Score != 0 {
		t.Fatalf("没有任何触发因素时冲动值应为 0，实际 %.2f", got.Score)
	}
}

func TestImpulseConsecutivePenalty(t *testing.T) {
	base := ComputeImpulse(ImpulseInput{AtMe: true, Consecutive: 0}).Score
	two := ComputeImpulse(ImpulseInput{AtMe: true, Consecutive: 2}).Score
	four := ComputeImpulse(ImpulseInput{AtMe: true, Consecutive: 4}).Score
	if two >= base {
		t.Fatalf("连续发言应有惩罚: base=%.2f two=%.2f", base, two)
	}
	if four >= two {
		t.Fatalf("连续更多轮惩罚应更重: two=%.2f four=%.2f", two, four)
	}
}

func TestImpulseJustSpokePenalty(t *testing.T) {
	far := ComputeImpulse(ImpulseInput{Question: true, SinceSpeak: 10 * time.Minute}).Score
	near := ComputeImpulse(ImpulseInput{Question: true, SinceSpeak: 3 * time.Second}).Score
	if near != 0 {
		t.Fatalf("刚说完就应被压到 0，实际 %.2f", near)
	}
	if far <= near {
		t.Fatalf("隔了很久再说话不应受罚: far=%.2f near=%.2f", far, near)
	}
}

func TestImpulseClamped(t *testing.T) {
	got := ComputeImpulse(ImpulseInput{
		AtMe: true, NameCalled: true, FromMaster: true,
		Question: true, ReplyToBot: true, NewCount: 9, Idle: time.Hour,
	})
	if got.Score > 1 {
		t.Fatalf("冲动值应被夹在 1 以内，实际 %.2f", got.Score)
	}
	got2 := ComputeImpulse(ImpulseInput{Consecutive: 9, SinceSpeak: time.Second})
	if got2.Score < 0 {
		t.Fatalf("冲动值不应为负，实际 %.2f", got2.Score)
	}
}

func TestLooksLikeQuestion(t *testing.T) {
	cases := map[string]bool{
		"你在干嘛呢":   true,
		"这东西怎么用？": true,
		"是不是这样":   true,
		"今天天气不错":  false,
		"我刚吃完饭":   false,
	}
	for text, want := range cases {
		if got := LooksLikeQuestion(text); got != want {
			t.Fatalf("%q 期望 %v，实际 %v", text, want, got)
		}
	}
}
