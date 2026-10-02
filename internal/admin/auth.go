package admin

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"dadyumo/internal/config"
	"dadyumo/internal/logx"
)

const (
	sessionCookie = "dadyumo_session"
	csrfCookie    = "dadyumo_csrf"
	sessionTTL    = 4 * time.Hour
)

// initCredentials 确保存在可用的登录凭证与会话密钥。
//
// 关于明文密码的存活时机，这里有个必须注意的坑：
// 早期版本在生成随机密码后、同一次启动里就立刻把明文抹掉了，
// 结果「首次启动自动生成密码」变成了「生成了一个谁也拿不到的密码」——
// 用户根本来不及去读 config.json。
// 现在改成：明文一直保留到用户第一次登录成功，才由 clearPlainPassword 抹掉。
func initCredentials(store *config.Store) error {
	cfg := store.Get()

	if cfg.Admin.SessionSecret == "" {
		b := make([]byte, 32)
		if _, e := rand.Read(b); e != nil {
			return fmt.Errorf("生成会话密钥失败: %w", e)
		}
		secret := hex.EncodeToString(b)
		if err := store.Update(func(c *config.Config) (bool, error) {
			c.Admin.SessionSecret = secret
			return true, nil
		}); err != nil {
			return err
		}
		logx.Info("已自动生成管理端会话密钥")
	}

	cfg = store.Get()
	if cfg.Admin.PasswordBcrypt == "" {
		if cfg.Admin.PasswordPlain == "" {
			b := make([]byte, 8)
			_, _ = rand.Read(b)
			plain := "dadyumo-" + hex.EncodeToString(b)
			if err := store.Update(func(c *config.Config) (bool, error) {
				c.Admin.PasswordPlain = plain
				return true, nil
			}); err != nil {
				return err
			}
			logx.Warn("管理端未配置密码，已生成随机密码，请查看 config.json 中的 admin.password_plain")
		}
		cfg = store.Get()
		hash, e := bcrypt.GenerateFromPassword([]byte(cfg.Admin.PasswordPlain), bcrypt.DefaultCost)
		if e != nil {
			return fmt.Errorf("生成密码哈希失败: %w", e)
		}
		if err := store.Update(func(c *config.Config) (bool, error) {
			c.Admin.PasswordBcrypt = string(hash)
			// 明文此刻必须留着：用户还没登录过，删了他就再也拿不到密码了
			return true, nil
		}); err != nil {
			return err
		}
		logx.Warn("管理端初始密码已生成，登录成功后明文会自动从配置中清除",
			"用户名", cfg.Admin.Username, "去哪里看", store.Path()+" 的 admin.password_plain")
	}
	return nil
}

// clearPlainPassword 首次登录成功后抹掉明文密码，用完即焚。
func clearPlainPassword(store *config.Store) {
	_ = store.Update(func(c *config.Config) (bool, error) {
		if c.Admin.PasswordPlain == "" {
			return false, nil
		}
		c.Admin.PasswordPlain = ""
		return true, nil
	})
	logx.Info("管理端明文密码已清除（登录成功，用完即焚）")
}

// verifyPassword 校验密码
func verifyPassword(cfg config.Config, user, pass string) bool {
	if user != cfg.Admin.Username {
		return false
	}
	if cfg.Admin.PasswordBcrypt == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(cfg.Admin.PasswordBcrypt), []byte(pass)) == nil
}

// signSession 生成签名会话串： base64(payload).hmac
func signSession(secret, payload string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// parseSession 校验并解析会话串
func parseSession(secret, token string) (user string, ok bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(raw)
	want := mac.Sum(nil)
	if subtle.ConstantTimeCompare(sig, want) != 1 {
		return "", false
	}
	// payload: user|expireUnix
	fields := strings.SplitN(string(raw), "|", 2)
	if len(fields) != 2 {
		return "", false
	}
	var exp int64
	if _, err := fmt.Sscan(fields[1], &exp); err != nil {
		return "", false
	}
	if time.Now().Unix() > exp {
		return "", false
	}
	return fields[0], true
}

// loginLimiter 登录失败限流（按 IP）
type loginLimiter struct {
	mu        sync.Mutex
	failures  map[string]int
	lockUntil map[string]time.Time
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{failures: map[string]int{}, lockUntil: map[string]time.Time{}}
}

// allow 是否允许本次登录尝试
func (l *loginLimiter) allow(ip string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if until, ok := l.lockUntil[ip]; ok {
		if time.Now().Before(until) {
			return false, time.Until(until)
		}
		delete(l.lockUntil, ip)
		l.failures[ip] = 0
	}
	return true, 0
}

func (l *loginLimiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failures[ip]++
	n := l.failures[ip]
	var lock time.Duration
	switch {
	case n >= 10:
		lock = 15 * time.Minute
	case n >= 6:
		lock = 2 * time.Minute
	case n >= 4:
		lock = 30 * time.Second
	}
	if lock > 0 {
		l.lockUntil[ip] = time.Now().Add(lock)
		logx.Warn("管理端登录失败次数过多，已临时锁定", "ip", ip, "次数", n, "时长", lock.String())
	}
}

func (l *loginLimiter) reset(ip string) {
	l.mu.Lock()
	delete(l.failures, ip)
	delete(l.lockUntil, ip)
	l.mu.Unlock()
}

func clientIP(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		return strings.TrimSpace(strings.Split(v, ",")[0])
	}
	if v := r.Header.Get("X-Real-IP"); v != "" {
		return v
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
