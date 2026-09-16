package handler

import (
	"github.com/gin-gonic/gin"

	"github.com/SakuraOpenSource/levis/internal/service"
)

// AdminHomeConfig 返回公开主页的完整配置（含关闭状态）。
//
// 管理端编辑页用它回填表单；公开访客走 bootstrap，按需才能看到启用后的配置。
func (h *Handler) AdminHomeConfig(c *gin.Context) {
	OK(c, h.settings().GetHomeConfig())
}

// AdminUpdateHomeConfig 保存公开主页的配置。
//
// 长度上限、链接 scheme 与图标取值表由 service 校验，这里只做 JSON 绑定。
func (h *Handler) AdminUpdateHomeConfig(c *gin.Context) {
	var req service.HomeConfig
	if !bindJSON(c, &req) {
		return
	}
	cfg, err := h.settings().SaveHomeConfig(req)
	respond(c, cfg, err)
}

// AdminSiteSettingsRequest 是保存站点设置的入参。
type AdminSiteSettingsRequest struct {
	SiteName        string `json:"site_name"`
	SiteDescription string `json:"site_description"`
	// TrafficPricePerGBCents 是流量包兜底单价（分/GB）。用指针区分"未携带"与
	// "显式清零"：旧客户端不传这个字段时不应把已配好的价格清掉，0 表示清除定价。
	TrafficPricePerGBCents *int64 `json:"traffic_price_per_gb_cents"`
}

// AdminSiteSettings 返回安装后可编辑的站点设置（名称、简介与流量包兜底单价）。
func (h *Handler) AdminSiteSettings(c *gin.Context) {
	name, description := h.settings().Site()
	OK(c, gin.H{
		"site_name":                  name,
		"site_description":           description,
		"traffic_price_per_gb_cents": h.settings().TrafficPricePerGB(),
	})
}

// AdminUpdateSiteSettings 保存站点名称、简介与流量包兜底单价。
//
// 安装页之后唯一能改站点名称与简介的地方，保存后前端重拉 bootstrap 刷新标题。
func (h *Handler) AdminUpdateSiteSettings(c *gin.Context) {
	var req AdminSiteSettingsRequest
	if !bindJSON(c, &req) {
		return
	}
	name, description, err := h.settings().SaveSiteSettings(req.SiteName, req.SiteDescription)
	if err != nil {
		respond(c, nil, err)
		return
	}
	price := h.settings().TrafficPricePerGB()
	if req.TrafficPricePerGBCents != nil {
		if err := h.settings().SaveTrafficPricePerGB(*req.TrafficPricePerGBCents); err != nil {
			respond(c, nil, err)
			return
		}
		price = *req.TrafficPricePerGBCents
	}
	OK(c, gin.H{
		"site_name":                  name,
		"site_description":           description,
		"traffic_price_per_gb_cents": price,
	})
}
