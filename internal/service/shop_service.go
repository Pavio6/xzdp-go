package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"strconv"
	"time"

	"github.com/allegro/bigcache/v3"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"hmdp-backend/internal/model"
	"hmdp-backend/internal/utils"
)

const lockRetryDelay = 50 * time.Millisecond // 拿不到互斥锁时的短暂休眠时间，避免热点击穿
const shopBloomSize = 1 << 20                // 约 1M 位布隆过滤器
var shopBloomSeeds = []uint32{17, 29, 37}    // 简单多哈希种子
const defaultLocalShopCacheTTL = 30 * time.Second

// ShopService 处理商铺相关业务逻辑
type ShopService struct {
	db         *gorm.DB
	rdb        *redis.Client
	log        *zap.Logger
	localCache *bigcache.BigCache
}

// NewShopService 创建 ShopService 实例
func NewShopService(db *gorm.DB, rdb *redis.Client, log *zap.Logger) *ShopService {
	cache := initShopLocalCache(log)
	return &ShopService{db: db, rdb: rdb, log: log, localCache: cache}
}

// GetByID 根据shopId获取shop信息 - 使用互斥锁解决缓存击穿问题
func (s *ShopService) GetByID(ctx context.Context, id int64) (*model.Shop, error) {
	key := utils.CACHE_SHOP_KEY + strconv.FormatInt(id, 10)
	lockKey := utils.LOCK_SHOP_KEY + strconv.FormatInt(id, 10)

	if shop, ok := s.getLocalShop(key); ok {
		if s.log != nil {
			s.log.Info("shop cache hit (local)", zap.Int64("shopId", id))
		}
		return shop, nil
	}

	for {
		// 1.从 Redis 查询商铺缓存
		cached, err := s.rdb.Get(ctx, key).Result()
		// err为空 说明查询到了缓存
		if err == nil {
			var shop model.Shop
			if unmarshalErr := json.Unmarshal([]byte(cached), &shop); unmarshalErr != nil {
				return nil, unmarshalErr
			}
			// 设置本地缓存
			s.setLocalShop(key, []byte(cached))
			return &shop, nil
		}
		// 如果err不是redis.Nil，说明是其他错误，直接返回
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
			if unmarshalErr := json.Unmarshal([]byte(cached), &shop); unmarshalErr != nil {
				return nil, unmarshalErr
			}
			s.setLocalShop(key, []byte(cached))
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
// 启动或定时预先将热点数据加载到 Redis，并设置逻辑过期时间
// 自动兜底方案：也可以在未命中时查询数据库并写入逻辑过期缓存
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

// GetByIDWithBloom 使用布隆过滤器先拦截不存在的 ID，降低缓存穿透风险
// Bloom 判定“可能存在”才继续后续缓存/数据库流程；判定“不存在”直接返回 nil
func (s *ShopService) GetByIDWithBloom(ctx context.Context, id int64) (*model.Shop, error) {
	bloomKey := utils.SHOP_BLOOM_KEY
	maybe, err := s.bloomMightContain(ctx, bloomKey, id)
	if err != nil {
		return nil, err
	}
	if !maybe {
		// 布隆认为不存在，直接返回 nil，避免穿透
		return nil, nil
	}

	shop, err := s.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return shop, nil
}

// loadShopAndCache 查询数据库并将结果写入 Redis，配合互斥锁使用
func (s *ShopService) loadShopAndCache(ctx context.Context, id int64, key string) (*model.Shop, error) {
	var shop model.Shop
	err := s.db.WithContext(ctx).First(&shop, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
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
	// 设置本地缓存
	s.setLocalShop(key, data)
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
		s.deleteLocalShop(key)
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

// bloomMightContain 检查布隆过滤器是否可能包含该 ID
func (s *ShopService) bloomMightContain(ctx context.Context, key string, id int64) (bool, error) {
	// 获取该id对应的多个位偏移
	offsets := bloomOffsets(id)
	// 检查这些偏移位是否都为1
	for _, off := range offsets {
		bit, err := s.rdb.GetBit(ctx, key, int64(off)).Result()
		if err != nil {
			return false, err
		}
		if bit == 0 {
			return false, nil
		}
	}
	return true, nil
}

// bloomAdd 将 ID 写入布隆过滤器
func (s *ShopService) bloomAdd(ctx context.Context, key string, id int64) error {
	offsets := bloomOffsets(id)
	// 使用管道批量写位 减少往返
	pipe := s.rdb.Pipeline()
	for _, off := range offsets {
		// 将对应偏移的位设置为1
		pipe.SetBit(ctx, key, int64(off), 1)
	}
	// 提交管道命令
	_, err := pipe.Exec(ctx)
	return err
}

// bloomOffsets 生成布隆过滤器位偏移
func bloomOffsets(id int64) []uint32 {
	res := make([]uint32, 0, len(shopBloomSeeds))
	// 遍历每个哈希种子 生成多组哈希
	for _, seed := range shopBloomSeeds {
		// 创建 FNV-1a 哈希算法实例
		h := fnv.New32a()
		// 将id转为字符串写入哈希计算
		_, _ = h.Write([]byte(strconv.FormatInt(id, 10)))
		// 返回哈希值加上种子后的偏移位置
		sum := h.Sum32() + seed
		res = append(res, sum%shopBloomSize)
	}
	return res
}
// initShopLocalCache 初始化本地缓存
func initShopLocalCache(log *zap.Logger) *bigcache.BigCache {
	// 设置本地缓存的默认 TTL，并使用清理窗口控制过期扫描频率
	ttl := localShopCacheTTL()
	config := bigcache.DefaultConfig(ttl)
	if ttl > 0 {
		// 清理窗口设为 TTL 的一半，降低过期键清理的抖动
		config.CleanWindow = ttl / 2
	}
	cache, err := bigcache.New(context.Background(), config)
	if err != nil && log != nil {
		log.Warn("init shop local cache failed", zap.Error(err))
		return nil
	}
	return cache
}
// localShopCacheTTL 获取本地缓存 TTL 支持通过环境变量配置
func localShopCacheTTL() time.Duration {
	if raw := os.Getenv("SHOP_LOCAL_CACHE_TTL"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil {
			return d
		}
	}
	return defaultLocalShopCacheTTL
}
// getLocalShop 从本地缓存获取店铺信息
func (s *ShopService) getLocalShop(key string) (*model.Shop, bool) {
	if s.localCache == nil {
		return nil, false
	}
	data, err := s.localCache.Get(key)
	if err != nil {
		return nil, false
	}
	var shop model.Shop
	if unmarshalErr := json.Unmarshal(data, &shop); unmarshalErr != nil {
		s.localCache.Delete(key)
		return nil, false
	}
	return &shop, true
}
// setLocalShop 将店铺信息存入本地缓存
func (s *ShopService) setLocalShop(key string, data []byte) {
	if s.localCache == nil || len(data) == 0 {
		return
	}
	_ = s.localCache.Set(key, data)
	if s.log != nil {
		s.log.Info("shop cache set (local)", zap.String("key", key))
	}
}
// deleteLocalShop 从本地缓存删除店铺信息
func (s *ShopService) deleteLocalShop(key string) {
	if s.localCache == nil {
		return
	}
	s.localCache.Delete(key)
	if s.log != nil {
		s.log.Info("shop cache delete (local)", zap.String("key", key))
	}
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

	// 按 GEO 结果的顺序输出，并附上距离
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
