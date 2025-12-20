package handler

import (
	"hmdp-backend/internal/dto/result"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"hmdp-backend/internal/model"
	"hmdp-backend/internal/service"
	"hmdp-backend/internal/utils"
)

type ShopHandler struct {
	service *service.ShopService
}

func NewShopHandler(svc *service.ShopService) *ShopHandler {
	return &ShopHandler{service: svc}
}
// QueryShopByID 根据ID查询店铺
func (h *ShopHandler) QueryShopByID(ctx *gin.Context) {
	id, err := strconv.ParseInt(ctx.Param("id"), 10, 64)
	if err != nil {
		ctx.JSON(http.StatusBadRequest, result.Fail(err.Error()))
		return
	}
	shop, err := h.service.GetByIDWithLogicalExpire(ctx.Request.Context(), id)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, result.Fail(err.Error()))
		return
	}
	ctx.JSON(http.StatusOK, result.OkWithData(shop))
}

func (h *ShopHandler) SaveShop(ctx *gin.Context) {
	var shop model.Shop
	if err := ctx.ShouldBindJSON(&shop); err != nil {
		ctx.JSON(http.StatusBadRequest, result.Fail("invalid payload"))
		return
	}
	if err := h.service.Create(ctx.Request.Context(), &shop); err != nil {
		ctx.JSON(http.StatusInternalServerError, result.Fail(err.Error()))
		return
	}
	ctx.JSON(http.StatusOK, result.OkWithData(shop.ID))
}
// UpdateShop 更新店铺信息
func (h *ShopHandler) UpdateShop(ctx *gin.Context) {
	var shop model.Shop
	if err := ctx.ShouldBindJSON(&shop); err != nil {
		ctx.JSON(http.StatusBadRequest, result.Fail("invalid payload"))
		return
	}
	if err := h.service.Update(ctx.Request.Context(), &shop); err != nil {
		ctx.JSON(http.StatusInternalServerError, result.Fail(err.Error()))
		return
	}
	ctx.JSON(http.StatusOK, result.Ok())
}

func (h *ShopHandler) QueryShopByType(ctx *gin.Context) {
	typeIDStr := ctx.Query("typeId")
	if typeIDStr == "" {
		ctx.JSON(http.StatusBadRequest, result.Fail("typeId is required"))
		return
	}
	typeID, err := strconv.ParseInt(typeIDStr, 10, 64)
	if err != nil {
		ctx.JSON(http.StatusBadRequest, result.Fail("invalid typeId"))
		return
	}
	page := utils.ParsePage(ctx.Query("current"), 1)
	shops, err := h.service.QueryByType(ctx.Request.Context(), typeID, page, utils.DEFAULT_PAGE_SIZE)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, result.Fail(err.Error()))
		return
	}
	ctx.JSON(http.StatusOK, result.OkWithData(shops))
}

func (h *ShopHandler) QueryShopByName(ctx *gin.Context) {
	name := ctx.Query("name")
	page := utils.ParsePage(ctx.Query("current"), 1)
	shops, err := h.service.QueryByName(ctx.Request.Context(), name, page, utils.MAX_PAGE_SIZE)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, result.Fail(err.Error()))
		return
	}
	ctx.JSON(http.StatusOK, result.OkWithData(shops))
}
