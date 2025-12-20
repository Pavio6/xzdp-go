package service

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"hmdp-backend/internal/model"
	"hmdp-backend/internal/utils"
)

type ShopTypeService struct {
	db  *gorm.DB
	rdb *redis.Client
}

func NewShopTypeService(db *gorm.DB, rdb *redis.Client) *ShopTypeService {
	return &ShopTypeService{db: db, rdb: rdb}
}

func (s *ShopTypeService) List(ctx context.Context) ([]model.ShopType, error) {
	key := utils.CACHE_SHOP_TYPE_KEY
	cached, err := s.rdb.Get(ctx, key).Result()
	if err == nil {
		var types []model.ShopType
		if unmarshalErr := json.Unmarshal([]byte(cached), &types); unmarshalErr != nil {
			return nil, unmarshalErr
		}
		return types, nil
	}
	if !errors.Is(err, redis.Nil) {
		return nil, err
	}

	var types []model.ShopType
	err = s.db.WithContext(ctx).
		Order("sort ASC").
		Find(&types).Error
	if err != nil {
		return nil, err
	}

	data, err := json.Marshal(types)
	if err != nil {
		return nil, err
	}
	if cacheErr := s.rdb.Set(ctx, key, data, 0).Err(); cacheErr != nil {
		return nil, cacheErr
	}
	return types, nil
}
