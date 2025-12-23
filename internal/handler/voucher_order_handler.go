package handler

import (
	"hmdp-backend/internal/dto/result"
	"hmdp-backend/internal/middleware"
	"hmdp-backend/internal/service"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

type VoucherOrderHandler struct {
	voucherOrderSvc *service.VoucherOrderService
}

func NewVoucherOrderHandler(svc *service.VoucherOrderService) *VoucherOrderHandler {
	return &VoucherOrderHandler{voucherOrderSvc: svc}
}

// SeckillVoucher 处理秒杀优惠券
func (h *VoucherOrderHandler) SeckillVoucher(ctx *gin.Context) {
	// 解析优惠券ID
	voucherID, err := strconv.ParseInt(ctx.Param("id"), 10, 64)
	if err != nil {
		ctx.JSON(http.StatusBadRequest, result.Fail("invalid voucher id"))
		return
	}

	// 从上下文获取登录用户信息
	user, ok := middleware.GetLoginUser(ctx)
	if !ok {
		ctx.JSON(http.StatusUnauthorized, result.Fail("未登录"))
		return
	}

	// 调用业务层执行秒杀下单：校验时间/库存、扣减库存、生成订单
	orderID, svcErr := h.voucherOrderSvc.Seckill(ctx.Request.Context(), voucherID, user.ID)
	if svcErr != nil {
		ctx.JSON(http.StatusBadRequest, result.Fail(svcErr.Error()))
		return
	}

	ctx.JSON(http.StatusOK, result.OkWithData(orderID))
}
