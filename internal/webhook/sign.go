package webhook

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// seedFromSecret 用 AppSecret 重复填充得到 32 字节 ed25519 seed
func seedFromSecret(secret string) []byte {
	seed := secret
	for len(seed) < ed25519.SeedSize {
		seed = strings.Repeat(seed, 2)
	}
	return []byte(seed[:ed25519.SeedSize])
}

// PublicKey 由 AppSecret 推导出平台侧公钥
func PublicKey(secret string) ed25519.PublicKey {
	_, priv, err := ed25519.GenerateKey(strings.NewReader(string(seedFromSecret(secret))))
	if err != nil {
		return nil
	}
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return nil
	}
	return pub
}

// Sign 用 AppSecret 对应私钥对消息签名（用于 op=13 回调地址验证的响应）
func Sign(secret string, msg []byte) string {
	_, priv, err := ed25519.GenerateKey(strings.NewReader(string(seedFromSecret(secret))))
	if err != nil {
		return ""
	}
	return hex.EncodeToString(ed25519.Sign(priv, msg))
}

// SignatureSkew 允许的签名时间戳偏差。
//
// ed25519 只能证明「是持 secret 的一方在某个 ts 上签过」，不能证明 ts 是新鲜的——
// 只判非空的话，攻击者截获任意一条合法回调就能原样重放任意次，永久有效。
// 重复放大会重复写 memory.Group.recent、污染上下文烧 token、让冲动值虚高、白刷屏，
// 所以必须卡窗口。这里给 5 分钟，够覆盖正常网络抖动与平台重试。
const SignatureSkew = 5 * time.Minute

// VerifyRequest 校验回调请求签名：msg = X-Signature-Timestamp + body，且时间戳必须新鲜
func VerifyRequest(secret string, r *http.Request, body []byte) bool {
	pub := PublicKey(secret)
	if pub == nil {
		return false
	}
	sigHex := r.Header.Get("X-Signature-Ed25519")
	if sigHex == "" {
		return false
	}
	sig, err := hex.DecodeString(sigHex)
	if err != nil || len(sig) != ed25519.SignatureSize || sig[63]&224 != 0 {
		return false
	}
	ts := r.Header.Get("X-Signature-Timestamp")
	if ts == "" {
		return false
	}
	// 先卡新鲜度再做密码学校验：早失败早返回，也不给 CPU 白干活
	tsUnix, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return false
	}
	if delta := time.Since(time.Unix(tsUnix, 0)); delta > SignatureSkew || delta < -SignatureSkew {
		return false
	}
	var msg bytes.Buffer
	msg.WriteString(ts)
	msg.Write(body)
	return ed25519.Verify(pub, msg.Bytes(), sig)
}

// ReadBody 读取并还原请求体（后续还要再解析一次）
//
// 调用方必须先用 http.MaxBytesReader 给 r.Body 套上上限：ed25519 验签必须拿到完整 body，
// 「先读后验」本身无法避免，但读多少必须先有闸门，否则一个超大 body 就能把内存吃光。
func ReadBody(r *http.Request) ([]byte, error) {
	return io.ReadAll(r.Body)
}
