package config

import (
	"fmt"
	"time"

	"github.com/spf13/viper"
)

// Config mirrors the Spring Boot application.yaml structure.
type Config struct {
	Server  ServerConfig  `mapstructure:"server"`
	MySQL   MySQLConfig   `mapstructure:"mysql"`
	Redis   RedisConfig   `mapstructure:"redis"`
	Kafka   KafkaConfig   `mapstructure:"kafka"`
	SMTP    SMTPConfig    `mapstructure:"smtp"`
	App     AppConfig     `mapstructure:"app"`
	Logging LoggingConfig `mapstructure:"logging"`
}

// ServerConfig defines HTTP server options.
type ServerConfig struct {
	Port int `mapstructure:"port"`
}

// MySQLConfig configures the relational database connection.
type MySQLConfig struct {
	DSN             string        `mapstructure:"dsn"`
	MaxIdleConns    int           `mapstructure:"maxIdleConns"`
	MaxOpenConns    int           `mapstructure:"maxOpenConns"`
	ConnMaxLifetime time.Duration `mapstructure:"connMaxLifetime"`
}

// RedisConfig configures the Redis client connection.
type RedisConfig struct {
	Addr     string `mapstructure:"addr"`
	Password string `mapstructure:"password"`
	DB       int    `mapstructure:"db"`
}

// KafkaConfig configures Kafka producer/consumer settings.
type KafkaConfig struct {
	Brokers []string `mapstructure:"brokers"`
	Topic   string   `mapstructure:"topic"`
	RetryTopic string `mapstructure:"retryTopic"`
	DLQTopic   string `mapstructure:"dlqTopic"`
	GroupID string   `mapstructure:"groupId"`
}

// SMTPConfig configures email notifications.
type SMTPConfig struct {
	Host string `mapstructure:"host"`
	Port int    `mapstructure:"port"`
	User string `mapstructure:"user"`
	Pass string `mapstructure:"pass"`
	To   string `mapstructure:"to"`
}

// AppConfig carries miscellaneous application settings.
type AppConfig struct {
	ImageUploadDir string `mapstructure:"imageUploadDir"`
}

// LoggingConfig controls structured logging output.
type LoggingConfig struct {
	Level string `mapstructure:"level"`
}

// Load loads configuration from a YAML file path.
func Load(path string) (*Config, error) {
	vp := viper.New()
	vp.SetConfigFile(path)
	if err := vp.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := vp.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}
	return &cfg, nil
}

// MustLoad wraps Load and panics on failure.
func MustLoad(path string) *Config {
	cfg, err := Load(path)
	if err != nil {
		panic(err)
	}
	return cfg
}
