package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"

	"hmdp-backend/internal/config"
	"hmdp-backend/internal/data"
	"hmdp-backend/internal/utils"
)

// TestPreheatShopBloom 将商铺 ID 1~14 写入布隆过滤器
func TestPreheatShopBloom(t *testing.T) {
	ctx := context.Background()

	cfgPath := os.Getenv("CONFIG_PATH")
	if cfgPath == "" {
		cfgPath = filepath.Join("..", "..", "configs", "app.yaml")
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	rdb := data.NewRedis(cfg.Redis)
	defer rdb.Close()

	svc := NewShopService(nil, rdb, zap.NewNop())
	for id := int64(1); id <= 14; id++ {
		if err := svc.bloomAdd(ctx, utils.SHOP_BLOOM_KEY, id); err != nil {
			t.Fatalf("bloom add id=%d: %v", id, err)
		}
	}

	t.Log("preheated bloom filter with ids 1..14")
}
