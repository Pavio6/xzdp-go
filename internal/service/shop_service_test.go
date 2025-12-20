package service

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"hmdp-backend/internal/config"
	"hmdp-backend/internal/data"
	"hmdp-backend/internal/model"
	"hmdp-backend/internal/utils"
	"hmdp-backend/pkg/logger"
)

// TestWarmShopCacheWithLogicalExpire 测试方法：将指定 ID 的商铺写入 Redis 并设置逻辑过期时间
func TestWarmShopCacheWithLogicalExpire(t *testing.T) {

	cfgPath := os.Getenv("CONFIG_PATH")
	if cfgPath == "" {
		cfgPath = filepath.Join("..", "..", "configs", "app.yaml")
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	log, err := logger.New(cfg.Logging.Level)
	if err != nil {
		t.Fatalf("init logger: %v", err)
	}
	db, err := data.NewMySQL(cfg.MySQL, log)
	if err != nil {
		t.Fatalf("init mysql: %v", err)
	}
	rdb := data.NewRedis(cfg.Redis)

	shopID := int64(1)
	if envID := os.Getenv("SHOP_ID"); envID != "" {
		parsed, parseErr := strconv.ParseInt(envID, 10, 64)
		if parseErr != nil {
			t.Fatalf("invalid SHOP_ID: %v", parseErr)
		}
		shopID = parsed
	}

	svc := NewShopService(db, rdb)
	key := utils.CACHE_SHOP_KEY + strconv.FormatInt(shopID, 10)
	var shop model.Shop
	if err := db.WithContext(context.Background()).First(&shop, shopID).Error; err != nil {
		t.Fatalf("query shop: %v", err)
	}
	if err := svc.saveShopWithLogicalExpire(key, &shop, time.Duration(utils.CACHE_SHOP_TTL)*time.Minute); err != nil {
		t.Fatalf("save logical expire cache: %v", err)
	}
}
