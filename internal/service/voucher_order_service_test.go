package service

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"hmdp-backend/internal/model"

	"github.com/redis/go-redis/v9"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// TestSeckillNoOversell 并发 200 次秒杀请求，验证不会超卖
func TestSeckillNoOversell(t *testing.T) {
	ctx := context.Background()

	dsn := os.Getenv("TEST_DSN")
	if dsn == "" {
		dsn = "root:root@tcp(127.0.0.1:3306)/hmdp?parseTime=true&loc=Local&charset=utf8mb4"
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Skipf("skip: cannot connect mysql: %v", err)
	}
	sqlDB, err := db.DB()
	if err == nil {
		// 控制连接数，避免并发压测触发 MySQL max_connections
		sqlDB.SetMaxOpenConns(50)
		sqlDB.SetMaxIdleConns(10)
		defer sqlDB.Close()
	}

	rdb := redis.NewClient(&redis.Options{
		Addr: "127.0.0.1:6379",
		DB:   0,
	})
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("skip: cannot connect redis: %v", err)
	}
	defer rdb.Close()

	svc := NewVoucherOrderService(db, rdb)

	// 使用现有的券 ID
	const voucherID = int64(12)

	begin := time.Now().Add(-time.Minute)
	end := time.Now().Add(5 * time.Minute)
	const stock = 100

	// 调整秒杀信息到可用窗口并重置库存
	if err := db.WithContext(ctx).Model(&model.SeckillVoucher{}).
		Where("voucher_id = ?", voucherID).
		Updates(map[string]interface{}{
			"stock":       stock,
			"begin_time":  begin,
			"end_time":    end,
			"update_time": time.Now(),
		}).Error; err != nil {
		t.Fatalf("prepare seckill voucher: %v", err)
	}

	// 并发请求
	const workers = 200
	var wg sync.WaitGroup
	var success int64

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			// 每个请求使用不同的 userId，避免潜在的唯一约束
			userID := int64(1000 + idx)
			if _, err := svc.Seckill(ctx, voucherID, userID); err == nil {
				// 原子自增计数
				atomic.AddInt64(&success, 1)
			}
		}(i)
	}
	wg.Wait()

	// 查询最终库存
	var left int
	if err := db.Raw("SELECT stock FROM tb_seckill_voucher WHERE voucher_id = ?", voucherID).Scan(&left).Error; err != nil {
		t.Fatalf("query stock: %v", err)
	}

	if success > stock {
		t.Fatalf("oversold: success %d > stock %d", success, stock)
	}
	if left < 0 {
		t.Fatalf("stock negative: %d", left)
	}
	t.Logf("success=%d, left=%d", success, left)

}

// TestSeckillOneOrderPerUser 同一用户尝试多次下单，仅成功一次（依赖已有 voucher_id=12）
func TestSeckillOneOrderPerUser(t *testing.T) {
	ctx := context.Background()

	dsn := os.Getenv("TEST_DSN")
	if dsn == "" {
		dsn = "root:root@tcp(127.0.0.1:3306)/hmdp?parseTime=true&loc=Local&charset=utf8mb4"
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Skipf("skip: cannot connect mysql: %v", err)
	}
	sqlDB, err := db.DB()
	if err == nil {
		sqlDB.SetMaxOpenConns(50)
		sqlDB.SetMaxIdleConns(10)
		defer sqlDB.Close()
	}

	rdb := redis.NewClient(&redis.Options{
		Addr: "127.0.0.1:6379",
		DB:   0,
	})
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("skip: cannot connect redis: %v", err)
	}
	defer rdb.Close()

	svc := NewVoucherOrderService(db, rdb)

	const voucherID = int64(12)

	// 同一用户并发多次下单
	const attempts = 200
	var wg sync.WaitGroup
	var success int64
	userID := int64(1)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := svc.Seckill(ctx, voucherID, userID); err == nil {
				atomic.AddInt64(&success, 1)
			}
		}()
	}
	wg.Wait()

	// 校验仅成功一次
	if success != 1 {
		t.Fatalf("expected 1 success for single user, got %d", success)
	}

	// 校验订单表只有一条记录
	var orderCount int64
	if err := db.WithContext(ctx).Model(&model.VoucherOrder{}).
		Where("voucher_id = ? AND user_id = ?", voucherID, userID).
		Count(&orderCount).Error; err != nil {
		t.Fatalf("count orders: %v", err)
	}
	if orderCount != 1 {
		t.Fatalf("expected 1 order record, got %d", orderCount)
	}

	t.Logf("single user attempts=%d, success=%d for voucher %d", attempts, success, voucherID)
}
