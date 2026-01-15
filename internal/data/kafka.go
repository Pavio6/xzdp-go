package data

import (
	"time"

	"github.com/segmentio/kafka-go"

	"hmdp-backend/internal/config"
)

// NewKafkaWriter 构建 Kafka 生产者，使用强一致写入（acks=all）
func NewKafkaWriter(cfg config.KafkaConfig, topic string) *kafka.Writer {
	return &kafka.Writer{
		Addr:         kafka.TCP(cfg.Brokers...), // broker地址列表
		Topic:        topic,                     // 写入的topic名称
		RequiredAcks: kafka.RequireAll,          // 需所有ISR副本确认
		Balancer:     &kafka.Hash{},             // 使用Hash分区器，按key做分区
		Async:        false,                     // 同步发送
		BatchTimeout: 50 * time.Millisecond,     // 最多等50ms批量发送
		MaxAttempts:  5,                         // 生产端内置重试次数
		WriteBackoffMin: 200 * time.Millisecond, // 生产端重试退避
		WriteBackoffMax: 2 * time.Second,
	}
}

// NewKafkaReader 构建 Kafka 消费者，手动提交 offset 确保至少一次语义
func NewKafkaReader(cfg config.KafkaConfig, topic string, groupID string) *kafka.Reader {
	return kafka.NewReader(kafka.ReaderConfig{
		Brokers:        cfg.Brokers,
		GroupID:        groupID, // 消费者组ID
		Topic:          topic,
		MinBytes:       1,    // 单次拉取的最小字节数
		MaxBytes:       10e6, // 单次拉取的最大字节数10MB
		MaxWait:        time.Second,
		CommitInterval: 0, // 禁用自动提交，改为手动提交offset
	})
}
