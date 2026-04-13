package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/extra/redisotel/v9"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
	"gorm.io/plugin/opentelemetry/tracing"

	"hmdp-backend/internal/config"
	"hmdp-backend/internal/data"
	"hmdp-backend/internal/handler"
	"hmdp-backend/internal/middleware"
	"hmdp-backend/internal/observability"
	"hmdp-backend/internal/router"
	"hmdp-backend/internal/service"
	"hmdp-backend/internal/utils"
	"hmdp-backend/pkg/logger"
)

func main() {
	cfgPath := os.Getenv("HMDP_CONFIG")
	if cfgPath == "" {
		cfgPath = "configs/app.yaml"
	}
	// 加载配置
	cfg := config.MustLoad(cfgPath)
	serviceName := cfg.Observability.ServiceName
	if serviceName == "" {
		serviceName = "hmdp-backend"
	}
	environment := cfg.Observability.Environment
	if environment == "" {
		environment = "local"
	}
	log, err := logger.New(cfg.Logging.Level, environment)
	if err != nil {
		panic(err)
	}
	defer log.Sync()
	log = log.With(
		zap.String("service", serviceName),
		zap.String("env", environment),
	)
	log.Info("loaded config", zap.String("path", cfgPath))

	tracingCfg := observability.TracingConfig{
		Enabled:          cfg.Observability.Tracing.Enabled,
		OTLPGrpcEndpoint: cfg.Observability.Tracing.OTLPGrpcEndpoint,
		Insecure:         cfg.Observability.Tracing.Insecure,
		SampleRate:       cfg.Observability.Tracing.SampleRate,
	}
	resourceCfg := observability.ResourceConfig{
		ServiceName: serviceName,
		Environment: environment,
	}
	// 初始化 OTel
	tracingShutdown, err := observability.SetupTracing(context.Background(), tracingCfg, resourceCfg)
	if err != nil {
		log.Fatal("tracing init failed", zap.Error(err))
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tracingShutdown(shutdownCtx); err != nil {
			log.Warn("tracing shutdown failed", zap.Error(err))
		}
	}()

	// 初始化 MySQL
	db, err := data.NewMySQL(cfg.MySQL, log)
	if err != nil {
		log.Fatal("mysql init failed", zap.Error(err))
	}
	if cfg.Observability.Tracing.Enabled {
		if err := db.Use(tracing.NewPlugin()); err != nil {
			log.Warn("gorm tracing plugin init failed", zap.Error(err))
		}
	}
	// 
	sqlDB, err := db.DB()
	if err != nil {
		log.Fatal("mysql db handle", zap.Error(err))
	}
	defer sqlDB.Close()
	log.Info("connected to mysql")

	// 初始化 Redis
	redisClient := data.NewRedis(cfg.Redis)
	if err := data.Ping(context.Background(), redisClient); err != nil {
		log.Fatal("redis ping failed", zap.Error(err))
	}
	defer redisClient.Close()
	if cfg.Observability.Tracing.Enabled {
		if err := redisotel.InstrumentTracing(redisClient); err != nil {
			log.Warn("redis tracing init failed", zap.Error(err))
		}
	}
	log.Info("connected to redis", zap.String("addr", cfg.Redis.Addr))

	// 初始化 Kafka
	// 主业务的生产者
	kafkaWriter := data.NewKafkaWriter(cfg.Kafka, cfg.Kafka.Topic)
	// 重试和死信的生产者
	kafkaRetryWriter := data.NewKafkaWriter(cfg.Kafka, cfg.Kafka.RetryTopic)
	kafkaDLQWriter := data.NewKafkaWriter(cfg.Kafka, cfg.Kafka.DLQTopic)
	// 缓存补偿的生产者
	cacheInvalidateWriter := data.NewKafkaWriter(cfg.Kafka, cfg.Kafka.CacheInvalidateTopic)
	cacheInvalidateDLQWriter := data.NewKafkaWriter(cfg.Kafka, cfg.Kafka.CacheInvalidateDLQTopic)
	// 主业务消费者
	kafkaReader := data.NewKafkaReader(cfg.Kafka, cfg.Kafka.Topic, cfg.Kafka.GroupID)
	// 重试消费者 - 重新处理失败消息
	kafkaRetryReader := data.NewKafkaReader(cfg.Kafka, cfg.Kafka.RetryTopic, cfg.Kafka.GroupID+"-retry")
	// 死信消费者 - 审计与告警
	kafkaDLQReader := data.NewKafkaReader(cfg.Kafka, cfg.Kafka.DLQTopic, cfg.Kafka.GroupID+"-dlq")
	// 缓存补偿消费者
	cacheInvalidateReader := data.NewKafkaReader(cfg.Kafka, cfg.Kafka.CacheInvalidateTopic, cfg.Kafka.GroupID+"-shop-cache")
	cacheInvalidateDLQReader := data.NewKafkaReader(cfg.Kafka, cfg.Kafka.CacheInvalidateDLQTopic, cfg.Kafka.GroupID+"-shop-cache-dlq")
	defer kafkaWriter.Close()
	defer kafkaRetryWriter.Close()
	defer kafkaDLQWriter.Close()
	defer cacheInvalidateWriter.Close()
	defer cacheInvalidateDLQWriter.Close()
	defer kafkaReader.Close()
	defer kafkaRetryReader.Close()
	defer kafkaDLQReader.Close()
	defer cacheInvalidateReader.Close()
	defer cacheInvalidateDLQReader.Close()
	log.Info("configured kafka",
		zap.Strings("brokers", cfg.Kafka.Brokers),
		zap.String("topic", cfg.Kafka.Topic),
		zap.String("retryTopic", cfg.Kafka.RetryTopic),
		zap.String("dlqTopic", cfg.Kafka.DLQTopic),
		zap.String("cacheInvalidateTopic", cfg.Kafka.CacheInvalidateTopic),
		zap.String("cacheInvalidateDLQTopic", cfg.Kafka.CacheInvalidateDLQTopic),
		zap.String("groupID", cfg.Kafka.GroupID),
		zap.String("retryGroupID", cfg.Kafka.GroupID+"-retry"),
	)

	// 构建 Service Registry（传入统一 logger）
	smtpCfg := utils.SMTPConfig{
		Host: cfg.SMTP.Host,
		Port: cfg.SMTP.Port,
		User: cfg.SMTP.User,
		Pass: cfg.SMTP.Pass,
		To:   cfg.SMTP.To,
	}
	var seckillMetrics *observability.SeckillMetrics
	var metricsRegistry *prometheus.Registry
	// 注册Prometheus指标
	if cfg.Observability.Metrics.Enabled {
		metricsRegistry = observability.NewMetricsRegistry()
		seckillMetrics = observability.NewSeckillMetrics(metricsRegistry, serviceName)
	}
	services := service.NewRegistry(
		db,
		redisClient,
		kafkaWriter,
		kafkaRetryWriter,
		kafkaDLQWriter,
		cacheInvalidateWriter,
		cacheInvalidateDLQWriter,
		kafkaReader,
		kafkaRetryReader,
		kafkaDLQReader,
		cacheInvalidateReader,
		cacheInvalidateDLQReader,
		smtpCfg,
		cfg.App.ShopCache,
		seckillMetrics,
		log,
	)

	// 初始化 Gin 引擎
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.Use(gin.Recovery())
	engine.Use(middleware.ErrorHandler(log))
	engine.Use(middleware.RequestIDMiddleware(cfg.Observability.Logging.RequestIDHeader))
	// 集成 OpenTelemetry 中间件
	if cfg.Observability.Tracing.Enabled {
		engine.Use(otelgin.Middleware(serviceName))
	}
	if cfg.Observability.Metrics.Enabled {
		// 初始化 HTTP 指标中间件
		metrics := observability.NewHTTPMetrics(metricsRegistry, serviceName)
		engine.Use(metrics.Middleware())
		metricsPath := cfg.Observability.Metrics.Path
		if metricsPath == "" {
			metricsPath = "/metrics"
		}
		// 注册 Prometheus 指标端点
		engine.GET(metricsPath, gin.WrapH(metrics.Handler()))
	}
	engine.Use(middleware.RequestLogger(log))

	uploadDir := cfg.App.ImageUploadDir
	if uploadDir == "" {
		uploadDir = utils.IMAGE_UPLOAD_DIR
	}
	log.Info("configured upload directory", zap.String("path", uploadDir))
	// 注册健康检查端点
	healthHandler := handler.NewHealthHandler(sqlDB, redisClient, cfg.Kafka.Brokers, log)
	engine.GET("/healthz", healthHandler.Healthz)
	engine.GET("/readyz", healthHandler.Readyz)

	router.RegisterRoutes(engine, services, uploadDir, redisClient)

	addr := fmt.Sprintf(":%d", cfg.Server.Port)
	server := &http.Server{
		Addr:    addr,
		Handler: engine,
	}
	// 启动 HTTP 服务（异步）
	go func() {
		log.Info("starting http server", zap.String("addr", addr))
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal("server run failed", zap.Error(err))
		}
	}()

	// 监听系统信号，执行优雅关闭
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Info("shutting down server...")

	ctxShutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(ctxShutdown); err != nil {
		log.Fatal("server shutdown failed", zap.Error(err))
	}
	log.Info("server exited")
}
