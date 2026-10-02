package brain

import "strings"

// MoodLevel 情绪等级
const (
	MoodNone   MoodLevel = iota // 正常，可以嘴臭
	MoodLow                     // 轻微低落，收着点
	MoodHigh                    // 明显难受，必须切换共情
	MoodCrisis                  // 危机信号，绝对不能有任何调侃
)

// MoodLevel 情绪等级类型
type MoodLevel int

func (m MoodLevel) String() string {
	switch m {
	case MoodLow:
		return "轻微低落"
	case MoodHigh:
		return "明显难受"
	case MoodCrisis:
		return "危机信号"
	}
	return "正常"
}

// 危机信号：出现这些词时，任何玩笑都是不可接受的
var crisisWords = []string{
	"不想活", "活不下去", "活着没意思", "自杀", "轻生", "自残", "结束生命",
	"想死", "去死", "没意义了", "撑不下去了", "不想活了",
}

// 明显难受：失业、失恋、生病、变故、崩溃
var heavyWords = []string{
	"失业", "被裁", "裁员", "开除", "找不到工作", "面试挂", "offer没了", "被拒了",
	"分手", "失恋", "离婚", "被甩",
	"挂科", "没考上", "落榜", "考研失败", "考砸",
	"生病", "住院", "手术", "确诊", "进医院", "化疗",
	"抑郁", "焦虑", "崩溃", "想哭", "难受", "失眠", "扛不住", "撑不住", "压力大",
	"去世", "走了", "逝世", "过世", "亲人", "抢救",
	"破产", "亏完了", "被骗", "被坑", "血亏",
}

// 轻微低落
var lowWords = []string{
	"烦", "累", "emo", "丧", "摆烂", "没劲", "心累", "好难", "太难了", "无语",
	"倒霉", "晦气", "又失败了", "搞砸", "搞砸了", "背", "水逆",
}

// 玩梗信号：命中这些说明大概率是在开玩笑，不该被判定为负面情绪
var jokeWords = []string{
	"哈哈", "233", "笑死", "doge", "整活", "绷不住", "乐", "绝了", "太6了",
	"开玩笑", "狗头", "滑稽", "草", "hhhh", "lol", "emm",
}

// MoodSignal 情绪检测结果
type MoodSignal struct {
	Level    MoodLevel
	Kind     string
	Keywords []string
	Joking   bool
}

// Detect 关键词情绪检测（L0 层，零成本）。
// 这是「什么时候该收起嘴臭」的第一道闸，宁可误判为有情绪，也不要漏判。
func Detect(text string) MoodSignal {
	if strings.TrimSpace(text) == "" {
		return MoodSignal{}
	}
	lower := strings.ToLower(text)
	joking := false
	var jokeHits []string
	for _, w := range jokeWords {
		if strings.Contains(lower, w) {
			joking = true
			jokeHits = append(jokeHits, w)
		}
	}

	hits := func(words []string) []string {
		var out []string
		for _, w := range words {
			if strings.Contains(lower, w) {
				out = append(out, w)
			}
		}
		return out
	}

	// 危机词不受玩梗信号影响：宁可错判，不可漏判
	if hs := hits(crisisWords); len(hs) > 0 {
		return MoodSignal{Level: MoodCrisis, Kind: "危机", Keywords: hs, Joking: joking}
	}
	if hs := hits(heavyWords); len(hs) > 0 {
		if joking && len(hs) == 1 {
			// 只有单个命中且明显在玩梗，降级处理，但仍要求收着点
			return MoodSignal{Level: MoodLow, Kind: "疑似玩梗", Keywords: hs, Joking: true}
		}
		return MoodSignal{Level: MoodHigh, Kind: "明显难受", Keywords: hs, Joking: joking}
	}
	if hs := hits(lowWords); len(hs) > 0 {
		if joking {
			return MoodSignal{Level: MoodNone, Keywords: hs, Joking: true}
		}
		return MoodSignal{Level: MoodLow, Kind: "轻微低落", Keywords: hs}
	}
	return MoodSignal{}
}
