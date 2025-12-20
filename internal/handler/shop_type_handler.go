package handler

import (
	"hmdp-backend/internal/dto/result"
	"net/http"

	"github.com/gin-gonic/gin"

	"hmdp-backend/internal/service"
)

type ShopTypeHandler struct {
	service *service.ShopTypeService
}

func NewShopTypeHandler(svc *service.ShopTypeService) *ShopTypeHandler {
	return &ShopTypeHandler{service: svc}
}

// QueryTypeList 查询商铺类型列表
func (h *ShopTypeHandler) QueryTypeList(ctx *gin.Context) {
	types, err := h.service.List(ctx.Request.Context())
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, result.Fail(err.Error()))
		return
	}
	ctx.JSON(http.StatusOK, result.OkWithData(types))
}
