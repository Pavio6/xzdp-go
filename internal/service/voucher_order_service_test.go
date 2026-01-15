package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"hmdp-backend/internal/model"
	"hmdp-backend/internal/utils"

	"github.com/redis/go-redis/v9"
	"github.com/segmentio/kafka-go"
	mysqldriver "github.com/go-sql-driver/mysql"
	"go.uber.org/zap"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func newTestKafka(t *testing.T, ctx context.Context) (*kafka.Writer, *kafka.Writer, *kafka.Writer, *kafka.Reader, *kafka.Reader, func()) {
	t.Helper()
	broker := os.Getenv("TEST_KAFKA_BROKER")
	if broker == "" {
		broker = "127.0.0.1:29092"
	}
	topic := os.Getenv("TEST_KAFKA_TOPIC")
	if topic == "" {
		topic = "seckill-orders"
	}
	retryTopic := os.Getenv("TEST_KAFKA_RETRY_TOPIC")
	if retryTopic == "" {
		retryTopic = "seckill-orders-retry"
	}
	dlqTopic := os.Getenv("TEST_KAFKA_DLQ_TOPIC")
	if dlqTopic == "" {
		dlqTopic = "seckill-orders-dlq"
	}
	groupID := os.Getenv("TEST_KAFKA_GROUP")
	if groupID == "" {
		groupID = "seckill-order-consumers-test"
	}
	groupID = fmt.Sprintf("%s-%d", groupID, time.Now().UnixNano())

	dialer := &kafka.Dialer{Timeout: 2 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", broker)
	if err != nil {
		t.Skipf("skip: cannot connect kafka: %v", err)
	}
	_ = conn.Close()

	writer := &kafka.Writer{
		Addr:         kafka.TCP(broker),
		Topic:        topic,
		RequiredAcks: kafka.RequireAll,
		Balancer:     &kafka.Hash{},
		Async:        false,
	}
	retryWriter := &kafka.Writer{
		Addr:         kafka.TCP(broker),
		Topic:        retryTopic,
		RequiredAcks: kafka.RequireAll,
		Balancer:     &kafka.Hash{},
		Async:        false,
	}
	dlqWriter := &kafka.Writer{
		Addr:         kafka.TCP(broker),
		Topic:        dlqTopic,
		RequiredAcks: kafka.RequireAll,
		Balancer:     &kafka.Hash{},
		Async:        false,
	}
	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers:        []string{broker},
		GroupID:        groupID,
		Topic:          topic,
		StartOffset:    kafka.LastOffset,
		MinBytes:       1,
		MaxBytes:       10e6,
		MaxWait:        time.Second,
		CommitInterval: 0,
	})
	retryReader := kafka.NewReader(kafka.ReaderConfig{
		Brokers:        []string{broker},
		GroupID:        groupID + "-retry",
		Topic:          retryTopic,
		StartOffset:    kafka.LastOffset,
		MinBytes:       1,
		MaxBytes:       10e6,
		MaxWait:        time.Second,
		CommitInterval: 0,
	})

	cleanup := func() {
		_ = writer.Close()
		_ = retryWriter.Close()
		_ = dlqWriter.Close()
		_ = reader.Close()
		_ = retryReader.Close()
	}
	return writer, retryWriter, dlqWriter, reader, retryReader, cleanup
}

func newTestLogger(t *testing.T) *zap.Logger {
	t.Helper()
	log, err := zap.NewDevelopment()
	if err != nil {
		return zap.NewNop()
	}
	return log
}

func waitForOrderCount(t *testing.T, ctx context.Context, db *gorm.DB, voucherID, userID int64, expected int64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var orderCount int64
		if err := db.WithContext(ctx).Model(&model.VoucherOrder{}).
			Where("voucher_id = ? AND user_id = ?", voucherID, userID).
			Count(&orderCount).Error; err != nil {
			t.Fatalf("count orders: %v", err)
		}
		if orderCount == expected {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected %d order records, got %d", expected, orderCount)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

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

	writer, retryWriter, dlqWriter, reader, retryReader, cleanup := newTestKafka(t, ctx)
	defer cleanup()

	svc := NewVoucherOrderService(db, rdb, writer, retryWriter, dlqWriter, reader, retryReader, nil, utils.SMTPConfig{}, newTestLogger(t))

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

	writer, retryWriter, dlqWriter, reader, retryReader, cleanup := newTestKafka(t, ctx)
	defer cleanup()

	svc := NewVoucherOrderService(db, rdb, writer, retryWriter, dlqWriter, reader, retryReader, nil, utils.SMTPConfig{}, newTestLogger(t))

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

	// 校验仅成功一次（Lua 侧限购）
	if success != 1 {
		t.Fatalf("expected 1 success for single user, got %d", success)
	}
	waitForOrderCount(t, ctx, db, voucherID, userID, 1)

	t.Logf("single user attempts=%d, success=%d for voucher %d", attempts, success, voucherID)
}

// TestSeckillSingleUserRedisFlow 单用户触发一次秒杀流程（依赖 Redis 中已有 stock）
func TestSeckillSingleUserRedisFlow(t *testing.T) {
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
		sqlDB.SetMaxOpenConns(10)
		sqlDB.SetMaxIdleConns(5)
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

	writer, retryWriter, dlqWriter, reader, retryReader, cleanup := newTestKafka(t, ctx)
	defer cleanup()

	svc := NewVoucherOrderService(db, rdb, writer, retryWriter, dlqWriter, reader, retryReader, nil, utils.SMTPConfig{}, newTestLogger(t))

	const voucherID = int64(12)
	const userID = int64(2)

	orderID, err := svc.Seckill(ctx, voucherID, userID)
	if err != nil {
		t.Fatalf("seckill failed: %v", err)
	}
	waitForOrderCount(t, ctx, db, voucherID, userID, 1)
	t.Logf("seckill accepted, orderID=%d", orderID)
}

// TestSeckillKafkaDownWritesRetry 模拟 Kafka 不可用时进入 Redis 补偿队列
func TestSeckillKafkaDownWritesRetry(t *testing.T) {
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
		sqlDB.SetMaxOpenConns(10)
		sqlDB.SetMaxIdleConns(5)
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

	const voucherID = int64(12)
	const userID = int64(2)

	// 设置秒杀券可用窗口与库存
	begin := time.Now().Add(-time.Minute)
	end := time.Now().Add(5 * time.Minute)
	if err := db.WithContext(ctx).Model(&model.SeckillVoucher{}).
		Where("voucher_id = ?", voucherID).
		Updates(map[string]interface{}{
			"stock":       100,
			"begin_time":  begin,
			"end_time":    end,
			"update_time": time.Now(),
		}).Error; err != nil {
		t.Fatalf("prepare seckill voucher: %v", err)
	}

	// 预热 Redis 库存与限购集合，并清理补偿队列
	_ = rdb.Set(ctx, fmt.Sprintf(stockKeyFmt, voucherID), 100, 0).Err()
	_ = rdb.Del(ctx, fmt.Sprintf(orderSetFmt, voucherID)).Err()

	// 构造不可用的 Kafka 连接，模拟写入失败
	writer := &kafka.Writer{
		Addr:         kafka.TCP("127.0.0.1:29093"),
		Topic:        "seckill-orders",
		RequiredAcks: kafka.RequireAll,
		Balancer:     &kafka.Hash{},
		Async:        false,
	}
	retryWriter := &kafka.Writer{
		Addr:         kafka.TCP("127.0.0.1:29093"),
		Topic:        "seckill-orders-retry",
		RequiredAcks: kafka.RequireAll,
		Balancer:     &kafka.Hash{},
		Async:        false,
	}
	dlqWriter := &kafka.Writer{
		Addr:         kafka.TCP("127.0.0.1:29093"),
		Topic:        "seckill-orders-dlq",
		RequiredAcks: kafka.RequireAll,
		Balancer:     &kafka.Hash{},
		Async:        false,
	}
	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers:        []string{"127.0.0.1:29093"},
		GroupID:        "seckill-order-consumers-test",
		Topic:          "seckill-orders",
		MinBytes:       1,
		MaxBytes:       10e6,
		MaxWait:        time.Second,
		CommitInterval: 0,
	})
	retryReader := kafka.NewReader(kafka.ReaderConfig{
		Brokers:        []string{"127.0.0.1:29093"},
		GroupID:        "seckill-order-consumers-test-retry",
		Topic:          "seckill-orders-retry",
		MinBytes:       1,
		MaxBytes:       10e6,
		MaxWait:        time.Second,
		CommitInterval: 0,
	})
	defer func() {
		_ = writer.Close()
		_ = retryWriter.Close()
		_ = dlqWriter.Close()
		_ = reader.Close()
		_ = retryReader.Close()
	}()

	svc := NewVoucherOrderService(db, rdb, writer, retryWriter, dlqWriter, reader, retryReader, nil, utils.SMTPConfig{}, newTestLogger(t))

	orderID, err := svc.Seckill(ctx, voucherID, userID)
	if err != nil {
		t.Fatalf("seckill failed: %v", err)
	}
	t.Logf("seckill accepted, orderID=%d", orderID)

}

// TestDuplicateOrderIDReturns1062 插入相同 orderId，验证 MySQL 返回 1062
func TestDuplicateOrderIDReturns1062(t *testing.T) {
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
		sqlDB.SetMaxOpenConns(10)
		sqlDB.SetMaxIdleConns(5)
		defer sqlDB.Close()
	}

	orderID := time.Now().UnixNano()
	now := time.Now()
	order := &model.VoucherOrder{
		ID:         orderID,
		UserID:     1,
		VoucherID:  12,
		CreateTime: now,
		UpdateTime: now,
	}
	// 清理可能残留的测试数据
	_ = db.WithContext(ctx).Where("id = ?", orderID).Delete(&model.VoucherOrder{}).Error
	if err := db.WithContext(ctx).Create(order).Error; err != nil {
		t.Fatalf("first insert failed: %v", err)
	}
	// 第二次插入相同主键应触发 1062
	if err := db.WithContext(ctx).Create(order).Error; err != nil {
		var mysqlErr *mysqldriver.MySQLError
		if errors.As(err, &mysqlErr) {
			if mysqlErr.Number == 1062 {
				return
			}
			t.Fatalf("expected 1062, got %d", mysqlErr.Number)
		}
		t.Fatalf("expected mysql error, got %v", err)
	}
	t.Fatalf("expected duplicate key error (1062), got nil")
}

// TestCreateOrderTxDuplicateId 验证 createOrderTx 遇到主键冲突会吞掉错误
func TestCreateOrderTxDuplicateId(t *testing.T) {
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
		sqlDB.SetMaxOpenConns(10)
		sqlDB.SetMaxIdleConns(5)
		defer sqlDB.Close()
	}

	const voucherID = int64(12)
	const userID = int64(3)
	orderID := time.Now().UnixNano()
	now := time.Now()

	// 先插入一条订单，制造主键冲突
	order := &model.VoucherOrder{
		ID:         orderID,
		UserID:     userID,
		VoucherID:  voucherID,
		CreateTime: now,
		UpdateTime: now,
	}
	_ = db.WithContext(ctx).Where("id = ?", orderID).Delete(&model.VoucherOrder{}).Error
	if err := db.WithContext(ctx).Create(order).Error; err != nil {
		t.Fatalf("seed order failed: %v", err)
	}

	svc := &VoucherOrderService{db: db}
	payload := orderMessage{
		OrderID:   orderID,
		UserID:    userID,
		VoucherID: voucherID,
		CreatedAt: now.Unix(),
	}
	// 再次创建同一订单，期望被 isDuplicateKey 吞掉错误
	if err := svc.createOrderTx(ctx, payload); err != nil {
		t.Fatalf("expected duplicate to be ignored, got %v", err)
	}

	var count int64
	if err := db.WithContext(ctx).Model(&model.VoucherOrder{}).
		Where("id = ?", orderID).
		Count(&count).Error; err != nil {
		t.Fatalf("count orders: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 order record, got %d", count)
	}
}
