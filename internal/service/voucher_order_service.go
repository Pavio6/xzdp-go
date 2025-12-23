package service

import (
	"context"
	_ "embed"
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

//go:embed seckill.lua
var seckillLuaSource string

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
		db:         db,
		rdb:        rdb,
		idWorker:   utils.NewRedisIdWorker(rdb),
		queueKey:   orderQueue,
		seckillLua: redis.NewScript(seckillLuaSource),
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

	stockKey := fmt.Sprintf(stockKeyFmt, voucherID)
	orderSetKey := fmt.Sprintf(orderSetFmt, voucherID)

	res, err := s.seckillLua.Run(ctx, s.rdb, []string{stockKey, orderSetKey},
		userID, voucherID).Int()
	if err != nil {
		return 0, err
	}

	switch res {
	case 0:
		// 生成订单ID
		orderID, err := s.idWorker.NextId(ctx, "order")
		if err != nil {
			return 0, err
		}
		// Lua 校验成功，入队异步创建订单
		payload, _ := json.Marshal(map[string]interface{}{
			"userId":    userID,
			"voucherId": voucherID,
			"orderId":   orderID,
		})
		if err := s.rdb.RPush(ctx, s.queueKey, payload).Err(); err != nil {
			return 0, err
		}
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
		var raw map[string]interface{}
		if err := json.Unmarshal([]byte(res[1]), &raw); err != nil {
			log.Printf("consumeOrders unmarshal error: %v payload=%s", err, res[1])
			continue
		}
		parse := func(v interface{}) (int64, error) {
			switch val := v.(type) {
			case string:
				return strconv.ParseInt(val, 10, 64)
			case float64:
				return int64(val), nil
			case json.Number:
				return val.Int64()
			default:
				return 0, fmt.Errorf("unexpected type %T", v)
			}
		}
		uid, err1 := parse(raw["userId"])
		vid, err2 := parse(raw["voucherId"])
		oid, err3 := parse(raw["orderId"])
		if err1 != nil || err2 != nil || err3 != nil {
			log.Printf("consumeOrders parse ids error: uidErr=%v vidErr=%v oidErr=%v payload=%s", err1, err2, err3, res[1])
			continue
		}

		nowTime := time.Now()
		// 事务内扣减 DB 库存并创建订单，确保数据库内一致性
		if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			res := tx.Model(&model.SeckillVoucher{}).
				Where("voucher_id = ? AND stock > 0", vid).
				Update("stock", gorm.Expr("stock - 1"))
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected == 0 {
				return errors.New("db stock not enough")
			}
			order := &model.VoucherOrder{
				ID:         oid,
				UserID:     uid,
				VoucherID:  vid,
				CreateTime: nowTime,
				UpdateTime: nowTime,
			}
			return tx.Create(order).Error
		}); err != nil {
			log.Printf("consumeOrders txn error: %v payload=%s", err, res[1])
		}
	}
}
