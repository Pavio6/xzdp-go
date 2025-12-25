package handler

import (
	"hmdp-backend/internal/dto"
	"hmdp-backend/internal/dto/result"
	"hmdp-backend/internal/middleware"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"hmdp-backend/internal/service"
)

type UserHandler struct {
	userService *service.UserService
}

func NewUserHandler(userSvc *service.UserService) *UserHandler {
	return &UserHandler{userService: userSvc}
}

// SendCode 根据手机号发送验证码
func (h *UserHandler) SendCode(ctx *gin.Context) {
	// 获取URL参数
	phone := ctx.DefaultQuery("phone", "")
	// 1.调用service发送验证码并保存到redis
	if err := h.userService.SendCode(ctx.Request.Context(), phone); err != nil {
		ctx.JSON(http.StatusInternalServerError, result.Fail(err.Error()))
	}

	ctx.JSON(http.StatusOK, result.Ok())
}

// Login 登录
func (h *UserHandler) Login(ctx *gin.Context) {
	var form dto.LoginForm
	if err := ctx.ShouldBindJSON(&form); err != nil {
		ctx.JSON(http.StatusBadRequest, result.Fail(err.Error()))
		return
	}
	token, err := h.userService.Login(ctx.Request.Context(), form)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, result.Fail(err.Error()))
		return
	}
	ctx.JSON(http.StatusOK, result.OkWithData(token))
}

// Logout 退出登录
func (h *UserHandler) Logout(ctx *gin.Context) {
	ctx.JSON(http.StatusOK, result.Fail("功能未完成"))
}

// Me 获取用户个人信息
func (h *UserHandler) Me(ctx *gin.Context) {
	user, b := middleware.GetLoginUser(ctx)
	if !b {
		return
	}
	ctx.JSON(http.StatusOK, result.OkWithData(user))
}

// Info 获取用户的信息
func (h *UserHandler) Info(ctx *gin.Context) {
	// 路径参数id转为int类型
	id, err := strconv.ParseInt(ctx.Param("id"), 10, 64)
	if err != nil {
		ctx.JSON(http.StatusBadRequest, result.Fail("invalid id"))
		return
	}
	// 调用service获取用户信息
	info, err := h.userService.FindByID(ctx.Request.Context(), id)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, result.Fail(err.Error()))
		return
	}
	if info == nil {
		ctx.JSON(http.StatusOK, result.Ok())
		return
	}
	// info.CreateTime = nil
	// info.UpdateTime = nil
	ctx.JSON(http.StatusOK, result.OkWithData(info))
}

// GetUserByID 通过 /user/:id 获取用户信息（用于用户主页）
func (h *UserHandler) GetUserByID(ctx *gin.Context) {
	id, err := strconv.ParseInt(ctx.Param("id"), 10, 64)
	if err != nil {
		ctx.JSON(http.StatusBadRequest, result.Fail("invalid id"))
		return
	}
	info, err := h.userService.FindByID(ctx.Request.Context(), id)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, result.Fail(err.Error()))
		return
	}
	if info == nil {
		ctx.JSON(http.StatusNotFound, result.Fail("user not found"))
		return
	}
	ctx.JSON(http.StatusOK, result.OkWithData(info))
}

// Sign 用户签到
func (h *UserHandler) Sign(ctx *gin.Context) {
	loginUser, ok := middleware.GetLoginUser(ctx)
	if !ok || loginUser == nil {
		ctx.JSON(http.StatusUnauthorized, result.Fail("未登录"))
		return
	}
	if err := h.userService.Sign(ctx.Request.Context(), loginUser.ID, time.Now()); err != nil {
		ctx.JSON(http.StatusInternalServerError, result.Fail(err.Error()))
		return
	}
	ctx.JSON(http.StatusOK, result.Ok())
}

// SignCount 本月连续签到天数（从当日向前统计，遇到未签到即停止）
func (h *UserHandler) SignCount(ctx *gin.Context) {
	loginUser, ok := middleware.GetLoginUser(ctx)
	if !ok || loginUser == nil {
		ctx.JSON(http.StatusUnauthorized, result.Fail("未登录"))
		return
	}
	count, err := h.userService.CountContinuousSign(ctx.Request.Context(), loginUser.ID, time.Now())
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, result.Fail(err.Error()))
		return
	}
	ctx.JSON(http.StatusOK, result.OkWithData(count))
}
