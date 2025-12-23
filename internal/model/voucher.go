package model

import "time"

// Voucher mirrors tb_voucher.
type Voucher struct {
	ID          int64      `gorm:"column:id;primaryKey;autoIncrement" json:"id"`
	ShopID      int64      `gorm:"column:shop_id" json:"shopId"`
	Title       string     `gorm:"column:title" json:"title"`
	SubTitle    string     `gorm:"column:sub_title" json:"subTitle"`
	Rules       string     `gorm:"column:rules" json:"rules"`
	PayValue    int64      `gorm:"column:pay_value" json:"payValue"`
	ActualValue int64      `gorm:"column:actual_value" json:"actualValue"`
	Type        int        `gorm:"column:type" json:"type"`
	Status      int        `gorm:"column:status" json:"status"`
	CreateTime  time.Time  `gorm:"column:create_time;autoCreateTime" json:"createTime"`
	UpdateTime  time.Time  `gorm:"column:update_time;autoUpdateTime" json:"updateTime"`
	Stock       *int       `gorm:"-" json:"stock,omitempty"`
	BeginTime   *time.Time `gorm:"-" json:"beginTime,omitempty"`
	EndTime     *time.Time `gorm:"-" json:"endTime,omitempty"`
}

func (Voucher) TableName() string { return "tb_voucher" }
