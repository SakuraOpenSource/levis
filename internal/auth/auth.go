// Package auth 提供密码哈希与 JWT 签发/校验。
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

// TokenTTL 是登录态有效期。
const TokenTTL = 7 * 24 * time.Hour

// Cookie 名称。CSRF cookie 需要被前端 JS 读取，因此不能是 httpOnly；
// token cookie 必须是 httpOnly，避免 XSS 直接取走凭证。
const (
	CookieToken = "levis_token"
	CookieCSRF  = "levis_csrf"
	HeaderCSRF  = "X-CSRF-Token"
)

// ErrInvalidToken 表示 token 缺失、过期或签名不匹配。
var ErrInvalidToken = errors.New("凭证无效或已过期")

// Claims 是 JWT 载荷。Role 一并签入，省去每次请求都查库判断权限；
// 但涉及权限变更的敏感操作仍应以数据库为准。
type Claims struct {
	jwt.RegisteredClaims
	UserID uint   `json:"uid"`
	Role   string `json:"role"`
}

// DefaultPasswordCost 是生产环境的 bcrypt 计算强度。
//
// 12 比 bcrypt 默认的 10 更耐暴破，单次约几十毫秒，登录场景可接受。
const DefaultPasswordCost = 12

// passwordCost 是当前生效的 bcrypt 强度，只由 SetPasswordCost 改动。
//
// 做成变量而非常量是为了让测试降下来：cost 每加 1 耗时翻倍，而 race
// 检测器又会把开销再放大约一个数量级，按生产强度跑整套接口测试会撞上
// go test 的默认超时。
var passwordCost = DefaultPasswordCost

// SetPasswordCost 覆盖 bcrypt 强度并返回一个还原函数。
//
// 仅供测试在 TestMain 里调用，生产代码不应触碰它 —— 调低强度等于削弱
// 全站密码存储。低于 bcrypt 允许的下限时按下限处理。
func SetPasswordCost(cost int) (restore func()) {
	if cost < bcrypt.MinCost {
		cost = bcrypt.MinCost
	}
	previous := passwordCost
	passwordCost = cost
	return func() { passwordCost = previous }
}

// HashPassword 用 bcrypt 计算密码哈希。
func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), passwordCost)
	if err != nil {
		return "", fmt.Errorf("密码加密失败: %w", err)
	}
	return string(hash), nil
}

// CheckPassword 校验明文密码与哈希是否匹配。
func CheckPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// GenerateToken 为用户签发 JWT。
func GenerateToken(secret string, userID uint, role string) (string, time.Time, error) {
	expires := time.Now().Add(TokenTTL)
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   fmt.Sprint(userID),
			ExpiresAt: jwt.NewNumericDate(expires),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
		UserID: userID,
		Role:   role,
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("签发凭证失败: %w", err)
	}
	return signed, expires, nil
}

// ParseToken 校验并解析 JWT。
func ParseToken(secret, token string) (*Claims, error) {
	claims := &Claims{}
	parsed, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (any, error) {
		return []byte(secret), nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}))
	if err != nil || !parsed.Valid {
		return nil, ErrInvalidToken
	}
	if claims.UserID == 0 {
		return nil, ErrInvalidToken
	}
	return claims, nil
}

// GenerateCSRFToken 生成一个随机 CSRF token。
func GenerateCSRFToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成 CSRF 令牌失败: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// SecureCompare 以恒定时间比较两个字符串，避免时序侧信道。
func SecureCompare(a, b string) bool {
	return len(a) > 0 && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
