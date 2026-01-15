import http from 'k6/http';
import { check, sleep } from 'k6';
import { SharedArray } from 'k6/data';
import { Counter, Rate } from 'k6/metrics';

const baseUrl = __ENV.BASE_URL || 'http://127.0.0.1:8081';
const voucherId = __ENV.VOUCHER_ID || '12';
const tokensFile = __ENV.TOKENS_FILE || 'tokens.csv';
const rampWindow = __ENV.RAMP_WINDOW || '10s';

const tokens = new SharedArray('tokens', () => {
  const lines = open(tokensFile)
    .split(/\r?\n/)
    .map((line) => line.trim())
    .filter((line) => line.length > 0);
  return lines
    .map((line) => line.split(',')[0].trim())
    .filter((token) => token.length > 0);
});

const successCount = new Counter('seckill_success');
const failureCount = new Counter('seckill_failure');
const status200 = new Counter('status_200');
const status400 = new Counter('status_400');
const status401 = new Counter('status_401');
const status429 = new Counter('status_429');
const status500 = new Counter('status_500');
const bizSuccessRate = new Rate('biz_success_rate');

export const options = {
  scenarios: {
    flash_sale: {
      executor: 'per-vu-iterations',
      vus: tokens.length,
      iterations: 1,
      maxDuration: '2m',
    },
  },
  thresholds: {
    // Keep business success rate visible; remove strict http_req_failed threshold for flash-sale tests.
    biz_success_rate: ['rate>=0'],
  },
};

export default function () {
  const token = tokens[__VU - 1];
  if (!token) {
    failureCount.add(1);
    return;
  }

  // Spread requests over a ramp window to mimic real traffic.
  const rampMs = parseDurationMs(rampWindow);
  if (rampMs > 0) {
    sleep(Math.random() * (rampMs / 1000));
  }

  const url = `${baseUrl}/voucher-order/seckill/${voucherId}`;
  const res = http.post(url, null, {
    headers: {
      authorization: token,
    },
  });

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

  if (res.status === 200) status200.add(1);
  else if (res.status === 400) status400.add(1);
  else if (res.status === 401) status401.add(1);
  else if (res.status === 429) status429.add(1);
  else if (res.status >= 500) status500.add(1);

  if (!ok) {
    bizSuccessRate.add(0);
    failureCount.add(1);
    return;
  }

  const body = res.json();
  if (body && body.success === true) {
    bizSuccessRate.add(1);
    successCount.add(1);
  } else {
    bizSuccessRate.add(0);
    failureCount.add(1);
  }
}

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

export function handleSummary(data) {
  const totalRequests = data.metrics.http_reqs
    ? data.metrics.http_reqs.values.count
    : 0;
  const totalDuration = data.state ? Math.round(data.state.testRunDurationMs) : 0;
  const p95 = data.metrics.http_req_duration
    ? Math.round(data.metrics.http_req_duration.values['p(95)'])
    : 0;
  const avg = data.metrics.http_req_duration
    ? Math.round(data.metrics.http_req_duration.values.avg)
    : 0;
  const failRate = data.metrics.http_req_failed
    ? data.metrics.http_req_failed.values.rate
    : 0;

  const bizRate = data.metrics.biz_success_rate
    ? data.metrics.biz_success_rate.values.rate
    : 0;
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
