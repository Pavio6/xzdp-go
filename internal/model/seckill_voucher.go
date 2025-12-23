package model

import "time"

// SeckillVoucher mirrors tb_seckill_voucher.
type SeckillVoucher struct {
	VoucherID  int64     `gorm:"column:voucher_id;primaryKey" json:"voucherId"`
	Stock      int       `gorm:"column:stock" json:"stock"`
	CreateTime time.Time `gorm:"column:create_time;autoCreateTime" json:"createTime"`
	BeginTime  time.Time `gorm:"column:begin_time" json:"beginTime"`
	EndTime    time.Time `gorm:"column:end_time" json:"endTime"`
	UpdateTime time.Time `gorm:"column:update_time;autoUpdateTime" json:"updateTime"`
}

func (SeckillVoucher) TableName() string { return "tb_seckill_voucher" }
