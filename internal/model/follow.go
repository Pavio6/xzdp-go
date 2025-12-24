package model

import "time"

// Follow mirrors tb_follow.
type Follow struct {
	ID           int64     `gorm:"column:id;primaryKey;autoIncrement" json:"id"`
	UserID       int64     `gorm:"column:user_id" json:"userId"`
	FollowUserID int64     `gorm:"column:follow_user_id" json:"followUserId"`
	CreateTime   time.Time `gorm:"column:create_time;autoCreateTime" json:"createTime"`
}

func (Follow) TableName() string { return "tb_follow" }
