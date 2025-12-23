package utils

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisIdWorker 全局ID生成器
type RedisIdWorker struct {
	client *redis.Client
}

const (
	// 开始时间戳（例如：2024-01-01 00:00:00）
	beginTimestamp = int64(1704067200)
	// 31bit 时间戳最大值
	maxTimestamp = int64((1 << 31) - 1)
	// 32bit 序列号最大值
	maxSequence = int64((1 << 32) - 1)
	// 每日 Key 的过期时间，留出一点缓冲
	keyTTL = 48 * time.Hour
)

func NewRedisIdWorker(client *redis.Client) *RedisIdWorker {
	return &RedisIdWorker{client: client}
}

// NextId 生成全局唯一ID
func (w *RedisIdWorker) NextId(ctx context.Context, keyPrefix string) (int64, error) {
	// 1. 生成时间戳
	now := time.Now()
	nowEpoch := now.Unix()
	timestamp := nowEpoch - beginTimestamp
	if timestamp < 0 {
		return 0, fmt.Errorf("timestamp is before beginTimestamp")
	}
	if timestamp > maxTimestamp {
		return 0, fmt.Errorf("timestamp overflow: %d exceeds %d", timestamp, maxTimestamp)
	}

	// 2. 生成序列号
	// 获取当前日期，用于 Redis Key
	date := now.Format("2006:01:02")
	key := fmt.Sprintf("icr:%s:%s", keyPrefix, date)

	// 利用 Redis 的 INCR 自增
	// 即使多实例并发，Redis 内部是单线程执行，保证了原子性
	count, err := w.client.Incr(ctx, key).Result()
	if err != nil {
		return 0, err
	}
	if count == 1 {
		// 仅在新 Key 创建时设置过期，避免每次写都会刷新 TTL
		ok, err := w.client.Expire(ctx, key, keyTTL).Result()
		if err != nil {
			return 0, err
		}
		if !ok {
			return 0, fmt.Errorf("failed to set expiration for key %s", key)
		}
	}
	if count > maxSequence {
		return 0, fmt.Errorf("sequence overflow: %d exceeds %d", count, maxSequence)
	}

	// 3. 拼接并返回
	// 时间戳向左移动 32 位，然后与序列号进行 或运算
	return (timestamp << 32) | count, nil
}
