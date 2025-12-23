package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"hmdp-backend/internal/model"
	"hmdp-backend/internal/utils"
)

const (
	stockKeyFmt = "seckill:stock:vid:%d"
	orderSetFmt = "order:vid:%d"
	orderQueue  = "order:queue"
)

// VoucherOrderService 处理秒杀下单逻辑
type VoucherOrderService struct {
	db         *gorm.DB
	rdb        *redis.Client
	idWorker   *utils.RedisIdWorker
	seckillLua *redis.Script
	queueKey   string
}

func NewVoucherOrderService(db *gorm.DB, rdb *redis.Client) *VoucherOrderService {
	svc := &VoucherOrderService{
		db:       db,
		rdb:      rdb,
		idWorker: utils.NewRedisIdWorker(rdb),
		queueKey: orderQueue,
		seckillLua: redis.NewScript(`
local stockKey = KEYS[1]
local orderSetKey = KEYS[2]
local queueKey = KEYS[3]
local userId = ARGV[1]
local voucherId = ARGV[2]
local orderId = ARGV[3]

local stock = tonumber(redis.call("get", stockKey))
if not stock or stock <= 0 then
  return 1
end
if redis.call("sismember", orderSetKey, userId) == 1 then
  return 2
end

redis.call("decr", stockKey)
redis.call("sadd", orderSetKey, userId)
redis.call("rpush", queueKey, cjson.encode({userId=userId, voucherId=voucherId, orderId=orderId}))
return 0
		`),
	}
	go svc.consumeOrders(context.Background())
	return svc
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

	// 生成订单ID，写入 Lua 脚本，由脚本原子扣减库存并入队
	orderID, err := s.idWorker.NextId(ctx, "order")
	if err != nil {
		return 0, err
	}

	stockKey := fmt.Sprintf(stockKeyFmt, voucherID)
	orderSetKey := fmt.Sprintf(orderSetFmt, voucherID)

	res, err := s.seckillLua.Run(ctx, s.rdb, []string{stockKey, orderSetKey, s.queueKey},
		userID, voucherID, orderID).Int()
	if err != nil {
		return 0, err
	}
	switch res {
	case 0:
		// 入队成功，异步创建订单
		return orderID, nil
	case 1:
		return 0, errors.New("库存不足")
	case 2:
		return 0, errors.New("每人限购一单")
	default:
		return 0, errors.New("秒杀失败")
	}
}

func (s *VoucherOrderService) consumeOrders(ctx context.Context) {
	for {
		// 阻塞获取队列消息（res[0] 为队列名，res[1] 为消息内容）
		res, err := s.rdb.BLPop(ctx, 0, s.queueKey).Result()
		if err != nil {
			log.Printf("consumeOrders BLPop error: %v", err)
			continue
		}
		if len(res) != 2 {
			continue
		}
		// 解析订单消息（兼容字符串/数字）
		var raw map[string]string
		if err := json.Unmarshal([]byte(res[1]), &raw); err != nil {
			log.Printf("consumeOrders unmarshal error: %v payload=%s", err, res[1])
			continue
		}
		uid, err1 := strconv.ParseInt(raw["userId"], 10, 64)
		vid, err2 := strconv.ParseInt(raw["voucherId"], 10, 64)
		oid, err3 := strconv.ParseInt(raw["orderId"], 10, 64)
		if err1 != nil || err2 != nil || err3 != nil {
			log.Printf("consumeOrders parse ids error: uidErr=%v vidErr=%v oidErr=%v payload=%s", err1, err2, err3, res[1])
			continue
		}

		// 创建订单记录
		nowTime := time.Now()
		order := &model.VoucherOrder{
			ID:         oid,
			UserID:     uid,
			VoucherID:  vid,
			CreateTime: nowTime,
			UpdateTime: nowTime,
		}
		if err := s.db.WithContext(ctx).Create(order).Error; err != nil {
			log.Printf("consumeOrders create order error: %v ", err)
		}
	}
}
