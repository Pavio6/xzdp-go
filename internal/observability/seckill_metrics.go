package observability

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// SeckillMetrics 定义秒杀相关的指标
type SeckillMetrics struct {
	seckillTotal        *prometheus.CounterVec
	seckillLatency      *prometheus.HistogramVec // 秒杀请求耗时分布
	kafkaPublishTotal   *prometheus.CounterVec
	kafkaConsumeTotal   *prometheus.CounterVec
	kafkaConsumeLatency *prometheus.HistogramVec // Kafka消费处理耗时分布
	retryTotal          *prometheus.CounterVec
}

// NewSeckillMetrics 创建 SeckillMetrics 实例，并注册到给定的 Registry
func NewSeckillMetrics(registry *prometheus.Registry, serviceName string) *SeckillMetrics {
	if registry == nil {
		registry = NewMetricsRegistry()
	}
	// 公共标签，为所有metric添加服务名称
	constLabels := prometheus.Labels{}
	if serviceName != "" {
		constLabels["service"] = serviceName
	}

	// ==================== 秒杀请求总数 ====================
	// Counter：只增不减，用于统计请求次数
	// labels:
	//   result: 成功/失败（success / fail）
	//   reason: 失败原因（stock_not_enough / duplicate / system_error）
	seckillTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   "seckill",        // 命名空间（业务模块）
		Subsystem:   "order",          // 子模块（订单）
		Name:        "requests_total", // 指标名
		Help:        "Total seckill requests.",
		ConstLabels: constLabels, // 固定标签（如服务名、实例ID）
	}, []string{"result", "reason"})

	// ==================== 秒杀请求耗时 ====================
	// Histogram：统计延迟分布（用于 P95 / P99）
	// labels:
	//   result: 成功/失败
	seckillLatency := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   "seckill",
		Subsystem:   "order",
		Name:        "request_duration_seconds",
		Help:        "Seckill request duration in seconds.",
		Buckets:     prometheus.DefBuckets, // 默认分桶（适合一般延迟统计）
		ConstLabels: constLabels,
	}, []string{"result"})

	// ==================== Kafka 发送次数 ====================
	// 统计 Producer 发布消息的情况
	// labels:
	//   topic: Kafka topic 名称
	//   result: 成功/失败
	kafkaPublishTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   "seckill",
		Subsystem:   "kafka",
		Name:        "publish_total",
		Help:        "Total kafka publish attempts.",
		ConstLabels: constLabels,
	}, []string{"topic", "result"})

	// ==================== Kafka 消费次数 ====================
	// 统计 Consumer 处理结果
	// labels:
	//   topic: topic 名称
	//   result: success / fail
	kafkaConsumeTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   "seckill",
		Subsystem:   "kafka",
		Name:        "consume_total",
		Help:        "Total kafka consume results.",
		ConstLabels: constLabels,
	}, []string{"topic", "result"})

	// ==================== Kafka 消费耗时 ====================
	// 用于监控消费处理延迟（是否出现积压 / 慢消费）
	// labels:
	//   topic: topic 名称
	//   result: success / fail
	kafkaConsumeLatency := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   "seckill",
		Subsystem:   "kafka",
		Name:        "consume_duration_seconds",
		Help:        "Kafka consume handling duration in seconds.",
		Buckets:     prometheus.DefBuckets,
		ConstLabels: constLabels,
	}, []string{"topic", "result"})

	// ==================== 重试 / 死信队列 ====================
	// 统计重试次数和进入 DLQ 的情况
	// labels:
	//   phase: retry / dlq / backoff 等阶段
	retryTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   "seckill",
		Subsystem:   "kafka",
		Name:        "retry_total",
		Help:        "Total retry or DLQ events.",
		ConstLabels: constLabels,
	}, []string{"phase"})

	// 注册所有指标到 Prometheus Registry
	registry.MustRegister(
		seckillTotal,
		seckillLatency,
		kafkaPublishTotal,
		kafkaConsumeTotal,
		kafkaConsumeLatency,
		retryTotal,
	)

	// 返回封装后的指标结构体，供业务代码调用
	return &SeckillMetrics{
		seckillTotal:        seckillTotal,
		seckillLatency:      seckillLatency,
		kafkaPublishTotal:   kafkaPublishTotal,
		kafkaConsumeTotal:   kafkaConsumeTotal,
		kafkaConsumeLatency: kafkaConsumeLatency,
		retryTotal:          retryTotal,
	}

}

// ObserveSeckill 记录一次秒杀请求的结果与耗时
func (m *SeckillMetrics) ObserveSeckill(result, reason string, duration time.Duration) {
	if m == nil {
		return
	}
	if reason == "" {
		reason = "unknown"
	}
	m.seckillTotal.WithLabelValues(result, reason).Inc()
	m.seckillLatency.WithLabelValues(result).Observe(duration.Seconds())
}

// ObserveKafkaPublish 记录一次 Kafka 消息发布的结果
func (m *SeckillMetrics) ObserveKafkaPublish(topic, result string) {
	if m == nil {
		return
	}
	m.kafkaPublishTotal.WithLabelValues(topic, result).Inc()
}

// ObserveKafkaConsume 记录一次 Kafka 消息消费的结果与耗时
func (m *SeckillMetrics) ObserveKafkaConsume(topic, result string, duration time.Duration) {
	if m == nil {
		return
	}
	m.kafkaConsumeTotal.WithLabelValues(topic, result).Inc()
	m.kafkaConsumeLatency.WithLabelValues(topic, result).Observe(duration.Seconds())
}

// ObserveRetry 记录一次重试或死信处理事件
func (m *SeckillMetrics) ObserveRetry(phase string) {
	if m == nil {
		return
	}
	m.retryTotal.WithLabelValues(phase).Inc()
}
