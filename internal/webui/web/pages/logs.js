// 请求日志页（WebUI 重构 C5）：环形缓冲快照 + since 游标增量 + kind 过滤。
// 条目倒序（新→旧）；SSE 长流在流结束时才入环，实时性以完成序为准。
// 增量合并：新条目前插进本地视图，旧条目保留（环形缓冲总量 ≤256，全量重拉无必要）。

import { apiGet } from '../assets/api.js';
import { esc, formatNum, renderError } from '../assets/ui.js';
import { icon } from '../assets/icons.js';

const REFRESH_MS = 5000;
const PAGE_LIMIT = 200;

// kind → 展示（徽章变体 + 文案）
const KINDS = {
  ok: { label: '成功', badge: 'badge-success' },
  quota: { label: '额度', badge: 'badge-warning' },
  denied: { label: '模型拒绝', badge: 'badge-danger' },
  auth: { label: '鉴权', badge: 'badge-danger' },
  rejected: { label: '已拒', badge: 'badge-warning' },
  error: { label: '错误', badge: 'badge-danger' },
};

let kindFilter = '';  // 空 = 全部
let lastSeq = 0;      // 增量游标：只拉比它新的
let fullReload = true;
let view = [];        // 本地展示视图（新→旧，已按 kind 过滤，上限 PAGE_LIMIT）
let total = 0;
let dropped = 0;

export const page = {
  polls: [{ fn: () => load(), ms: REFRESH_MS }],

  mount(el) {
    fullReload = true;
    lastSeq = 0;
    view = [];
    el.innerHTML =
      '<div class="page-head"><div><h1>请求日志</h1><div class="sub">最近请求（环形缓冲，按完成序）</div></div>' +
      '<div class="page-actions"><select class="input" id="log-kind">' +
      '<option value="">全部类型</option>' +
      Object.entries(KINDS).map(([k, v]) => '<option value="' + k + '">' + esc(v.label) + '</option>').join('') +
      '</select></div></div>' +
      '<div id="logs-body"></div>';

    document.getElementById('log-kind').addEventListener('change', (e) => {
      kindFilter = e.target.value;
      fullReload = true; // 换过滤条件从服务端全量重拉
      lastSeq = 0;
      view = [];
      load();
    });
    load();
  },

  unmount() {},
};

function rowHtml(e) {
  const k = KINDS[e.kind] || { label: e.kind, badge: 'badge-muted' };
  const t = (e.ts || '').replace('T', ' ').slice(11, 23); // HH:MM:SS.mmm
  return (
    '<tr>' +
    '<td data-label="时间" class="num">' + esc(t) + '</td>' +
    '<td data-label="类型"><span class="badge ' + k.badge + '">' + esc(k.label) + '</span></td>' +
    '<td data-label="状态" class="num">' + (e.status || '-') + '</td>' +
    '<td data-label="模型">' + esc(e.model || '-') + '</td>' +
    '<td data-label="账号">' + esc(e.account || '-') + '</td>' +
    '<td data-label="耗时" class="num">' + esc(formatNum(e.durationMs)) + 'ms</td>' +
    '<td data-label="详情" class="wrap">' + esc(e.error || e.path || '-') + '</td>' +
    '</tr>'
  );
}

function render() {
  const body = document.getElementById('logs-body');
  if (!body) return;

  const totalLine = '累计 ' + formatNum(total) + ' 条' + (dropped ? '（已逐出 ' + formatNum(dropped) + '）' : '');
  if (!view.length) {
    body.innerHTML =
      '<div class="banner banner-info">' + icon('list', 'icon icon-sm') + '<div>' + esc(totalLine) +
      (kindFilter ? '，当前过滤下暂无匹配' : '，暂无记录') + '</div></div>';
    return;
  }

  body.innerHTML =
    '<div class="banner banner-info">' + icon('list', 'icon icon-sm') + '<div>' + esc(totalLine) + '</div></div>' +
    '<div class="table-wrap"><table><thead><tr><th>时间</th><th>类型</th><th>状态</th><th>模型</th><th>账号</th><th>耗时</th><th>详情</th></tr></thead><tbody>' +
    view.map(rowHtml).join('') +
    '</tbody></table></div>';
}

async function load() {
  const body = document.getElementById('logs-body');
  if (!body) return;

  const sinceParam = fullReload ? '' : '&since=' + lastSeq;
  let r;
  try {
    r = await apiGet('/api/logs?limit=' + PAGE_LIMIT + sinceParam);
  } catch (e) {
    if (e.message === '未登录') return;
    renderError(body, e.message, () => { fullReload = true; load(); });
    return;
  }
  if (!body.isConnected) return;

  total = r.total || 0;
  dropped = r.dropped || 0;
  const fresh = r.entries || [];
  if (fresh.length && fresh[0].seq > lastSeq) {
    lastSeq = fresh[0].seq; // 倒序：首条即最新完成序
  }

  const filtered = fresh.filter((e) => !kindFilter || e.kind === kindFilter);
  if (fullReload) {
    view = filtered.slice(0, PAGE_LIMIT);
  } else if (filtered.length) {
    view = filtered.concat(view).slice(0, PAGE_LIMIT); // 新条目前插
  }
  fullReload = false;
  render();
}