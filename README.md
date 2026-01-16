### 前端项目启动
**前端部署在nginx中**

**启动nginx**
`brew services start nginx`

**关闭nginx**
`brew services stop nginx`



1. 根据id查询商铺信息 -> 二级缓存（本地 BigCache L1 + Redis L2），分布式锁/逻辑过期 解决缓存击穿，使用 Bloom Filter 解决缓存穿透
2. 优惠券秒杀 -> 使用Lua脚本将库存校验、扣减和下订单放到一个原子操作中，并异步下订单，使用Redis Stream 进行消费

3. 用户签到 -> 使用BitMap 记录每日签到记录，通过用户本月连续签到天数
4. 用户登录 -> 使用手机号和验证码进行登录，并使用Redis Set缓存用户信息，并设置过期时间 -> 需要auth的路径访问后会刷新token的有效期
5. 更新商铺信息 -> 先更新数据库，再删缓存，反之容易造成数据不一致
6. 达人探店 -> 使用SortedSet实现点赞排行榜
7. 好友关注 -> 基于Set实现关注、取消关注、共同关注
8. 附近商户 -> 使用Redis GEO实现地理位置检索与排序


* 消费幂等只针对的是consumeOrders这个方法，也就是createOrderTx（创建订单），并不会从请求入口开始执行

### 商铺查询二级缓存（L1 + L2）
1. 读路径顺序：L1 本地缓存（BigCache）→ Redis → DB
2. L1 TTL 默认 30s（可通过 `SHOP_LOCAL_CACHE_TTL` 覆盖），用于热点加速
3. Redis 命中或 DB 回填后写入 L1，减少重复反序列化与网络开销
4. 更新商铺时：先更新数据库，再删除 Redis，同时删除 L1，避免脏读
5. L1 仅作加速层，不保证强一致，以 Redis/DB 为准

### 脏读 vs 缓存不一致（面试要点）
1. 数据库“脏读”指读取到了未提交的数据，只有在 `Read Uncommitted` 隔离级别下才可能发生。
2. 本项目更常见的问题是“读旧值/缓存不一致”，并非数据库意义上的脏读。
3. 产生“读旧值”的典型窗口：更新后缓存未及时删除、删除缓存失败、并发读在删除前命中旧缓存。
4. 处理方式：更新 DB 后删除 Redis + L1，本地缓存 TTL 较短（30s），降低不一致窗口。

### 秒杀压测（k6）
1. 生成测试 token 并写入 Redis
`go run cmd/gen_tokens/main.go -in hmdp_tb_user.csv -out tokens.csv -redis 127.0.0.1:6379 -db 0`

2. 启动服务并输出日志到文件
`rm -f server.log`
`go run cmd/server/main.go > server.log 2>&1 &`

3. 运行 k6 压测（1000+ 用户抢 100 库存）
`k6 run -e BASE_URL=http://127.0.0.1:8081 -e VOUCHER_ID=12 -e TOKENS_FILE=../../tokens.csv -e RAMP_WINDOW=10s scripts/k6/seckill.js`

4. 查看消费与落库耗时日志
`grep -n "handleConsume success\\|handleConsume failed" server.log`

### DLQ 告警与重放
1. 配置 SMTP（configs/app.yaml）
```
smtp:
  host: "smtp.qq.com"
  port: 465
  user: "your@qq.com"
  pass: "your_smtp_auth_code"
  to: "alert_receiver@qq.com"
```

2. 人工审核后手动选择 DLQ 消息内容（从 Kafka UI 复制 JSON）

3. 测试 DLQ（消费失败重试 3 次后进入 DLQ 并发送告警）
`FORCE_SECKILL_FAIL_COUNT=4 go run cmd/server/main.go > server.log 2>&1 &`
`./scripts/simulate_dlq_consume_fail.sh`

4. 自动重放到主 Topic（默认同步 Redis）
`PAYLOAD='{"orderId":1,"userId":1,"voucherId":12,"createdAt":0,"retryCount":4,"lastError":"..."}' ./scripts/replay_dlq.sh`
`PAYLOAD_FILE=dlq_payload.json ./scripts/replay_dlq.sh`

5. 停止服务与清理日志
`./scripts/stop_server_8081.sh`
`rm -f server.log`

6. 重置库存和订单表(MySQL和Redis)
`./scripts/reset_seckill.sh`
