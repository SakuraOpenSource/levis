package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/SakuraOpenSource/levis/internal/model"
	"github.com/SakuraOpenSource/levis/internal/service"
)

// kycRecord 是实名记录响应中测试关心的部分。
type kycRecord struct {
	ID           uint   `json:"id"`
	UserID       uint   `json:"user_id"`
	RealName     string `json:"real_name"`
	IDNumber     string `json:"id_number"`
	Status       string `json:"status"`
	RejectReason string `json:"reject_reason"`
	Username     string `json:"username"`
}

// mineKYC 读当前用户的实名状态。
func mineKYC(t *testing.T, handler http.Handler, cookies []*http.Cookie) *kycRecord {
	t.Helper()
	rec := doAs(t, handler, http.MethodGet, "/api/kyc", nil, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("读取实名状态失败: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Record *kycRecord `json:"record"`
	}
	decodeJSON(t, rec, &body)
	return body.Record
}

// 提交后落盘两张照片，状态为待审核。
func TestSubmitVerification(t *testing.T) {
	rt, handler, _, users := installedWithUsers(t, "alice")

	rec := submitKYC(t, handler, users["alice"], "张三", validID1)
	if rec.Code != http.StatusOK {
		t.Fatalf("提交实名应返回 200，实际 %d %s", rec.Code, rec.Body.String())
	}
	var record kycRecord
	decodeJSON(t, rec, &record)
	if record.Status != model.KYCPending {
		t.Errorf("提交后状态应为 pending，实际 %s", record.Status)
	}
	if record.RealName != "张三" {
		t.Errorf("姓名应为张三，实际 %s", record.RealName)
	}

	if files := uploadedFiles(t, rt); len(files) != 2 {
		t.Fatalf("应落盘 2 张照片，实际 %d 个文件: %v", len(files), files)
	}
	// 照片路径属于实现细节，响应里绝不能出现。
	for _, key := range []string{"front_path", "back_path", "stored_path"} {
		if strings.Contains(rec.Body.String(), key) {
			t.Errorf("响应不该包含 %s：%s", key, rec.Body.String())
		}
	}
}

// 用户侧响应里的身份证号必须打码 —— 完整号码不该再离开服务端一次。
func TestVerificationIDNumberMasked(t *testing.T) {
	_, handler, _, users := installedWithUsers(t, "alice")

	rec := submitKYC(t, handler, users["alice"], "张三", validID1)
	if strings.Contains(rec.Body.String(), validID1) {
		t.Errorf("提交响应泄露了完整身份证号: %s", rec.Body.String())
	}
	var submitted kycRecord
	decodeJSON(t, rec, &submitted)
	if submitted.IDNumber != model.MaskIDNumber(validID1) {
		t.Errorf("提交响应号码应打码，实际 %s", submitted.IDNumber)
	}

	record := mineKYC(t, handler, users["alice"])
	if record == nil {
		t.Fatal("查询自己的实名记录返回 null")
	}
	if record.IDNumber == validID1 {
		t.Error("查询接口泄露了完整身份证号")
	}
	if record.IDNumber != model.MaskIDNumber(validID1) {
		t.Errorf("查询接口号码应打码，实际 %s", record.IDNumber)
	}
	// 打码要留下可辨认的头尾，用户得能确认自己填的是哪张证。
	if !strings.HasPrefix(record.IDNumber, validID1[:6]) ||
		!strings.HasSuffix(record.IDNumber, validID1[14:]) {
		t.Errorf("打码后应保留前 6 后 4 位，实际 %s", record.IDNumber)
	}
}

// 从未提交过时 record 为 null，前端据此显示提交表单。
func TestVerificationEmptyBeforeSubmit(t *testing.T) {
	_, handler, _, users := installedWithUsers(t, "alice")
	if record := mineKYC(t, handler, users["alice"]); record != nil {
		t.Fatalf("未提交时 record 应为 null，实际 %+v", record)
	}
}

// 证件照内联下发，但只在 MIME 白名单之内，且不进任何缓存。
func TestVerificationPhotoServedInline(t *testing.T) {
	_, handler, _, users := installedWithUsers(t, "alice")
	if rec := submitKYC(t, handler, users["alice"], "张三", validID1); rec.Code != http.StatusOK {
		t.Fatalf("提交实名失败: %d %s", rec.Code, rec.Body.String())
	}

	for _, side := range []string{service.SideFront, service.SideBack} {
		rec := doAs(t, handler, http.MethodGet, "/api/kyc/photo/"+side, nil, users["alice"])
		if rec.Code != http.StatusOK {
			t.Fatalf("取 %s 照片应返回 200，实际 %d %s", side, rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Content-Type"); got != "image/png" {
			t.Errorf("%s 照片类型应为 image/png，实际 %q", side, got)
		}
		if got := rec.Header().Get("Content-Disposition"); got != "inline" {
			t.Errorf("%s 照片应内联展示，实际 %q", side, got)
		}
		// 证件照尤其不该被任何中间缓存留下副本。
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s 照片应带 no-store，实际 %q", side, got)
		}
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s 照片应带 nosniff，实际 %q", side, got)
		}
	}

	// side 只能是 front 或 back，别的值不该落到文件系统上去试。
	if rec := doAs(t, handler, http.MethodGet, "/api/kyc/photo/other", nil,
		users["alice"]); rec.Code != http.StatusBadRequest {
		t.Errorf("非法 side 应返回 400，实际 %d", rec.Code)
	}
}

// 非图片内容要被 MIME 白名单挡下，且不留残渣。
func TestVerificationRejectsNonImage(t *testing.T) {
	rt, handler, _, users := installedWithUsers(t, "alice")

	rec := doUpload(t, handler, http.MethodPost, "/api/kyc",
		map[string]string{"real_name": "张三", "id_number": validID1},
		[]uploadFile{
			{Field: "front", Name: "front.png", Data: []byte("<html><body>not an image</body></html>")},
			{Field: "back", Name: "back.png", Data: pngBytes(512)},
		}, users["alice"])
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非图片照片应返回 400，实际 %d %s", rec.Code, rec.Body.String())
	}
	if files := uploadedFiles(t, rt); len(files) != 0 {
		t.Fatalf("被拒的提交不该留下文件，实际 %v", files)
	}
	if record := mineKYC(t, handler, users["alice"]); record != nil {
		t.Fatalf("被拒的提交不该建记录，实际 %+v", record)
	}
}

// 姓名、身份证号与缺失照片都要在落盘之前拦下。
func TestVerificationValidation(t *testing.T) {
	rt, handler, _, users := installedWithUsers(t, "alice")

	cases := []struct {
		name   string
		fields map[string]string
		files  []uploadFile
	}{
		{"姓名过短", map[string]string{"real_name": "张", "id_number": validID1}, nil},
		{"姓名含数字", map[string]string{"real_name": "张三123", "id_number": validID1}, nil},
		{"号码位数不对", map[string]string{"real_name": "张三", "id_number": "1101011990030778"}, nil},
		{"校验位不对", map[string]string{"real_name": "张三", "id_number": invalidID}, nil},
		{
			"缺人像面",
			map[string]string{"real_name": "张三", "id_number": validID1},
			[]uploadFile{{Field: "back", Name: "back.png", Data: pngBytes(512)}},
		},
		{
			"缺国徽面",
			map[string]string{"real_name": "张三", "id_number": validID1},
			[]uploadFile{{Field: "front", Name: "front.png", Data: pngBytes(512)}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := tc.files
			if files == nil {
				files = []uploadFile{
					{Field: "front", Name: "front.png", Data: pngBytes(512)},
					{Field: "back", Name: "back.png", Data: pngBytes(512)},
				}
			}
			rec := doUpload(t, handler, http.MethodPost, "/api/kyc", tc.fields, files, users["alice"])
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s 应返回 400，实际 %d %s", tc.name, rec.Code, rec.Body.String())
			}
		})
	}
	if files := uploadedFiles(t, rt); len(files) != 0 {
		t.Fatalf("校验失败不该留下文件，实际 %v", files)
	}
}

// 审核中的申请不能重复提交，已通过的记录也不能再改。
func TestVerificationResubmitRules(t *testing.T) {
	_, handler, admin, users := installedWithUsers(t, "alice")
	if rec := submitKYC(t, handler, users["alice"], "张三", validID1); rec.Code != http.StatusOK {
		t.Fatalf("首次提交失败: %d %s", rec.Code, rec.Body.String())
	}

	// pending 状态下重复提交是冲突。
	if rec := submitKYC(t, handler, users["alice"], "张三", validID1); rec.Code != http.StatusConflict {
		t.Fatalf("审核中重复提交应返回 409，实际 %d %s", rec.Code, rec.Body.String())
	}

	record := mineKYC(t, handler, users["alice"])
	id := itoa(record.ID)

	// 驳回后可以改了再交。
	rec := doAs(t, handler, http.MethodPost, "/api/admin/verifications/"+id+"/reject",
		map[string]string{"reason": "照片模糊，请重拍"}, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("驳回失败: %d %s", rec.Code, rec.Body.String())
	}
	rejected := mineKYC(t, handler, users["alice"])
	if rejected.Status != model.KYCRejected {
		t.Fatalf("驳回后状态应为 rejected，实际 %s", rejected.Status)
	}
	// 驳回原因必须回给用户，否则他不知道该改什么。
	if rejected.RejectReason != "照片模糊，请重拍" {
		t.Errorf("驳回原因应回传，实际 %q", rejected.RejectReason)
	}
	if rec := submitKYC(t, handler, users["alice"], "张三", validID1); rec.Code != http.StatusOK {
		t.Fatalf("驳回后重新提交应返回 200，实际 %d %s", rec.Code, rec.Body.String())
	}

	// 通过之后就锁死了。
	current := mineKYC(t, handler, users["alice"])
	if rec := doAs(t, handler, http.MethodPost,
		"/api/admin/verifications/"+itoa(current.ID)+"/approve", nil, admin); rec.Code != http.StatusOK {
		t.Fatalf("审核通过失败: %d %s", rec.Code, rec.Body.String())
	}
	if rec := submitKYC(t, handler, users["alice"], "李四", validID2); rec.Code != http.StatusConflict {
		t.Fatalf("已通过后再提交应返回 409，实际 %d %s", rec.Code, rec.Body.String())
	}
}

// 重新提交是覆盖而非新增：一人始终只有一条记录，旧照片也不该留在盘上。
func TestVerificationResubmitReplacesRecordAndPhotos(t *testing.T) {
	rt, handler, admin, users := installedWithUsers(t, "alice")
	if rec := submitKYC(t, handler, users["alice"], "张三", validID1); rec.Code != http.StatusOK {
		t.Fatalf("首次提交失败: %d %s", rec.Code, rec.Body.String())
	}
	first := mineKYC(t, handler, users["alice"])
	firstFiles := uploadedFiles(t, rt)
	if len(firstFiles) != 2 {
		t.Fatalf("首次提交应落盘 2 张，实际 %v", firstFiles)
	}

	rec := doAs(t, handler, http.MethodPost,
		"/api/admin/verifications/"+itoa(first.ID)+"/reject",
		map[string]string{"reason": "重拍"}, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("驳回失败: %d %s", rec.Code, rec.Body.String())
	}
	if rec := submitKYC(t, handler, users["alice"], "张三", validID1); rec.Code != http.StatusOK {
		t.Fatalf("重新提交失败: %d %s", rec.Code, rec.Body.String())
	}

	second := mineKYC(t, handler, users["alice"])
	if second.ID != first.ID {
		t.Errorf("重新提交应覆盖同一条记录，ID 由 %d 变成了 %d", first.ID, second.ID)
	}
	if second.Status != model.KYCPending {
		t.Errorf("重新提交后应回到 pending，实际 %s", second.Status)
	}
	// 旧照片已无人引用，必须删掉，否则每次重交都在盘上多留一份证件照。
	after := uploadedFiles(t, rt)
	if len(after) != 2 {
		t.Fatalf("重新提交后应仍只有 2 张照片，实际 %d 张: %v", len(after), after)
	}
	for _, old := range firstFiles {
		for _, now := range after {
			if old == now {
				t.Errorf("旧照片未被清理: %s", old)
			}
		}
	}

	// 审核列表里也只该有一条。
	rec = doAs(t, handler, http.MethodGet, "/api/admin/verifications", nil, admin)
	var page struct {
		Total int64 `json:"total"`
	}
	decodeJSON(t, rec, &page)
	if page.Total != 1 {
		t.Fatalf("同一用户应只有 1 条记录，实际 %d 条", page.Total)
	}
}

// 同一号码不能被两个账号通过认证。
func TestVerificationRejectsDuplicateIDNumber(t *testing.T) {
	_, handler, admin, users := installedWithUsers(t, "alice", "bob")
	passKYC(t, handler, admin, users["alice"], "张三", validID1)

	rec := submitKYC(t, handler, users["bob"], "李四", validID1)
	if rec.Code != http.StatusConflict {
		t.Fatalf("重复号码应返回 409，实际 %d %s", rec.Code, rec.Body.String())
	}
	// 换个号码就该放行。
	if rec := submitKYC(t, handler, users["bob"], "李四", validID2); rec.Code != http.StatusOK {
		t.Fatalf("换号码后应能提交，实际 %d %s", rec.Code, rec.Body.String())
	}
}

// 被驳回的记录不该占着号码：同一号码换个账号仍可再认证。
func TestVerificationRejectedDoesNotHoldIDNumber(t *testing.T) {
	_, handler, admin, users := installedWithUsers(t, "alice", "bob")
	if rec := submitKYC(t, handler, users["alice"], "张三", validID1); rec.Code != http.StatusOK {
		t.Fatalf("alice 提交失败: %d %s", rec.Code, rec.Body.String())
	}
	record := mineKYC(t, handler, users["alice"])
	rec := doAs(t, handler, http.MethodPost,
		"/api/admin/verifications/"+itoa(record.ID)+"/reject",
		map[string]string{"reason": "信息不符"}, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("驳回失败: %d %s", rec.Code, rec.Body.String())
	}

	if rec := submitKYC(t, handler, users["bob"], "李四", validID1); rec.Code != http.StatusOK {
		t.Fatalf("号码被驳回记录占用了: %d %s", rec.Code, rec.Body.String())
	}
}

// 越权访问他人的证件照与记录一律 404。
func TestVerificationCrossUserAccessReturns404(t *testing.T) {
	_, handler, _, users := installedWithUsers(t, "alice", "bob")
	if rec := submitKYC(t, handler, users["alice"], "张三", validID1); rec.Code != http.StatusOK {
		t.Fatalf("alice 提交失败: %d %s", rec.Code, rec.Body.String())
	}

	// bob 没提交过，取照片只能是 404，不该拿到 alice 的。
	rec := doAs(t, handler, http.MethodGet, "/api/kyc/photo/front", nil, users["bob"])
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未提交者取照片应返回 404，实际 %d %s", rec.Code, rec.Body.String())
	}
	if record := mineKYC(t, handler, users["bob"]); record != nil {
		t.Fatalf("bob 不该看到任何记录，实际 %+v", record)
	}

	// 管理端接口对普通用户一律 403。
	aliceRecord := mineKYC(t, handler, users["alice"])
	id := itoa(aliceRecord.ID)
	for _, path := range []string{
		"/api/admin/verifications",
		"/api/admin/verifications/" + id,
		"/api/admin/verifications/" + id + "/photo/front",
	} {
		if rec := doAs(t, handler, http.MethodGet, path, nil,
			users["bob"]); rec.Code != http.StatusForbidden {
			t.Errorf("普通用户访问 %s 应返回 403，实际 %d", path, rec.Code)
		}
	}
}

// 审核列表打码、详情给完整号码 —— 管理员要拿它与照片比对。
func TestAdminVerificationDetailShowsFullIDNumber(t *testing.T) {
	_, handler, admin, users := installedWithUsers(t, "alice")
	if rec := submitKYC(t, handler, users["alice"], "张三", validID1); rec.Code != http.StatusOK {
		t.Fatalf("提交失败: %d %s", rec.Code, rec.Body.String())
	}

	rec := doAs(t, handler, http.MethodGet, "/api/admin/verifications", nil, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("审核列表失败: %d %s", rec.Code, rec.Body.String())
	}
	var page struct {
		Items []kycRecord `json:"items"`
		Total int64       `json:"total"`
	}
	decodeJSON(t, rec, &page)
	if page.Total != 1 {
		t.Fatalf("应有 1 条记录，实际 %d", page.Total)
	}
	// 列表是概览，完整号码只在详情里出现。
	if page.Items[0].IDNumber != model.MaskIDNumber(validID1) {
		t.Errorf("审核列表号码应打码，实际 %s", page.Items[0].IDNumber)
	}
	if page.Items[0].Username != "alice" {
		t.Errorf("审核列表应带提交人，实际 %q", page.Items[0].Username)
	}

	rec = doAs(t, handler, http.MethodGet,
		"/api/admin/verifications/"+itoa(page.Items[0].ID), nil, admin)
	var detail kycRecord
	decodeJSON(t, rec, &detail)
	if detail.IDNumber != validID1 {
		t.Errorf("审核详情应给出完整号码，实际 %s", detail.IDNumber)
	}

	// 管理员能看照片。
	for _, side := range []string{service.SideFront, service.SideBack} {
		rec := doAs(t, handler, http.MethodGet,
			"/api/admin/verifications/"+itoa(detail.ID)+"/photo/"+side, nil, admin)
		if rec.Code != http.StatusOK {
			t.Fatalf("管理员取 %s 照片应返回 200，实际 %d", side, rec.Code)
		}
	}

	// 按状态过滤可用，非法状态被拒。
	rec = doAs(t, handler, http.MethodGet, "/api/admin/verifications?status=approved", nil, admin)
	decodeJSON(t, rec, &page)
	if page.Total != 0 {
		t.Errorf("尚无通过记录，approved 过滤应为空，实际 %d", page.Total)
	}
	if rec := doAs(t, handler, http.MethodGet, "/api/admin/verifications?status=nope", nil,
		admin); rec.Code != http.StatusBadRequest {
		t.Errorf("非法状态过滤应返回 400，实际 %d", rec.Code)
	}
}

// 驳回必须填原因；已审核过的记录不能再审一次。
func TestAdminReviewRules(t *testing.T) {
	_, handler, admin, users := installedWithUsers(t, "alice")
	if rec := submitKYC(t, handler, users["alice"], "张三", validID1); rec.Code != http.StatusOK {
		t.Fatalf("提交失败: %d %s", rec.Code, rec.Body.String())
	}
	id := itoa(mineKYC(t, handler, users["alice"]).ID)

	if rec := doAs(t, handler, http.MethodPost, "/api/admin/verifications/"+id+"/reject",
		map[string]string{"reason": "   "}, admin); rec.Code != http.StatusBadRequest {
		t.Fatalf("空原因驳回应返回 400，实际 %d", rec.Code)
	}
	if rec := doAs(t, handler, http.MethodPost, "/api/admin/verifications/"+id+"/approve", nil,
		admin); rec.Code != http.StatusOK {
		t.Fatalf("通过应返回 200，实际 %d", rec.Code)
	}
	if rec := doAs(t, handler, http.MethodPost, "/api/admin/verifications/"+id+"/approve", nil,
		admin); rec.Code != http.StatusConflict {
		t.Fatalf("重复审核应返回 409，实际 %d", rec.Code)
	}
	// 不存在的记录是 404。
	if rec := doAs(t, handler, http.MethodPost, "/api/admin/verifications/9999/approve", nil,
		admin); rec.Code != http.StatusNotFound {
		t.Fatalf("审核不存在的记录应返回 404，实际 %d", rec.Code)
	}
}

// 未安装实名认证插件时，第三方认证接口返回 503 而不是报内部错误。
func TestExternalVerificationWithoutPlugin(t *testing.T) {
	_, handler, _, users := installedWithUsers(t, "alice")

	rec := doAs(t, handler, http.MethodPost, "/api/kyc/external",
		map[string]string{"real_name": "张三", "id_number": validID1}, users["alice"])
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("无插件发起第三方实名称应返回 503，实际 %d %s", rec.Code, rec.Body.String())
	}

	rec = doAs(t, handler, http.MethodGet, "/api/kyc/external", nil, users["alice"])
	if rec.Code != http.StatusNotFound {
		t.Fatalf("无记录查询第三方实名称应返回 404，实际 %d %s", rec.Code, rec.Body.String())
	}
}

// 第三方认证接口同样要求登录。
func TestExternalVerificationRequiresAuth(t *testing.T) {
	_, handler, _, _ := installedWithUsers(t, "alice")

	if rec := doAs(t, handler, http.MethodPost, "/api/kyc/external",
		map[string]string{"real_name": "张三", "id_number": validID1}, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("未登录发起第三方实名称应返回 401，实际 %d", rec.Code)
	}
	if rec := doAs(t, handler, http.MethodGet, "/api/kyc/external", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("未登录查询第三方实名称应返回 401，实际 %d", rec.Code)
	}
}
