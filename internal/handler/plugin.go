package handler

import (
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/SakuraOpenSource/levis/internal/model"
	"github.com/SakuraOpenSource/levis/internal/plugin"
	"github.com/SakuraOpenSource/levis/internal/service"
)

// 本文件是 /api/admin/plugins 的接口层。
//
// 插件的「状态」分散在两处：进程与连接在 plugin.Manager 里，启用意图与配置在
// 数据库里。这些 handler 的活儿就是把两边拼成一个视图，并保证两边同时更新。

// AdminPlugins 返回插件列表。
func (h *Handler) AdminPlugins(c *gin.Context) {
	if !h.pluginsReady(c) {
		return
	}
	svc := h.pluginSvc()
	list := h.plugins.List()
	items := make([]service.PluginDetail, 0, len(list))
	for _, inst := range list {
		detail, err := svc.Detail(inst)
		if err != nil {
			respond(c, nil, err)
			return
		}
		items = append(items, *detail)
	}
	OK(c, gin.H{"items": items, "scopes": model.AllPluginScopes()})
}

// AdminPlugin 返回单个插件的详情，含配置字段定义与当前值。
func (h *Handler) AdminPlugin(c *gin.Context) {
	inst, ok := h.pluginInstance(c)
	if !ok {
		return
	}
	detail, err := h.pluginSvc().Detail(inst)
	respond(c, detail, err)
}

// AdminInstallPlugin 安装一个包含后端和管理前端的插件 ZIP。
func (h *Handler) AdminInstallPlugin(c *gin.Context) {
	if !h.pluginsReady(c) {
		return
	}
	file, err := c.FormFile("file")
	if err != nil {
		BadRequest(c, "请上传 ZIP 插件文件")
		return
	}
	opened, err := file.Open()
	if err != nil {
		BadRequest(c, "无法读取插件文件")
		return
	}
	defer opened.Close()
	id, err := plugin.InstallArchive(h.rt.DataDir(), opened)
	if err != nil {
		if strings.Contains(err.Error(), "已存在") {
			Conflict(c, err.Error())
			return
		}
		BadRequest(c, err.Error())
		return
	}
	if err := h.plugins.Reload(c.Request.Context()); err != nil {
		Internal(c, "插件安装后扫描失败")
		return
	}
	OK(c, gin.H{"id": id})
}

// AdminPluginFrontend serves a plugin's same-origin management frontend.
func (h *Handler) AdminPluginFrontend(c *gin.Context) {
	if !h.pluginsReady(c) {
		return
	}
	id := c.Param("id")
	if err := plugin.ValidID(id); err != nil {
		NotFound(c, "插件不存在")
		return
	}
	frontendRoot := filepath.Join(h.rt.DataDir(), plugin.RootName(id), "frontend")
	rel := strings.TrimPrefix(c.Param("path"), "/")
	if rel == "" || rel == "." {
		rel = "index.html"
	}
	clean := path.Clean("/" + rel)[1:]
	if clean == "" || strings.HasPrefix(clean, "../") || clean == ".." {
		NotFound(c, "资源不存在")
		return
	}
	full := filepath.Join(frontendRoot, filepath.FromSlash(clean))
	rootAbs, _ := filepath.Abs(frontendRoot)
	fullAbs, _ := filepath.Abs(full)
	if fullAbs != rootAbs && !strings.HasPrefix(fullAbs, rootAbs+string(filepath.Separator)) {
		NotFound(c, "资源不存在")
		return
	}
	if info, err := os.Stat(fullAbs); err != nil || info.IsDir() {
		fullAbs = filepath.Join(rootAbs, "index.html")
	}
	c.Header("Content-Security-Policy", "default-src 'self'; frame-ancestors 'self'; object-src 'none'")
	c.Header("X-Frame-Options", "SAMEORIGIN")
	c.Header("X-Content-Type-Options", "nosniff")
	c.File(fullAbs)
}

// AdminReloadPlugins 重新扫描插件目录。
//
// 已在运行的插件保持不动 —— 重扫是为了发现新放进来的插件，不该顺手把正在
// 工作的插件全部重启一遍。
func (h *Handler) AdminReloadPlugins(c *gin.Context) {
	if !h.pluginsReady(c) {
		return
	}
	if err := h.plugins.Reload(c.Request.Context()); err != nil {
		respond(c, nil, err)
		return
	}
	h.AdminPlugins(c)
}

// AdminPluginConfigRequest 是保存配置的入参。
type AdminPluginConfigRequest struct {
	// Values 的键必须是 manifest 声明过的字段，其余一律忽略。
	Values map[string]string `json:"values"`
	// Scopes 是管理员授予的权限位。nil 表示不改动授权，只存配置。
	Scopes []string `json:"scopes"`
}

// AdminUpdatePluginConfig 保存配置与授权，并把新配置下发给运行中的插件。
func (h *Handler) AdminUpdatePluginConfig(c *gin.Context) {
	inst, ok := h.pluginInstance(c)
	if !ok {
		return
	}
	var req AdminPluginConfigRequest
	if !bindJSON(c, &req) {
		return
	}

	svc := h.pluginSvc()
	// 授权先于配置，且不依赖插件是否在运行：权限位是管理员的决定，取值范围写死
	// 在 model 里，不来自 manifest。反过来要求「先启用才能授权」会逼着管理员先
	// 让一个未授权的进程跑起来。
	if req.Scopes != nil {
		if err := svc.SetScopes(inst.ID(), req.Scopes); err != nil {
			respond(c, nil, err)
			return
		}
	}
	// 配置字段的定义只有插件自己知道，所以没提交值时不去碰它 —— 否则「只改授权」
	// 这个操作会因为插件没运行而整个失败。
	if len(req.Values) > 0 {
		if err := svc.SaveConfig(inst, req.Values); err != nil {
			respond(c, nil, err)
			return
		}
	}
	// 下发失败要如实报出来：值已经落库了（插件下次启动会读到），但管理员必须
	// 知道当前跑着的那个进程还在用旧配置，否则会以为改动已经生效。
	if err := h.plugins.Reconfigure(c.Request.Context(), inst.ID()); err != nil {
		respond(c, nil, err)
		return
	}

	detail, err := svc.Detail(inst)
	respond(c, detail, err)
}

// AdminFrontendPluginConfig returns the Epay frontend configuration without exposing secrets.
func (h *Handler) AdminFrontendPluginConfig(c *gin.Context) {
	inst, ok := h.pluginInstance(c)
	if !ok {
		return
	}
	values, err := h.pluginSvc().FrontendConfig(inst.ID())
	respond(c, values, err)
}

// AdminUpdateFrontendPluginConfig saves frontend-owned configuration fields.
func (h *Handler) AdminUpdateFrontendPluginConfig(c *gin.Context) {
	inst, ok := h.pluginInstance(c)
	if !ok {
		return
	}
	var req struct {
		Values map[string]string `json:"values"`
	}
	if !bindJSON(c, &req) {
		return
	}
	if err := h.pluginSvc().SaveFrontendConfig(inst.ID(), req.Values); err != nil {
		respond(c, nil, err)
		return
	}
	if err := h.plugins.Reconfigure(c.Request.Context(), inst.ID()); err != nil {
		respond(c, nil, err)
		return
	}
	OK(c, gin.H{"ok": true})
}

// AdminEnablePlugin 启用插件并拉起进程。
func (h *Handler) AdminEnablePlugin(c *gin.Context) {
	h.setPluginEnabled(c, true)
}

// AdminDisablePlugin 停用插件并结束进程。
func (h *Handler) AdminDisablePlugin(c *gin.Context) {
	h.setPluginEnabled(c, false)
}

// setPluginEnabled 是启用与停用的共同实现。
//
// 先写库再动进程：库里记的是管理员的意图，主程序重启后据此恢复。反过来的话，
// 进程起来了但意图没落库，重启后插件就悄悄不见了。
func (h *Handler) setPluginEnabled(c *gin.Context, enabled bool) {
	inst, ok := h.pluginInstance(c)
	if !ok {
		return
	}
	svc := h.pluginSvc()
	if err := svc.SetEnabled(inst.ID(), enabled); err != nil {
		respond(c, nil, err)
		return
	}

	var err error
	if enabled {
		err = h.plugins.Enable(c.Request.Context(), inst.ID())
	} else {
		err = h.plugins.Disable(inst.ID())
	}
	if err != nil {
		// 进程没起来时把意图回退，避免界面显示「已启用」而实际没跑。
		if enabled {
			_ = svc.SetEnabled(inst.ID(), false)
		}
		// 文件权限不对、缺可执行文件这类问题是管理员自己能修的，报 400 并把
		// 原因原样带出去；500 会让人以为是程序坏了，而消息里正好写着该怎么修。
		if errors.Is(err, plugin.ErrNotLoadable) {
			BadRequest(c, err.Error())
			return
		}
		respond(c, nil, err)
		return
	}

	detail, detailErr := svc.Detail(inst)
	respond(c, detail, detailErr)
}

// AdminPluginLogs 返回插件最近的日志行。
func (h *Handler) AdminPluginLogs(c *gin.Context) {
	inst, ok := h.pluginInstance(c)
	if !ok {
		return
	}
	OK(c, gin.H{"lines": inst.Logs()})
}

// pluginsReady 报告插件管理器是否可用，不可用时已写回响应。
func (h *Handler) pluginsReady(c *gin.Context) bool {
	if h.plugins == nil {
		Fail(c, 503, "PLUGIN_UNAVAILABLE", "插件系统未启用")
		return false
	}
	return true
}

// pluginInstance 按路径参数取插件实例。
func (h *Handler) pluginInstance(c *gin.Context) (*plugin.Instance, bool) {
	if !h.pluginsReady(c) {
		return nil, false
	}
	id := c.Param("id")
	inst, err := h.plugins.Get(id)
	if err != nil {
		NotFound(c, "插件不存在")
		return nil, false
	}
	return inst, true
}
