# 秒杀压测步骤

本文档记录本项目使用 k6 测试秒杀优惠券接口峰值 QPS 的完整步骤。

## 1. 启动依赖

在项目根目录执行：

```bash
docker compose up -d 
```

确认依赖状态：

```bash
docker compose ps
```


## 3. 生成压测用户和 Token

当前登录中间件只校验 Redis 中的 token 信息，不会回表查询 `tb_user`。因此秒杀压测只需要生成手机号 CSV、生成 token，并把 token 写入 Redis。

生成 10000 个手机号和 token：

```bash
COUNT=10000 ./scripts/prepare_seckill_tokens.sh
```

该脚本会完成：

```text
hmdp_tb_user.csv  生成 10000 行手机号
tokens.csv        生成 10000 个 token
Redis             写入 login:token:{token} hash
```

检查数量：

```bash
wc -l hmdp_tb_user.csv tokens.csv
```

如果只想生成 5000 个：

```bash
COUNT=5000 ./scripts/prepare_seckill_tokens.sh
```

## 4. 重置秒杀库存、订单和缓存

每次重新压测前都要重置 Redis 和 MySQL，否则会受到“一人一单”限购集合或旧订单影响。

重置 `voucher_id=12`，库存设置为 `100000`：

```bash
./scripts/reset_seckill.sh 12 100000
```

该脚本会执行：

```text
Redis:
  SET seckill:stock:vid:12 100000
  DEL order:vid:12

MySQL:
  DELETE FROM tb_voucher_order;
  UPDATE tb_seckill_voucher SET stock=100000 WHERE voucher_id=12;
```

可手动检查：

```bash
docker exec redis-hmdp redis-cli -n 0 GET seckill:stock:vid:12
docker exec redis-hmdp redis-cli -n 0 SCARD order:vid:12
docker exec mysql-hmdp mysql -uroot -proot hmdp \
  -e "SELECT COUNT(*) AS orders FROM tb_voucher_order WHERE voucher_id=12; SELECT stock FROM tb_seckill_voucher WHERE voucher_id=12;"
```

## 5. 启动后端服务

```bash
go run cmd/server/main.go
```

确认服务可访问：

```bash
curl http://127.0.0.1:8081/healthz
```

预期返回：

```json
{"status":"ok"}
```

## 6. 执行 k6 秒杀压测

在项目根目录执行：

```bash
k6 run -e BASE_URL=http://127.0.0.1:8081 \
  -e VOUCHER_ID=12 \
  -e TOKENS_FILE=../../tokens.csv \
  -e RAMP_WINDOW=1s \
  scripts/k6/seckill.js
```

参数说明：

```text
BASE_URL      后端服务地址
VOUCHER_ID   秒杀券 ID
TOKENS_FILE  token 文件。k6 的 open() 按脚本目录解析相对路径，脚本在 scripts/k6/ 下，所以根目录 tokens.csv 写成 ../../tokens.csv
RAMP_WINDOW  请求随机打散窗口，1s 表示 10000 个用户尽量集中在 1 秒内发起请求
```

当前脚本使用 `per-vu-iterations`：

```text
VU 数 = tokens.csv 行数
每个 VU 请求 1 次
```

因此 10000 个 token 会发起 10000 次请求。

## 7. 计算 QPS

k6 输出示例：

```text
--- Seckill Summary ---
total requests: 10000
total duration: 1807 ms
avg latency: 523 ms
p95 latency: 1180 ms
http req failed rate: 0.00%
biz success rate: 100.00%
success count: 10000
failure count: 0
status 200: 10000
```

QPS 计算：

```text
QPS = total requests / total duration seconds
QPS = 10000 / 1.807 ≈ 5534
```

这个 QPS 是接口接单 QPS，链路主要是：

```text
Gin -> Redis Lua -> Kafka publish
```

由于订单是 Kafka 异步落库，最终落库 TPS 需要结合 Kafka lag 和 MySQL 订单数一起看。

## 8. 压测后校验落库

查询最终订单数和 DB 库存：

```bash
docker exec mysql-hmdp mysql -uroot -proot hmdp \
  -e "SELECT COUNT(*) AS orders FROM tb_voucher_order WHERE voucher_id=12; SELECT stock FROM tb_seckill_voucher WHERE voucher_id=12;"
```

查询 Redis 库存和限购集合：

```bash
docker exec redis-hmdp redis-cli -n 0 GET seckill:stock:vid:12
docker exec redis-hmdp redis-cli -n 0 SCARD order:vid:12
```

如果压测前库存是 `100000`，成功下单 `10000`，正常情况下应接近：

```text
Redis stock: 90000
Redis order set size: 10000
MySQL orders: 10000
MySQL stock: 90000
```

## 9. 常见问题

### 全部 401

说明 token 没通过登录校验。通常是 Redis token 过期、Redis 重启、token 文件路径错误，或 Redis DB 不一致。

重新生成 token：

```bash
COUNT=10000 ./scripts/prepare_seckill_tokens.sh
```

### 全部 400

说明请求通过登录，但业务被拒绝。常见原因是：

```text
库存不足
重复下单
秒杀未开始或已结束
```

重新压测前执行：

```bash
./scripts/reset_seckill.sh 12 100000
```

### connection refused

说明后端服务没有监听 `8081`。

检查：

```bash
curl http://127.0.0.1:8081/healthz
```

重新启动：

```bash
go run cmd/server/main.go
```

### Docker 中运行 k6

如果 k6 在 Docker 容器中运行，`127.0.0.1` 指向的是 k6 容器自身。此时使用：

```bash
-e BASE_URL=http://host.docker.internal:8081
```

## 10. 面试表述

可以这样描述测试口径：

```text
我用 k6 模拟 10000 个不同用户在 1 秒窗口内请求秒杀接口。
压测前会重置 Redis 库存、Redis 限购集合、MySQL 订单和 DB 库存。
压测结果关注接口 QPS、平均延迟、P95、HTTP 错误率和业务成功率。
由于系统使用 Kafka 异步落库，所以接口 QPS 和订单落库 TPS 是分开的，还会结合 Kafka lag 和 MySQL 订单数验证最终一致性。
```
