package router

import (
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	"hmdp-backend/internal/handler"
	"hmdp-backend/internal/middleware"
	"hmdp-backend/internal/service"
)

// RegisterRoutes 统一注册所有模块的路由
func RegisterRoutes(engine *gin.Engine, services *service.Registry, uploadDir string, rdb *redis.Client) {
	engine.Use(middleware.CORSMiddleware())
	engine.Use(middleware.LoginMiddleware(rdb))

	shopHandler := handler.NewShopHandler(services.Shop)
	shopTypeHandler := handler.NewShopTypeHandler(services.ShopType)
	voucherHandler := handler.NewVoucherHandler(services.Voucher)
	blogHandler := handler.NewBlogHandler(services.Blog, services.User)
	uploadHandler := handler.NewUploadHandler(uploadDir)
	userHandler := handler.NewUserHandler(services.User)
	voucherOrderHandler := handler.NewVoucherOrderHandler(services.VoucherOrder)
	followHandler := handler.NewFollowHandler(services.Follow, services.User)

	shopGroup := engine.Group("/shop")
	shopGroup.GET("/:id", shopHandler.QueryShopByID)
	shopGroup.POST("", shopHandler.SaveShop)
	shopGroup.PUT("", shopHandler.UpdateShop)
	shopGroup.GET("/of/type", shopHandler.QueryShopByType)
	shopGroup.GET("/of/name", shopHandler.QueryShopByName)

	engine.GET("/shop-type/list", shopTypeHandler.QueryTypeList)

	voucherGroup := engine.Group("/voucher")
	voucherGroup.POST("", voucherHandler.AddVoucher)
	voucherGroup.POST("/seckill", voucherHandler.AddSeckillVoucher)
	voucherGroup.GET("/list/:shopId", voucherHandler.QueryVoucherOfShop)

	blogGroup := engine.Group("/blog")
	blogGroup.POST("", blogHandler.SaveBlog)
	blogGroup.PUT("/like/:id", blogHandler.LikeBlog)
	blogGroup.GET("/:id", blogHandler.QueryBlogByID)
	blogGroup.GET("/likes/:id", blogHandler.QueryBlogLikes)
	blogGroup.GET("/of/me", blogHandler.QueryMyBlog)
	blogGroup.GET("/of/user", blogHandler.QueryBlogOfUser)
	blogGroup.GET("/of/follow", blogHandler.QueryFollowFeed)
	blogGroup.GET("/hot", blogHandler.QueryHotBlog)

	uploadGroup := engine.Group("/upload")
	uploadGroup.POST("/blog", uploadHandler.UploadImage)
	uploadGroup.GET("/blog/delete", uploadHandler.DeleteBlogImage)

	userGroup := engine.Group("/user")
	userGroup.POST("/code", userHandler.SendCode)
	userGroup.POST("/login", userHandler.Login)
	userGroup.POST("/logout", userHandler.Logout)
	userGroup.GET("/me", userHandler.Me)
	userGroup.GET("/info/:id", userHandler.Info)
	userGroup.GET("/:id", userHandler.GetUserByID)
	userGroup.POST("/sign", userHandler.Sign)
	userGroup.GET("/sign/count", userHandler.SignCount)

	followGroup := engine.Group("/follow")
	followGroup.PUT("/:id/:follow", followHandler.Follow) // follow=true 关注，false 取关
	followGroup.GET("/or/not/:id", followHandler.IsFollowed)
	followGroup.GET("/common/:id", followHandler.CommonFollow)

	voucherOrderGroup := engine.Group("/voucher-order")
	voucherOrderGroup.POST("/seckill/:id", voucherOrderHandler.SeckillVoucher)

}
