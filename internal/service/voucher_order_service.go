package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"hmdp-backend/internal/model"
	"hmdp-backend/internal/utils"
)

// VoucherOrderService 处理秒杀下单逻辑
type VoucherOrderService struct {
	db       *gorm.DB
	rdb      *redis.Client
	idWorker *utils.RedisIdWorker
}

func NewVoucherOrderService(db *gorm.DB, rdb *redis.Client) *VoucherOrderService {
	return &VoucherOrderService{
		db:       db,
		rdb:      rdb,
		idWorker: utils.NewRedisIdWorker(rdb),
	}
}

// Seckill 下单处理：校验时间/库存，扣减库存后创建订单
func (s *VoucherOrderService) Seckill(ctx context.Context, voucherID, userID int64) (int64, error) {
	// 查询秒杀券信息
	var info struct {
		ID        int64
		BeginTime time.Time
		EndTime   time.Time
		Stock     int
		Status    int
	}
	err := s.db.WithContext(ctx).Table("tb_voucher AS v").
		Select("v.id, v.status, sv.begin_time, sv.end_time, sv.stock").
		Joins("LEFT JOIN tb_seckill_voucher sv ON v.id = sv.voucher_id").
		Where("v.id = ?", voucherID).
		Take(&info).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, errors.New("优惠券不存在")
	}
	if err != nil {
		return 0, err
	}
	if info.Status != 1 {
		return 0, errors.New("优惠券已下架或过期")
	}

	now := time.Now()
	if now.Before(info.BeginTime) {
		return 0, errors.New("秒杀尚未开始")
	}
	if now.After(info.EndTime) {
		return 0, errors.New("秒杀已结束")
	}
	if info.Stock <= 0 {
		return 0, errors.New("库存不足")
	}

	// 一人一单：用分布式锁串行化同一用户的秒杀请求，避免并发绕过校验
	lockKey := fmt.Sprintf("lock:order:%d", userID)
	// 使用 SET NX 带 TTL，保证占锁与过期是一个原子操作，并记录唯一 token
	lockToken := uuid.NewString()
	locked, err := s.rdb.SetArgs(ctx, lockKey, lockToken, redis.SetArgs{
		Mode: "NX",
		TTL:  5 * time.Second,
	}).Result()
	if err != nil {
		return 0, err
	}
	if locked != "OK" {
		return 0, errors.New("请勿重复下单")
	}
	// 使用 Lua 脚本校验 token 后删除，防止误删他人锁
	defer func() {
		script := redis.NewScript(`
			if redis.call("get", KEYS[1]) == ARGV[1] then
				return redis.call("del", KEYS[1])
			end
			return 0
		`)
		_, _ = script.Run(ctx, s.rdb, []string{lockKey}, lockToken).Result()
	}()

	// 一人一单：先检查该用户是否已经下过单，避免重复购买
	var existed int64
	if err := s.db.WithContext(ctx).
		Model(&model.VoucherOrder{}).
		Where("voucher_id = ? AND user_id = ?", voucherID, userID).
		Count(&existed).Error; err != nil {
		return 0, err
	}
	if existed > 0 {
		return 0, errors.New("每人限购一单")
	}

	// 原子扣减库存，确保并发下库存不会超卖
	// 通过数据库行锁进行并发控制
	update := s.db.WithContext(ctx).
		Model(&model.SeckillVoucher{}).
		Where("voucher_id = ? AND stock > 0", voucherID).
		Update("stock", gorm.Expr("stock - 1"))
	if update.Error != nil {
		return 0, update.Error
	}
	if update.RowsAffected == 0 {
		return 0, errors.New("库存不足")
	}
	if update.RowsAffected == 0 {
		return 0, errors.New("库存不足")
	}
	// 生成订单ID
	orderID, err := s.idWorker.NextId(ctx, "order")
	if err != nil {
		return 0, err
	}

	// 创建订单记录
	nowTime := time.Now()
	order := &model.VoucherOrder{
		ID:         orderID,
		UserID:     userID,
		VoucherID:  voucherID,
		CreateTime: nowTime,
		UpdateTime: nowTime,
	}
	if err := s.db.WithContext(ctx).Create(order).Error; err != nil {
		return 0, err
	}
	return orderID, nil
}
