package handler

import (
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/SakuraOpenSource/levis/internal/httpx"
)

// ServiceNATMappingRequest 是新增 NAT 端口映射的入参。
type ServiceNATMappingRequest struct {
	// Protocol 为 tcp / udp，缺省 tcp。
	Protocol string `json:"protocol"`
	// HostPort 为宿主（公网）端口，0 表示由上游自动分配。
	HostPort  int32  `json:"host_port"`
	GuestPort int32  `json:"guest_port"`
	Remark    string `json:"remark"`
}

// ServiceNATMappings 返回上游主机的 NAT 端口映射列表。
func (h *Handler) ServiceNATMappings(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	items, err := h.billing().ServiceNATMappings(httpx.CurrentUserID(c), id)
	respond(c, gin.H{"items": items}, err)
}

// ServiceCreateNAT 为上游主机新增一条 NAT 端口映射，返回创建后的映射
// （host_port 传 0 时由上游自动分配，以返回的 host_port 为准）。
func (h *Handler) ServiceCreateNAT(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	var req ServiceNATMappingRequest
	if !bindJSON(c, &req) {
		return
	}
	mapping, err := h.billing().ServiceCreateNAT(httpx.CurrentUserID(c), id,
		req.Protocol, req.HostPort, req.GuestPort, req.Remark)
	if err != nil {
		respond(c, nil, err)
		return
	}
	// 与前端约定包装在 mapping 键下，items/mapping 两种读取方式都稳定。
	respond(c, gin.H{"mapping": mapping}, nil)
}

// ServiceDeleteNAT 删除一条 NAT 端口映射，:mid 为列表返回的 mapping_id
// （上游侧 ID，超出 uint 范围也可能合法，因此用 ParseUint 解析）。
func (h *Handler) ServiceDeleteNAT(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	mid, err := strconv.ParseUint(c.Param("mid"), 10, 64)
	if err != nil || mid == 0 {
		BadRequest(c, "无效的映射 ID")
		return
	}
	if err := h.billing().ServiceDeleteNAT(httpx.CurrentUserID(c), id, mid); err != nil {
		respond(c, nil, err)
		return
	}
	OK(c, gin.H{"message": "映射已删除"})
}
