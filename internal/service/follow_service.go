package service

import (
	"context"
	"fmt"
	"strconv"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"hmdp-backend/internal/model"
)

// FollowService 关注相关业务
type FollowService struct {
	db  *gorm.DB
	rdb *redis.Client
}

func NewFollowService(db *gorm.DB, rdb *redis.Client) *FollowService {
	return &FollowService{db: db, rdb: rdb}
}

// Follow 关注或取关 targetID
func (s *FollowService) Follow(ctx context.Context, userID, targetID int64, follow bool) error {
	if userID == targetID {
		return nil
	}
	key := followKey(userID)
	if follow {
		record := &model.Follow{
			UserID:       userID,
			FollowUserID: targetID,
		}
		if err := s.db.WithContext(ctx).Create(record).Error; err != nil {
			return err
		}
		// 将关注关系写入 Redis Set，便于求交集
		return s.rdb.SAdd(ctx, key, targetID).Err()
	}
	// 取关
	if err := s.db.WithContext(ctx).
		Where("user_id = ? AND follow_user_id = ?", userID, targetID).
		Delete(&model.Follow{}).Error; err != nil {
		return err
	}
	return s.rdb.SRem(ctx, key, targetID).Err()
}

// IsFollowing 查询 userID 是否已关注 targetID
func (s *FollowService) IsFollowing(ctx context.Context, userID, targetID int64) (bool, error) {
	var count int64
	err := s.db.WithContext(ctx).
		Model(&model.Follow{}).
		Where("user_id = ? AND follow_user_id = ?", userID, targetID).
		Count(&count).Error
	return count > 0, err
}

// CommonFollowIDs 求 userID 与 targetID 的共同关注用户ID列表（Redis SINTER）
func (s *FollowService) CommonFollowIDs(ctx context.Context, userID, targetID int64) ([]int64, error) {
	if userID == targetID {
		return nil, nil
	}
	res, err := s.rdb.SInter(ctx, followKey(userID), followKey(targetID)).Result()
	if err != nil {
		return nil, err
	}
	var ids []int64
	for _, v := range res {
		if id, convErr := toInt64(v); convErr == nil {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func followKey(userID int64) string {
	return fmt.Sprintf("follow:%d", userID)
}

func toInt64(s string) (int64, error) {
	return strconv.ParseInt(s, 10, 64)
}
