// 数据层（WebUI 重构 C4）：fetch 封装——超时/在途去重/统一错误/401 会话过期。
// 401 语义（原 F6/F4 契约）：豁免名单内的路径 401 不触发 showLogin（登录框自身在用）；
// 其余 401 派发 session:expired 事件（main.js 停轮询并弹登录框）。

const DEFAULT_TIMEOUT_MS = 10_000;

// 这些端点的 401 是"密码错误"而非"会话过期"，不得触发全局过期流程
const EXEMPT_401 = ['/api/auth-status', '/api/login'];

export class ApiError extends Error {
  constructor(status, message) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
  }
}

// 在途请求去重：同 method+path 并发只有一个真实请求共享结果
const inflight = new Map();

async function request(method, path, body, opts = {}) {
  const key = method + ' ' + path;
  const hit = inflight.get(key);
  if (hit) return hit;

  const p = doRequest(method, path, body, opts).finally(() => inflight.delete(key));
  inflight.set(key, p);
  return p;
}

async function doRequest(method, path, body, opts) {
  const ctrl = new AbortController();
  const timeoutMs = opts.timeoutMs === undefined ? DEFAULT_TIMEOUT_MS : opts.timeoutMs;
  const timer = timeoutMs > 0 ? setTimeout(() => ctrl.abort(), timeoutMs) : null;

  try {
    const init = {
      method,
      headers: { 'Content-Type': 'application/json' },
      signal: ctrl.signal,
      credentials: 'same-origin',
    };
    if (body !== undefined && body !== null) init.body = JSON.stringify(body);

    const res = await fetch(path, init);
    let data = null;
    try {
      data = await res.json();
    } catch {
      /* 非 JSON（如空响应）按 null 处理 */
    }

    if (!res.ok) {
      const msg = (data && (data.error || data.message)) || `请求失败（HTTP ${res.status}）`;
      if (res.status === 401 && !EXEMPT_401.includes(path)) {
        // 会话过期：通知全局停轮询、弹登录框（F4），再抛给调用方
        window.dispatchEvent(new CustomEvent('session:expired'));
        throw new ApiError(401, '未登录');
      }
      throw new ApiError(res.status, msg);
    }
    return data;
  } catch (err) {
    if (err.name === 'AbortError') {
      throw new ApiError(0, '请求超时');
    }
    throw err;
  } finally {
    if (timer) clearTimeout(timer);
  }
}

export function apiGet(path, opts) {
  return request('GET', path, undefined, opts);
}

export function apiPost(path, body, opts) {
  return request('POST', path, body === undefined ? {} : body, opts);
}