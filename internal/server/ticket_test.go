package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SakuraOpenSource/levis/internal/model"
	"github.com/SakuraOpenSource/levis/internal/service"
)

// ticketDetail 是工单详情响应中测试关心的部分。
type ticketDetail struct {
	ID       uint   `json:"id"`
	TicketNo string `json:"ticket_no"`
	Subject  string `json:"subject"`
	Status   string `json:"status"`
	Replies  []struct {
		ID          uint   `json:"id"`
		IsStaff     bool   `json:"is_staff"`
		AuthorName  string `json:"author_name"`
		Body        string `json:"body"`
		Attachments []struct {
			ID        uint   `json:"id"`
			FileName  string `json:"file_name"`
			MimeType  string `json:"mime_type"`
			SizeBytes int64  `json:"size_bytes"`
		} `json:"attachments"`
	} `json:"replies"`
}

// createTicket 建一张带 files 附件的工单，返回详情。
func createTicket(
	t *testing.T, handler http.Handler, cookies []*http.Cookie, subject string, files []uploadFile,
) ticketDetail {
	t.Helper()
	rec := doUpload(t, handler, http.MethodPost, "/api/tickets",
		map[string]string{"subject": subject, "body": "第一条内容"}, files, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("建单失败: %d %s", rec.Code, rec.Body.String())
	}
	var ticket ticketDetail
	decodeJSON(t, rec, &ticket)
	return ticket
}

// fetchTicket 读工单详情，附带断言状态码。
func fetchTicket(
	t *testing.T, handler http.Handler, path string, cookies []*http.Cookie,
) ticketDetail {
	t.Helper()
	rec := doAs(t, handler, http.MethodGet, path, nil, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("读取 %s 失败: %d %s", path, rec.Code, rec.Body.String())
	}
	var ticket ticketDetail
	decodeJSON(t, rec, &ticket)
	return ticket
}

// 建单要一次写出工单与首条回复，附件元数据一并落好。
func TestCreateTicketWithAttachment(t *testing.T) {
	rt, handler, _, users := installedWithUsers(t, "alice")
	ticket := createTicket(t, handler, users["alice"], "机器起不来", []uploadFile{
		{Field: "files", Name: "日志.png", Data: pngBytes(2048)},
	})

	if ticket.Status != model.TicketOpen {
		t.Errorf("新建工单状态应为 open，实际 %s", ticket.Status)
	}
	if !strings.HasPrefix(ticket.TicketNo, "TKT") {
		t.Errorf("工单号应以 TKT 开头，实际 %s", ticket.TicketNo)
	}
	if len(ticket.Replies) != 1 {
		t.Fatalf("建单应带 1 条首帖，实际 %d 条", len(ticket.Replies))
	}
	first := ticket.Replies[0]
	if first.IsStaff {
		t.Error("用户的首帖不该标记为客服")
	}
	if first.AuthorName != "alice" {
		t.Errorf("作者名应快照为 alice，实际 %s", first.AuthorName)
	}
	if len(first.Attachments) != 1 {
		t.Fatalf("应有 1 个附件，实际 %d 个", len(first.Attachments))
	}
	attachment := first.Attachments[0]
	if attachment.FileName != "日志.png" {
		t.Errorf("原始文件名应保留，实际 %s", attachment.FileName)
	}
	// MIME 取自内容嗅探，不是客户端声明的 application/octet-stream。
	if attachment.MimeType != "image/png" {
		t.Errorf("MIME 应由内容嗅探得出，实际 %s", attachment.MimeType)
	}
	if attachment.SizeBytes != 2048 {
		t.Errorf("附件大小应为 2048，实际 %d", attachment.SizeBytes)
	}

	if files := uploadedFiles(t, rt); len(files) != 1 {
		t.Fatalf("uploads 下应有 1 个文件，实际 %d 个: %v", len(files), files)
	}
	// 落盘名必须是服务端生成的随机串，原始文件名不能参与路径拼接。
	for _, path := range uploadedFiles(t, rt) {
		if strings.Contains(path, "日志") {
			t.Errorf("落盘路径不该包含原始文件名: %s", path)
		}
	}
}

// 附件下载一律按 attachment 处理：内联展示等于让上传内容在本站域下执行。
func TestTicketAttachmentServedAsDownload(t *testing.T) {
	_, handler, _, users := installedWithUsers(t, "alice")
	ticket := createTicket(t, handler, users["alice"], "附件测试", []uploadFile{
		{Field: "files", Name: "payload.html", Data: []byte("<script>alert(1)</script>")},
	})
	attachmentID := ticket.Replies[0].Attachments[0].ID

	path := "/api/tickets/" + itoa(ticket.ID) + "/attachments/" + itoa(attachmentID)
	rec := doAs(t, handler, http.MethodGet, path, nil, users["alice"])
	if rec.Code != http.StatusOK {
		t.Fatalf("下载附件应返回 200，实际 %d %s", rec.Code, rec.Body.String())
	}
	disposition := rec.Header().Get("Content-Disposition")
	if !strings.HasPrefix(disposition, "attachment") {
		t.Errorf("Content-Disposition 应为 attachment，实际 %q", disposition)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("应带 nosniff，实际 %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("应带 no-store，实际 %q", got)
	}
	if rec.Body.String() != "<script>alert(1)</script>" {
		t.Errorf("附件内容不符: %q", rec.Body.String())
	}
}

// 越权访问一律 404 而非 403：403 等于确认「这个 ID 存在，只是不属于你」。
func TestTicketCrossUserAccessReturns404(t *testing.T) {
	_, handler, _, users := installedWithUsers(t, "alice", "bob")
	ticket := createTicket(t, handler, users["alice"], "alice 的工单", []uploadFile{
		{Field: "files", Name: "a.png", Data: pngBytes(512)},
	})
	attachmentID := ticket.Replies[0].Attachments[0].ID
	id := itoa(ticket.ID)

	// send 按路由形态选 JSON 还是 multipart —— 回复接口收的是表单。
	cases := []struct {
		name string
		send func() *httptest.ResponseRecorder
	}{
		{"读详情", func() *httptest.ResponseRecorder {
			return doAs(t, handler, http.MethodGet, "/api/tickets/"+id, nil, users["bob"])
		}},
		{"下载附件", func() *httptest.ResponseRecorder {
			return doAs(t, handler, http.MethodGet,
				"/api/tickets/"+id+"/attachments/"+itoa(attachmentID), nil, users["bob"])
		}},
		{"回复", func() *httptest.ResponseRecorder {
			return doUpload(t, handler, http.MethodPost, "/api/tickets/"+id+"/replies",
				map[string]string{"body": "我来插一句"}, nil, users["bob"])
		}},
		{"关闭", func() *httptest.ResponseRecorder {
			return doAs(t, handler, http.MethodPost, "/api/tickets/"+id+"/close", nil, users["bob"])
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := tc.send()
			if rec.Code != http.StatusNotFound {
				t.Fatalf("越权 %s 应返回 404，实际 %d %s", tc.name, rec.Code, rec.Body.String())
			}
		})
	}

	// bob 的列表里也不该出现 alice 的工单。
	rec := doAs(t, handler, http.MethodGet, "/api/tickets", nil, users["bob"])
	var page struct {
		Items []ticketDetail `json:"items"`
		Total int64          `json:"total"`
	}
	decodeJSON(t, rec, &page)
	if page.Total != 0 || len(page.Items) != 0 {
		t.Fatalf("bob 的工单列表应为空，实际 %+v", page)
	}
}

// 换个 ticket_id 也不能拿到别人的附件：附件 ID 全局唯一，鉴权必须绑定工单。
func TestTicketAttachmentIDCannotBeSwapped(t *testing.T) {
	_, handler, _, users := installedWithUsers(t, "alice", "bob")
	alice := createTicket(t, handler, users["alice"], "alice 的工单", []uploadFile{
		{Field: "files", Name: "secret.png", Data: pngBytes(600)},
	})
	bob := createTicket(t, handler, users["bob"], "bob 的工单", nil)
	aliceAttachment := alice.Replies[0].Attachments[0].ID

	// bob 用自己的工单 ID 配 alice 的附件 ID。
	path := "/api/tickets/" + itoa(bob.ID) + "/attachments/" + itoa(aliceAttachment)
	rec := doAs(t, handler, http.MethodGet, path, nil, users["bob"])
	if rec.Code != http.StatusNotFound {
		t.Fatalf("跨工单取附件应返回 404，实际 %d %s", rec.Code, rec.Body.String())
	}
}

// 超过 20 MiB 的附件必须被拒，且不能在 uploads 下留下任何残渣。
func TestTicketAttachmentTooLargeRejected(t *testing.T) {
	rt, handler, _, users := installedWithUsers(t, "alice")

	oversized := service.MaxAttachmentBytes + 1024
	rec := doUpload(t, handler, http.MethodPost, "/api/tickets",
		map[string]string{"subject": "大附件", "body": "内容"},
		[]uploadFile{{Field: "files", Name: "big.bin", Data: make([]byte, oversized)}},
		users["alice"])
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("超限附件应返回 400，实际 %d %s", rec.Code, rec.Body.String())
	}
	if files := uploadedFiles(t, rt); len(files) != 0 {
		t.Fatalf("被拒的上传不该落盘，实际留下 %d 个文件: %v", len(files), files)
	}
	// 工单本身也不该留下半张。
	rec = doAs(t, handler, http.MethodGet, "/api/tickets", nil, users["alice"])
	var page struct {
		Total int64 `json:"total"`
	}
	decodeJSON(t, rec, &page)
	if page.Total != 0 {
		t.Fatalf("上传失败却建了工单，共 %d 张", page.Total)
	}
}

// 状态流转：用户建单 open → 客服回复 answered → 用户回复 open。
func TestTicketStatusFlow(t *testing.T) {
	_, handler, admin, users := installedWithUsers(t, "alice")
	ticket := createTicket(t, handler, users["alice"], "状态流转", nil)
	id := itoa(ticket.ID)

	rec := doUpload(t, handler, http.MethodPost, "/api/admin/tickets/"+id+"/replies",
		map[string]string{"body": "已收到，正在处理"}, nil, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("客服回复失败: %d %s", rec.Code, rec.Body.String())
	}
	if got := fetchTicket(t, handler, "/api/tickets/"+id, users["alice"]).Status; got != model.TicketAnswered {
		t.Fatalf("客服回复后状态应为 answered，实际 %s", got)
	}

	rec = doUpload(t, handler, http.MethodPost, "/api/tickets/"+id+"/replies",
		map[string]string{"body": "还是不行"}, nil, users["alice"])
	if rec.Code != http.StatusOK {
		t.Fatalf("用户回复失败: %d %s", rec.Code, rec.Body.String())
	}
	after := fetchTicket(t, handler, "/api/tickets/"+id, users["alice"])
	if after.Status != model.TicketOpen {
		t.Fatalf("用户回复后状态应回到 open，实际 %s", after.Status)
	}
	if len(after.Replies) != 3 {
		t.Fatalf("应有 3 条回复，实际 %d 条", len(after.Replies))
	}
	// 客服身份要如实标记，前端靠它区分左右气泡。
	if !after.Replies[1].IsStaff {
		t.Error("第二条应标记为客服回复")
	}
	if after.Replies[2].IsStaff {
		t.Error("第三条是用户回复，不该标记为客服")
	}
}

// 关闭后拒绝回复，管理员重开后可再回复。
func TestTicketClosedRejectsReplyUntilReopened(t *testing.T) {
	_, handler, admin, users := installedWithUsers(t, "alice")
	ticket := createTicket(t, handler, users["alice"], "关了再开", nil)
	id := itoa(ticket.ID)

	if rec := doAs(t, handler, http.MethodPost, "/api/tickets/"+id+"/close", nil,
		users["alice"]); rec.Code != http.StatusNoContent {
		t.Fatalf("关闭工单应返回 204，实际 %d %s", rec.Code, rec.Body.String())
	}
	if got := fetchTicket(t, handler, "/api/tickets/"+id, users["alice"]).Status; got != model.TicketClosed {
		t.Fatalf("关闭后状态应为 closed，实际 %s", got)
	}

	// 关闭状态下双方都不能回复。
	rec := doUpload(t, handler, http.MethodPost, "/api/tickets/"+id+"/replies",
		map[string]string{"body": "再补一句"}, nil, users["alice"])
	if rec.Code != http.StatusConflict {
		t.Fatalf("已关闭工单用户回复应返回 409，实际 %d %s", rec.Code, rec.Body.String())
	}
	rec = doUpload(t, handler, http.MethodPost, "/api/admin/tickets/"+id+"/replies",
		map[string]string{"body": "客服补一句"}, nil, admin)
	if rec.Code != http.StatusConflict {
		t.Fatalf("已关闭工单客服回复应返回 409，实际 %d %s", rec.Code, rec.Body.String())
	}
	// 重复关闭是冲突而不是静默成功。
	if rec := doAs(t, handler, http.MethodPost, "/api/tickets/"+id+"/close", nil,
		users["alice"]); rec.Code != http.StatusConflict {
		t.Fatalf("重复关闭应返回 409，实际 %d", rec.Code)
	}

	if rec := doAs(t, handler, http.MethodPost, "/api/admin/tickets/"+id+"/reopen", nil,
		admin); rec.Code != http.StatusNoContent {
		t.Fatalf("重开工单应返回 204，实际 %d %s", rec.Code, rec.Body.String())
	}
	rec = doUpload(t, handler, http.MethodPost, "/api/tickets/"+id+"/replies",
		map[string]string{"body": "重开后再说"}, nil, users["alice"])
	if rec.Code != http.StatusOK {
		t.Fatalf("重开后回复应返回 200，实际 %d %s", rec.Code, rec.Body.String())
	}
	// 未关闭的工单没什么可重开的。
	if rec := doAs(t, handler, http.MethodPost, "/api/admin/tickets/"+id+"/reopen", nil,
		admin); rec.Code != http.StatusConflict {
		t.Fatalf("重开未关闭的工单应返回 409，实际 %d", rec.Code)
	}
}

// 管理端能看到全部工单并按状态过滤；普通用户碰不到这些接口。
func TestAdminTicketListing(t *testing.T) {
	_, handler, admin, users := installedWithUsers(t, "alice", "bob")
	aliceTicket := createTicket(t, handler, users["alice"], "alice 的问题", nil)
	createTicket(t, handler, users["bob"], "bob 的问题", nil)

	rec := doAs(t, handler, http.MethodGet, "/api/admin/tickets", nil, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("管理端列表失败: %d %s", rec.Code, rec.Body.String())
	}
	var page struct {
		Items []struct {
			ID       uint   `json:"id"`
			Status   string `json:"status"`
			Username string `json:"username"`
		} `json:"items"`
		Total int64 `json:"total"`
	}
	decodeJSON(t, rec, &page)
	if page.Total != 2 {
		t.Fatalf("管理端应看到 2 张工单，实际 %d", page.Total)
	}
	// 列表要带上提交人，否则管理员得逐条点开才知道是谁。
	for _, item := range page.Items {
		if item.Username == "" {
			t.Errorf("工单 %d 缺少提交人用户名", item.ID)
		}
	}

	// 把 alice 那张变成 answered，再按状态过滤。
	rec = doUpload(t, handler, http.MethodPost,
		"/api/admin/tickets/"+itoa(aliceTicket.ID)+"/replies",
		map[string]string{"body": "回一句"}, nil, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("客服回复失败: %d %s", rec.Code, rec.Body.String())
	}
	rec = doAs(t, handler, http.MethodGet, "/api/admin/tickets?status=answered", nil, admin)
	decodeJSON(t, rec, &page)
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].ID != aliceTicket.ID {
		t.Fatalf("按 answered 过滤应只剩 alice 那张，实际 %+v", page)
	}

	// 非法状态值要被拒，不能当成「不过滤」静默处理。
	if rec := doAs(t, handler, http.MethodGet, "/api/admin/tickets?status=nope", nil,
		admin); rec.Code != http.StatusBadRequest {
		t.Errorf("非法状态过滤应返回 400，实际 %d", rec.Code)
	}

	// 普通用户访问管理端接口一律 403。
	for _, path := range []string{
		"/api/admin/tickets",
		"/api/admin/tickets/" + itoa(aliceTicket.ID),
	} {
		if rec := doAs(t, handler, http.MethodGet, path, nil,
			users["bob"]); rec.Code != http.StatusForbidden {
			t.Errorf("普通用户访问 %s 应返回 403，实际 %d", path, rec.Code)
		}
	}
}

// 管理员可以读任意工单的详情与附件 —— 不然没法处理问题。
func TestAdminReadsAnyTicket(t *testing.T) {
	_, handler, admin, users := installedWithUsers(t, "alice")
	ticket := createTicket(t, handler, users["alice"], "带附件", []uploadFile{
		{Field: "files", Name: "screenshot.png", Data: pngBytes(700)},
	})
	id := itoa(ticket.ID)

	detail := fetchTicket(t, handler, "/api/admin/tickets/"+id, admin)
	if detail.Subject != "带附件" || len(detail.Replies) != 1 {
		t.Fatalf("管理端详情不完整: %+v", detail)
	}
	attachmentID := itoa(detail.Replies[0].Attachments[0].ID)
	rec := doAs(t, handler, http.MethodGet,
		"/api/admin/tickets/"+id+"/attachments/"+attachmentID, nil, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("管理端下载附件应返回 200，实际 %d %s", rec.Code, rec.Body.String())
	}
	if !strings.HasPrefix(rec.Header().Get("Content-Disposition"), "attachment") {
		t.Error("管理端下载同样必须是 attachment")
	}
}

// 空主题、空正文、超量附件都要在落盘之前拦下。
func TestTicketValidation(t *testing.T) {
	rt, handler, _, users := installedWithUsers(t, "alice")

	cases := []struct {
		name   string
		fields map[string]string
		files  []uploadFile
	}{
		{"主题为空", map[string]string{"subject": "  ", "body": "内容"}, nil},
		{"正文为空", map[string]string{"subject": "标题", "body": "   "}, nil},
		{
			"主题超长",
			map[string]string{"subject": strings.Repeat("题", service.MaxTicketSubjectLen+1), "body": "内容"},
			nil,
		},
		{
			"附件超量",
			map[string]string{"subject": "标题", "body": "内容"},
			func() []uploadFile {
				files := make([]uploadFile, service.MaxAttachments+1)
				for i := range files {
					files[i] = uploadFile{Field: "files", Name: "f.png", Data: pngBytes(64)}
				}
				return files
			}(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doUpload(t, handler, http.MethodPost, "/api/tickets", tc.fields, tc.files, users["alice"])
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s 应返回 400，实际 %d %s", tc.name, rec.Code, rec.Body.String())
			}
		})
	}
	if files := uploadedFiles(t, rt); len(files) != 0 {
		t.Fatalf("校验失败的请求不该落盘，实际留下 %v", files)
	}
}

// 未登录访问工单接口应是 401，不能靠 404 蒙混过去。
func TestTicketRequiresAuth(t *testing.T) {
	_, handler := installedServer(t)
	for _, path := range []string{"/api/tickets", "/api/tickets/1"} {
		if rec := do(t, handler, http.MethodGet, path, nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("未登录访问 %s 应返回 401，实际 %d", path, rec.Code)
		}
	}
}
