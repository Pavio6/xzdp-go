package service

import (
	"context"
	"time"

	"gorm.io/gorm"

	"hmdp-backend/internal/model"
)

// VoucherService 处理普通券与秒杀券逻辑
type VoucherService struct {
	db         *gorm.DB
	seckillSvc *SeckillVoucherService
}

// VoucherWithSeckill 用于返回携带秒杀信息的券
type VoucherWithSeckill struct {
	ID          int64      `gorm:"column:id" json:"id"`
	ShopID      int64      `gorm:"column:shop_id" json:"shopId"`
	Title       string     `gorm:"column:title" json:"title"`
	SubTitle    string     `gorm:"column:sub_title" json:"subTitle"`
	Rules       string     `gorm:"column:rules" json:"rules"`
	PayValue    int64      `gorm:"column:pay_value" json:"payValue"`
	ActualValue int64      `gorm:"column:actual_value" json:"actualValue"`
	Type        int        `gorm:"column:type" json:"type"`
	Status      int        `gorm:"column:status" json:"status"`
	CreateTime  time.Time  `gorm:"column:create_time" json:"createTime"`
	UpdateTime  time.Time  `gorm:"column:update_time" json:"updateTime"`
	Stock       *int       `gorm:"column:stock" json:"stock,omitempty"`
	BeginTime   *time.Time `gorm:"column:begin_time" json:"beginTime,omitempty"`
	EndTime     *time.Time `gorm:"column:end_time" json:"endTime,omitempty"`
}

// NewVoucherService 创建 VoucherService 实例
func NewVoucherService(db *gorm.DB, seckillSvc *SeckillVoucherService) *VoucherService {
	return &VoucherService{db: db, seckillSvc: seckillSvc}
}

func (s *VoucherService) Create(ctx context.Context, voucher *model.Voucher) error {
	return s.db.WithContext(ctx).Create(voucher).Error
}

func (s *VoucherService) QueryVoucherOfShop(ctx context.Context, shopID int64) ([]VoucherWithSeckill, error) {
	var vouchers []VoucherWithSeckill
	query := `
        SELECT v.id, v.shop_id, v.title, v.sub_title, v.rules, v.pay_value,
               v.actual_value, v.type, v.status, v.create_time, v.update_time,
               sv.stock, sv.begin_time, sv.end_time
        FROM tb_voucher v
        LEFT JOIN tb_seckill_voucher sv ON v.id = sv.voucher_id
        WHERE v.shop_id = ? AND v.status = 1`
	err := s.db.WithContext(ctx).Raw(query, shopID).Scan(&vouchers).Error
	return vouchers, err
}

func (s *VoucherService) AddSeckillVoucher(ctx context.Context, voucher *model.Voucher) error {
	if err := s.Create(ctx, voucher); err != nil {
		return err
	}
	stock := 0
	if voucher.Stock != nil {
		stock = *voucher.Stock
	}
	var begin time.Time
	if voucher.BeginTime != nil {
		begin = *voucher.BeginTime
	}
	var end time.Time
	if voucher.EndTime != nil {
		end = *voucher.EndTime
	}
	sec := &model.SeckillVoucher{
		VoucherID: voucher.ID,
		Stock:     stock,
		BeginTime: begin,
		EndTime:   end,
	}
	return s.seckillSvc.Create(ctx, sec)
}
