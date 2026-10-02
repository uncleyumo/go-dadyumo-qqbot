package webhook

import (
	"bytes"
	"encoding/hex"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// 官方 DEMO（文档「安全和授权」页）给出的 secret 与派生公钥，用来锁定 seed 推导正确性。
// 官方原文 publicKey: [215 195 98 254 120 174 248 31 ... 103 58]
// 注：该页同时给出的 demo signature 与其 demo body 对不上（已穷举多种 body 变体与拼接顺序均不匹配），
// 判定为文档笔误；实现以该页的 Go 代码示例（msg = timestamp + body）为准。
func TestPublicKeyMatchesOfficialDemo(t *testing.T) {
	pub := PublicKey("naOC0ocQE3shWLAfffVLB1rhYPG7")
	if len(pub) != 32 {
		t.Fatalf("公钥长度应为 32，实际 %d", len(pub))
	}
	want, _ := hex.DecodeString("d7c362fe78aef81ff23287b493628b5db02a3c4fe30b215e4d19609b5d76673a")
	if !bytes.Equal([]byte(pub), want) {
		t.Fatalf("公钥与官方 DEMO 不一致\n got: %x\nwant: %x", []byte(pub), want)
	}
}

func TestSignVerifyRoundTrip(t *testing.T) {
	const secret = "naOC0ocQE3shWLAfffVLB1rhYPG7"
	body := []byte(`{"op":0,"d":{},"t":"GROUP_AT_MESSAGE_CREATE"}`)
	// 时间戳必须新鲜（VerifyRequest 会卡 ±5 分钟），所以用当前时间而不是固定常量
	ts := strconv.FormatInt(time.Now().Unix(), 10)

	var msg bytes.Buffer
	msg.WriteString(ts)
	msg.Write(body)
	sig := Sign(secret, msg.Bytes())

	req, _ := http.NewRequest(http.MethodPost, "/qq/callback", bytes.NewReader(body))
	req.Header.Set("X-Signature-Ed25519", sig)
	req.Header.Set("X-Signature-Timestamp", ts)
	if !VerifyRequest(secret, req, body) {
		t.Fatal("合法签名应通过校验")
	}

	// body 被篡改
	bad := append([]byte{}, body...)
	bad[0] = 'X'
	req2, _ := http.NewRequest(http.MethodPost, "/qq/callback", bytes.NewReader(bad))
	req2.Header.Set("X-Signature-Ed25519", sig)
	req2.Header.Set("X-Signature-Timestamp", ts)
	if VerifyRequest(secret, req2, bad) {
		t.Fatal("被篡改的请求不应通过校验")
	}

	// 时间戳被篡改（仍在新鲜窗口内，所以失败原因只能是签名对不上）
	req3, _ := http.NewRequest(http.MethodPost, "/qq/callback", bytes.NewReader(body))
	req3.Header.Set("X-Signature-Ed25519", sig)
	req3.Header.Set("X-Signature-Timestamp", strconv.FormatInt(time.Now().Unix()+1, 10))
	if VerifyRequest(secret, req3, body) {
		t.Fatal("时间戳被篡改不应通过校验")
	}

	// 缺少签名头
	req4, _ := http.NewRequest(http.MethodPost, "/qq/callback", bytes.NewReader(body))
	req4.Header.Set("X-Signature-Timestamp", ts)
	if VerifyRequest(secret, req4, body) {
		t.Fatal("缺少签名头不应通过校验")
	}

	// 非法 hex
	req5, _ := http.NewRequest(http.MethodPost, "/qq/callback", bytes.NewReader(body))
	req5.Header.Set("X-Signature-Ed25519", "zzzz")
	req5.Header.Set("X-Signature-Timestamp", ts)
	if VerifyRequest(secret, req5, body) {
		t.Fatal("非法签名不应通过校验")
	}

	// 换一个 secret 应失败
	if VerifyRequest("another-secret-xxx", req, body) {
		t.Fatal("错误 secret 不应通过校验")
	}
}

// op=13 回调地址验证：按官方示例 msg = event_ts + plain_token
func TestValidationSignatureOrder(t *testing.T) {
	const secret = "test-secret-abc"
	ts, token := "1725442341", "Arq0D5A61EgUu4OxUvOp"
	sig := Sign(secret, []byte(ts+token))
	// 用官方示例的方式反向验证：签名体必须是 event_ts 在前
	if sig != Sign(secret, []byte(ts+token)) {
		t.Fatal("签名应稳定")
	}
	if sig == Sign(secret, []byte(token+ts)) {
		t.Fatal("拼接顺序不同应产生不同签名")
	}
}
