package handler

import (
	"hmdp-backend/internal/dto/result"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"hmdp-backend/internal/model"
	"hmdp-backend/internal/service"
)

type VoucherHandler struct {
	service *service.VoucherService
}

func NewVoucherHandler(svc *service.VoucherService) *VoucherHandler {
	return &VoucherHandler{service: svc}
}

func (h *VoucherHandler) AddVoucher(ctx *gin.Context) {
	var voucher model.Voucher
	if err := ctx.ShouldBindJSON(&voucher); err != nil {
		ctx.JSON(http.StatusBadRequest, result.Fail("invalid payload"))
		return
	}
	if err := h.service.Create(ctx.Request.Context(), &voucher); err != nil {
		ctx.JSON(http.StatusInternalServerError, result.Fail(err.Error()))
		return
	}
	ctx.JSON(http.StatusOK, result.OkWithData(voucher.ID))
}

// AddSeckillVoucher 添加秒杀优惠券
func (h *VoucherHandler) AddSeckillVoucher(ctx *gin.Context) {
	var voucher model.Voucher
	if err := ctx.ShouldBindJSON(&voucher); err != nil {
		ctx.JSON(http.StatusBadRequest, result.Fail("invalid payload"))
		return
	}
	if err := h.service.AddSeckillVoucher(ctx.Request.Context(), &voucher); err != nil {
		ctx.JSON(http.StatusInternalServerError, result.Fail(err.Error()))
		return
	}
	ctx.JSON(http.StatusOK, result.OkWithData(voucher.ID))
}

// 获取指定店铺的优惠券列表
func (h *VoucherHandler) QueryVoucherOfShop(ctx *gin.Context) {
	shopID, err := strconv.ParseInt(ctx.Param("shopId"), 10, 64)
	if err != nil {
		ctx.JSON(http.StatusBadRequest, result.Fail("invalid shop id"))
		return
	}
	vouchers, err := h.service.QueryVoucherOfShop(ctx.Request.Context(), shopID)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, result.Fail(err.Error()))
		return
	}
	ctx.JSON(http.StatusOK, result.OkWithData(vouchers))
}
