package handler

import (
	"github.com/gin-gonic/gin"

	"github.com/SakuraOpenSource/levis/internal/httpx"
	"github.com/SakuraOpenSource/levis/internal/service"
)

// refunds() 构造退款服务；plugins 经接口注入，测试可传 nil。
func (h *Handler) refunds() *service.RefundService {
	return service.NewRefundService(h.db(), h.plugins, h.wallet())
}

// Refunds 当前用户的退款申请列表。
func (h *Handler) Refunds(c *gin.Context) {
	page, pageSize, offset := Pagination(c)
	items, total, err := h.refunds().List(httpx.CurrentUserID(c), offset, pageSize)
	if err != nil {
		respond(c, nil, err)
		return
	}
	OK(c, Page{Items: items, Total: total, Page: page, PageSize: pageSize})
}

// CreateRefund 用户提交退款申请。策略判定为自动通过时当场退款。
func (h *Handler) CreateRefund(c *gin.Context) {
	var in service.RefundCreateInput
	if err := c.ShouldBindJSON(&in); err != nil {
		Fail(c, 400, "BAD_REQUEST", "请求格式错误")
		return
	}
	item, err := h.refunds().Create(c.Request.Context(), httpx.CurrentUserID(c), in)
	respond(c, item, err)
}

// CancelRefund 用户撤回待审的退款申请。
func (h *Handler) CancelRefund(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	item, err := h.refunds().Cancel(httpx.CurrentUserID(c), id)
	respond(c, item, err)
}

// AdminRefunds 管理员分页查看退款申请，status 可选过滤。
func (h *Handler) AdminRefunds(c *gin.Context) {
	page, pageSize, offset := Pagination(c)
	items, total, err := h.refunds().AdminList(c.Query("status"), offset, pageSize)
	if err != nil {
		respond(c, nil, err)
		return
	}
	OK(c, Page{Items: items, Total: total, Page: page, PageSize: pageSize})
}

// AdminReviewRefund 管理员审批（通过/驳回）。通过即执行渠道与余额退款。
func (h *Handler) AdminReviewRefund(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	var in service.RefundReviewInput
	if err := c.ShouldBindJSON(&in); err != nil {
		Fail(c, 400, "BAD_REQUEST", "请求格式错误")
		return
	}
	item, err := h.refunds().Review(c.Request.Context(), httpx.CurrentUserID(c), id, in)
	respond(c, item, err)
}

// AdminRetryRefund 管理员对渠道退款失败的申请重试。
func (h *Handler) AdminRetryRefund(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	item, err := h.refunds().RetryFailed(c.Request.Context(), id)
	respond(c, item, err)
}

// AdminRefundPolicy GET 当前退款策略。
func (h *Handler) AdminRefundPolicy(c *gin.Context) {
	policy, err := h.refunds().Policy()
	respond(c, policy, err)
}

// AdminUpdateRefundPolicy PUT 保存退款策略。
func (h *Handler) AdminUpdateRefundPolicy(c *gin.Context) {
	var in service.RefundPolicyInput
	if err := c.ShouldBindJSON(&in); err != nil {
		Fail(c, 400, "BAD_REQUEST", "请求格式错误")
		return
	}
	policy, err := h.refunds().SavePolicy(in)
	respond(c, policy, err)
}
