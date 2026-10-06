package handler

import (
	"github.com/SakuraOpenSource/levis/internal/httpx"
	"github.com/SakuraOpenSource/levis/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *Handler) AffiliateCommissions(c *gin.Context) {
	page, size, offset := Pagination(c)
	items, total, err := h.affiliate().Commissions(httpx.CurrentUserID(c), offset, size)
	if err != nil {
		respond(c, nil, err)
		return
	}
	OK(c, Page{Items: items, Total: total, Page: page, PageSize: size})
}
func (h *Handler) AffiliateWithdrawals(c *gin.Context) {
	h.affiliateWithdrawalList(c, httpx.CurrentUserID(c))
}
func (h *Handler) AdminAffiliateWithdrawals(c *gin.Context) { h.affiliateWithdrawalList(c, 0) }
func (h *Handler) affiliateWithdrawalList(c *gin.Context, userID uint) {
	page, size, offset := Pagination(c)
	items, total, err := h.affiliate().Withdrawals(userID, c.Query("status"), offset, size)
	if err != nil {
		respond(c, nil, err)
		return
	}
	OK(c, Page{Items: items, Total: total, Page: page, PageSize: size})
}
func (h *Handler) AffiliateWithdraw(c *gin.Context) {
	var req service.AffiliateWithdrawalInput
	if !bindJSON(c, &req) {
		return
	}
	out, err := h.affiliate().Withdraw(httpx.CurrentUserID(c), req)
	respond(c, out, err)
}
func (h *Handler) AdminAffiliateReview(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	var req service.AffiliateReviewInput
	if !bindJSON(c, &req) {
		return
	}
	out, err := h.affiliate().Review(httpx.CurrentUserID(c), id, req)
	respond(c, out, err)
}
func (h *Handler) affiliate() *service.AffiliateService { return service.NewAffiliateService(h.db()) }
func (h *Handler) Affiliate(c *gin.Context) {
	out, err := h.affiliate().Summary(httpx.CurrentUserID(c))
	respond(c, out, err)
}
func (h *Handler) AffiliateJoin(c *gin.Context) {
	out, err := h.affiliate().Join(httpx.CurrentUserID(c))
	respond(c, out, err)
}
func (h *Handler) AdminAffiliateSettings(c *gin.Context) {
	out, err := h.affiliate().Settings()
	respond(c, out, err)
}
func (h *Handler) AdminUpdateAffiliateSettings(c *gin.Context) {
	var req service.AffiliateSettings
	if !bindJSON(c, &req) {
		return
	}
	out, err := h.affiliate().UpdateSettings(req)
	respond(c, out, err)
}
