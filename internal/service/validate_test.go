package service

import (
	"testing"

	"github.com/SakuraOpenSource/levis/internal/model"
)

func TestValidateRealName(t *testing.T) {
	ok := []struct{ in, want string }{
		{"张三", "张三"},
		{"  李四  ", "李四"}, // 首尾空白应被去掉
		{"欧阳修之", "欧阳修之"},
		{"John Smith", "John Smith"},
		{"买买提·吐尔逊", "买买提·吐尔逊"},
		{"山田太郎", "山田太郎"},
	}
	for _, tc := range ok {
		got, err := ValidateRealName(tc.in)
		if err != nil {
			t.Errorf("ValidateRealName(%q) 意外失败: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ValidateRealName(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}

	bad := []string{
		"",
		"张",                  // 太短
		"张3",                 // 含数字，通常是把号码填进了姓名框
		"110101199003078888", // 整串身份证号
		"张三<script>",
		"张三@example.com",
	}
	for _, in := range bad {
		if _, err := ValidateRealName(in); err == nil {
			t.Errorf("ValidateRealName(%q) 应当失败", in)
		}
	}
}

func TestValidateIDNumberAcceptsValidChecksum(t *testing.T) {
	// 校验位由 GB 11643 的加权模 11 算法算得。
	cases := []struct{ in, want string }{
		{"110101199003077838", "110101199003077838"},
		{"11010119900307002X", "11010119900307002X"},
		{"11010119900307002x", "11010119900307002X"}, // 末位小写应归一化为大写
		{" 11010119900307002X ", "11010119900307002X"},
		{"440524188001010014", "440524188001010014"},
		{"510102198512014567", "510102198512014567"},
	}
	for _, tc := range cases {
		got, err := ValidateIDNumber(tc.in)
		if err != nil {
			t.Errorf("ValidateIDNumber(%q) 意外失败: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ValidateIDNumber(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
}

func TestValidateIDNumberRejectsInvalid(t *testing.T) {
	bad := []struct{ name, in string }{
		{"空串", ""},
		{"15 位老号码", "110101900307783"},
		{"少一位", "11010119900307783"},
		{"多一位", "1101011990030778381"},
		{"校验位错", "110101199003077831"},
		{"末位误填 X", "11010119900307783X"},
		{"前 17 位含字母", "1101011990030778X8"},
		{"末位非法字母", "11010119900307783A"},
		{"全零", "000000000000000000"},
	}
	for _, tc := range bad {
		if _, err := ValidateIDNumber(tc.in); err == nil {
			t.Errorf("%s：ValidateIDNumber(%q) 应当失败", tc.name, tc.in)
		}
	}
}

// 每个合法号码只有一个正确校验位，改动任意一位都应被识破。
func TestValidateIDNumberDetectsSingleDigitTypo(t *testing.T) {
	const valid = "110101199003077838"
	for i := range 17 {
		for d := byte('0'); d <= '9'; d++ {
			if valid[i] == d {
				continue
			}
			mutated := []byte(valid)
			mutated[i] = d
			if _, err := ValidateIDNumber(string(mutated)); err == nil {
				t.Errorf("改动第 %d 位得到的 %s 竟被接受", i+1, mutated)
			}
		}
	}
}

// escapeLike 必须把 LIKE 通配符与转义符本身都转掉，配合 ESCAPE 子句工作。
func TestEscapeLike(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", "plain"},
		{"100%", `100\%`},
		{"a_b", `a\_b`},
		{`back\slash`, `back\\slash`},
		{"混排%中文", `混排\%中文`},
	}
	for _, c := range cases {
		if got := escapeLike(c.in); got != c.want {
			t.Errorf("escapeLike(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// 管理端用户搜索带 % 时应按字面匹配而不是全表通配。
func TestAdminUserSearchEscapesLikeWildcards(t *testing.T) {
	db := newTestDB(t)
	seed := []model.User{
		{Username: "alice", Email: "alice@example.com", PasswordHash: "x", Role: model.RoleUser, Status: model.UserActive},
		{Username: "bob", Email: "bob@example.com", PasswordHash: "x", Role: model.RoleUser, Status: model.UserActive},
	}
	for i := range seed {
		if err := db.Create(&seed[i]).Error; err != nil {
			t.Fatalf("造数失败: %v", err)
		}
	}
	svc := NewAdminService(db, nil, nil, nil)
	items, total, err := svc.Users("%", 0, 20)
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	if total != 0 || len(items) != 0 {
		t.Fatalf("搜索 %% 应按字面匹配查不到任何人，实际 total=%d items=%d", total, len(items))
	}
	items, total, err = svc.Users("ali", 0, 20)
	if err != nil || total != 1 || len(items) != 1 {
		t.Fatalf("普通关键词应命中 alice：err=%v total=%d", err, total)
	}
}
