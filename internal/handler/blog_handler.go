package handler

import (
	"hmdp-backend/internal/dto/result"
	"hmdp-backend/internal/middleware"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"hmdp-backend/internal/model"
	"hmdp-backend/internal/service"
	"hmdp-backend/internal/utils"
)

type BlogHandler struct {
	blogService *service.BlogService
	userService *service.UserService
}

func NewBlogHandler(blogSvc *service.BlogService, userSvc *service.UserService) *BlogHandler {
	return &BlogHandler{blogService: blogSvc, userService: userSvc}
}

// SaveBlog 保存博客
func (h *BlogHandler) SaveBlog(ctx *gin.Context) {
	var blog model.Blog
	if err := ctx.ShouldBindJSON(&blog); err != nil {
		ctx.JSON(http.StatusBadRequest, result.Fail("invalid payload"))
		return
	}
	loginUser, b := middleware.GetLoginUser(ctx)
	if !b {
		return
	}
	if loginUser == nil {
		ctx.JSON(http.StatusUnauthorized, result.Fail("未登录"))
		return
	}
	blog.UserID = loginUser.ID
	if err := h.blogService.Create(ctx.Request.Context(), &blog); err != nil {
		ctx.JSON(http.StatusInternalServerError, result.Fail(err.Error()))
		return
	}
	ctx.JSON(http.StatusOK, result.OkWithData(blog.ID))
}

// LikeBlog 点赞博客
func (h *BlogHandler) LikeBlog(ctx *gin.Context) {
	id, err := strconv.ParseInt(ctx.Param("id"), 10, 64)
	if err != nil {
		ctx.JSON(http.StatusBadRequest, result.Fail("invalid id"))
		return
	}
	user, ok := middleware.GetLoginUser(ctx)
	if !ok || user == nil {
		ctx.JSON(http.StatusUnauthorized, result.Fail("未登录"))
		return
	}
	_, err = h.blogService.ToggleLike(ctx.Request.Context(), id, user.ID)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, result.Fail(err.Error()))
		return
	}
	ctx.JSON(http.StatusOK, result.Ok())
}

func (h *BlogHandler) QueryMyBlog(ctx *gin.Context) {
	loginUser, b := middleware.GetLoginUser(ctx)
	if !b {
		return
	}
	if loginUser == nil {
		ctx.JSON(http.StatusUnauthorized, result.Fail("未登录"))
		return
	}
	page := utils.ParsePage(ctx.Query("current"), 1)
	blogs, err := h.blogService.QueryByUser(ctx.Request.Context(), loginUser.ID, page, utils.MAX_PAGE_SIZE)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, result.Fail(err.Error()))
		return
	}
	for i := range blogs {
		isLike, err := h.blogService.IsLiked(ctx.Request.Context(), blogs[i].ID, loginUser.ID)
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, result.Fail(err.Error()))
			return
		}
		blogs[i].IsLike = &isLike
	}
	ctx.JSON(http.StatusOK, result.OkWithData(blogs))
}

func (h *BlogHandler) QueryHotBlog(ctx *gin.Context) {
	page := utils.ParsePage(ctx.Query("current"), 1)
	blogs, err := h.blogService.QueryHot(ctx.Request.Context(), page, utils.MAX_PAGE_SIZE)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, result.Fail(err.Error()))
		return
	}
	loginUser, _ := middleware.GetLoginUser(ctx)
	for i := range blogs {
		user, err := h.userService.FindByID(ctx.Request.Context(), blogs[i].UserID)
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, result.Fail(err.Error()))
			return
		}
		if user != nil {
			blogs[i].Name = user.NickName
			blogs[i].Icon = user.Icon
		}
		// 判断用户是否点赞
		if loginUser != nil {
			isLike, err := h.blogService.IsLiked(ctx.Request.Context(), blogs[i].ID, loginUser.ID)
			if err != nil {
				ctx.JSON(http.StatusInternalServerError, result.Fail(err.Error()))
				return
			}
			blogs[i].IsLike = &isLike
		}
	}
	ctx.JSON(http.StatusOK, result.OkWithData(blogs))
}

// QueryBlogByID 获取单条笔记，附带作者信息
func (h *BlogHandler) QueryBlogByID(ctx *gin.Context) {
	id, err := strconv.ParseInt(ctx.Param("id"), 10, 64)
	if err != nil {
		ctx.JSON(http.StatusBadRequest, result.Fail("invalid id"))
		return
	}
	loginUser, _ := middleware.GetLoginUser(ctx)
	blog, err := h.blogService.GetByID(ctx.Request.Context(), id)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, result.Fail(err.Error()))
		return
	}
	if blog == nil {
		ctx.JSON(http.StatusNotFound, result.Fail("blog not found"))
		return
	}
	user, err := h.userService.FindByID(ctx.Request.Context(), blog.UserID)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, result.Fail(err.Error()))
		return
	}
	if user != nil {
		blog.Name = user.NickName
		blog.Icon = user.Icon
	}
	if loginUser != nil {
		isLike, err := h.blogService.IsLiked(ctx.Request.Context(), blog.ID, loginUser.ID)
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, result.Fail(err.Error()))
			return
		}
		blog.IsLike = &isLike
	}
	ctx.JSON(http.StatusOK, result.OkWithData(blog))
}

// QueryBlogLikes 查询最早点赞的前5个用户
func (h *BlogHandler) QueryBlogLikes(ctx *gin.Context) {
	blogID, err := strconv.ParseInt(ctx.Param("id"), 10, 64)
	if err != nil {
		ctx.JSON(http.StatusBadRequest, result.Fail("invalid id"))
		return
	}
	ids, err := h.blogService.TopLikerIDs(ctx.Request.Context(), blogID, 5)
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

// QueryBlogOfUser 获取指定用户的笔记列表（用于用户主页）
func (h *BlogHandler) QueryBlogOfUser(ctx *gin.Context) {
	userID, err := strconv.ParseInt(ctx.Query("id"), 10, 64)
	if err != nil || userID <= 0 {
		ctx.JSON(http.StatusBadRequest, result.Fail("invalid user id"))
		return
	}
	page := utils.ParsePage(ctx.Query("current"), 1)

	blogs, err := h.blogService.QueryByUser(ctx.Request.Context(), userID, page, utils.MAX_PAGE_SIZE)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, result.Fail(err.Error()))
		return
	}
	// 填充作者信息一次
	author, err := h.userService.FindByID(ctx.Request.Context(), userID)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, result.Fail(err.Error()))
		return
	}

	// 若当前有登录用户，标记是否点赞
	loginUser, _ := middleware.GetLoginUser(ctx)

	for i := range blogs {
		if author != nil {
			blogs[i].Name = author.NickName
			blogs[i].Icon = author.Icon
		}
		if loginUser != nil {
			isLike, err := h.blogService.IsLiked(ctx.Request.Context(), blogs[i].ID, loginUser.ID)
			if err != nil {
				ctx.JSON(http.StatusInternalServerError, result.Fail(err.Error()))
				return
			}
			blogs[i].IsLike = &isLike
		}
	}
	ctx.JSON(http.StatusOK, result.OkWithData(blogs))
}
