# XZDP Backend (Go)

![Go](https://img.shields.io/badge/Go-1.21%2B-00ADD8?logo=go)
![Build](https://img.shields.io/badge/build-passing-brightgreen)
![License](https://img.shields.io/badge/license-MIT-blue)
![Go Report Card](https://goreportcard.com/badge/github.com/yourname/hmdp-backend)

> 一个高并发本地生活服务平台后端，涵盖商铺、用户、关注流、优惠券与秒杀等核心场景。

## Introduction
面向高并发下的“秒杀 + 社交关注流 + 热点商铺查询”，提供一致性与性能兼顾的后端实现。

## Feature Highlights
- ⚡ 秒杀高并发：Redis Lua 原子校验 + Kafka 异步下单
- 🧠 缓存体系：互斥锁/逻辑过期/Bloom Filter/本地缓存的组合防护
- 🧵 关注流：Redis ZSet 推送收件箱 + 滚动分页
- 🔁 可靠性：重试队列 + DLQ + 补偿兜底

## Tech Stack

### Tech Stack
| Layer | Tech |
| --- | --- |
| Language | Go |
| Web | Gin |
| ORM | Gorm |
| Cache | Redis, BigCache |
| MQ | Kafka |
| DB | MySQL |
| Auth | JWT |
| Infra | Docker |

## Directory Structure
```text
cmd/                 # 应用入口
configs/             # 配置文件
internal/            # 业务核心（handler/service/router/middleware）
pkg/                 # 可复用公共包
scripts/             # 脚本与压测工具
```

## Getting Started

### Prerequisites
- Go 1.24+
- MySQL
- Redis
- Kafka

### Installation
```bash
git clone <your-repo-url>
cd hmdp-backend
go mod tidy
```

### Configuration
- 编辑 `configs/app.yaml`
- 确保 MySQL / Redis / Kafka 连接信息正确

### Run
```bash
go run cmd/server/main.go
```

## API Documentation
当前未内置 Swagger。路由定义可参考：
- `internal/router/router.go`

核心接口示例：
- `POST /voucher-order/seckill/:id`
- `GET /blog/of/follow`
- `GET /shop/:id`

## Optimization & Challenges

### 秒杀高并发（Seckill）
- Redis Lua 原子校验库存与重复下单，避免超卖
- Kafka 异步下单削峰，提升接口吞吐
- DB 条件更新与唯一约束保证幂等
- 重试队列 + DLQ，覆盖临时故障与不可恢复异常

### 热点商铺缓存体系
- 互斥锁防击穿：未命中时单请求回源
- 逻辑过期：过期返回旧值，异步重建
- Bloom Filter 防穿透：Redis 位图 + 多哈希
- 本地缓存：BigCache 构建二级缓存

### 关注流与滚动分页
- 推模式：笔记创建时写入粉丝收件箱（ZSet）
- 滚动分页：`lastID/offset` 处理同分数重复
- DB 批量查询后按 Redis 顺序重排

## Testing
单次下单：
```bash
TOKEN="替换成你的token"
VOUCHER_ID=12
curl -X POST "http://127.0.0.1:8081/voucher-order/seckill/${VOUCHER_ID}" \
  -H "authorization: ${TOKEN}"
```

压测（k6）：
```bash
k6 run -e BASE_URL=http://127.0.0.1:8081 \
  -e VOUCHER_ID=12 \
  -e TOKENS_FILE=../../tokens.csv \
  -e RAMP_WINDOW=10s \
  scripts/k6/seckill.js
```

## License
MIT
