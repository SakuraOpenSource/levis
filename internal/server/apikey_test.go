package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/SakuraOpenSource/levis/internal/model"
	"github.com/SakuraOpenSource/levis/internal/service"
)

// apiKeyItem 是 Key 列表项中测试关心的部分。
type apiKeyItem struct {
	ID     uint     `json:"id"`
	Name   string   `json:"name"`
	Prefix string   `json:"prefix"`
	Scopes []string `json:"scopes"`
	Status string   `json:"status"`
}

// listKeys 读当前用户的 Key 列表，返回原始响应与解析结果。
func listKeys(
	t *testing.T, handler http.Handler, cookies []*http.Cookie,
) (string, []apiKeyItem, []string) {
	t.Helper()
	rec := doAs(t, handler, http.MethodGet, "/api/api-keys", nil, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("读取 Key 列表失败: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Items  []apiKeyItem `json:"items"`
		Scopes []string     `json:"scopes"`
	}
	decodeJSON(t, rec, &body)
	return rec.Body.String(), body.Items, body.Scopes
}

// 未实名不能创建 Key；通过审核之后才放行。
func TestCreateAPIKeyRequiresApprovedKYC(t *testing.T) {
	_, handler, admin, users := installedWithUsers(t, "alice")
	alice := users["alice"]

	create := func() int {
		rec := doAs(t, handler, http.MethodPost, "/api/api-keys", map[string]any{
			"name":   "自动化脚本",
			"scopes": []string{model.ScopeBalanceRead},
		}, alice)
		return rec.Code
	}

	// 从未提交过。
	if code := create(); code != http.StatusForbidden {
		t.Fatalf("未实名创建 Key 应返回 403，实际 %d", code)
	}

	// 提交但还在审核中，仍然不行 —— 闸门是「已通过」，不是「已提交」。
	if rec := submitKYC(t, handler, alice, "张三", validID1); rec.Code != http.StatusOK {
		t.Fatalf("提交实名失败: %d %s", rec.Code, rec.Body.String())
	}
	if code := create(); code != http.StatusForbidden {
		t.Fatalf("审核中创建 Key 应返回 403，实际 %d", code)
	}

	// 被驳回同样不行。
	record := mineKYC(t, handler, alice)
	rec := doAs(t, handler, http.MethodPost,
		"/api/admin/verifications/"+itoa(record.ID)+"/reject",
		map[string]string{"reason": "重拍"}, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("驳回失败: %d %s", rec.Code, rec.Body.String())
	}
	if code := create(); code != http.StatusForbidden {
		t.Fatalf("被驳回后创建 Key 应返回 403，实际 %d", code)
	}

	// 通过之后放行。
	if rec := submitKYC(t, handler, alice, "张三", validID1); rec.Code != http.StatusOK {
		t.Fatalf("重新提交失败: %d %s", rec.Code, rec.Body.String())
	}
	current := mineKYC(t, handler, alice)
	if rec := doAs(t, handler, http.MethodPost,
		"/api/admin/verifications/"+itoa(current.ID)+"/approve", nil, admin); rec.Code != http.StatusOK {
		t.Fatalf("审核通过失败: %d %s", rec.Code, rec.Body.String())
	}
	if code := create(); code != http.StatusOK {
		t.Fatalf("实名通过后创建 Key 应返回 200，实际 %d", code)
	}
}

// 明文只在创建响应里出现一次，列表接口既不返回明文也不返回 hash。
func TestAPIKeySecretShownOnlyOnce(t *testing.T) {
	_, handler, admin, users := installedWithUsers(t, "alice")
	alice := users["alice"]
	passKYC(t, handler, admin, alice, "张三", validID1)

	rec := doAs(t, handler, http.MethodPost, "/api/api-keys", map[string]any{
		"name":   "自动化脚本",
		"scopes": []string{model.ScopeBalanceRead, model.ScopeOrderWrite},
	}, alice)
	if rec.Code != http.StatusOK {
		t.Fatalf("创建 Key 失败: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Key    apiKeyItem `json:"key"`
		Secret string     `json:"secret"`
	}
	decodeJSON(t, rec, &created)

	if !strings.HasPrefix(created.Secret, "lvs_") {
		t.Errorf("明文应以 lvs_ 开头，实际 %q", created.Secret)
	}
	// lvs_ + 32 位十六进制 = 128 位熵。
	if len(created.Secret) != len("lvs_")+32 {
		t.Errorf("明文长度应为 %d，实际 %d", len("lvs_")+32, len(created.Secret))
	}
	// 创建响应里也不能出现 hash。
	if strings.Contains(rec.Body.String(), service.HashAPIKey(created.Secret)) {
		t.Error("创建响应泄露了 key_hash")
	}
	if created.Key.Prefix != created.Secret[:len("lvs_")+8] {
		t.Errorf("prefix 应为明文前若干位，实际 %q", created.Key.Prefix)
	}

	raw, items, scopes := listKeys(t, handler, alice)
	if len(items) != 1 {
		t.Fatalf("应有 1 把 Key，实际 %d 把", len(items))
	}
	if strings.Contains(raw, created.Secret) {
		t.Error("列表接口泄露了明文 Key")
	}
	if strings.Contains(raw, service.HashAPIKey(created.Secret)) {
		t.Error("列表接口泄露了 key_hash")
	}
	if strings.Contains(raw, "key_hash") {
		t.Errorf("列表响应不该出现 key_hash 字段: %s", raw)
	}
	if items[0].Status != model.APIKeyActive {
		t.Errorf("新建 Key 状态应为 active，实际 %s", items[0].Status)
	}
	// 权限位按 AllScopes 的顺序稳定输出，前端展示与库里存的顺序才一致。
	want := []string{model.ScopeBalanceRead, model.ScopeOrderWrite}
	if strings.Join(items[0].Scopes, ",") != strings.Join(want, ",") {
		t.Errorf("权限位 = %v，期望 %v", items[0].Scopes, want)
	}
	// 列表要附上可选权限清单，前端就不必再硬编码一份。
	if strings.Join(scopes, ",") != strings.Join(model.AllScopes(), ",") {
		t.Errorf("scopes 清单 = %v，期望 %v", scopes, model.AllScopes())
	}
}

// 创建入参的校验：名称、权限位、有效期。
func TestCreateAPIKeyValidation(t *testing.T) {
	_, handler, admin, users := installedWithUsers(t, "alice")
	alice := users["alice"]
	passKYC(t, handler, admin, alice, "张三", validID1)

	cases := []struct {
		name string
		body map[string]any
	}{
		{"名称为空", map[string]any{"name": "  ", "scopes": []string{model.ScopeBalanceRead}}},
		{"未勾选权限", map[string]any{"name": "k", "scopes": []string{}}},
		{"未知权限位", map[string]any{"name": "k", "scopes": []string{"admin:*"}}},
		{"有效期为负", map[string]any{
			"name": "k", "scopes": []string{model.ScopeBalanceRead}, "expires_in_days": -1,
		}},
		{"有效期过长", map[string]any{
			"name": "k", "scopes": []string{model.ScopeBalanceRead}, "expires_in_days": 4000,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doAs(t, handler, http.MethodPost, "/api/api-keys", tc.body, alice)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s 应返回 400，实际 %d %s", tc.name, rec.Code, rec.Body.String())
			}
		})
	}
	if _, items, _ := listKeys(t, handler, alice); len(items) != 0 {
		t.Fatalf("非法入参不该建出 Key，实际 %d 把", len(items))
	}
}

// 吊销后同一把 Key 立刻失效。
func TestRevokedKeyStopsWorking(t *testing.T) {
	_, handler, admin, users := installedWithUsers(t, "alice")
	alice := users["alice"]
	passKYC(t, handler, admin, alice, "张三", validID1)
	secret := createKey(t, handler, alice, "只读", []string{model.ScopeBalanceRead})

	if rec := doKey(t, handler, http.MethodGet, "/api/open/v1/account", nil,
		secret); rec.Code != http.StatusOK {
		t.Fatalf("吊销前应能调用，实际 %d %s", rec.Code, rec.Body.String())
	}

	_, items, _ := listKeys(t, handler, alice)
	id := itoa(items[0].ID)
	if rec := doAs(t, handler, http.MethodDelete, "/api/api-keys/"+id, nil,
		alice); rec.Code != http.StatusNoContent {
		t.Fatalf("吊销应返回 204，实际 %d %s", rec.Code, rec.Body.String())
	}

	rec := doKey(t, handler, http.MethodGet, "/api/open/v1/account", nil, secret)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("吊销后应立刻 401，实际 %d %s", rec.Code, rec.Body.String())
	}

	// 记录留着以备追查，状态置为 revoked。
	_, after, _ := listKeys(t, handler, alice)
	if len(after) != 1 {
		t.Fatalf("吊销不该删行，实际剩 %d 条", len(after))
	}
	if after[0].Status != model.APIKeyRevoked {
		t.Errorf("状态应为 revoked，实际 %s", after[0].Status)
	}
	// 重复吊销是冲突。
	if rec := doAs(t, handler, http.MethodDelete, "/api/api-keys/"+id, nil,
		alice); rec.Code != http.StatusConflict {
		t.Errorf("重复吊销应返回 409，实际 %d", rec.Code)
	}
}

// 别人的 Key 既看不到也吊销不了，且返回 404 而非 403。
func TestAPIKeyCrossUserIsolation(t *testing.T) {
	_, handler, admin, users := installedWithUsers(t, "alice", "bob")
	passKYC(t, handler, admin, users["alice"], "张三", validID1)
	passKYC(t, handler, admin, users["bob"], "李四", validID2)
	createKey(t, handler, users["alice"], "alice 的 Key", []string{model.ScopeBalanceRead})

	_, aliceKeys, _ := listKeys(t, handler, users["alice"])
	if len(aliceKeys) != 1 {
		t.Fatalf("alice 应有 1 把 Key，实际 %d", len(aliceKeys))
	}
	if _, bobKeys, _ := listKeys(t, handler, users["bob"]); len(bobKeys) != 0 {
		t.Fatalf("bob 的列表应为空，实际 %d 把", len(bobKeys))
	}

	rec := doAs(t, handler, http.MethodDelete,
		"/api/api-keys/"+itoa(aliceKeys[0].ID), nil, users["bob"])
	if rec.Code != http.StatusNotFound {
		t.Fatalf("吊销他人的 Key 应返回 404，实际 %d %s", rec.Code, rec.Body.String())
	}
	// 确认 alice 的 Key 没被动。
	_, after, _ := listKeys(t, handler, users["alice"])
	if after[0].Status != model.APIKeyActive {
		t.Errorf("alice 的 Key 被他人吊销了，状态 %s", after[0].Status)
	}
}

// 持有量上限拦在创建处，避免一个账号无限堆 Key。
func TestAPIKeyLimit(t *testing.T) {
	_, handler, admin, users := installedWithUsers(t, "alice")
	alice := users["alice"]
	passKYC(t, handler, admin, alice, "张三", validID1)

	for i := 0; i < service.MaxAPIKeys; i++ {
		createKey(t, handler, alice, "key", []string{model.ScopeBalanceRead})
	}
	rec := doAs(t, handler, http.MethodPost, "/api/api-keys", map[string]any{
		"name":   "再来一把",
		"scopes": []string{model.ScopeBalanceRead},
	}, alice)
	if rec.Code != http.StatusConflict {
		t.Fatalf("超过上限应返回 409，实际 %d %s", rec.Code, rec.Body.String())
	}

	// 吊销一把之后又能建了 —— 上限只算启用中的。
	_, items, _ := listKeys(t, handler, alice)
	if rec := doAs(t, handler, http.MethodDelete, "/api/api-keys/"+itoa(items[0].ID), nil,
		alice); rec.Code != http.StatusNoContent {
		t.Fatalf("吊销失败: %d", rec.Code)
	}
	rec = doAs(t, handler, http.MethodPost, "/api/api-keys", map[string]any{
		"name":   "补一把",
		"scopes": []string{model.ScopeBalanceRead},
	}, alice)
	if rec.Code != http.StatusOK {
		t.Fatalf("吊销后应能再建，实际 %d %s", rec.Code, rec.Body.String())
	}
}

// Key 管理接口本身仍属浏览器端：未登录 401，且写操作要过 CSRF。
func TestAPIKeyManagementRequiresSession(t *testing.T) {
	_, handler := installedServer(t)
	if rec := do(t, handler, http.MethodGet, "/api/api-keys", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("未登录读列表应返回 401，实际 %d", rec.Code)
	}
	// 用 API Key 也不能反过来管理 Key：那条路径上没有 CSRF 与登录态语义。
	if rec := doKey(t, handler, http.MethodGet, "/api/api-keys", nil,
		"lvs_00000000000000000000000000000000"); rec.Code != http.StatusUnauthorized {
		t.Errorf("拿 Key 访问管理接口应返回 401，实际 %d", rec.Code)
	}
}
