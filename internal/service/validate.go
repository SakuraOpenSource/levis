package service

import (
	"net/mail"
	"regexp"
	"strings"
	"unicode"
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

// ValidateRealName 校验真实姓名，返回去除首尾空白后的结果。
//
// 只允许中日韩文字、拉丁字母、间隔号与空格：数字和标点出现在姓名里几乎总是
// 录入错误（把身份证号填进了姓名框之类），拦在存下来之前比事后清理省事。
func ValidateRealName(name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	count := utf8.RuneCountInString(trimmed)
	if count < 2 || count > 32 {
		return "", ErrBadRequest("姓名需为 2-32 个字符")
	}
	for _, r := range trimmed {
		switch {
		case unicode.Is(unicode.Han, r),
			unicode.Is(unicode.Hiragana, r),
			unicode.Is(unicode.Katakana, r),
			unicode.Is(unicode.Hangul, r),
			r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z',
			r == '·', r == '•', r == ' ', r == '.', r == '-', r == '\'':
		default:
			return "", ErrBadRequest("姓名含不支持的字符")
		}
	}
	return trimmed, nil
}

// idWeights 是身份证前 17 位的加权因子（GB 11643 规定）。
var idWeights = [17]int{7, 9, 10, 5, 8, 4, 2, 1, 6, 3, 7, 9, 10, 5, 8, 4, 2}

// idCheckCodes 是加权和模 11 后对应的校验位字符。
var idCheckCodes = [11]byte{'1', '0', 'X', '9', '8', '7', '6', '5', '4', '3', '2'}

// ValidateIDNumber 校验 18 位居民身份证号，返回规范化（末位大写）后的结果。
//
// 除长度与字符集外还验算校验位：这能挡住绝大多数手滑输入，代价只有十几行
// 代码。注意这只是格式自校验，不等于该号码真实存在 —— 真实性要靠人工核对
// 证件照，或日后接入实名核验接口。
func ValidateIDNumber(id string) (string, error) {
	normalized := strings.ToUpper(strings.TrimSpace(id))
	if len(normalized) != 18 {
		return "", ErrBadRequest("身份证号需为 18 位")
	}

	sum := 0
	for i := range 17 {
		c := normalized[i]
		if c < '0' || c > '9' {
			return "", ErrBadRequest("身份证号前 17 位必须是数字")
		}
		sum += int(c-'0') * idWeights[i]
	}
	last := normalized[17]
	if last != 'X' && (last < '0' || last > '9') {
		return "", ErrBadRequest("身份证号末位必须是数字或 X")
	}
	if idCheckCodes[sum%11] != last {
		return "", ErrBadRequest("身份证号校验位不正确，请核对后重填")
	}
	return normalized, nil
}
