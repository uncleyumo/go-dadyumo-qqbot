package brain

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"dadyumo/internal/config"
	"dadyumo/internal/memepool"
	"dadyumo/internal/memory"
	"dadyumo/internal/takeover"
)

func manualReq(group, text, leaseID, clientID string) ManualTextRequest {
	return ManualTextRequest{
		GroupID:     group,
		Text:        text,
		LeaseID:     leaseID,
		ClientMsgID: clientID,
	}
}

func TestManualTextSendsAndMarksSpoke(t *testing.T) {
	e, _, _ := newTakeoverEngine(t)
	sender := &recordingSender{}
	e.sender = sender
	g := e.mem.Group("G1", "")
	g.TouchMember("U1", "张三")
	if err := e.AcquireLease("G1", "L1", false, 0); err != nil {
		t.Fatal(err)
	}

	res, err := e.SendManualText(context.Background(), manualReq("G1", "我来说两句", "L1", "CID00001"))
	if err != nil {
		t.Fatalf("手动发送应成功，got %v", err)
	}
	if !res.Sent || res.SentText != "我来说两句" || res.Duplicate {
		t.Fatalf("结果不对：%+v", res)
	}
	if res.LeaseExpiresAt == 0 {
		t.Fatal("window 档应返回租约到期时间")
	}
	if n, _ := sender.count(); n != 1 {
		t.Fatalf("应发出 1 条，got %d", n)
	}
	if got := g.LastText(); got != "我来说两句" {
		t.Fatalf("MarkManualSpoke 应更新 lastText，got %q", got)
	}
	// 人工发言不是 AI 连着开口，不能增加 consecutive。
	if g.Consecutive() != 0 {
		t.Fatalf("手动发言不该增加 consecutive，got %d", g.Consecutive())
	}
}

func TestManualTextIdempotentAndBusy(t *testing.T) {
	e, _, _ := newTakeoverEngine(t)
	sender := &recordingSender{}
	e.sender = sender
	e.mem.Group("G1", "").TouchMember("U1", "张三")
	if err := e.AcquireLease("G1", "L1", false, 0); err != nil {
		t.Fatal(err)
	}

	req := manualReq("G1", "只发一次", "L1", "CID00002")
	if _, err := e.SendManualText(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	dup, err := e.SendManualText(context.Background(), req)
	if err != nil {
		t.Fatalf("同 id 重试不该报错，got %v", err)
	}
	if !dup.Duplicate {
		t.Fatal("同 client_msg_id 重试应返回 duplicate=true")
	}
	if n, _ := sender.count(); n != 1 {
		t.Fatalf("幂等重试不该再发一条，got %d", n)
	}

	// 同群已有发送在途：TryLock 失败返回 busy。
	lock := e.groupSendLock("G1")
	lock.Lock()
	defer lock.Unlock()
	if _, err := e.SendManualText(context.Background(), manualReq("G1", "插队", "L1", "CID00003")); !errors.Is(err, ErrManualBusy) {
		t.Fatalf("单飞锁应返回 busy，got %v", err)
	}
}

func TestManualTextAtCleaningAndFlatten(t *testing.T) {
	e, _, _ := newTakeoverEngine(t)
	sender := &recordingSender{}
	e.sender = sender
	e.mem.Group("G1", "").TouchMember("U1", "张三")
	if err := e.AcquireLease("G1", "L1", false, 0); err != nil {
		t.Fatal(err)
	}

	req := manualReq("G1", "第一行\n第二行 <qqbot-at-user id=\"U1\" />", "L1", "CID00004")
	req.AtOpenIDs = []string{"U1"}
	res, err := e.SendManualText(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	want := `<qqbot-at-user id="U1" /> 第一行 第二行`
	if res.SentText != want {
		t.Fatalf("拼装结果不对：got %q want %q", res.SentText, want)
	}
	if strings.Count(res.SentText, "qqbot-at-user") != 1 {
		t.Fatalf("正文里原有的 @ 标签应被清洗掉，got %q", res.SentText)
	}
	if strings.ContainsAny(res.SentText, "\r\n") {
		t.Fatalf("带 @ 的 markdown 内容不能有换行，got %q", res.SentText)
	}
	if n, _ := sender.count(); n != 1 {
		t.Fatalf("应发出 1 条，got %d", n)
	}
}

func TestManualTextRejectsUnknownAt(t *testing.T) {
	e, _, _ := newTakeoverEngine(t)
	e.sender = &recordingSender{}
	e.mem.Group("G1", "").TouchMember("U1", "张三")
	if err := e.AcquireLease("G1", "L1", false, 0); err != nil {
		t.Fatal(err)
	}
	req := manualReq("G1", "叫你", "L1", "CID00005")
	req.AtOpenIDs = []string{"NOT-A-MEMBER"}
	if _, err := e.SendManualText(context.Background(), req); !errors.Is(err, ErrManualBadAt) {
		t.Fatalf("非成员 @ 应返回 bad_at，got %v", err)
	}
}

func TestManualTextGate(t *testing.T) {
	e, st, _ := newTakeoverEngine(t)
	e.sender = &recordingSender{}
	e.mem.Group("G1", "").TouchMember("U1", "张三")

	// off：全部拒绝。
	if err := st.SetMode("G1", takeover.ModeOff); err != nil {
		t.Fatal(err)
	}
	if _, err := e.SendManualText(context.Background(), manualReq("G1", "hi", "", "CID00006")); !errors.Is(err, ErrTakeoverModeOff) {
		t.Fatalf("off 应返回 mode_off，got %v", err)
	}

	// window：必须带 lease。
	if err := st.SetMode("G1", takeover.ModeWindow); err != nil {
		t.Fatal(err)
	}
	if _, err := e.SendManualText(context.Background(), manualReq("G1", "hi", "", "CID00007")); !errors.Is(err, ErrManualLeaseRequired) {
		t.Fatalf("window 无租约应返回 lease_required，got %v", err)
	}
	if _, err := e.SendManualText(context.Background(), manualReq("G1", "hi", "NOPE", "CID00008")); !errors.Is(err, ErrTakeoverLeaseExpired) {
		t.Fatalf("window 无效租约应返回 lease_expired，got %v", err)
	}

	// always：不需要租约。
	if err := st.SetMode("G1", takeover.ModeAlways); err != nil {
		t.Fatal(err)
	}
	if _, err := e.SendManualText(context.Background(), manualReq("G1", "hi", "", "CID00009")); err != nil {
		t.Fatalf("always 应直接放行，got %v", err)
	}
}

func TestManualTextRejectsWhenGloballyPaused(t *testing.T) {
	e, st, _ := newTakeoverEngine(t)
	e.sender = &recordingSender{}
	e.mem.Group("G1", "").TouchMember("U1", "张三")
	e.store = config.NewStoreFrom(func(c *config.Config) { c.Paused = true })
	if err := st.SetMode("G1", takeover.ModeAlways); err != nil {
		t.Fatal(err)
	}
	if _, err := e.SendManualText(context.Background(), manualReq("G1", "hi", "", "CID00010")); !errors.Is(err, ErrManualPaused) {
		t.Fatalf("全局关停时手动发送应返回 paused，got %v", err)
	}
}

func TestManualTextRejectsMissingClientMsgID(t *testing.T) {
	e, st, _ := newTakeoverEngine(t)
	e.sender = &recordingSender{}
	e.mem.Group("G1", "")
	if err := st.SetMode("G1", takeover.ModeAlways); err != nil {
		t.Fatal(err)
	}
	if _, err := e.SendManualText(context.Background(), manualReq("G1", "hi", "", "")); !errors.Is(err, ErrManualBadClientMsgID) {
		t.Fatalf("缺 client_msg_id 应报错，got %v", err)
	}
}

func TestManualMemeSendsAndWritesMemory(t *testing.T) {
	e, st, _ := newTakeoverEngine(t)
	sender := &recordingSender{}
	e.sender = sender
	pool := memepool.New(memepool.DefaultConfig(), &memStorageStub{})
	id, err := pool.Add([]byte("fake-jpeg-meme"), "image/jpeg", "笑死我了", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	e.SetMemePool(pool, sender)
	g := e.mem.Group("G1", "")
	if err := st.SetMode("G1", takeover.ModeAlways); err != nil {
		t.Fatal(err)
	}

	res, err := e.SendManualMeme(context.Background(), ManualMemeRequest{
		GroupID: "G1", MemeID: id, ClientMsgID: "CID00011",
	})
	if err != nil {
		t.Fatalf("手动发表情包应成功，got %v", err)
	}
	if !res.Sent || res.SentText != manualMemeText {
		t.Fatalf("结果不对：%+v", res)
	}
	if _, n := sender.count(); n != 1 {
		t.Fatalf("应发出 1 张图，got %d", n)
	}
	if m, _ := pool.Get(id); m.Uses != 1 {
		t.Fatalf("发送成功后应 MarkUsed，got uses=%d", m.Uses)
	}
	found := false
	for _, l := range g.Recent(10) {
		if l.Role == memory.RoleBot && l.Content == manualMemeText {
			found = true
		}
	}
	if !found {
		t.Fatal("表情包发送后应写一条「（发了个表情包）」记忆")
	}

	// 幂等重试不再上传。
	dup, err := e.SendManualMeme(context.Background(), ManualMemeRequest{
		GroupID: "G1", MemeID: id, ClientMsgID: "CID00011",
	})
	if err != nil || !dup.Duplicate {
		t.Fatalf("同 id 重试应返回 duplicate，got %+v err=%v", dup, err)
	}
	if _, n := sender.count(); n != 1 {
		t.Fatalf("幂等重试不该再发一张，got %d", n)
	}
}

func TestManualMemeRejectsUnknownID(t *testing.T) {
	e, st, _ := newTakeoverEngine(t)
	sender := &recordingSender{}
	pool := memepool.New(memepool.DefaultConfig(), &memStorageStub{})
	e.SetMemePool(pool, sender)
	e.mem.Group("G1", "")
	if err := st.SetMode("G1", takeover.ModeAlways); err != nil {
		t.Fatal(err)
	}
	if _, err := e.SendManualMeme(context.Background(), ManualMemeRequest{
		GroupID: "G1", MemeID: 999, ClientMsgID: "CID00012",
	}); !errors.Is(err, ErrManualBadMeme) {
		t.Fatalf("不存在的表情包应返回 bad_meme，got %v", err)
	}
}
