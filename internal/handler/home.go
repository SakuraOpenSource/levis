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

// AdminSiteSettingsRequest 是保存站点名称与简介的入参。
type AdminSiteSettingsRequest struct {
	SiteName        string `json:"site_name"`
	SiteDescription string `json:"site_description"`
}

// AdminSiteSettings 返回安装后可编辑的站点名称与简介。
func (h *Handler) AdminSiteSettings(c *gin.Context) {
	name, description := h.settings().Site()
	OK(c, gin.H{"site_name": name, "site_description": description})
}

// AdminUpdateSiteSettings 保存站点名称与简介。
//
// 安装页之后唯一能改这两项的地方，保存后前端重拉 bootstrap 刷新标题。
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
	OK(c, gin.H{"site_name": name, "site_description": description})
}
