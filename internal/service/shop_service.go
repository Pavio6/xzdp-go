package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"hmdp-backend/internal/model"
	"hmdp-backend/internal/utils"
)

const lockRetryDelay = 50 * time.Millisecond // 拿不到互斥锁时的短暂休眠时间，避免热点击穿

// ShopService 处理商铺相关业务逻辑
type ShopService struct {
	db  *gorm.DB
	rdb *redis.Client
	log *zap.Logger
}

// NewShopService 创建 ShopService 实例
func NewShopService(db *gorm.DB, rdb *redis.Client, log *zap.Logger) *ShopService {
	return &ShopService{db: db, rdb: rdb, log: log}
}

// GetByID 根据shopId获取shop信息 - 使用互斥锁解决缓存击穿问题
func (s *ShopService) GetByID(ctx context.Context, id int64) (*model.Shop, error) {
	key := utils.CACHE_SHOP_KEY + strconv.FormatInt(id, 10)
	lockKey := utils.LOCK_SHOP_KEY + strconv.FormatInt(id, 10)

	for {
		// 1.从 Redis 查询商铺缓存
		cached, err := s.rdb.Get(ctx, key).Result()
		if err == nil {
			// 这里是防止缓存穿透而将空值放到了redis中
			if cached == "" {
				return nil, errors.New("shop not found")
			}
			var shop model.Shop
			if unmarshalErr := json.Unmarshal([]byte(cached), &shop); unmarshalErr != nil {
				return nil, unmarshalErr
			}
			return &shop, nil
		}
		if !errors.Is(err, redis.Nil) {
			return nil, err
		}

		// 2.缓存未命中，尝试获取互斥锁；若失败则短暂休眠后重试，避免热点 Key 的缓存击穿
		locked, lockErr := s.tryLock(ctx, lockKey)
		if lockErr != nil {
			return nil, lockErr
		}
		// 获取锁失败，继续循环等待
		if !locked {
			time.Sleep(lockRetryDelay)
			continue
		}
		// DoubleCheck 拿到锁后再次查询缓存，因为在获取锁的时候 可能其他协程已经把缓存写入了
		// 这样避免重复查询数据库和写缓存
		cached, err = s.rdb.Get(ctx, key).Result()
		if err == nil {
			var shop model.Shop
			if cached == "" {
				return nil, errors.New("shop not found")
			}
			if unmarshalErr := json.Unmarshal([]byte(cached), &shop); unmarshalErr != nil {
				return nil, unmarshalErr
			}
			_ = s.unlock(ctx, lockKey)
			return &shop, nil
		}
		if !errors.Is(err, redis.Nil) {
			_ = s.unlock(ctx, lockKey)
			return nil, err
		}

		// 3.成功获取锁且缓存仍未构建，查询数据库并回填缓存，最后释放互斥锁
		shop, loadErr := s.loadShopAndCache(ctx, id, key)
		_ = s.unlock(ctx, lockKey)
		return shop, loadErr
	}
}

// GetByIDWithLogicalExpire 根据shopId获取shop信息 - 使用逻辑过期时间解决热点 Key 缓存击穿
// 逻辑过期前提是：Redis 里必须有旧值可以返回
func (s *ShopService) GetByIDWithLogicalExpire(ctx context.Context, id int64) (*model.Shop, error) {
	key := utils.CACHE_SHOP_KEY + strconv.FormatInt(id, 10)
	lockKey := utils.LOCK_SHOP_KEY + strconv.FormatInt(id, 10)

	// 1.从 Redis 查询缓存，未命中则直接返回空
	cached, err := s.rdb.Get(ctx, key).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, nil
		}
		return nil, err
	}
	if cached == "" {
		return nil, nil
	}

	// 2.反序列化逻辑过期包装数据
	var redisData utils.RedisData
	// 先将redis中数据反序列化为redisData
	if unmarshalErr := json.Unmarshal([]byte(cached), &redisData); unmarshalErr != nil {
		return nil, unmarshalErr
	}
	// redisData.Data 是 interface{}，先二次序列化为 JSON 再反序列化成 Shop
	dataBytes, marshalErr := json.Marshal(redisData.Data)
	if marshalErr != nil {
		return nil, marshalErr
	}
	var shop model.Shop
	// 将通用 data 还原为具体的 Shop 结构
	if unmarshalErr := json.Unmarshal(dataBytes, &shop); unmarshalErr != nil {
		return nil, unmarshalErr
	}

	// 3.未过期，直接返回商铺信息
	if redisData.ExpireTime.After(time.Now()) {
		return &shop, nil
	}

	// 4.已过期：尝试获取互斥锁，获取失败直接返回旧数据
	locked, lockErr := s.tryLock(ctx, lockKey)
	if lockErr != nil {
		return nil, lockErr
	}
	// 失败 直接返回redis中的旧数据
	if !locked {
		return &shop, nil
	}

	// 5.获取锁成功：异步重建缓存，避免阻塞当前请求
	go func() {
		defer func() {
			_ = s.unlock(context.Background(), lockKey)
		}()
		_ = s.rebuildShopCacheWithLogicalExpire(id, key)
	}()
	// 先返回旧数据
	return &shop, nil
}

// loadShopAndCache 查询数据库并将结果写入 Redis，配合互斥锁使用
func (s *ShopService) loadShopAndCache(ctx context.Context, id int64, key string) (*model.Shop, error) {
	var shop model.Shop
	err := s.db.WithContext(ctx).First(&shop, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		// 写入空值，防止缓存穿透
		_ = s.rdb.Set(ctx, key, "", time.Duration(utils.CACHE_NULL_TTL)*time.Minute).Err()
		return nil, errors.New("shop not found")
	}
	if err != nil {
		return nil, err
	}

	data, err := json.Marshal(&shop)
	if err != nil {
		return nil, err
	}
	// 使用string类型缓存商铺信息 添加上过期时间
	if err := s.rdb.Set(ctx, key, data, time.Duration(utils.CACHE_SHOP_TTL)*time.Minute).Err(); err != nil {
		return nil, err
	}
	return &shop, nil
}

// rebuildShopCacheWithLogicalExpire 查询数据库并写入逻辑过期缓存
func (s *ShopService) rebuildShopCacheWithLogicalExpire(id int64, key string) error {
	var shop model.Shop
	err := s.db.WithContext(context.Background()).First(&shop, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	// 测试场景：逻辑过期时间先调小到 200ms，便于快速验证重建
	return s.saveShopWithLogicalExpire(key, &shop, time.Duration(utils.CACHE_SHOP_TTL)*time.Minute)
}

// saveShopWithLogicalExpire 将数据和逻辑过期时间一起写入 Redis
func (s *ShopService) saveShopWithLogicalExpire(key string, shop *model.Shop, ttl time.Duration) error {
	redisData := utils.RedisData{
		ExpireTime: time.Now().Add(ttl),
		Data:       shop,
	}
	data, err := json.Marshal(redisData)
	if err != nil {
		return err
	}
	// 逻辑过期不依赖 Redis TTL，这里不设置过期时间
	return s.rdb.Set(context.Background(), key, data, 0).Err()
}

// tryLock 尝试获取锁
func (s *ShopService) tryLock(ctx context.Context, key string) (bool, error) {
	// 利用 Redis SETNX 实现简单互斥锁，并设置 TTL 防止死锁
	return s.rdb.SetNX(ctx, key, "1", time.Duration(utils.LOCK_SHOP_TTL)*time.Second).Result()
}

// unlock 释放锁
func (s *ShopService) unlock(ctx context.Context, key string) error {
	return s.rdb.Del(ctx, key).Err()
}

func (s *ShopService) Create(ctx context.Context, shop *model.Shop) error {
	return s.db.WithContext(ctx).Create(shop).Error
}

// Update 更新商铺信息
func (s *ShopService) Update(ctx context.Context, shop *model.Shop) error {
	if shop == nil || shop.ID == 0 {
		return errors.New("invalid shop id")
	}
	key := utils.CACHE_SHOP_KEY + strconv.FormatInt(shop.ID, 10)
	// 通过事务保证先更新数据库再删除缓存，出现错误时整体回滚
	// 更新操作 先更新数据库 删除redis缓存 保证redis和数据库数据一致性
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 使用 Updates 忽略零值字段，避免覆盖 create_time 等只读列
		if err := tx.Model(&model.Shop{ID: shop.ID}).Updates(shop).Error; err != nil {
			return err
		}
		if err := s.rdb.Del(ctx, key).Err(); err != nil {
			return err
		}
		return nil
	})
}

func (s *ShopService) QueryByType(ctx context.Context, typeID int64, page, size int) ([]model.Shop, error) {
	var shops []model.Shop
	offset := (page - 1) * size
	if offset < 0 {
		offset = 0
	}
	err := s.db.WithContext(ctx).
		Where("type_id = ?", typeID).
		Offset(offset).
		Limit(size).
		Order("id ASC").
		Find(&shops).Error
	return shops, err
}

func (s *ShopService) QueryByName(ctx context.Context, name string, page, size int) ([]model.Shop, error) {
	var shops []model.Shop
	offset := (page - 1) * size
	if offset < 0 {
		offset = 0
	}
	query := s.db.WithContext(ctx)
	if name != "" {
		query = query.Where("name LIKE ?", "%%"+name+"%%")
	}
	err := query.Order("id ASC").Offset(offset).Limit(size).Find(&shops).Error
	return shops, err
}

// QueryByTypeWithLocation 根据类型 + 坐标查询店铺，按距离排序
// x、y 为用户经纬度，page/size 用于分页，优先使用 Redis GEO，缺少坐标时可退回 QueryByType。
func (s *ShopService) QueryByTypeWithLocation(ctx context.Context, typeID int64, page, size int, x, y float64) ([]model.Shop, error) {
	if page <= 0 {
		page = 1
	}
	if size <= 0 {
		size = utils.DEFAULT_PAGE_SIZE
	}
	// page=1时 start=0 end=5  0~4
	// page=2时 start=5 end=10 5~9
	start := (page - 1) * size
	end := page * size
	key := utils.SHOP_GEO_KEY + strconv.FormatInt(typeID, 10)

	// 直接使用 GEOSEARCH，COUNT 使用 end
	query := &redis.GeoSearchLocationQuery{
		GeoSearchQuery: redis.GeoSearchQuery{
			Longitude:  x,
			Latitude:   y,
			Radius:     20000,
			RadiusUnit: "m",
			Sort:       "ASC", // 距离升序
			Count:      end,   // 取到当前页末尾
		},
		WithDist:  true, // 需要距离信息
		WithCoord: true, // 返回坐标
	}
	locs, err := s.rdb.GeoSearchLocation(ctx, key, query).Result()
	if err != nil {
		return nil, err
	}
	if s.log != nil {
		raw := make([]string, 0, len(locs))
		for i, loc := range locs {
			raw = append(raw, fmt.Sprintf("%d:%s:%.2f", i, loc.Name, loc.Dist))
		}
		s.log.Sugar().Infow("geo search raw", "page", page, "start", start, "end", end, "count", len(locs), "raw", raw)
	}
	if len(locs) <= start {
		return []model.Shop{}, nil
	}
	if len(locs) > end {
		locs = locs[:end]
	}
	locs = locs[start:]

	// 取出 shopIds，按顺序回表查询并带回距离
	ids := make([]int64, 0, len(locs))
	for _, loc := range locs {
		id, parseErr := strconv.ParseInt(loc.Name, 10, 64)
		if parseErr != nil {
			return nil, parseErr
		}
		ids = append(ids, id)
	}

	var shops []model.Shop
	if err := s.db.WithContext(ctx).Where("id IN ?", ids).Find(&shops).Error; err != nil {
		return nil, err
	}
	shopMap := make(map[int64]model.Shop, len(shops))
	for _, shop := range shops {
		shopMap[shop.ID] = shop
	}

	// 按 GEO 结果的顺序输出，并附上距离（单位米）
	res := make([]model.Shop, 0, len(ids))
	for _, loc := range locs {
		id, _ := strconv.ParseInt(loc.Name, 10, 64)
		if shop, ok := shopMap[id]; ok {
			dist := loc.Dist
			shop.Distance = &dist
			res = append(res, shop)
		}
	}
	return res, nil
}
