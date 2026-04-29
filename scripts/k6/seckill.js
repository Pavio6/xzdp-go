import http from 'k6/http';
import { check, sleep } from 'k6';
import { SharedArray } from 'k6/data';
import { Counter, Rate } from 'k6/metrics';

// k6 脚本通过环境变量接收参数。
// 运行示例：
//   k6 run -e BASE_URL=http://127.0.0.1:8081 \
//     -e VOUCHER_ID=12 \
//     -e TOKENS_FILE=../../tokens.csv \
//     -e RAMP_WINDOW=1s \
//     scripts/k6/seckill.js
//
// __ENV 是 k6 提供的全局对象，用于读取命令行 -e 传入的环境变量。
// 注意：k6 的 open() 读取相对路径时，是相对当前脚本文件所在目录解析。
// 本脚本在 scripts/k6/ 下，所以根目录 tokens.csv 要写成 ../../tokens.csv。
const baseUrl = __ENV.BASE_URL || 'http://127.0.0.1:8081';
const voucherId = __ENV.VOUCHER_ID || '12';
const tokensFile = __ENV.TOKENS_FILE || '../../tokens.csv';
const rampWindow = __ENV.RAMP_WINDOW || '10s';

// SharedArray 用于在所有 VU 之间共享一份只读数据。
// 如果直接在 default function 中读取文件，每个 VU 都会重复读取，压测开销会变大。
//
// tokens.csv 当前每行一个 token；如果以后改成 token,phone 这种格式，
// 这里 line.split(',')[0] 也只会取第一列 token。
const tokens = new SharedArray('tokens', () => {
  const lines = open(tokensFile)
    .split(/\r?\n/)
    .map((line) => line.trim())
    .filter((line) => line.length > 0);
  return lines
    .map((line) => line.split(',')[0].trim())
    .filter((token) => token.length > 0);
});

// 自定义指标。
// Counter：计数器，只增不减，适合统计成功数、失败数、状态码数量。
// Rate：比率指标，适合统计业务成功率。
const successCount = new Counter('seckill_success');
const failureCount = new Counter('seckill_failure');
const status200 = new Counter('status_200');
const status400 = new Counter('status_400');
const status401 = new Counter('status_401');
const status429 = new Counter('status_429');
const status500 = new Counter('status_500');
const bizSuccessRate = new Rate('biz_success_rate');

// options 是 k6 的压测配置。
// 本脚本使用 per-vu-iterations，含义是：
//   - 创建 tokens.length 个虚拟用户（VU）
//   - 每个 VU 执行 1 次请求
//   - 也就是 10000 个 token 就会发起 10000 次请求
//
// 这个模型适合秒杀场景：大量不同用户在短时间内各请求一次。
export const options = {
  scenarios: {
    flash_sale: {
      // 每个 VU 固定执行指定次数。
      executor: 'per-vu-iterations',
      // VU 数等于 token 数，保证每个虚拟用户拿到一个不同 token。
      vus: tokens.length,
      // 每个 VU 只执行一次秒杀请求，符合“一人抢一次”的压测模型。
      iterations: 1,
      // 整个场景最长允许执行 2 分钟，避免异常情况下无限等待。
      maxDuration: '2m',
    },
  },
  thresholds: {
    // 这里没有设置严格阈值，只保留业务成功率指标，避免压测因业务拒绝直接中断。
    // 例如库存不足、重复下单都会导致业务失败，但这类失败在秒杀场景中可能是预期现象。
    biz_success_rate: ['rate>=0'],
  },
};

// default function 是每个 VU 真正执行的逻辑。
// 在本脚本中，每个 VU 只会进入一次。
export default function () {
  // __VU 是 k6 提供的当前虚拟用户编号，从 1 开始。
  // JavaScript 数组从 0 开始，所以这里用 __VU - 1 取 token。
  const token = tokens[__VU - 1];
  if (!token) {
    failureCount.add(1);
    return;
  }

  // 将请求随机打散到 rampWindow 时间窗口内。
  // 例如 RAMP_WINDOW=1s 时，10000 个 VU 会在 0~1 秒之间随机 sleep 后发请求。
  // 这样比所有 VU 完全同一毫秒发出更接近真实流量，也能减少压测端瞬时调度抖动。
  const rampMs = parseDurationMs(rampWindow);
  if (rampMs > 0) {
    sleep(Math.random() * (rampMs / 1000));
  }

  // 秒杀接口地址。
  const url = `${baseUrl}/voucher-order/seckill/${voucherId}`;

  // 发起 POST 请求。
  // 第二个参数是请求体，这里为 null，因为秒杀接口只需要 path 中的 voucherId。
  // authorization 头携带 token，后端 LoginMiddleware 会用它去 Redis 读取登录用户。
  const res = http.post(url, null, {
    headers: {
      authorization: token,
    },
  });

  // check 用来做断言。
  // 这里认为 HTTP 200 且响应能解析为 JSON，才进入后续业务成功判断。
  const ok = check(res, {
    'status is 200': (r) => r.status === 200,
    'json parse ok': (r) => {
      try {
        r.json();
        return true;
      } catch {
        return false;
      }
    },
  });

  // 单独统计常见 HTTP 状态码，方便排查压测失败原因：
  //   200：接口正常返回
  //   400：业务拒绝，例如库存不足、重复下单、秒杀未开始
  //   401：token 无效或过期
  //   429：如果后续加限流，可能出现
  //   5xx：服务端异常
  if (res.status === 200) status200.add(1);
  else if (res.status === 400) status400.add(1);
  else if (res.status === 401) status401.add(1);
  else if (res.status === 429) status429.add(1);
  else if (res.status >= 500) status500.add(1);

  // HTTP 层不满足预期时，直接记为业务失败。
  if (!ok) {
    bizSuccessRate.add(0);
    failureCount.add(1);
    return;
  }

  // 后端统一返回结构中，body.success === true 表示业务成功。
  // 注意：HTTP 200 不一定等于业务成功，所以这里单独判断业务字段。
  const body = res.json();
  if (body && body.success === true) {
    bizSuccessRate.add(1);
    successCount.add(1);
  } else {
    bizSuccessRate.add(0);
    failureCount.add(1);
  }
}

// 将 1s、500ms、2m 这类字符串转换成毫秒。
// k6 的 sleep 接收秒，所以调用处会再除以 1000。
function parseDurationMs(input) {
  const v = String(input || '').trim().toLowerCase();
  if (v.endsWith('ms')) {
    return parseInt(v.slice(0, -2), 10) || 0;
  }
  if (v.endsWith('s')) {
    return (parseFloat(v.slice(0, -1)) || 0) * 1000;
  }
  if (v.endsWith('m')) {
    return (parseFloat(v.slice(0, -1)) || 0) * 60 * 1000;
  }
  return parseInt(v, 10) || 0;
}

// handleSummary 是 k6 在压测结束后调用的汇总函数。
// 这里把 k6 原始 metrics 转换成更适合秒杀压测阅读的摘要输出。
export function handleSummary(data) {
  // http_reqs 是 k6 内置指标，表示总 HTTP 请求数。
  const totalRequests = data.metrics.http_reqs
    ? data.metrics.http_reqs.values.count
    : 0;
  // testRunDurationMs 是本次测试整体耗时，单位毫秒。
  // 可以用 totalRequests / (totalDuration / 1000) 手动计算平均 QPS。
  const totalDuration = data.state ? Math.round(data.state.testRunDurationMs) : 0;
  // http_req_duration 是 k6 内置请求耗时指标。
  // avg 是平均耗时，p(95) 是 95 分位耗时。
  const p95 = data.metrics.http_req_duration
    ? Math.round(data.metrics.http_req_duration.values['p(95)'])
    : 0;
  const avg = data.metrics.http_req_duration
    ? Math.round(data.metrics.http_req_duration.values.avg)
    : 0;
  // http_req_failed 是 k6 内置失败率。
  // 对本脚本来说，非 2xx/3xx 通常会被计入失败，比如 400、401、5xx。
  const failRate = data.metrics.http_req_failed
    ? data.metrics.http_req_failed.values.rate
    : 0;

  // 业务成功率，来自上面自定义的 bizSuccessRate。
  const bizRate = data.metrics.biz_success_rate
    ? data.metrics.biz_success_rate.values.rate
    : 0;

  // 最终打印到 stdout 的摘要。
  // 示例中的 QPS 可以手动计算：
  //   QPS = total requests / (total duration / 1000)
  const lines = [
    '--- Seckill Summary ---',
    `total requests: ${totalRequests}`,
    `total duration: ${totalDuration} ms`,
    `avg latency: ${avg} ms`,
    `p95 latency: ${p95} ms`,
    `http req failed rate: ${(failRate * 100).toFixed(2)}%`,
    `biz success rate: ${(bizRate * 100).toFixed(2)}%`,
    `success count: ${data.metrics.seckill_success ? data.metrics.seckill_success.values.count : 0}`,
    `failure count: ${data.metrics.seckill_failure ? data.metrics.seckill_failure.values.count : 0}`,
    `status 200: ${data.metrics.status_200 ? data.metrics.status_200.values.count : 0}`,
    `status 400: ${data.metrics.status_400 ? data.metrics.status_400.values.count : 0}`,
    `status 401: ${data.metrics.status_401 ? data.metrics.status_401.values.count : 0}`,
    `status 429: ${data.metrics.status_429 ? data.metrics.status_429.values.count : 0}`,
    `status 5xx: ${data.metrics.status_500 ? data.metrics.status_500.values.count : 0}`,
  ];

  return {
    stdout: `${lines.join('\n')}\n`,
  };
}
