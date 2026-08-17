package server

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/SakuraOpenSource/levis/internal/auth"
	"github.com/SakuraOpenSource/levis/internal/model"
	"github.com/SakuraOpenSource/levis/internal/runtime"
)

// 本文件放本轮新增接口（工单、实名、API Key、开放接口）共用的测试助手。
// 请求层的基础助手（do / doAs / installedServer / loginAs / decodeJSON）在
// captcha_test.go 里，这里只补 multipart 与几个装数据的快捷方式。

// 几个通过校验位验算的身份证号，供实名认证相关测试使用。
const (
	validID1 = "110101199003077838"
	validID2 = "11010119900307002X"
	validID3 = "440524188001010014"
	// invalidID 前 17 位合法但校验位不对，用于验证格式校验确实在跑。
	invalidID = "310101199001011230"
)

// uploadFile 是 multipart 表单里的一个文件字段。
type uploadFile struct {
	Field string
	Name  string
	Data  []byte
}

// doUpload 发一个 multipart 请求，语义与 doAs 相同（自动补齐 CSRF 双提交）。
func doUpload(
	t *testing.T, handler http.Handler, method, path string,
	fields map[string]string, files []uploadFile, cookies []*http.Cookie,
) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for name, value := range fields {
		if err := writer.WriteField(name, value); err != nil {
			t.Fatalf("写入表单字段 %s 失败: %v", name, err)
		}
	}
	for _, file := range files {
		part, err := writer.CreateFormFile(file.Field, file.Name)
		if err != nil {
			t.Fatalf("创建表单文件 %s 失败: %v", file.Name, err)
		}
		if _, err := part.Write(file.Data); err != nil {
			t.Fatalf("写入表单文件 %s 失败: %v", file.Name, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("关闭 multipart writer 失败: %v", err)
	}

	req := httptest.NewRequest(method, path, bytes.NewReader(body.Bytes()))
	req.Header.Set("Content-Type", writer.FormDataContentType())
	var csrf string
	for _, cookie := range cookies {
		req.AddCookie(cookie)
		if cookie.Name == auth.CookieCSRF {
			csrf = cookie.Value
		}
	}
	if csrf == "" {
		csrf = "test-csrf-token"
		req.AddCookie(&http.Cookie{Name: auth.CookieCSRF, Value: csrf})
	}
	req.Header.Set(auth.HeaderCSRF, csrf)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// doKey 用 API Key 发请求，刻意不带任何 cookie 也不带 CSRF 令牌 ——
// 开放接口既不该认 cookie，也不该要求双提交令牌。
func doKey(
	t *testing.T, handler http.Handler, method, path string, body any, secret string,
) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("序列化请求体失败: %v", err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// pngMagic 是 PNG 的文件头，服务端靠嗅探它来判定 MIME。
var pngMagic = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}

// pngBytes 造一个 size 字节、能被嗅探为 image/png 的假图片。
func pngBytes(size int) []byte {
	if size < len(pngMagic) {
		size = len(pngMagic)
	}
	out := make([]byte, size)
	copy(out, pngMagic)
	for i := len(pngMagic); i < size; i++ {
		out[i] = byte(i % 251)
	}
	return out
}

// installedWithUsers 起一个已安装的服务并建好 names 里的普通用户。
//
// 注册验证码默认开启，测试里没法认图，所以先关掉再建号。
func installedWithUsers(
	t *testing.T, names ...string,
) (*runtime.Runtime, http.Handler, []*http.Cookie, map[string][]*http.Cookie) {
	t.Helper()
	rt, handler := installedServer(t)
	admin := loginAs(t, handler, "admin", "password123")

	rec := doAs(t, handler, http.MethodPut, "/api/admin/settings/captcha",
		captchaConfig{Charset: "digit", Length: 6}, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("关闭注册验证码失败: %d %s", rec.Code, rec.Body.String())
	}

	users := make(map[string][]*http.Cookie, len(names))
	for _, name := range names {
		rec := do(t, handler, http.MethodPost, "/api/auth/register", map[string]string{
			"username": name,
			"email":    name + "@example.com",
			"password": "password123",
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("注册 %s 失败: %d %s", name, rec.Code, rec.Body.String())
		}
		users[name] = loginAs(t, handler, name, "password123")
	}
	return rt, handler, admin, users
}

// uploadedFiles 列出 uploads 目录下的全部文件。
//
// 「被拒的上传不该落盘」这类断言靠它来查：只看接口状态码不够，垃圾文件是
// 悄悄堆积的。
func uploadedFiles(t *testing.T, rt *runtime.Runtime) []string {
	t.Helper()
	root := filepath.Join(rt.DataDir(), "uploads")
	var out []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			// 目录还没建起来等同于「没有文件」。
			return nil
		}
		if !entry.IsDir() {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历上传目录失败: %v", err)
	}
	return out
}

// grantBalance 用假充值给用户加余额。
func grantBalance(t *testing.T, handler http.Handler, cookies []*http.Cookie, cents int64) {
	t.Helper()
	rec := doAs(t, handler, http.MethodPost, "/api/wallet/recharge",
		map[string]int64{"amount_cents": cents}, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("充值失败: %d %s", rec.Code, rec.Body.String())
	}
}

// seedProductVia 通过管理接口建一个上架的月付商品，返回商品 ID。
func seedProductVia(
	t *testing.T, handler http.Handler, admin []*http.Cookie, name string, priceCents int64,
) uint {
	t.Helper()
	rec := doAs(t, handler, http.MethodPost, "/api/admin/categories", map[string]any{
		"name": name + "-分组",
		"slug": "cat-" + name,
	}, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("创建分组失败: %d %s", rec.Code, rec.Body.String())
	}
	var category struct {
		ID uint `json:"id"`
	}
	decodeJSON(t, rec, &category)

	rec = doAs(t, handler, http.MethodPost, "/api/admin/products", map[string]any{
		"category_id":   category.ID,
		"name":          name,
		"price_cents":   priceCents,
		"billing_cycle": model.CycleMonthly,
		"stock":         -1,
		"status":        model.ProductActive,
	}, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("创建商品失败: %d %s", rec.Code, rec.Body.String())
	}
	var product struct {
		ID uint `json:"id"`
	}
	decodeJSON(t, rec, &product)
	return product.ID
}

// submitKYC 提交一份带两张假证件照的实名申请，返回记录。
func submitKYC(
	t *testing.T, handler http.Handler, cookies []*http.Cookie, realName, idNumber string,
) *httptest.ResponseRecorder {
	t.Helper()
	return doUpload(t, handler, http.MethodPost, "/api/kyc",
		map[string]string{"real_name": realName, "id_number": idNumber},
		[]uploadFile{
			{Field: "front", Name: "front.png", Data: pngBytes(1024)},
			{Field: "back", Name: "back.png", Data: pngBytes(2048)},
		}, cookies)
}

// passKYC 提交实名申请并由管理员通过，返回记录 ID。
func passKYC(
	t *testing.T, handler http.Handler, admin, cookies []*http.Cookie, realName, idNumber string,
) uint {
	t.Helper()
	rec := submitKYC(t, handler, cookies, realName, idNumber)
	if rec.Code != http.StatusOK {
		t.Fatalf("提交实名失败: %d %s", rec.Code, rec.Body.String())
	}
	var record struct {
		ID uint `json:"id"`
	}
	decodeJSON(t, rec, &record)

	rec = doAs(t, handler, http.MethodPost,
		"/api/admin/verifications/"+itoa(record.ID)+"/approve", nil, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("审核通过失败: %d %s", rec.Code, rec.Body.String())
	}
	return record.ID
}

// createKey 创建一把 API Key 并返回明文。调用方须先让用户通过实名认证。
func createKey(
	t *testing.T, handler http.Handler, cookies []*http.Cookie, name string, scopes []string,
) string {
	t.Helper()
	rec := doAs(t, handler, http.MethodPost, "/api/api-keys", map[string]any{
		"name":   name,
		"scopes": scopes,
	}, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("创建 API Key 失败: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Secret string `json:"secret"`
	}
	decodeJSON(t, rec, &created)
	if created.Secret == "" {
		t.Fatal("创建响应里没有明文 Key")
	}
	return created.Secret
}

// itoa 把 ID 拼进 URL。
func itoa(id uint) string {
	return strconv.FormatUint(uint64(id), 10)
}
