package data

import (
	"time"

	"go.uber.org/zap"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"hmdp-backend/internal/config"
)

// NewMySQL opens a GORM connection with sane defaults.
func NewMySQL(cfg config.MySQLConfig, log *zap.Logger) (*gorm.DB, error) {
	gormCfg := &gorm.Config{
		Logger: gormlogger.New(
			zap.NewStdLog(log),
			gormlogger.Config{
				SlowThreshold:             time.Second,
				IgnoreRecordNotFoundError: true,
				LogLevel:                  gormlogger.Info,
			},
		),
	}
	db, err := gorm.Open(mysql.Open(cfg.DSN), gormCfg)
	if err != nil {
		return nil, err
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	if cfg.MaxIdleConns > 0 {
		sqlDB.SetMaxIdleConns(cfg.MaxIdleConns)
	}
	if cfg.MaxOpenConns > 0 {
		sqlDB.SetMaxOpenConns(cfg.MaxOpenConns)
	}
	if cfg.ConnMaxLifetime > 0 {
		sqlDB.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	}
	return db, nil
}
