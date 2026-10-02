package memepool

import (
	"path/filepath"
	"testing"
	"time"
)

// 优选评分的解析：真实模型爱在 JSON 外面包话，单引号也常见。
// 解析不出来必须整轮放弃，而不是硬凑一个错分数——
// 错分数会把好图误杀，比这轮不清垃圾糟得多。
func TestParseScoresTolerant(t *testing.T) {
	memes := []Meme{{ID: 11}, {ID: 22}, {ID: 33}}
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"裸 JSON", `{"1":0.9,"2":0.1,"3":0.5}`, 3},
		{"包在解释里", `好的，评分如下：{"1":0.9,"2":0.1,"3":0.5} 希望有用`, 3},
		{"包在代码块里", "```json\n{\"1\":0.9,\"2\":0.1,\"3\":0.5}\n```", 3},
		{"前面有废话", `我看看\n{"1":0.9,"2":0.1,"3":0.5}\n还有什么问题？`, 3},
	}
	for _, c := range cases {
		got, err := parseScores(c.in, memes)
		if err != nil {
			t.Errorf("%s: 不该失败: %v", c.name, err)
			continue
		}
		if len(got) != c.want {
			t.Errorf("%s: 应解析 %d 条，got %d", c.name, c.want, len(got))
		}
	}
}

func TestParseScoresClampsRange(t *testing.T) {
	memes := []Meme{{ID: 1}, {ID: 2}}
	got, err := parseScores(`{"1":5.0,"2":-2.0}`, memes)
	if err != nil {
		t.Fatal(err)
	}
	if got[1] != 1.0 || got[2] != 0.0 {
		t.Errorf("越界分数应被夹到 0~1，got %v", got)
	}
}

// 行号越界必须被丢掉，绝不能错位记到别的图上——
// 那会让一张好图因为另一张的分被踢掉
func TestParseScoresRejectsOutOfRangeIndex(t *testing.T) {
	memes := []Meme{{ID: 11}}
	got, err := parseScores(`{"0":0.1,"1":0.8,"2":0.2,"99":0.1}`, memes)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[11] != 0.8 {
		t.Errorf("只应保留合法行号 1，got %v", got)
	}
}

func TestParseScoresRejectsGarbage(t *testing.T) {
	memes := []Meme{{ID: 11}}
	for _, in := range []string{"", "我觉得都还行", "{}", "[1,2,3]", `{"abc":0.5}`} {
		if _, err := parseScores(in, memes); err == nil {
			t.Errorf("%q 应被拒绝，而不是硬凑一个分数", in)
		}
	}
}

// 描述里带大括号时不能被抠错
func TestExtractJSONObjectHandlesBracesInText(t *testing.T) {
	got := extractJSONObject(`前面有 {不是 json} 后面 {"1":0.5} 结束`)
	if got != `{"1":0.5}` {
		t.Errorf("应抠出最外层 JSON，got %q", got)
	}
}

// 低分且没人用过的该被清掉
func TestPruneLowQualityRemovesJunk(t *testing.T) {
	p := New(testCfg(), newMemStorage())
	now := time.Now()
	junk, _ := p.Add(img(1), "image/jpeg", "一张风景照", now)
	good, _ := p.Add(img(2), "image/jpeg", "笑死我了", now)

	p.ApplyQuality(map[int64]float64{junk: 0.05, good: 0.9})

	dropped := p.PruneLowQuality()
	if len(dropped) != 1 || dropped[0] != junk {
		t.Errorf("低分且没人用过的该被淘汰，got %v", dropped)
	}
	if _, ok := p.Get(good); !ok {
		t.Error("高分的不该被淘汰")
	}
}

// 用量能救评分：一张图很烂但发过好几次，说明在这个群里它有效果
func TestPruneKeepsLowScoredButUsed(t *testing.T) {
	p := New(testCfg(), newMemStorage())
	now := time.Now()
	id, _ := p.Add(img(1), "image/jpeg", "很烂但好用", now)
	p.MarkUsed(id, now)
	p.MarkUsed(id, now)
	p.ApplyQuality(map[int64]float64{id: 0.01})

	if dropped := p.PruneLowQuality(); len(dropped) != 0 {
		t.Errorf("发过的图不该因低分被淘汰，got %v", dropped)
	}
}

// 手动删除：元数据和存储都要清
func TestRemoveDeletesBoth(t *testing.T) {
	st := newMemStorage()
	p := New(testCfg(), st)
	now := time.Now()
	id, _ := p.Add(img(1), "image/jpeg", "要删的", now)

	if err := p.Remove(id); err != nil {
		t.Fatalf("删除应成功: %v", err)
	}
	if _, ok := p.Get(id); ok {
		t.Error("元数据应被删掉")
	}
	if st.delCount() != 1 {
		t.Errorf("MinIO 对象也应被删，deleted=%v", st.deleted)
	}
	// 重复删不应报错
	if err := p.Remove(id); err != ErrNotFound {
		t.Errorf("重复删应报 ErrNotFound，got %v", err)
	}
}

// 控制台要能看见排序结果和权重明细
func TestRankedExposesWeights(t *testing.T) {
	cfg := testCfg()
	p := New(cfg, newMemStorage())
	now := time.Now()
	cold, _ := p.Add(img(1), "image/jpeg", "没人用", now)
	hot, _ := p.Add(img(2), "image/jpeg", "常用", now)
	for i := 0; i < 5; i++ {
		p.MarkUsed(hot, now)
	}

	rows := p.Ranked(now)
	if len(rows) != 2 {
		t.Fatalf("应有 2 行，got %d", len(rows))
	}
	if rows[0].ID != hot {
		t.Errorf("用得多的应排第一，got %d", rows[0].ID)
	}
	if rows[0].Rank != 1 {
		t.Errorf("Rank 应从 1 开始，got %d", rows[0].Rank)
	}
	if rows[0].Uses != 5 {
		t.Errorf("Uses 应如实展示，got %d", rows[0].Uses)
	}
	if rows[0].Affection <= rows[1].Affection {
		t.Error("好感度应能区分出高低")
	}
	if rows[1].ExpiresIn <= 0 {
		t.Errorf("应能看到剩余天数，got %d", rows[1].ExpiresIn)
	}
	_ = cold
}

// 落盘与恢复：重启后评分和到期时间不能丢
func TestSaveAndLoadRoundTrip(t *testing.T) {
	st := newMemStorage()
	cfg := testCfg()
	p := New(cfg, st)
	now := time.Now()
	id, _ := p.Add(img(1), "image/jpeg", "很老的图", now)
	for i := 0; i < 7; i++ {
		p.MarkUsed(id, now)
	}
	p.ApplyQuality(map[int64]float64{id: 0.77})

	path := filepath.Join(t.TempDir(), "memes.json")
	if err := p.Save(path); err != nil {
		t.Fatal(err)
	}

	p2 := New(cfg, st)
	if err := p2.Load(path); err != nil {
		t.Fatal(err)
	}
	m, ok := p2.Get(id)
	if !ok {
		t.Fatal("恢复后应还在池里")
	}
	if m.Uses != 7 {
		t.Errorf("使用次数应恢复，got %d", m.Uses)
	}
	if m.Quality != 0.77 {
		t.Errorf("评分应恢复，got %v", m.Quality)
	}
	// 落盘用 unix 秒，纳秒部分本来就丢，比较时对齐到秒
	if m.ExpiresAt.Unix() != p.memes[id].ExpiresAt.Unix() {
		t.Errorf("到期时间应恢复: %v vs %v", m.ExpiresAt, p.memes[id].ExpiresAt)
	}
}

// 坏文件不能让机器人起不来
func TestLoadBadFileReportsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memes.json")
	writeFile(t, path, "{这不是 json")

	p := New(testCfg(), newMemStorage())
	err := p.Load(path)
	if err == nil {
		t.Fatal("坏文件应报错")
	}
	var pe *ParseError
	if !asParseError(err, &pe) {
		t.Errorf("应是 ParseError，got %T", err)
	}
}

// 版本对不上要明确报出来，而不是按错误的结构解
func TestLoadVersionMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memes.json")
	writeFile(t, path, `{"version":99,"memes":[]}`)

	p := New(testCfg(), newMemStorage())
	err := p.Load(path)
	var ve *VersionError
	if err == nil || !asVersionError(err, &ve) {
		t.Errorf("应是 VersionError，got %v", err)
	}
}

// 已过期的条目恢复时不进来，省得刚启动就先淘汰一轮
func TestLoadSkipsExpired(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memes.json")
	old := time.Now().Add(-60 * 24 * time.Hour).Unix()
	fresh := time.Now().Add(-1 * time.Hour).Unix()
	nowU := time.Now().Unix()
	writeFile(t, path, `{"version":1,"memes":[
	 {"id":1,"object_key":"memes/1.jpg","descr":"过期了","added_at":`+itoa(old)+
		`,"uses":0,"last_used":0,"quality":0.5,"expires_at":`+itoa(nowU)+`},
	 {"id":2,"object_key":"memes/2.jpg","descr":"还在","added_at":`+itoa(fresh)+
		`,"uses":0,"last_used":0,"quality":0.5,"expires_at":`+itoa(nowU+86400*30)+`}
	]}`)

	p := New(testCfg(), newMemStorage())
	if err := p.Load(path); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.Get(1); ok {
		t.Error("已过期的不该被恢复")
	}
	if _, ok := p.Get(2); !ok {
		t.Error("没到期的应被恢复")
	}
}

// 文件不存在是正常情况（首次运行），不该报错
func TestLoadMissingFileIsNotError(t *testing.T) {
	p := New(testCfg(), newMemStorage())
	if err := p.Load(filepath.Join(t.TempDir(), "nope.json")); err != nil {
		t.Errorf("首次运行没有文件是正常的，got %v", err)
	}
}