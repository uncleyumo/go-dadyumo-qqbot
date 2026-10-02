package memepool

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// S3Storage 是走 S3 协议的对象存储（MinIO 就是这个协议）。
//
// 为什么手写签名而不引 minio-go / aws-sdk-go：那是几十 MB 依赖和一大片
// 传递依赖，就为了 PUT/GET/DELETE 三个动作、传几个 KB 的图片。
// S3 的 SigV4 签名就是一段固定的字节拼接，自己写反而更可控，
// 出问题时的报错也更直白。
//
// 只支持这三个动作，且不做分片上传——单次 PUT 硬上限 5MB（超过 MinIO 会拒）。
// 这条约束**不会成为实际问题**：入池的图都已经过 Compress（长边 ≤1024、
// 质量 80），通常只有一两百 KB；GIF/动图因为要保动画是原样上传的，
// 但群里那些动图本身也远小于 5MB。真撞上了会返回 MinIO 的错误，
// 池子那边会记日志并跳过这一张，不影响其他图。
type S3Storage struct {
	endpoint string // http://127.0.0.1:9000
	region   string
	bucket   string
	access   string
	secret   string
	client   *http.Client
}

// S3Config MinIO 连接参数
type S3Config struct {
	Endpoint  string // http://127.0.0.1:9000
	Region    string
	Bucket    string
	AccessKey string
	SecretKey string
	Timeout   time.Duration
}

// NewS3 校验参数后构造。参数不全会返回错误而不是静默降级。
func NewS3(cfg S3Config) (*S3Storage, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" {
		return nil, fmt.Errorf("MinIO 缺少 endpoint 或 bucket")
	}
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, fmt.Errorf("MinIO 缺少密钥")
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 15 * time.Second
	}
	cfg.Endpoint = strings.TrimRight(cfg.Endpoint, "/")
	return &S3Storage{
		endpoint: cfg.Endpoint,
		region:   cfg.Region,
		bucket:   cfg.Bucket,
		access:   cfg.AccessKey,
		secret:   cfg.SecretKey,
		client:   &http.Client{Timeout: cfg.Timeout},
	}, nil
}

// Put 上传一个对象。同 key 覆盖——入池前已经按内容哈希去重了。
func (s *S3Storage) Put(key string, data []byte, contentType string) error {
	if contentType == "" {
		contentType = "image/jpeg"
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.client.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, s.url(key), strings.NewReader(string(data)))
	if err != nil {
		return err
	}
	req.ContentLength = int64(len(data))
	req.Header.Set("Content-Type", contentType)
	if err := s.sign(req, data); err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer drain(resp)
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("MinIO PUT HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// Get 读回一个对象。
func (s *S3Storage) Get(key string) ([]byte, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), s.client.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url(key), nil)
	if err != nil {
		return nil, "", err
	}
	// 签名的 payload 哈希，对无 body 的 GET 传空串哈希
	if err := s.sign(req, nil); err != nil {
		return nil, "", err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer drain(resp)
	if resp.StatusCode == http.StatusNotFound {
		return nil, "", ErrNotFound
	}
	if resp.StatusCode/100 != 2 {
		return nil, "", fmt.Errorf("MinIO GET HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, "", err
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "image/jpeg"
	}
	return raw, ct, nil
}

// Delete 删一个对象。
//
// 对象不存在时**不报错**：淘汰一张图时它可能早就被清过了，
// 那不是错误，幂等即可。
func (s *S3Storage) Delete(key string) error {
	ctx, cancel := context.WithTimeout(context.Background(), s.client.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, s.url(key), nil)
	if err != nil {
		return err
	}
	if err := s.sign(req, nil); err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer drain(resp)
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("MinIO DELETE HTTP %d", resp.StatusCode)
	}
	return nil
}

func (s *S3Storage) url(key string) string {
	// 斜杠不转义（对象名按路径分段），其余转义
	parts := strings.Split(key, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return s.endpoint + "/" + s.bucket + "/" + strings.Join(parts, "/")
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	_ = resp.Body.Close()
}

// sign 补上 SigV4 签名所需的头。
//
// AWS Signature Version 4：把所有参与签名的头按名字排序，拼成
// "头:值\n" 的规范串，再连同 method/URI/payload 哈希一起做 HMAC。
// 任何一环对不上，S3 就返回 403 SignatureDoesNotMatch。
func (s *S3Storage) sign(req *http.Request, payload []byte) error {
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	sum := sha256.Sum256(payload)
	payloadHash := hex.EncodeToString(sum[:])

	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	// Host 头由 req.Host 决定（Go 忽略 Header["Host"]），所以只设 req.Host。
	if req.Host == "" {
		req.Host = req.URL.Host
	}

	// 收集所有需要签名的头：host + x-amz-*
	var names []string
	headers := map[string]string{"host": req.Host}
	for k, v := range req.Header {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-amz-") {
			names = append(names, lk)
			headers[lk] = strings.TrimSpace(v[0])
		}
	}
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)

	// 规范头串必须**逐字包含 SignedHeaders 里列的每一个头**。
	// 漏掉 host 这一行是最经典的坑：签名照算不误，上游一律
	// SignatureDoesNotMatch，而且报错里一个字都不会提示你漏了什么。
	var canonHeaders strings.Builder
	canonHeaders.WriteString("host:" + headers["host"] + "\n")
	for _, n := range names {
		canonHeaders.WriteString(n + ":" + headers[n] + "\n")
	}
	signedHeaders := "host;" + strings.Join(names, ";")

	// 规范请求
	canonURI := escapePath(req.URL.Path)
	canonReq := strings.Join([]string{
		req.Method,
		canonURI,
		req.URL.RawQuery,
		canonHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")
	crSum := sha256.Sum256([]byte(canonReq))

	// 待签字符串
	scope := dateStamp + "/" + s.region + "/s3/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hex.EncodeToString(crSum[:]),
	}, "\n")

	// 派生签名密钥
	kDate := hmacSHA256([]byte("AWS4"+s.secret), dateStamp)
	kRegion := hmacSHA256(kDate, s.region)
	kService := hmacSHA256(kRegion, "s3")
	kSigning := hmacSHA256(kService, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(kSigning, stringToSign))

	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		s.access, scope, signedHeaders, signature))
	return nil
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// escapePath 按 S3 要求转义路径：不转义 "/"，其余按 RFC3986。
// 与 url.PathEscape 不同的是它保留斜杠。
func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}