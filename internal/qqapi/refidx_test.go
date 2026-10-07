package qqapi

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/tencent-connect/botgo/dto"
)

// refIdx 必须跟着锚点一起取出来，否则 message_reference 永远填不上。
func TestAnchorCarriesRefIdx(t *testing.T) {
	p := NewAnchorPool(20)
	p.Add("g1", "m1", "openid-a", "张三", "REFIDX_abc==")
	p.Add("g1", "m2", "openid-b", "李四", "")

	msgID, refIdx, _, ok, _ := p.PickAndReserve("g1", "openid-a")
	if !ok {
		t.Fatal("应能取到锚点")
	}
	if msgID != "m1" {
		t.Errorf("应按发送人选中 m1，实际 %q", msgID)
	}
	if refIdx != "REFIDX_abc==" {
		t.Errorf("refIdx 应原样带出，实际 %q", refIdx)
	}
}

// refIdx 为空是常态，不是错误：那条锚点就是没有引用 id。
func TestAnchorRefIdxMayBeEmpty(t *testing.T) {
	p := NewAnchorPool(20)
	p.Add("g1", "m1", "openid-a", "张三", "")
	_, refIdx, _, ok, _ := p.PickAndReserve("g1", "openid-a")
	if !ok {
		t.Fatal("缺 refIdx 不该影响锚点可用性")
	}
	if refIdx != "" {
		t.Errorf("应为空，实际 %q", refIdx)
	}
}

// 同一条消息补登时把迟到的 refIdx 补上。
//
// 平台可能第一条事件里还没有 message_scene（附件先到、正文后到），
// 补登是既有行为，这里要求 refIdx 也跟着补。
func TestAnchorBackfillsRefIdxOnRedelivery(t *testing.T) {
	p := NewAnchorPool(20)
	p.Add("g1", "m1", "", "", "") // 先到：还不知道是谁，也没有 refIdx
	p.Add("g1", "m1", "openid-a", "张三", "REFIDX_late==")

	_, refIdx, _, _, _ := p.PickAndReserve("g1", "openid-a")
	if refIdx != "REFIDX_late==" {
		t.Errorf("补登时应把迟到的 refIdx 补上，实际 %q", refIdx)
	}
}

// message_reference 与 msg_id 可以共存（官方示例就是两者并存），
// 但 **refIdx 为空时整个字段必须消失**。
//
// 这条是本轮最容易出事的地方：dto.MessageReference 的 MessageID 带
// `json:"message_id"` 且**没有 omitempty**，只要给了非 nil 的指针，
// 空字符串也会被序列化成 {"message_id":""}。平台拿到一个空引用会拒，
// 表现为「消息发不出去」，而本地毫无线索。
//
// 这里直接调 post 的构造逻辑（buildGroupMessage）而不是手搓 dto 对象：
// 手搓对象的话，「post 里那个 if refIdx != "" 的判断写错了」这条变异
// 是测不出来的——之前就踩过，测试全绿而实现是错的。
func TestPostOmitsMessageReferenceWhenRefIdxEmpty(t *testing.T) {
	b := marshalMsg(t, buildGroupMessage("在吗", "ROBOT1.0_a", "", 1))
	if strings.Contains(b, "message_reference") {
		t.Errorf("没有 refIdx 时 message_reference 必须整个消失，实际 %s", b)
	}
	if !strings.Contains(b, `"msg_id":"ROBOT1.0_a"`) {
		t.Errorf("被动回复必需的 msg_id 必须保留，实际 %s", b)
	}
}

func TestPostIncludesMessageReferenceWhenRefIdxPresent(t *testing.T) {
	b := marshalMsg(t, buildGroupMessage("在吗", "ROBOT1.0_a", "REFIDX_b==", 1))
	// SDK 的 MessageReference 里 ignore_get_message_error 没有 omitempty，
	// 所以序列化会多带一个 false 字段——只断言关键的那一项。
	if !strings.Contains(b, `"message_id":"REFIDX_b=="`) {
		t.Errorf("有 refIdx 时应带上 message_reference.message_id，实际 %s", b)
	}
	if !strings.Contains(b, `"msg_id":"ROBOT1.0_a"`) {
		t.Errorf("引用与被动 msg_id 必须共存（官方示例即两者并存），实际 %s", b)
	}
}

func marshalMsg(t *testing.T, msg *dto.MessageToCreate) string {
	t.Helper()
	b, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	return string(b)
}

// TestClientDropsRefIdxWhenNotMatched 客户端必须在 !matched 时丢掉 refIdx。
//
// 为什么用源码检查而不用运行时测试：sendGroup 要走真实 HTTP 才能观察到
// 发出去的 JSON，代价远大于收益（项目里 gates_test.go 记着同一类权衡）。
//
// 为什么这条必须钉死：matched 算错了不会有任何编译错误或 panic，
// 只是**引用气泡里静悄悄出现一个陌生人**——生产上只能靠人眼发现。
// 变异测试确认过：把 sendGroup 里的 `if !matched { refIdx = "" }` 删掉，
// 本包所有测试依然通过。
func TestClientDropsRefIdxWhenNotMatched(t *testing.T) {
	b, err := os.ReadFile("client.go")
	if err != nil {
		t.Fatal(err)
	}
	src := stripGoComments(string(b))

	// matched 必须被接住（不是 _），否则这个判断等于没有
	if !strings.Contains(src, "msgID, refIdx, seq, ok, matched := c.anchors.PickAndReserve") {
		t.Error("sendGroup 没有接住 matched —— 防护被绕过了")
	}
	// 核心：不是他就不带气泡
	if !strings.Contains(src, "if !matched {") {
		t.Fatalf("sendGroup 必须在 !matched 时丢掉 refIdx；" +
			"挂到别人消息上的引用气泡比没有气泡糟糕得多（生产上只能靠人眼发现）")
	}
	// 且这个清空必须发生在 post 之前
	idxGuard := strings.Index(src, "if !matched {")
	idxPost := strings.Index(src, "c.post(ctx")
	if idxGuard < 0 || idxPost < 0 || idxGuard > idxPost {
		t.Error("丢 refIdx 的判断必须放在 post 之前，否则请求已经发出去了")
	}
}

// stripGoComments 剥掉 // 注释与 /* */ 块，避免注释里的字样被当成代码。
func stripGoComments(src string) string {
	var out []string
	inBlock := false
	for _, line := range strings.Split(src, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case inBlock:
			if strings.Contains(t, "*/") {
				inBlock = false
			}
			continue
		case strings.HasPrefix(t, "/*"):
			if !strings.Contains(t, "*/") {
				inBlock = true
			}
			continue
		case strings.HasPrefix(t, "//"):
			continue
		}
		if i := strings.Index(line, " //"); i >= 0 {
			line = line[:i]
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}
