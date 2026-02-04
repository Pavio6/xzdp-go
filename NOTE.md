## 秒杀功能说明（Seckill）

本项目的秒杀流程使用 **Redis Lua + Kafka 异步落库 + 重试/DLQ** 组合，以保证高并发下的性能与一致性。

### 功能概述
- Redis Lua 脚本负责校验库存、是否重复下单、并扣减库存（原子操作）。
- 通过 Kafka 异步写订单，提升接口吞吐与响应速度。
- 消费端写库失败后进入重试队列；超过最大次数进入 DLQ（死信队列），可人工处理或告警。

### 业务流程（请求链路）
1. **客户端请求** `/voucher-order/seckill/{voucherId}`。
2. **Redis Lua** 原子校验与扣减：
   - 库存不足 → 直接失败
   - 重复下单 → 直接失败
   - 成功 → 返回订单 ID
3. **Kafka 生产**：将订单消息写入主 Topic（按 voucherId 分区）。
4. **Kafka 消费**：
   - 事务内创建订单
   - 订单创建成功后再扣减 DB 库存（防重复消费）
5. **失败处理**：
   - 可重试错误 → 写入 retry topic，按指数退避延迟处理
   - 超过最大次数 → 写入 DLQ，发送邮件告警（可选）

### 关键设计点
- **防超卖**：DB 扣减用 `UPDATE ... SET stock = stock - 1 WHERE stock > 0` 原子条件更新。
- **幂等**：订单表唯一约束，重复消费会触发 duplicate key，直接返回成功避免重复扣库存。
- **分区有序**：Kafka 使用 `voucherId` 作为 key，同券消息落同分区。
- **重试退避**：指数退避（1s, 2s, 4s...，最大 30s），超过次数进入 DLQ。

### 代码位置
- Lua 脚本：`internal/service/seckill.lua`
- 订单生产/消费/重试逻辑：`internal/service/voucher_order_service.go`
- ID 生成：`internal/utils/redisId_worker.go`

### 本地运行依赖
需要本地或容器内提供：
- MySQL
- Redis
- Kafka

配置文件：`configs/app.yaml`

### 测试方法

#### 1) 单次下单（curl）
```bash
TOKEN="替换成你的token"
VOUCHER_ID=12
curl -X POST "http://127.0.0.1:8081/voucher-order/seckill/${VOUCHER_ID}" \
  -H "authorization: ${TOKEN}"
```

#### 2) 压测秒杀（k6）
1. 生成测试 token：
```bash
go run cmd/gen_tokens/main.go -in hmdp_tb_user.csv -out tokens.csv -redis 127.0.0.1:6379 -db 0
```

2. 启动服务：
```bash
rm -f server.log
go run cmd/server/main.go > server.log 2>&1 &
```

3. 运行压测：
```bash
k6 run -e BASE_URL=http://127.0.0.1:8081 \
  -e VOUCHER_ID=12 \
  -e TOKENS_FILE=../../tokens.csv \
  -e RAMP_WINDOW=10s \
  scripts/k6/seckill.js
```

4. 查看消费落库日志：
```bash
rg -n "handleConsume success|handleConsume failed" server.log
```

#### 3) 测试重试与 DLQ（计数开关）
设置环境变量 `FORCE_SECKILL_CONSUME_FAIL_COUNT=n`，当 `RetryCount < n` 时强制失败。

示例：失败 3 次后成功
```bash
rm -f server.log
FORCE_SECKILL_CONSUME_FAIL_COUNT=3 go run cmd/server/main.go > server.log 2>&1 &
```

观察重试/DLQ：
```bash
rg -n "publish to retry|publish to dlq|handleConsume failed" server.log
```

> 提示：若 `n > maxRetryCount`（当前为 3），消息会进入 DLQ。

## 热点商铺查询与缓存体系

### 互斥锁防击穿（热点 Key）
- 先查本地缓存与 Redis 缓存，命中直接返回。
- 未命中则尝试获取互斥锁，失败时短暂休眠后重试。
- 获取锁后进行 double-check（先本地再 Redis），避免并发回源。
- 若仍未命中，则查询数据库并回填缓存，最后释放锁。

### 逻辑过期防击穿
- Redis 中存储数据与逻辑过期时间。
- 逻辑未过期直接返回；过期则尝试获取互斥锁并异步重建缓存。
- 获取锁失败直接返回旧值，避免阻塞请求。

> 逻辑过期前提：Redis 中必须有旧值可返回（启动/预热可提前写入）。

### Bloom Filter 防穿透
- 存储结构：Redis 位图（`SETBIT/GETBIT`），单 key（`SHOP_BLOOM_KEY`）存整个位图。
- 位图大小：`shopBloomSize = 1 << 20`（约 1,048,576 bit）。
- 哈希方式：FNV-1a + 3 个 seed（`17, 29, 37`）生成 3 个 offset。
- 写入：`bloomAdd` 使用 pipeline 批量 `SETBIT`。
- 查询：`bloomMightContain` 任意一位为 0 即判定“不存在”。

### 本地缓存（二级缓存）
- 使用 BigCache 作为本地缓存，结合 Redis 构建二级缓存，降低 Redis 压力并提升热点访问速度。

## 商铺更新一致性

### 更新/删除缓存策略
- 先更新数据库，再删除缓存。
- 删除失败时通过 Kafka 补偿重试，TTL 作为最终兜底。

> 为什么不是先删缓存再更新 DB：可能被并发读请求把旧数据回写缓存。

## 关注/点赞功能

### 关注/取关
- Redis Set 存储关注列表：`follow:<userId>`。
- 关注：DB 创建记录 + `SADD` 写入 Redis。
- 取关：DB 删除记录 + `SREM` 删除 Redis。
- 共同关注：`SINTER` 求交集。

### 点赞/取消点赞
- Redis ZSet 存储点赞记录：`blog_liked:<blogId>`。
- `ZADD` 记录点赞用户（member=userID），score 为时间戳。
- `ZRange` 获取最早点赞的前 N 个用户。

## 关注流（滚动分页）

### 推模式写入收件箱
- 用户创建笔记时，将 blogId 推送到粉丝收件箱。
- 数据结构：Redis ZSet（score=时间戳，member=blogId）。

### QueryFeed 滚动分页
- 使用 `ZRevRangeByScoreWithScores` 拉取一页 blogId + score。
- 通过 `lastID/offset` 实现滚动分页，处理同分数重复问题。
- 批量查询 DB 获取详情，按 Redis 顺序重排后返回。
