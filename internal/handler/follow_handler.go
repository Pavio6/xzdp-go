package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"hmdp-backend/internal/dto/result"
	"hmdp-backend/internal/middleware"
	"hmdp-backend/internal/model"
	"hmdp-backend/internal/service"
)

// FollowHandler 处理关注/取关相关接口
type FollowHandler struct {
	followSvc   *service.FollowService
	userService *service.UserService
}

func NewFollowHandler(followSvc *service.FollowService, userSvc *service.UserService) *FollowHandler {
	return &FollowHandler{followSvc: followSvc, userService: userSvc}
}

// Follow 切换关注状态：follow=true 关注，follow=false 取关
func (h *FollowHandler) Follow(ctx *gin.Context) {
	targetID, err := strconv.ParseInt(ctx.Param("id"), 10, 64)
	if err != nil {
		ctx.JSON(http.StatusBadRequest, result.Fail("invalid follow user id"))
		return
	}
	followFlag := ctx.Param("follow")
	follow := followFlag == "true"

	// 获取当前登录用户
	loginUser, ok := middleware.GetLoginUser(ctx)
	if !ok || loginUser == nil {
		ctx.JSON(http.StatusUnauthorized, result.Fail("未登录"))
		return
	}

	if err := h.followSvc.Follow(ctx.Request.Context(), loginUser.ID, targetID, follow); err != nil {
		ctx.JSON(http.StatusInternalServerError, result.Fail(err.Error()))
		return
	}
	ctx.JSON(http.StatusOK, result.Ok())
}

// IsFollowed 判断当前用户是否已关注指定用户
func (h *FollowHandler) IsFollowed(ctx *gin.Context) {
	targetID, err := strconv.ParseInt(ctx.Param("id"), 10, 64)
	if err != nil {
		ctx.JSON(http.StatusBadRequest, result.Fail("invalid follow user id"))
		return
	}
	loginUser, ok := middleware.GetLoginUser(ctx)
	if !ok || loginUser == nil {
		ctx.JSON(http.StatusUnauthorized, result.Fail("未登录"))
		return
	}
	flag, err := h.followSvc.IsFollowing(ctx.Request.Context(), loginUser.ID, targetID)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, result.Fail(err.Error()))
		return
	}
	ctx.JSON(http.StatusOK, result.OkWithData(flag))
}

// CommonFollow 查询与目标用户的共同关注列表（返回用户信息）
func (h *FollowHandler) CommonFollow(ctx *gin.Context) {
	targetID, err := strconv.ParseInt(ctx.Param("id"), 10, 64)
	if err != nil {
		ctx.JSON(http.StatusBadRequest, result.Fail("invalid user id"))
		return
	}
	loginUser, ok := middleware.GetLoginUser(ctx)
	if !ok || loginUser == nil {
		ctx.JSON(http.StatusUnauthorized, result.Fail("未登录"))
		return
	}
	ids, err := h.followSvc.CommonFollowIDs(ctx.Request.Context(), loginUser.ID, targetID)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, result.Fail(err.Error()))
		return
	}
	var users []model.User
	for _, id := range ids {
		u, err := h.userService.FindByID(ctx.Request.Context(), id)
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, result.Fail(err.Error()))
			return
		}
		if u != nil {
			users = append(users, *u)
		}
	}
	ctx.JSON(http.StatusOK, result.OkWithData(users))
}
