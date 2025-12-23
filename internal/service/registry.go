package service

import (
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

// Registry 聚合全部业务 Service，方便注入 handler
type Registry struct {
	Blog           *BlogService
	Shop           *ShopService
	ShopType       *ShopTypeService
	Voucher        *VoucherService
	SeckillVoucher *SeckillVoucherService
	User           *UserService
	VoucherOrder   *VoucherOrderService
}

// NewRegistry 使用共享 DB 与 Redis 构建所有服务
func NewRegistry(db *gorm.DB, rdb *redis.Client) *Registry {
	seckillSvc := NewSeckillVoucherService(db)
	return &Registry{
		Blog:           NewBlogService(db),
		Shop:           NewShopService(db, rdb),
		ShopType:       NewShopTypeService(db, rdb),
		Voucher:        NewVoucherService(db, seckillSvc),
		SeckillVoucher: seckillSvc,
		User:           NewUserService(db, rdb),
		VoucherOrder:   NewVoucherOrderService(db, rdb),
	}
}
