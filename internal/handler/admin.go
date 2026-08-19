package handler

import (
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/SakuraOpenSource/levis/internal/httpx"
	"github.com/SakuraOpenSource/levis/internal/model"
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

// AdminSyncProducts 从上游插件同步产品列表到本地。
func (h *Handler) AdminSyncProducts(c *gin.Context) {
	if !h.pluginsReady(c) {
		return
	}
	var req struct {
		PluginID string `json:"plugin_id"`
	}
	if !bindJSON(c, &req) {
		return
	}
	if req.PluginID == "" {
		BadRequest(c, "请指定上游插件")
		return
	}

	inst, err := h.plugins.Get(req.PluginID)
	if err != nil {
		NotFound(c, "插件不存在")
		return
	}
	if !inst.Has(pb.Capability_CAPABILITY_PROVISION_PRODUCT) {
		BadRequest(c, "该插件不支持产品对接")
		return
	}

	client := inst.Client()
	if client == nil {
		Internal(c, "插件未运行")
		return
	}

	reply, err := client.ListProducts(c.Request.Context(), &pb.ListProductsRequest{Page: 1, Limit: 100})
	if err != nil {
		Internal(c, "获取上游产品失败: "+err.Error())
		return
	}
	if reply.GetError() != "" {
		Internal(c, "上游返回错误: "+reply.GetError())
		return
	}

	created := 0
	for _, up := range reply.GetProducts() {
		// 检查是否已存在同步记录
		var existing model.Product
		result := h.db().Where("upstream_plugin_id = ? AND upstream_product_id = ?",
			req.PluginID, up.GetId()).First(&existing)
		if result.Error == nil {
			// 更新
			h.db().Model(&existing).Updates(map[string]any{
				"name":          up.GetName(),
				"description":   up.GetDescription(),
				"price_cents":   up.GetPriceCents(),
				"billing_cycle": up.GetBillingCycle(),
				"specs":         model.SpecList{},
				"stock":         stockFromAvailable(up.GetStockAvailable()),
				"status":        model.ProductActive,
			})
			continue
		}
		// 新建
		specs := model.SpecList{}
		for k, v := range up.GetSpecs() {
			specs = append(specs, model.Spec{Label: k, Value: v})
		}
		product := model.Product{
			Name:              up.GetName(),
			Description:       up.GetDescription(),
			PriceCents:        up.GetPriceCents(),
			BillingCyc:        up.GetBillingCycle(),
			Specs:             specs,
			Stock:             stockFromAvailable(up.GetStockAvailable()),
			Status:            model.ProductActive,
			UpstreamPluginID:  req.PluginID,
			UpstreamProductID: up.GetId(),
		}
		if err := h.db().Create(&product).Error; err != nil {
			continue
		}
		created++
	}

	OK(c, gin.H{"created": created, "total": len(reply.GetProducts())})
}

func stockFromAvailable(available bool) int {
	if available {
		return -1 // 不限
	}
	return 0
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
