package handler

import (
	"encoding/json"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/SakuraOpenSource/levis/internal/httpx"
	"github.com/SakuraOpenSource/levis/internal/model"
	"github.com/SakuraOpenSource/levis/internal/plugin"
	"github.com/SakuraOpenSource/levis/internal/service"
	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
)

// AdminStats 返回管理后台概览数据。
func (h *Handler) AdminStats(c *gin.Context) {
	stats, err := h.admin().Stats()
	respond(c, stats, err)
}

// ---------- 用户管理 ----------

// AdminUsers 分页返回用户列表，支持 keyword 模糊搜索。
func (h *Handler) AdminUsers(c *gin.Context) {
	page, pageSize, offset := Pagination(c)
	items, total, err := h.admin().Users(c.Query("keyword"), offset, pageSize)
	if err != nil {
		respond(c, nil, err)
		return
	}
	OK(c, Page{Items: items, Total: total, Page: page, PageSize: pageSize})
}

// AdminCreateUser 创建用户。
func (h *Handler) AdminCreateUser(c *gin.Context) {
	var req service.CreateUserRequest
	if !bindJSON(c, &req) {
		return
	}
	user, err := h.admin().CreateUser(req)
	respond(c, user, err)
}

// AdminUpdateUser 更新用户资料、角色、状态或余额。
func (h *Handler) AdminUpdateUser(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	var req service.UpdateUserRequest
	if !bindJSON(c, &req) {
		return
	}
	user, err := h.admin().UpdateUser(httpx.CurrentUserID(c), id, req)
	respond(c, user, err)
}

// AdminDeleteUser 删除用户。
func (h *Handler) AdminDeleteUser(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	if err := h.admin().DeleteUser(httpx.CurrentUserID(c), id); err != nil {
		respond(c, nil, err)
		return
	}
	noContent(c)
}

// ---------- 分组管理 ----------

// AdminCategories 返回全部分组（平铺）。
func (h *Handler) AdminCategories(c *gin.Context) {
	items, err := h.admin().Categories()
	respond(c, gin.H{"items": items}, err)
}

// AdminCreateCategory 创建分组。
func (h *Handler) AdminCreateCategory(c *gin.Context) {
	var req service.CategoryInput
	if !bindJSON(c, &req) {
		return
	}
	item, err := h.admin().CreateCategory(req)
	respond(c, item, err)
}

// AdminUpdateCategory 更新分组。
func (h *Handler) AdminUpdateCategory(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	var req service.CategoryInput
	if !bindJSON(c, &req) {
		return
	}
	item, err := h.admin().UpdateCategory(id, req)
	respond(c, item, err)
}

// AdminDeleteCategory 删除分组。
func (h *Handler) AdminDeleteCategory(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	if err := h.admin().DeleteCategory(id); err != nil {
		respond(c, nil, err)
		return
	}
	noContent(c)
}

// ---------- 商品管理 ----------

// AdminProducts 分页返回商品（含隐藏商品）。
func (h *Handler) AdminProducts(c *gin.Context) {
	page, pageSize, offset := Pagination(c)
	categoryID, _ := strconv.ParseUint(c.Query("category_id"), 10, 64)
	items, total, err := h.admin().Products(uint(categoryID), offset, pageSize)
	if err != nil {
		respond(c, nil, err)
		return
	}
	OK(c, Page{Items: items, Total: total, Page: page, PageSize: pageSize})
}

// AdminCreateProduct 创建商品。
func (h *Handler) AdminCreateProduct(c *gin.Context) {
	var req service.ProductInput
	if !bindJSON(c, &req) {
		return
	}
	item, err := h.admin().CreateProduct(req)
	respond(c, item, err)
}

// AdminUpdateProduct 更新商品。
func (h *Handler) AdminUpdateProduct(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	var req service.ProductInput
	if !bindJSON(c, &req) {
		return
	}
	item, err := h.admin().UpdateProduct(id, req)
	respond(c, item, err)
}

// AdminDeleteProduct 删除商品。
func (h *Handler) AdminDeleteProduct(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	if err := h.admin().DeleteProduct(id); err != nil {
		respond(c, nil, err)
		return
	}
	noContent(c)
}

// AdminProvisionPlugins 返回可用的上游产品对接插件列表。
func (h *Handler) AdminProvisionPlugins(c *gin.Context) {
	if !h.pluginsReady(c) {
		return
	}
	ids := h.plugins.ProvisionPlugins()
	items := make([]gin.H, 0, len(ids))
	for _, id := range ids {
		inst, err := h.plugins.Get(id)
		if err != nil {
			continue
		}
		snap := inst.Snapshot()
		items = append(items, gin.H{
			"id":   id,
			"name": snap.Name,
		})
	}
	OK(c, gin.H{"items": items})
}

// AdminUpstreamProducts 返回上游插件的产品列表，供管理端选择上游商品时使用。
func (h *Handler) AdminUpstreamProducts(c *gin.Context) {
	if !h.pluginsReady(c) {
		return
	}
	pluginID := c.Query("plugin_id")
	if pluginID == "" {
		BadRequest(c, "请指定上游插件")
		return
	}

	inst, client, err := h.provisionClient(c, pluginID)
	if err != nil {
		return
	}

	reply, err := client.ListProducts(inst.TokenContext(c.Request.Context()), &pb.ListProductsRequest{Page: 1, Limit: 200})
	if err != nil {
		Internal(c, "获取上游产品失败: "+err.Error())
		return
	}
	if reply.GetError() != "" {
		Internal(c, "上游返回错误: "+reply.GetError())
		return
	}

	items := make([]gin.H, 0, len(reply.GetProducts()))
	for _, up := range reply.GetProducts() {
		items = append(items, gin.H{
			"id":            up.GetId(),
			"name":          up.GetName(),
			"description":   up.GetDescription(),
			"group_name":    up.GetGroupName(),
			"price_cents":   up.GetPriceCents(),
			"billing_cycle": up.GetBillingCycle(),
		})
	}

	OK(c, gin.H{"items": items})
}

// AdminSyncProductInfo 从上游拉取单个商品的价格、计费周期与简介并更新本地记录。
func (h *Handler) AdminSyncProductInfo(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}

	var product model.Product
	if err := h.db().First(&product, id).Error; err != nil {
		NotFound(c, "商品不存在")
		return
	}
	if product.UpstreamPluginID == "" || product.UpstreamProductID == "" {
		BadRequest(c, "该商品未关联上游")
		return
	}

	inst, client, err := h.provisionClient(c, product.UpstreamPluginID)
	if err != nil {
		return
	}

	reply, err := client.GetProduct(inst.TokenContext(c.Request.Context()), &pb.GetProductRequest{Id: product.UpstreamProductID})
	if err != nil {
		Internal(c, "获取上游商品失败: "+err.Error())
		return
	}
	if reply.GetError() != "" {
		Internal(c, "上游返回错误: "+reply.GetError())
		return
	}
	up := reply.GetProduct()
	if up == nil {
		Internal(c, "上游未返回商品信息")
		return
	}

	updates := map[string]any{}
	if up.GetDescription() != "" {
		updates["description"] = up.GetDescription()
	}
	if up.GetPriceCents() > 0 {
		updates["price_cents"] = up.GetPriceCents()
	}
	if up.GetBillingCycle() != "" {
		updates["billing_cycle"] = up.GetBillingCycle()
	}
	if len(updates) == 0 {
		BadRequest(c, "上游未返回可同步的价格、周期或简介")
		return
	}
	if err := h.db().Model(&product).Updates(updates).Error; err != nil {
		Internal(c, "更新失败: "+err.Error())
		return
	}

	OK(c, gin.H{"message": "同步完成"})
}

// provisionClient 校验插件存在且支持产品对接，返回插件实例与其 gRPC 客户端。
func (h *Handler) provisionClient(c *gin.Context, pluginID string) (*plugin.Instance, pb.PluginClient, error) {
	inst, err := h.plugins.Get(pluginID)
	if err != nil {
		NotFound(c, "插件不存在")
		return nil, nil, err
	}
	if !inst.Has(pb.Capability_CAPABILITY_PROVISION_PRODUCT) {
		BadRequest(c, "该插件不支持产品对接")
		return nil, nil, err
	}
	client := inst.Client()
	if client == nil {
		Internal(c, "插件未运行")
		return nil, nil, err
	}
	return inst, client, nil
}

// ---------- 服务管理 ----------

// AdminUserServices 分页返回某用户的已购服务。
func (h *Handler) AdminUserServices(c *gin.Context) {
	userID, ok := IDParam(c, "id")
	if !ok {
		return
	}
	page, pageSize, offset := Pagination(c)
	items, total, err := h.admin().UserServices(userID, offset, pageSize)
	if err != nil {
		respond(c, nil, err)
		return
	}
	OK(c, Page{Items: items, Total: total, Page: page, PageSize: pageSize})
}

// AdminUpdateService 修改服务状态（停用 / 恢复）。
func (h *Handler) AdminUpdateService(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	var req struct {
		Status string `json:"status"`
	}
	if !bindJSON(c, &req) {
		return
	}
	item, err := h.admin().SetServiceStatus(id, req.Status)
	respond(c, item, err)
}

// AdminDeleteService 删除用户的服务。
func (h *Handler) AdminDeleteService(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	if err := h.admin().DeleteService(id); err != nil {
		respond(c, nil, err)
		return
	}
	noContent(c)
}

// ---------- 财务：支付方式 ----------

func (h *Handler) AdminPaymentPlugins(c *gin.Context) {
	items, err := h.admin().PaymentPlugins()
	respond(c, gin.H{"items": items}, err)
}

func (h *Handler) AdminPaymentMethods(c *gin.Context) {
	items, err := h.admin().PaymentMethods()
	if err != nil {
		respond(c, nil, err)
		return
	}
	// 解密 Config JSON 为 map 以便前端直接渲染及编辑回显
	type outItem struct {
		ID       uint              `json:"id"`
		Name     string            `json:"name"`
		PluginID string            `json:"plugin_id"`
		Config   map[string]string `json:"config"`
		Enabled  bool              `json:"enabled"`
		SortOrder int              `json:"sort_order"`
		CreatedAt string          `json:"created_at"`
		UpdatedAt string          `json:"updated_at"`
	}
	out := make([]outItem, 0, len(items))
	for _, m := range items {
		cfg := map[string]string{}
		if m.Config != "" {
			_ = jsonUnmarshal([]byte(m.Config), &cfg)
		}
		out = append(out, outItem{ID: m.ID, Name: m.Name, PluginID: m.PluginID, Config: cfg, Enabled: m.Enabled, SortOrder: m.SortOrder, CreatedAt: m.CreatedAt.Format("2006-01-02T15:04:05Z"), UpdatedAt: m.UpdatedAt.Format("2006-01-02T15:04:05Z")})
	}
	OK(c, gin.H{"items": out})
}

func jsonUnmarshal(data []byte, v any) error {
	// 避免在 handler 包直接 import encoding/json 的未使用警告，这里用标准库
	return json.Unmarshal(data, v)
}

func (h *Handler) AdminCreatePaymentMethod(c *gin.Context) {
	var req service.PaymentMethodInput
	if !bindJSON(c, &req) {
		return
	}
	item, err := h.admin().CreatePaymentMethod(req)
	if err != nil {
		respond(c, nil, err)
		return
	}
	cfg := map[string]string{}
	if item.Config != "" {
		_ = jsonUnmarshal([]byte(item.Config), &cfg)
	}
	respond(c, gin.H{"id": item.ID, "name": item.Name, "plugin_id": item.PluginID, "config": cfg, "enabled": item.Enabled, "sort_order": item.SortOrder}, nil)
}

func (h *Handler) AdminUpdatePaymentMethod(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	var req service.PaymentMethodInput
	if !bindJSON(c, &req) {
		return
	}
	item, err := h.admin().UpdatePaymentMethod(id, req)
	if err != nil {
		respond(c, nil, err)
		return
	}
	cfg := map[string]string{}
	if item.Config != "" {
		_ = jsonUnmarshal([]byte(item.Config), &cfg)
	}
	respond(c, gin.H{"id": item.ID, "name": item.Name, "plugin_id": item.PluginID, "config": cfg, "enabled": item.Enabled, "sort_order": item.SortOrder}, nil)
}

func (h *Handler) AdminDeletePaymentMethod(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	if err := h.admin().DeletePaymentMethod(id); err != nil {
		respond(c, nil, err)
		return
	}
	noContent(c)
}

// AdminCreateService 为指定用户手动创建服务。
func (h *Handler) AdminCreateService(c *gin.Context) {
	userID, ok := IDParam(c, "id")
	if !ok {
		return
	}
	var req service.AdminCreateServiceRequest
	if !bindJSON(c, &req) {
		return
	}
	item, err := h.admin().CreateServiceForUser(userID, req)
	respond(c, item, err)
}

// AdminBindService 绑定或解绑服务的上游主机。
func (h *Handler) AdminBindService(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	var req service.AdminBindServiceRequest
	if !bindJSON(c, &req) {
		return
	}
	item, err := h.admin().BindServiceUpstream(id, req)
	respond(c, item, err)
}
