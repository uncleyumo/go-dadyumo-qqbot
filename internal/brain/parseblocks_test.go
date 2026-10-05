package brain

import (
	"strings"
	"testing"
)

func TestParseDecisionBlocks(t *testing.T) {
	in := `<os>想甩一张</os>
<json>
{"act":"say","to":"李四","blocks":[
  {"t":"text","c":"看这个"},
  {"t":"img","id":42},
  {"t":"text","c":"补一句"}
],"tone":"roast","mood":"无语","mem":[]}
</json>`
	d, issue := ParseDecision(in)
	if issue != ParseOK {
		t.Fatalf("解析应成功，got %v", issue)
	}
	if d.Act != "say" || d.To != "李四" {
		t.Errorf("act/to 不对: %+v", d)
	}
	if len(d.Blocks) != 3 {
		t.Fatalf("应有 3 个块，got %d", len(d.Blocks))
	}
	if d.Blocks[0].T != BlockTypeText || d.Blocks[0].C != "看这个" {
		t.Errorf("第 1 块不对: %+v", d.Blocks[0])
	}
	if d.Blocks[1].T != BlockTypeImg || d.Blocks[1].ID != 42 {
		t.Errorf("第 2 块不对: %+v", d.Blocks[1])
	}
	if d.OS == "" {
		t.Error("os 应被解析出来")
	}
}

// 只发图不说话——群里最常见的表情包用法
func TestParseDecisionImageOnly(t *testing.T) {
	in := `<json>{"act":"say","to":"","blocks":[{"t":"img","id":7}],"tone":"roast","mem":[]}</json>`
	d, issue := ParseDecision(in)
	if issue != ParseOK {
		t.Fatalf("got %v", issue)
	}
	if len(d.Blocks) != 1 || d.Blocks[0].T != BlockTypeImg {
		t.Fatalf("应有一个图片块，got %+v", d.Blocks)
	}
	blocks, ok := allowBlocks(d.Blocks, d.Text, 5, nil, nil)
	if !ok || len(blocks) != 1 || blocks[0].ID != 7 {
		t.Errorf("只发图应算有内容，got %#v ok=%v", blocks, ok)
	}
}

// 兼容：模型还在用旧的 text 字段
func TestParseDecisionTextFallback(t *testing.T) {
	in := `<json>{"act":"say","to":"李四","text":"第一句\n第二句","tone":"roast","mem":[]}</json>`
	d, issue := ParseDecision(in)
	if issue != ParseOK {
		t.Fatalf("got %v", issue)
	}
	if d.Text == "" {
		t.Error("text 应被解析出来")
	}
	blocks, ok := allowBlocks(d.Blocks, d.Text, 5, nil, nil)
	if !ok || len(blocks) != 2 {
		t.Fatalf("应回落成两个 text 块，got %#v", blocks)
	}
}

// quiet 时 blocks 必须被清掉——不然会发出去
func TestParseDecisionQuietClearsBlocks(t *testing.T) {
	in := `<json>{"act":"quiet","to":"","blocks":[{"t":"text","c":"我不想说"}],"mem":[]}</json>`
	d, _ := ParseDecision(in)
	if d.Act != "quiet" {
		t.Fatalf("act 应为 quiet，got %q", d.Act)
	}
	if len(d.Blocks) != 0 {
		t.Errorf("quiet 时 blocks 应被清空，got %+v", d.Blocks)
	}
	if _, ok := allowBlocks(d.Blocks, d.Text, 5, nil, nil); ok {
		t.Error("quiet 时不该认为有内容可发")
	}
}

// blocks 里的文字也要过出口清理
func TestParseDecisionBlocksSanitized(t *testing.T) {
	in := `<json>{"act":"say","blocks":[{"t":"text","c":"带<os>标签的话"}],"mem":[]}</json>`
	d, _ := ParseDecision(in)
	if len(d.Blocks) != 1 {
		t.Fatalf("got %+v", d.Blocks)
	}
	if strings.Contains(d.Blocks[0].C, "<os>") {
		t.Errorf("blocks 里的文字应被清理，got %q", d.Blocks[0].C)
	}
}

// act 写错但有内容 → 仍算 say（否则模型填 blocks 就不说话了）
func TestParseDecisionBadActWithBlocks(t *testing.T) {
	in := `<json>{"act":"speak","blocks":[{"t":"img","id":3}],"mem":[]}</json>`
	d, _ := ParseDecision(in)
	if d.Act != "say" {
		t.Errorf("有 blocks 时 act 应回落成 say，got %q", d.Act)
	}
}

// 图片块的类型大小写要归一
func TestParseDecisionBlockTypeNormalized(t *testing.T) {
	in := `<json>{"act":"say","blocks":[{"T":"TEXT","c":"hi"}],"mem":[]}</json>`
	d, _ := ParseDecision(in)
	if len(d.Blocks) != 1 || d.Blocks[0].T != BlockTypeText {
		t.Errorf("块类型应归一成小写，got %+v", d.Blocks)
	}
}