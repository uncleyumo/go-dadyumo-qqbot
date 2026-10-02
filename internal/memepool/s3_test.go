package memepool

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// SigV4 是手写的（引 AWS SDK 要多背 40MB 依赖），那就必须真的验一遍签名形状。
// 少一个逗号、多一个空格，上游一律 403，而 403 在测试里长得和「桶不存在」一模一样。
func TestS3SignsRequests(t *testing.T) {
	var gotAuth, gotDate string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotDate = r.Header.Get("X-Amz-Date")
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write([]byte("bytes-content"))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s, err := NewS3(S3Config{
		Endpoint: srv.URL, Bucket: "qqbot",
		AccessKey: "AKIDEXAMPLE", SecretKey: "secret-key-xyz",
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Put("memes/1.jpg", []byte("hello"), "image/jpeg"); err != nil {
		t.Fatalf("Put 失败: %v", err)
	}
	for _, want := range []string{
		"AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/",
		"SignedHeaders=", "Signature=",
	} {
		if !strings.Contains(gotAuth, want) {
			t.Errorf("Authorization 缺 %q：%s", want, gotAuth)
		}
	}
	if gotDate == "" || len(gotDate) != 16 {
		t.Errorf("X-Amz-Date 应是 yyyyMMddTHHmmssZ，got %q", gotDate)
	}
	// 独立按 AWS 规范重算一遍签名。这一段是刻意不复用 s.sign：
	// 之前真实 MinIO 上传 403 SignatureDoesNotMatch，根因正是规范头串里
	// 漏了 host 那一行——签名照算不误，代码看着完全合理，只有真服务端会拒绝。
	// 这里用一份独立实现当断言，把那个坑钉死。
	if want := recomputeSigV4("PUT", "/qqbot/memes/1.jpg", "", srv.Listener.Addr().String(),
		gotDate, sha256Hex([]byte("hello")), "AKIDEXAMPLE", "secret-key-xyz", "us-east-1"); want != "" {
		if !strings.HasSuffix(gotAuth, "Signature="+want) {
			t.Errorf("签名对不上。\n实际: %s\n应为: %s", gotAuth, want)
		}
	}

	data, mime, err := s.Get("memes/1.jpg")
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	if string(data) != "bytes-content" || mime != "image/jpeg" {
		t.Errorf("读回来不对: %q %q", data, mime)
	}
}

// recomputeSigV4 是测试用的第二份实现，直接照 AWS 文档的步骤写。
func recomputeSigV4(method, uri, query, host, amzDate, payloadHash, ak, sk, region string) string {
	dateStamp := amzDate[:8]
	canonHeaders := "host:" + host + "\n" +
		"x-amz-content-sha256:" + payloadHash + "\n" +
		"x-amz-date:" + amzDate + "\n"
	signed := "host;x-amz-content-sha256;x-amz-date"
	canonReq := strings.Join([]string{method, uri, query, canonHeaders, signed, payloadHash}, "\n")

	scope := dateStamp + "/" + region + "/s3/aws4_request"
	sts := strings.Join([]string{
		"AWS4-HMAC-SHA256", amzDate, scope, sha256Hex([]byte(canonReq)),
	}, "\n")

	k := hmacSHA256([]byte("AWS4"+sk), dateStamp)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, "s3")
	k = hmacSHA256(k, "aws4_request")
	return hex.EncodeToString(hmacSHA256(k, sts))
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// 删一个本来就不存在的对象不该报错：淘汰流程不能因为 404 就中断
func TestS3DeleteMissingIsNotFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	s, _ := NewS3(S3Config{Endpoint: srv.URL, Bucket: "b", AccessKey: "a", SecretKey: "s"})
	if err := s.Delete("memes/nope.jpg"); err != nil {
		t.Errorf("删不存在的对象应静默成功，got %v", err)
	}
}

// 真连 MinIO 的往返测试。默认跳过，设 MEMEPOL_TEST_S3 才会跑：
// endpoint / bucket / 密钥从环境变量拿，绝不写死在仓库里。
// 手写 SigV4 只有真服务器才作数，httptest 的假端点不校验签名。
func TestS3LiveRoundTrip(t *testing.T) {
	ep := os.Getenv("MEMEPOL_TEST_S3")
	if ep == "" {
		t.Skip("设 MEMEPOL_TEST_S3=<endpoint> 才跑真 MinIO 往返")
	}
	s, err := NewS3(S3Config{
		Endpoint:  ep,
		Bucket:    os.Getenv("MEMEPOL_TEST_BUCKET"),
		AccessKey: os.Getenv("MEMEPOL_TEST_AK"),
		SecretKey: os.Getenv("MEMEPEL_TEST_SK"),
	})
	if err != nil {
		t.Fatal(err)
	}

	key := "memes/selftest.jpg"
	payload := []byte("\xff\xd8\xff\xe0selftest-not-a-real-jpeg")

	if err := s.Put(key, payload, "image/jpeg"); err != nil {
		t.Fatalf("真 MinIO 上传失败: %v", err)
	}
	got, mime, err := s.Get(key)
	if err != nil {
		t.Fatalf("真 MinIO 读回失败: %v", err)
	}
	if !bytes.Equal(got, payload) || mime != "image/jpeg" {
		t.Errorf("读回内容不对: %d 字节 %q", len(got), mime)
	}
	if err := s.Delete(key); err != nil {
		t.Fatalf("真 MinIO 删除失败: %v", err)
	}
	if _, _, err := s.Get(key); err == nil {
		t.Error("删完还读得到")
	}

}
