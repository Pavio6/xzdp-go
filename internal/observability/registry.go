package observability

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// NewMetricsRegistry 创建一个新的 Prometheus Registry，并注册 Go 和 Process 相关的指标
func NewMetricsRegistry() *prometheus.Registry {
	registry := prometheus.NewRegistry()
	registry.MustRegister(collectors.NewGoCollector()) // 自动收集Go运行时指标，如goroutine数量、GC统计等
	registry.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{})) // 自动收集进程相关指标，如CPU、内存使用等
	return registry
}
