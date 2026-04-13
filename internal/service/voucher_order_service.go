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
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"hmdp-backend/internal/model"
	"hmdp-backend/internal/observability"
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
	metrics     *observability.SeckillMetrics
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
	metrics *observability.SeckillMetrics,
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
		metrics:     metrics,
		log:         log,
	}
	svc.warmupScripts(context.Background())
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
// warmupScripts 预加载 Lua 脚本到 Redis
func (s *VoucherOrderService) warmupScripts(ctx context.Context) {
	if s.rdb == nil || s.seckillLua == nil {
		return
	}
	if _, err := s.seckillLua.Load(ctx, s.rdb).Result(); err != nil {
		s.log.Warn("seckill lua warmup failed", zap.Error(err))
	}
}

// Seckill 秒杀下单
func (s *VoucherOrderService) Seckill(ctx context.Context, voucherID, userID int64) (int64, error) {
	start := time.Now()
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
		s.metrics.ObserveSeckill("rejected", "not_found", time.Since(start))
		return 0, errors.New("优惠券不存在")
	}
	if err != nil {
		s.metrics.ObserveSeckill("rejected", "query_error", time.Since(start))
		return 0, err
	}
	if info.Status != 1 {
		s.metrics.ObserveSeckill("rejected", "inactive", time.Since(start))
		return 0, errors.New("优惠券已下架或过期")
	}

	now := time.Now()
	if now.Before(info.BeginTime) {
		s.metrics.ObserveSeckill("rejected", "not_started", time.Since(start))
		return 0, errors.New("秒杀尚未开始")
	}
	if now.After(info.EndTime) {
		s.metrics.ObserveSeckill("rejected", "ended", time.Since(start))
		return 0, errors.New("秒杀已结束")
	}
	// 库存不足直接返回
	if info.Stock <= 0 {
		s.metrics.ObserveSeckill("rejected", "no_stock", time.Since(start))
		return 0, errors.New("库存不足")
	}

	// 生成订单ID
	orderID, err := s.idWorker.NextId(ctx, "order")
	if err != nil {
		s.metrics.ObserveSeckill("rejected", "id_error", time.Since(start))
		return 0, err
	}

	stockKey := fmt.Sprintf(stockKeyFmt, voucherID)
	orderSetKey := fmt.Sprintf(orderSetFmt, voucherID)

	// 执行 Lua 脚本，完成库存校验与扣减、用户下单资格校验与标记
	res, err := s.seckillLua.Run(ctx, s.rdb, []string{stockKey, orderSetKey}, userID).Int()
	if err != nil {
		s.metrics.ObserveSeckill("rejected", "lua_error", time.Since(start))
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
			s.metrics.ObserveSeckill("accepted", "publish_failed", time.Since(start))
			return orderID, nil
		}
		s.metrics.ObserveSeckill("accepted", "ok", time.Since(start))
		return orderID, nil
	case 1:
		s.metrics.ObserveSeckill("rejected", "no_stock", time.Since(start))
		return 0, errors.New("库存不足")
	case 2:
		s.metrics.ObserveSeckill("rejected", "duplicate", time.Since(start))
		return 0, errors.New("每人限购一单")
	default:
		s.metrics.ObserveSeckill("rejected", "lua_failed", time.Since(start))
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
	return s.publishKafkaMessage(ctx, s.writer, msg, "")
}

type consumeOutcome int

const (
	consumeSuccess consumeOutcome = iota
	consumeRetryEnqueued
	consumeError
)

// consumeLoop 通用消费循环：负责拉取消息、反序列化、埋点与提交 offset 具体业务由 handler 处理
func (s *VoucherOrderService) consumeLoop(
	ctx context.Context,
	reader *kafka.Reader,
	name string,
	handler func(context.Context, orderMessage, kafka.Message, string, time.Time, trace.Span) (consumeOutcome, error),
) {
	s.log.Info(fmt.Sprintf("%s started", name))
	for {
		msg, err := reader.FetchMessage(ctx)
		if err != nil {
			s.log.Error(fmt.Sprintf("%s fetch message error", name), zap.Error(err))
			time.Sleep(time.Second)
			continue
		}

		var payload orderMessage
		if err := json.Unmarshal(msg.Value, &payload); err != nil {
			s.log.Error(fmt.Sprintf("%s parse message error", name), zap.Error(err))
			_ = reader.CommitMessages(ctx, msg)
			continue
		}

		topic := msg.Topic
		if topic == "" {
			topic = "unknown"
		}
		consumeCtx := observability.ExtractKafkaContext(ctx, msg.Headers)
		consumeCtx, span := s.startKafkaConsumeSpan(consumeCtx, topic)
		start := time.Now()

		outcome, err := handler(consumeCtx, payload, msg, topic, start, span)
		if err != nil {
			span.RecordError(err)
		}

		switch outcome {
		case consumeRetryEnqueued:
			s.metrics.ObserveKafkaConsume(topic, "retry", time.Since(start))
			s.log.Info(fmt.Sprintf("%s retry enqueued, committing offset", name),
				zap.Int64("orderId", payload.OrderID),
				zap.Int64("voucherId", payload.VoucherID),
			)
			span.End()
			if err := reader.CommitMessages(ctx, msg); err != nil {
				s.log.Error(fmt.Sprintf("%s commit error", name), zap.Error(err), zap.Int64("orderId", payload.OrderID))
			}
			continue
		case consumeError:
			s.metrics.ObserveKafkaConsume(topic, "error", time.Since(start))
			if err != nil {
				s.log.Error(fmt.Sprintf("%s handle error", name), zap.Error(err), zap.Int64("orderId", payload.OrderID), zap.Int64("voucherId", payload.VoucherID))
			} else {
				s.log.Error(fmt.Sprintf("%s handle error", name), zap.Int64("orderId", payload.OrderID), zap.Int64("voucherId", payload.VoucherID))
			}
			span.End()
			time.Sleep(200 * time.Millisecond)
			continue
		default:
			s.metrics.ObserveKafkaConsume(topic, "success", time.Since(start))
			span.End()
			if err := reader.CommitMessages(ctx, msg); err != nil {
				s.log.Error(fmt.Sprintf("%s commit error", name), zap.Error(err), zap.Int64("orderId", payload.OrderID))
			}
		}
	}
}

// consumeOrders 异步创建订单（Kafka 消费端）
func (s *VoucherOrderService) consumeOrders(ctx context.Context) {
	s.consumeLoop(ctx, s.reader, "consumeOrders", func(consumeCtx context.Context, payload orderMessage, _ kafka.Message, _ string, _ time.Time, _ trace.Span) (consumeOutcome, error) {
		if err := s.handleConsume(consumeCtx, payload); err != nil {
			if errors.Is(err, errRetryEnqueued) {
				return consumeRetryEnqueued, err
			}
			return consumeError, err
		}
		return consumeSuccess, nil
	})
}

// consumeRetryOrders 消费重试 Topic，按回退时间再次执行
func (s *VoucherOrderService) consumeRetryOrders(ctx context.Context) {
	s.consumeLoop(ctx, s.retryReader, "consumeRetryOrders", func(consumeCtx context.Context, payload orderMessage, _ kafka.Message, _ string, _ time.Time, _ trace.Span) (consumeOutcome, error) {
		s.log.Info("consumeRetryOrders received",
			zap.Int64("orderId", payload.OrderID),
			zap.Int64("voucherId", payload.VoucherID),
			zap.Int("retryCount", payload.RetryCount),
			zap.Int64("nextRetryAt", payload.NextRetryAt),
		)
		if err := s.handleConsume(consumeCtx, payload); err != nil {
			if errors.Is(err, errRetryEnqueued) {
				return consumeRetryEnqueued, err
			}
			return consumeError, err
		}
		return consumeSuccess, nil
	})
}

// consumeDLQ 消费死信队列 发送邮件告警
func (s *VoucherOrderService) consumeDLQ(ctx context.Context) {
	s.consumeLoop(ctx, s.dlqReader, "consumeDLQ", func(_ context.Context, payload orderMessage, _ kafka.Message, _ string, _ time.Time, span trace.Span) (consumeOutcome, error) {
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
				span.RecordError(err)
				s.log.Error("consumeDLQ email failed", zap.Error(err), zap.Int64("orderId", payload.OrderID))
			} else {
				s.log.Info("consumeDLQ email sent", zap.Int64("orderId", payload.OrderID))
			}
		} else {
			s.log.Warn("consumeDLQ email skipped: smtp not configured", zap.Int64("orderId", payload.OrderID))
		}
		return consumeSuccess, nil
	})
}

// handleConsume 处理订单消息，失败则进入重试或死信
func (s *VoucherOrderService) handleConsume(ctx context.Context, payload orderMessage) error {
	start := time.Now()
	// 延迟重试处理
	if payload.NextRetryAt > 0 {
		// 计算距离NextRetryAt时间点还有多久
		delay := time.Until(time.Unix(payload.NextRetryAt, 0))
		// 大于0 代表还没有到重试时间 等delay时间后再继续处理
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
		// 失败则进入重试队列
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
// retryPhaseLabel 返回重试阶段标签
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
	// 业务失败不重试，直接补偿 Redis
	if !isRetryableErr(err) {
		s.compensateRedis(ctx, payload)
		s.log.Info("对于业务错误，跳过重试", zap.Error(err), zap.Int64("orderId", payload.OrderID))
		return errRetryEnqueued
	}

	// 执行重试操作
	payload.RetryCount++
	payload.LastError = err.Error()
	backoff := retryBackoff(payload.RetryCount)
	// 未超过最大重试次数 则写入重试 Topic
	if payload.RetryCount <= maxRetryCount {
		payload.NextRetryAt = time.Now().Add(backoff).Unix()
		s.log.Info("publish to retry",
			zap.Int64("orderId", payload.OrderID),
			zap.Int64("voucherId", payload.VoucherID),
			zap.Int("retryCount", payload.RetryCount),
			zap.Int64("nextRetryAt", payload.NextRetryAt),
		)
		s.metrics.ObserveRetry("retry")
		if err := s.publishRetry(ctx, payload); err != nil {
			return err
		}
		return errRetryEnqueued
	}
	// 重试耗尽 补偿 Redis 后进入死信
	s.compensateRedis(ctx, payload)
	s.log.Info("publish to dlq",
		zap.Int64("orderId", payload.OrderID),
		zap.Int64("voucherId", payload.VoucherID),
		zap.Int("retryCount", payload.RetryCount),
	)
	s.metrics.ObserveRetry("dlq")
	if err := s.publishDLQ(ctx, payload); err != nil {
		return err
	}
	return errRetryEnqueued
}

// publishRetry 写入 Kafka 重试 Topic
func (s *VoucherOrderService) publishRetry(ctx context.Context, payload orderMessage) error {
	return s.publishKafkaMessage(ctx, s.retryWriter, payload, "publish retry failed")
}

// publishDLQ 写入 Kafka 死信 Topic - 后续人工读取DLQ做补偿处理或报警
func (s *VoucherOrderService) publishDLQ(ctx context.Context, payload orderMessage) error {
	return s.publishKafkaMessage(ctx, s.dlqWriter, payload, "publish dlq failed")
}
// publishKafkaMessage 写入消息到kafka
func (s *VoucherOrderService) publishKafkaMessage(ctx context.Context, writer *kafka.Writer, payload orderMessage, errorMsg string) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	message := kafka.Message{
		// 使用 voucherId 作为 key，保证同券消息落到同一分区
		Key:   []byte(strconv.FormatInt(payload.VoucherID, 10)), // 把一个int64整数转为十进制字符串
		Value: data,
	}
	topic := writer.Topic
	if topic == "" {
		topic = "unknown"
	}
	spanCtx, span := s.startKafkaProduceSpan(ctx, topic)
	defer span.End()
	observability.InjectKafkaHeaders(spanCtx, &message.Headers)
	if err := writer.WriteMessages(spanCtx, message); err != nil {
		span.RecordError(err)
		if errorMsg != "" {
			s.log.Error(errorMsg, zap.Error(err), zap.Int64("orderId", payload.OrderID))
		}
		s.metrics.ObserveKafkaPublish(topic, "error")
		return err
	}
	s.metrics.ObserveKafkaPublish(topic, "success")
	return nil
}

// createOrderTx 在事务内创建订单并扣减库存
func (s *VoucherOrderService) createOrderTx(ctx context.Context, payload orderMessage) error {
	// TEST-ONLY：创建订单时 强制消费失败，用于验证 retry/DLQ 流程 - 可控制失败次数
	if failCount := os.Getenv("FORCE_SECKILL_CONSUME_FAIL_COUNT"); failCount != "" {
		if n, err := strconv.Atoi(failCount); err == nil && n >= 0 {
			if payload.RetryCount < n {
				return errors.New("Force a transaction failure to trigger a retry test (count).")
			}
		}
	}

	nowTime := time.Now()
	// 开启事务
	// gorm会自动处理事务的提交和回滚，如果函数返回 error 则回滚，否则提交
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
		// SQL - UPDATE ... SET stock = stock - 1 WHERE stock > 0;
		// 这是一条原子SQL UPDATE ... WHERE ... 执行时会对目标行加锁
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

// isRetryableErr 判断该错误是否需要重试
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
	// 管道补偿操作(批量发送命令，减少RTT)
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
	// 1<<uint(retryCount-1)是位移：2^(retryCount-1)
	backoff := time.Second * time.Duration(1<<uint(retryCount-1))
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
// startKafkaProduceSpan 为 Kafka 生产操作创建 OpenTelemetry Span
func (s *VoucherOrderService) startKafkaProduceSpan(ctx context.Context, topic string) (context.Context, trace.Span) {
	if topic == "" {
		topic = "unknown"
	}
	tracer := otel.Tracer("hmdp-backend")
	return tracer.Start(ctx, "kafka.produce",
		// 给这个span标记类型为 Producer
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(
			attribute.String("messaging.system", "kafka"),
			attribute.String("messaging.destination", topic),
		),
	)
}
// startKafkaConsumeSpan 为 Kafka 消费操作创建 OpenTelemetry Span
func (s *VoucherOrderService) startKafkaConsumeSpan(ctx context.Context, topic string) (context.Context, trace.Span) {
	if topic == "" {
		topic = "unknown"
	}
	tracer := otel.Tracer("hmdp-backend")
	return tracer.Start(ctx, "kafka.consume",
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			attribute.String("messaging.system", "kafka"),
			attribute.String("messaging.destination", topic),
		),
	)
}
