package captcha

import (
	"bytes"
	"encoding/base64"
	"image/png"
	"strings"
	"testing"
	"time"
)

// newStore 构造一个自定义有效期的存储，供过期相关的用例免去真实等待。
func newStore(ttl time.Duration) *Store {
	return &Store{ttl: ttl, items: make(map[string]entry)}
}

// codeOf 取出某个 id 对应的答案。仅测试可用 —— 生产代码永远不下发答案。
func codeOf(t *testing.T, s *Store, id string) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.items[id]
	if !ok {
		t.Fatalf("验证码 %s 不在存储中", id)
	}
	return item.code
}

func TestIssueThenVerify(t *testing.T) {
	s := NewStore()
	ch, err := s.Issue(CharsetDigit, DefaultLength)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	if ch.ID == "" {
		t.Fatal("id 为空")
	}
	if !strings.HasPrefix(ch.Image, "data:image/png;base64,") {
		t.Fatalf("图片不是 PNG data URL: %.32s", ch.Image)
	}
	if ch.ExpiresIn != int(TTL.Seconds()) {
		t.Fatalf("ExpiresIn = %d，期望 %d", ch.ExpiresIn, int(TTL.Seconds()))
	}
	if !s.Verify(ch.ID, codeOf(t, s, ch.ID)) {
		t.Fatal("正确答案校验失败")
	}
}

// 答案绝不能出现在下发的图片字节里 —— 这正是不用 SVG 的原因。
func TestImageDoesNotLeakCode(t *testing.T) {
	s := NewStore()
	ch, err := s.Issue(CharsetMixed, 6)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	code := codeOf(t, s, ch.ID)
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(ch.Image, "data:image/png;base64,"))
	if err != nil {
		t.Fatalf("图片不是合法 base64: %v", err)
	}
	if bytes.Contains(raw, []byte(code)) {
		t.Fatalf("图片字节里出现了答案 %q", code)
	}
}

func TestVerifyIsCaseInsensitiveAndTrimmed(t *testing.T) {
	s := NewStore()
	ch, err := s.Issue(CharsetLetter, 5)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	answer := "  " + strings.ToLower(codeOf(t, s, ch.ID)) + "\t"
	if !s.Verify(ch.ID, answer) {
		t.Fatalf("小写带空格的答案 %q 应当通过", answer)
	}
}

// 一张图只能用一次：否则位数再多也能拿同一张图穷举。
func TestVerifyIsSingleUse(t *testing.T) {
	s := NewStore()
	ch, err := s.Issue(CharsetDigit, 4)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	code := codeOf(t, s, ch.ID)
	if !s.Verify(ch.ID, code) {
		t.Fatal("首次校验应通过")
	}
	if s.Verify(ch.ID, code) {
		t.Fatal("同一验证码被重复使用")
	}
	if s.Len() != 0 {
		t.Fatalf("校验后仍残留 %d 项", s.Len())
	}
}

// 答错也要立刻作废，不能留着让人接着试。
func TestWrongAnswerConsumesChallenge(t *testing.T) {
	s := NewStore()
	ch, err := s.Issue(CharsetDigit, 4)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	code := codeOf(t, s, ch.ID)
	if s.Verify(ch.ID, "@@@@") {
		t.Fatal("错误答案通过了校验")
	}
	if s.Verify(ch.ID, code) {
		t.Fatal("答错之后原答案仍然可用")
	}
}

func TestVerifyRejectsUnknownAndEmpty(t *testing.T) {
	s := NewStore()
	cases := []struct{ id, answer string }{
		{"", "1234"},
		{"deadbeef", ""},
		{"deadbeef", "1234"},
	}
	for _, tc := range cases {
		if s.Verify(tc.id, tc.answer) {
			t.Fatalf("Verify(%q, %q) 应当失败", tc.id, tc.answer)
		}
	}
}

func TestVerifyRejectsExpired(t *testing.T) {
	s := newStore(-time.Second) // 签发即过期
	ch, err := s.Issue(CharsetDigit, 4)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	s.mu.Lock()
	code := s.items[ch.ID].code
	s.mu.Unlock()
	if s.Verify(ch.ID, code) {
		t.Fatal("过期验证码通过了校验")
	}
}

// 签发时应顺手清掉已过期的项，否则公开接口会把内存越撑越大。
func TestIssueEvictsExpired(t *testing.T) {
	s := newStore(-time.Second)
	for range 5 {
		if _, err := s.Issue(CharsetDigit, 4); err != nil {
			t.Fatalf("签发失败: %v", err)
		}
	}
	if s.Len() != 1 {
		t.Fatalf("过期项未被清理，剩余 %d 项，期望 1", s.Len())
	}
}

func TestIssueHonoursCharsetAndLength(t *testing.T) {
	cases := []struct {
		charset string
		want    string
	}{
		{CharsetDigit, digitSet},
		{CharsetLetter, letterSet},
		{CharsetMixed, mixedDigitSet + mixedLetterSet},
		{"不认识的类型", mixedDigitSet + mixedLetterSet},
	}
	for _, tc := range cases {
		s := NewStore()
		for length := MinLength; length <= MaxLength; length++ {
			ch, err := s.Issue(tc.charset, length)
			if err != nil {
				t.Fatalf("charset=%s length=%d 签发失败: %v", tc.charset, length, err)
			}
			code := codeOf(t, s, ch.ID)
			if len(code) != length {
				t.Fatalf("charset=%s 期望 %d 位，得到 %q", tc.charset, length, code)
			}
			for i := range len(code) {
				if !strings.ContainsRune(tc.want, rune(code[i])) {
					t.Fatalf("charset=%s 出现越界字符 %q（答案 %q）", tc.charset, code[i], code)
				}
			}
		}
	}
}

func TestIssueClampsLength(t *testing.T) {
	cases := []struct{ in, want int }{
		{0, MinLength},
		{-3, MinLength},
		{MinLength - 1, MinLength},
		{MaxLength + 1, MaxLength},
		{999, MaxLength},
	}
	for _, tc := range cases {
		s := NewStore()
		ch, err := s.Issue(CharsetDigit, tc.in)
		if err != nil {
			t.Fatalf("length=%d 签发失败: %v", tc.in, err)
		}
		if got := len(codeOf(t, s, ch.ID)); got != tc.want {
			t.Fatalf("length=%d 收敛为 %d 位，期望 %d 位", tc.in, got, tc.want)
		}
	}
}

func TestValidCharset(t *testing.T) {
	for _, name := range []string{CharsetMixed, CharsetDigit, CharsetLetter} {
		if !ValidCharset(name) {
			t.Fatalf("%q 应当合法", name)
		}
	}
	for _, name := range []string{"", "MIXED", "number", "digit "} {
		if ValidCharset(name) {
			t.Fatalf("%q 不应合法", name)
		}
	}
}

// 任何可能被抽到的字符都必须有字模，否则 render 会在线上直接报错。
func TestEveryAlphabetCharHasGlyph(t *testing.T) {
	all := digitSet + letterSet + mixedDigitSet + mixedLetterSet
	for i := range len(all) {
		if _, ok := glyphs[all[i]]; !ok {
			t.Fatalf("字符 %q 缺少字模", all[i])
		}
	}
}

func TestGlyphShapesAreWellFormed(t *testing.T) {
	for char, shape := range glyphs {
		if len(shape) != glyphHeight {
			t.Fatalf("字符 %q 的字模有 %d 行，期望 %d 行", char, len(shape), glyphHeight)
		}
		filled := 0
		for i, row := range shape {
			if len(row) != glyphWidth {
				t.Fatalf("字符 %q 第 %d 行宽 %d，期望 %d", char, i, len(row), glyphWidth)
			}
			for j := range len(row) {
				switch row[j] {
				case '#':
					filled++
				case '.':
				default:
					t.Fatalf("字符 %q 第 %d 行出现非法像素 %q", char, i, row[j])
				}
			}
		}
		if filled == 0 {
			t.Fatalf("字符 %q 的字模是空白的", char)
		}
	}
}

// 剔除易混字符是刻意为之，这里锁死避免日后被顺手加回去。
func TestConfusableCharsExcluded(t *testing.T) {
	for _, ch := range "ILO" {
		if strings.ContainsRune(letterSet, ch) {
			t.Fatalf("纯字母集不应包含易混字符 %q", ch)
		}
	}
	mixed := mixedDigitSet + mixedLetterSet
	for _, ch := range "01ILOSZ" {
		if strings.ContainsRune(mixed, ch) {
			t.Fatalf("混排集不应包含易混字符 %q", ch)
		}
	}
}

func TestRenderProducesDecodablePNG(t *testing.T) {
	raw, err := render("A2C4")
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("PNG 解码失败: %v", err)
	}
	bounds := img.Bounds()
	wantWidth := paddingX*2 + 4*charAdvance
	if bounds.Dx() != wantWidth || bounds.Dy() != imageHeight {
		t.Fatalf("图片尺寸 %dx%d，期望 %dx%d", bounds.Dx(), bounds.Dy(), wantWidth, imageHeight)
	}
}

func TestRenderRejectsUnknownChar(t *testing.T) {
	if _, err := render("A中C"); err == nil {
		t.Fatal("含无字模字符时应当报错")
	}
}

// 同一份原文每次渲染都应不同，否则可以按图片指纹建表反查答案。
func TestRenderVariesBetweenCalls(t *testing.T) {
	first, err := render("2345")
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	second, err := render("2345")
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("两次渲染结果完全相同，图片缺少随机干扰")
	}
}

func TestIssueGeneratesUniqueIDs(t *testing.T) {
	s := NewStore()
	seen := make(map[string]bool)
	for range 64 {
		ch, err := s.Issue(CharsetDigit, 4)
		if err != nil {
			t.Fatalf("签发失败: %v", err)
		}
		if seen[ch.ID] {
			t.Fatalf("id 重复: %s", ch.ID)
		}
		seen[ch.ID] = true
	}
	if s.Len() != 64 {
		t.Fatalf("存储中有 %d 项，期望 64", s.Len())
	}
}

func TestStoreIsConcurrencySafe(t *testing.T) {
	s := NewStore()
	done := make(chan struct{})
	for range 8 {
		go func() {
			defer func() { done <- struct{}{} }()
			for range 20 {
				ch, err := s.Issue(CharsetMixed, 6)
				if err != nil {
					t.Errorf("签发失败: %v", err)
					return
				}
				s.Verify(ch.ID, "0000")
				s.Len()
			}
		}()
	}
	for range 8 {
		<-done
	}
}
