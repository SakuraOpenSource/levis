package auth

import (
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// TestDefaultPasswordCost 钉住生产强度。
//
// SetPasswordCost 让强度变成可改的，这条断言保证「可改」不会变成「被改」——
// 哪天有人为了让测试跑快而顺手调低默认值，这条用例会先失败。
func TestDefaultPasswordCost(t *testing.T) {
	if DefaultPasswordCost < 12 {
		t.Fatalf("生产 bcrypt 强度不得低于 12，当前为 %d", DefaultPasswordCost)
	}
	if passwordCost != DefaultPasswordCost {
		t.Fatalf("包初始化后的强度应为 %d，实际为 %d", DefaultPasswordCost, passwordCost)
	}
}

// TestSetPasswordCost 验证覆盖与还原，以及低于下限时被抬回下限。
func TestSetPasswordCost(t *testing.T) {
	restore := SetPasswordCost(bcrypt.MinCost)
	if passwordCost != bcrypt.MinCost {
		t.Fatalf("强度未被覆盖：期望 %d，实际 %d", bcrypt.MinCost, passwordCost)
	}
	restore()
	if passwordCost != DefaultPasswordCost {
		t.Fatalf("还原失败：期望 %d，实际 %d", DefaultPasswordCost, passwordCost)
	}

	// 传入非法值不该把强度设成 0 —— 那等于不加密。
	restore = SetPasswordCost(0)
	if passwordCost != bcrypt.MinCost {
		t.Fatalf("低于下限时应抬回 %d，实际 %d", bcrypt.MinCost, passwordCost)
	}
	restore()
}

// TestHashPasswordRoundTrip 确认降低强度只影响耗时，不影响校验结果，
// 且哈希串里记录的 cost 就是当时生效的强度。
func TestHashPasswordRoundTrip(t *testing.T) {
	restore := SetPasswordCost(bcrypt.MinCost)
	defer restore()

	hash, err := HashPassword("password123")
	if err != nil {
		t.Fatalf("计算哈希失败: %v", err)
	}
	cost, err := bcrypt.Cost([]byte(hash))
	if err != nil {
		t.Fatalf("解析哈希强度失败: %v", err)
	}
	if cost != bcrypt.MinCost {
		t.Fatalf("哈希强度应为 %d，实际 %d", bcrypt.MinCost, cost)
	}
	if !CheckPassword(hash, "password123") {
		t.Fatal("正确密码校验失败")
	}
	if CheckPassword(hash, "wrong-password") {
		t.Fatal("错误密码通过了校验")
	}
}
