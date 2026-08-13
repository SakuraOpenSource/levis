package service

import (
	"net/mail"
	"regexp"
	"strings"
	"unicode/utf8"
)

// 用户名允许字母、数字、下划线和短横线，长度 3-32。
var usernamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{3,32}$`)

// MinPasswordLength 是密码最小长度。
const MinPasswordLength = 8

// ValidateUsername 校验用户名格式。
func ValidateUsername(name string) error {
	if !usernamePattern.MatchString(name) {
		return ErrBadRequest("用户名需为 3-32 位字母、数字、下划线或短横线")
	}
	return nil
}

// ValidateEmail 校验邮箱格式，返回规范化（小写去空格）后的地址。
func ValidateEmail(email string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(email))
	addr, err := mail.ParseAddress(normalized)
	if err != nil || addr.Address != normalized {
		return "", ErrBadRequest("邮箱格式不正确")
	}
	if utf8.RuneCountInString(normalized) > 255 {
		return "", ErrBadRequest("邮箱过长")
	}
	return normalized, nil
}

// ValidatePassword 校验密码强度。
func ValidatePassword(password string) error {
	if utf8.RuneCountInString(password) < MinPasswordLength {
		return ErrBadRequest("密码至少需要 %d 位", MinPasswordLength)
	}
	// bcrypt 只取前 72 字节，更长的部分会被静默丢弃，这里直接拒绝以免误解。
	if len(password) > 72 {
		return ErrBadRequest("密码不能超过 72 字节")
	}
	return nil
}
