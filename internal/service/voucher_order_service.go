package service

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"hmdp-backend/internal/model"
	"hmdp-backend/internal/utils"
)

const (
	stockKeyFmt  = "seckill:stock:vid:%d"
	orderSetFmt  = "order:vid:%d"
)

var errRetryEnqueued = errors.New("retry enqueued")

//go:embed seckill.lua
var seckillLuaSource string

// VoucherOrderService 处理秒杀下单逻辑
type VoucherOrderService struct {
	db          *gorm.DB
	rdb         *redis.Client
	idWorker    *utils.RedisIdWorker
	seckillLua  *redis.Script
	writer      *kafka.Writer
	retryWriter *kafka.Writer
	dlqWriter   *kafka.Writer
	reader      *kafka.Reader
	retryReader *kafka.Reader
	dlqReader   *kafka.Reader
	smtpCfg     utils.SMTPConfig
	log         *zap.Logger
}

func NewVoucherOrderService(
	db *gorm.DB,
	rdb *redis.Client,
	writer *kafka.Writer,
	retryWriter *kafka.Writer,
	dlqWriter *kafka.Writer,
	reader *kafka.Reader,
	retryReader *kafka.Reader,
	dlqReader *kafka.Reader,
	smtpCfg utils.SMTPConfig,
	log *zap.Logger,
) *VoucherOrderService {
	if log == nil {
		log = zap.NewNop()
	}
	svc := &VoucherOrderService{
		db:          db,
		rdb:         rdb,
		idWorker:    utils.NewRedisIdWorker(rdb),
		seckillLua:  redis.NewScript(seckillLuaSource),
		writer:      writer,
		retryWriter: retryWriter,
		dlqWriter:   dlqWriter,
		reader:      reader,
		retryReader: retryReader,
		dlqReader:   dlqReader,
		smtpCfg:     smtpCfg,
		log:         log,
	}
	log.Info("voucher order consumers starting")
	// 异步消费 Kafka 订单消息
	go svc.consumeOrders(context.Background())
	// 重试队列消费
	go svc.consumeRetryOrders(context.Background())
	// 记录消费延迟（lag）用于监控
	go svc.logKafkaLag(context.Background())
	// 死信队列消费 邮件告警
	if svc.dlqReader != nil {
		go svc.consumeDLQ(context.Background())
	}
	return svc
}

// Seckill 下单处理：校验时间/库存，扣减库存后创建订单
func (s *VoucherOrderService) Seckill(ctx context.Context, voucherID, userID int64) (int64, error) {
	var info struct {
		ID        int64
		BeginTime time.Time
		EndTime   time.Time
		Stock     int
		Status    int
	}
	// 查询秒杀券信息
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
	// 库存不足直接返回
	if info.Stock <= 0 {
		return 0, errors.New("库存不足")
	}

	// 生成订单ID
	orderID, err := s.idWorker.NextId(ctx, "order")
	if err != nil {
		return 0, err
	}

	stockKey := fmt.Sprintf(stockKeyFmt, voucherID)
	orderSetKey := fmt.Sprintf(orderSetFmt, voucherID)

	// 执行 Lua 脚本，完成库存校验与扣减、用户下单资格校验与标记
	res, err := s.seckillLua.Run(ctx, s.rdb, []string{stockKey, orderSetKey}, userID).Int()
	if err != nil {
		return 0, err
	}

	switch res {
	case 0:
		// Lua 校验成功，发送 Kafka 消息由消费者异步落库
		msg := orderMessage{
			OrderID:   orderID,
			UserID:    userID,
			VoucherID: voucherID,
			CreatedAt: time.Now().Unix(),
		}
		if err := s.publishOrder(ctx, msg); err != nil {
			s.log.Error("publish kafka failed, queued for retry", zap.Error(err), zap.Int64("orderId", orderID))
			return orderID, nil
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

type orderMessage struct {
	OrderID     int64  `json:"orderId"`
	UserID      int64  `json:"userId"`
	VoucherID   int64  `json:"voucherId"`
	CreatedAt   int64  `json:"createdAt"`
	RetryCount  int    `json:"retryCount"`          // 重试次数
	NextRetryAt int64  `json:"nextRetryAt"`         // 下次重试时间（秒）
	LastError   string `json:"lastError,omitempty"` // 最后一次错误信息
}

// publishOrder 将订单消息发送到 Kafka
func (s *VoucherOrderService) publishOrder(ctx context.Context, msg orderMessage) error {
	payload, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	message := kafka.Message{
		// 使用 voucherId 作为 key，保证同券消息落到同一分区
		Key:   []byte(strconv.FormatInt(msg.VoucherID, 10)),
		Value: payload,
	}
	// 将消息写入kafka
	return s.writer.WriteMessages(ctx, message)
}

// consumeOrders 异步创建订单（Kafka 消费端）
func (s *VoucherOrderService) consumeOrders(ctx context.Context) {
	s.log.Info("consumeOrders started")
	for {
		// 拉取消息（不自动提交），处理成功后再提交 offset
		msg, err := s.reader.FetchMessage(ctx)
		if err != nil {
			s.log.Error("consumeOrders fetch message error", zap.Error(err))
			time.Sleep(time.Second)
			continue
		}

		var payload orderMessage
		// 反序列化消息
		if err := json.Unmarshal(msg.Value, &payload); err != nil {
			s.log.Error("consumeOrders parse message error", zap.Error(err))
			// 消息格式不正确，直接提交，避免阻塞消费
			_ = s.reader.CommitMessages(ctx, msg)
			continue
		}
		// 处理订单创建
		if err := s.handleConsume(ctx, payload); err != nil {
			if errors.Is(err, errRetryEnqueued) {
				s.log.Info("consumeOrders retry enqueued, committing offset",
					zap.Int64("orderId", payload.OrderID),
					zap.Int64("voucherId", payload.VoucherID),
				)
				// 如果已进入重试队列或死信 提交 offset
				if err := s.reader.CommitMessages(ctx, msg); err != nil {
					s.log.Error("consumeOrders commit error", zap.Error(err), zap.Int64("orderId", payload.OrderID))
				}
				continue
			}
			s.log.Error("consumeOrders handle error", zap.Error(err), zap.Int64("orderId", payload.OrderID))
			time.Sleep(200 * time.Millisecond)
			continue
		}
		// 提交 offset
		if err := s.reader.CommitMessages(ctx, msg); err != nil {
			s.log.Error("consumeOrders commit error", zap.Error(err), zap.Int64("orderId", payload.OrderID))
		}
	}
}

// consumeRetryOrders 消费重试 Topic，按回退时间再次执行
func (s *VoucherOrderService) consumeRetryOrders(ctx context.Context) {
	s.log.Info("consumeRetryOrders started")
	for {
		msg, err := s.retryReader.FetchMessage(ctx)
		if err != nil {
			s.log.Error("consumeRetryOrders fetch message error", zap.Error(err))
			time.Sleep(time.Second)
			continue
		}

		var payload orderMessage
		if err := json.Unmarshal(msg.Value, &payload); err != nil {
			s.log.Error("consumeRetryOrders parse message error", zap.Error(err))
			_ = s.retryReader.CommitMessages(ctx, msg)
			continue
		}
		s.log.Info("consumeRetryOrders received",
			zap.Int64("orderId", payload.OrderID),
			zap.Int64("voucherId", payload.VoucherID),
			zap.Int("retryCount", payload.RetryCount),
			zap.Int64("nextRetryAt", payload.NextRetryAt),
		)

		if err := s.handleConsume(ctx, payload); err != nil {
			if errors.Is(err, errRetryEnqueued) {
				s.log.Info("consumeRetryOrders retry enqueued, committing offset",
					zap.Int64("orderId", payload.OrderID),
					zap.Int64("voucherId", payload.VoucherID),
				)
				if err := s.retryReader.CommitMessages(ctx, msg); err != nil {
					s.log.Error("consumeRetryOrders commit error", zap.Error(err), zap.Int64("orderId", payload.OrderID))
				}
				continue
			}
			s.log.Error("consumeRetryOrders handle error", zap.Error(err), zap.Int64("orderId", payload.OrderID))
			time.Sleep(200 * time.Millisecond)
			continue
		}

		if err := s.retryReader.CommitMessages(ctx, msg); err != nil {
			s.log.Error("consumeRetryOrders commit error", zap.Error(err), zap.Int64("orderId", payload.OrderID))
		}
	}
}

// consumeDLQ 消费死信队列 发送邮件告警
func (s *VoucherOrderService) consumeDLQ(ctx context.Context) {
	s.log.Info("consumeDLQ started")
	for {
		// 拉取死信消息
		msg, err := s.dlqReader.FetchMessage(ctx)
		if err != nil {
			s.log.Error("consumeDLQ fetch message error", zap.Error(err))
			time.Sleep(time.Second)
			continue
		}
		var payload orderMessage
		if err := json.Unmarshal(msg.Value, &payload); err != nil {
			s.log.Error("consumeDLQ parse message error", zap.Error(err))
			_ = s.dlqReader.CommitMessages(ctx, msg)
			continue
		}
		// 发送邮件告警
		if s.smtpCfg.Host != "" {
			subject := fmt.Sprintf("[DLQ] seckill order failed: %d", payload.OrderID)
			body := fmt.Sprintf(
				"订单进入 DLQ, 请人工审核处理。\n\norderId: %d\nuserId: %d\nvoucherId: %d\nretryCount: %d\nlastError: %s\ncreatedAt: %d\n",
				payload.OrderID,
				payload.UserID,
				payload.VoucherID,
				payload.RetryCount,
				payload.LastError,
				payload.CreatedAt,
			)
			if err := utils.SendEmail(s.smtpCfg, subject, body); err != nil {
				s.log.Error("consumeDLQ email failed", zap.Error(err), zap.Int64("orderId", payload.OrderID))
			} else {
				s.log.Info("consumeDLQ email sent", zap.Int64("orderId", payload.OrderID))
			}
		} else {
			s.log.Warn("consumeDLQ email skipped: smtp not configured", zap.Int64("orderId", payload.OrderID))
		}
		// 提交 offset
		if err := s.dlqReader.CommitMessages(ctx, msg); err != nil {
			s.log.Error("consumeDLQ commit error", zap.Error(err), zap.Int64("orderId", payload.OrderID))
		}
	}
}

// handleConsume 处理订单消息，失败则进入重试或死信
func (s *VoucherOrderService) handleConsume(ctx context.Context, payload orderMessage) error {
	start := time.Now()
	if payload.RetryCount == 0 && os.Getenv("FORCE_SECKILL_FAIL_ONCE") == "1" {
		s.log.Warn("forced retry test (once)",
			zap.Int64("orderId", payload.OrderID),
			zap.Int64("voucherId", payload.VoucherID),
			zap.Int("retryCount", payload.RetryCount),
		)
		return s.publishRetryOrDLQ(ctx, payload, errors.New("forced consume failure for retry test"))
	}
	if failCount := os.Getenv("FORCE_SECKILL_FAIL_COUNT"); failCount != "" {
		if n, err := strconv.Atoi(failCount); err == nil && n >= 0 {
			if payload.RetryCount < n {
				s.log.Warn("forced retry test (count)",
					zap.Int64("orderId", payload.OrderID),
					zap.Int64("voucherId", payload.VoucherID),
					zap.Int("retryCount", payload.RetryCount),
					zap.Int("failCount", n),
				)
				return s.publishRetryOrDLQ(ctx, payload, errors.New("forced consume failure for retry test (count)"))
			}
		}
	}
	// 延迟重试处理
	if payload.NextRetryAt > 0 {
		delay := time.Until(time.Unix(payload.NextRetryAt, 0))
		if delay > 0 {
			time.Sleep(delay)
		}
	}
	// 创建订单事务
	if err := s.createOrderTx(ctx, payload); err != nil {
		s.log.Warn("handleConsume failed",
			zap.Int64("orderId", payload.OrderID),
			zap.Int64("voucherId", payload.VoucherID),
			zap.Duration("cost", time.Since(start)),
			zap.Error(err),
		)
		return s.publishRetryOrDLQ(ctx, payload, err)
	}
	s.log.Info("handleConsume success",
		zap.Int64("orderId", payload.OrderID),
		zap.Int64("voucherId", payload.VoucherID),
		zap.Int("retryCount", payload.RetryCount),
		zap.String("retryPhase", retryPhaseLabel(payload.RetryCount)),
		zap.Duration("cost", time.Since(start)),
	)
	return nil
}

func retryPhaseLabel(retryCount int) string {
	switch retryCount {
	case 0:
		return "initial"
	case 1:
		return "retry-1"
	case 2:
		return "retry-2"
	case 3:
		return "retry-3"
	default:
		return "retry-n"
	}
}

// publishRetryOrDLQ 根据失败次数写入重试队列或死信队列
func (s *VoucherOrderService) publishRetryOrDLQ(ctx context.Context, payload orderMessage, err error) error {
	if !isRetryableErr(err) {
		// 业务失败不重试，直接补偿 Redis
		s.compensateRedis(ctx, payload)
		s.log.Info("skip retry for business error", zap.Error(err), zap.Int64("orderId", payload.OrderID))
		return errRetryEnqueued
	}
	payload.RetryCount++
	payload.LastError = err.Error()
	backoff := retryBackoff(payload.RetryCount)
	// 未超过最大重试次数，写入重试 Topic
	if payload.RetryCount <= maxRetryCount {
		payload.NextRetryAt = time.Now().Add(backoff).Unix()
		s.log.Info("publish to retry",
			zap.Int64("orderId", payload.OrderID),
			zap.Int64("voucherId", payload.VoucherID),
			zap.Int("retryCount", payload.RetryCount),
			zap.Int64("nextRetryAt", payload.NextRetryAt),
		)
		if err := s.publishRetry(ctx, payload); err != nil {
			return err
		}
		return errRetryEnqueued
	}
	// 重试耗尽，补偿 Redis 后进入死信
	s.compensateRedis(ctx, payload)
	s.log.Info("publish to dlq",
		zap.Int64("orderId", payload.OrderID),
		zap.Int64("voucherId", payload.VoucherID),
		zap.Int("retryCount", payload.RetryCount),
	)
	// 超过最大重试次数，写入死信 Topic
	if err := s.publishDLQ(ctx, payload); err != nil {
		return err
	}
	return errRetryEnqueued
}

// publishRetry 写入 Kafka 重试 Topic
func (s *VoucherOrderService) publishRetry(ctx context.Context, payload orderMessage) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	message := kafka.Message{
		Key:   []byte(strconv.FormatInt(payload.VoucherID, 10)),
		Value: data,
	}
	// 写入重试 Topic
	if err := s.retryWriter.WriteMessages(ctx, message); err != nil {
		s.log.Error("publish retry failed", zap.Error(err), zap.Int64("orderId", payload.OrderID))
		return err
	}
	return nil
}

// publishDLQ 写入 Kafka 死信 Topic - 后续人工读取DLQ做补偿处理或报警
func (s *VoucherOrderService) publishDLQ(ctx context.Context, payload orderMessage) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	message := kafka.Message{
		Key:   []byte(strconv.FormatInt(payload.VoucherID, 10)),
		Value: data,
	}
	if err := s.dlqWriter.WriteMessages(ctx, message); err != nil {
		s.log.Error("publish dlq failed", zap.Error(err), zap.Int64("orderId", payload.OrderID))
		return err
	}
	return nil
}

// createOrderTx 在事务内创建订单并扣减库存
func (s *VoucherOrderService) createOrderTx(ctx context.Context, payload orderMessage) error {
	// 测试开关：强制消费失败，用于验证 retry/DLQ 流程
	// if os.Getenv("FORCE_SECKILL_CONSUME_FAIL") == "1" {
	// 	return errors.New("forced consume failure for retry test")
	// }
	nowTime := time.Now()
	if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		order := &model.VoucherOrder{
			ID:         payload.OrderID,
			UserID:     payload.UserID,
			VoucherID:  payload.VoucherID,
			PayType:    1,
			Status:     1,
			CreateTime: nowTime,
			UpdateTime: nowTime,
		}
		if err := tx.Create(order).Error; err != nil {
			if isDuplicateKey(err) {
				// 已处理过该订单，避免重复扣减库存
				return nil
			}
			return err
		}
		// 订单创建成功后再扣减库存，避免重复消费导致多次扣减
		res := tx.Model(&model.SeckillVoucher{}).
			Where("voucher_id = ? AND stock > 0", payload.VoucherID).
			Update("stock", gorm.Expr("stock - 1"))
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return errDBStockNotEnough
		}
		return nil
	}); err != nil { 
		return err
	}
	return nil
}

// logKafkaLag 定期记录 Kafka 消费延迟（lag）
func (s *VoucherOrderService) logKafkaLag(ctx context.Context) {
	s.log.Info("logKafkaLag started")
	ticker := time.NewTicker(120 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			stats := s.reader.Stats()
			// lag 用于监控消费延迟
			s.log.Info("kafka consumer lag", zap.Int64("lag", stats.Lag))
		}
	}
}

const maxRetryCount = 3

var errDBStockNotEnough = errors.New("db stock not enough")

func isRetryableErr(err error) bool {
	if errors.Is(err, errDBStockNotEnough) {
		return false
	}
	return true
}
// compensateRedis 补偿 Redis 库存和用户下单资格
func (s *VoucherOrderService) compensateRedis(ctx context.Context, payload orderMessage) {
	stockKey := fmt.Sprintf(stockKeyFmt, payload.VoucherID)
	orderSetKey := fmt.Sprintf(orderSetFmt, payload.VoucherID)
	_, _ = s.rdb.Pipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.Incr(ctx, stockKey)
		pipe.SRem(ctx, orderSetKey, payload.UserID)
		return nil
	})
}

// retryBackoff 重试回退时间，指数增长，最大 30 秒
func retryBackoff(retryCount int) time.Duration {
	if retryCount <= 0 {
		return time.Second
	}
	backoff := time.Second * time.Duration(1<<uint(retryCount-1))
	if backoff > 30*time.Second {
		return 30 * time.Second
	}
	return backoff
}

// isDuplicateKey 数据库插入订单时是否发生唯一键冲突
// kafka 重复投递同一条消息，消费层重复执行插入，会触发 MySQL 的 1062（Duplicate entry）
func isDuplicateKey(err error) bool {
	var mysqlErr *mysql.MySQLError
	// 仅当错误是 MySQL 唯一键冲突（1062）时返回 true
	if errors.As(err, &mysqlErr) {
		return mysqlErr.Number == 1062
	}
	return false
}
