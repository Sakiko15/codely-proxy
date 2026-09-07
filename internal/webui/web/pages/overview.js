// 总览页（WebUI 重构 C4）：池状态统计卡 + 聚合额度进度 + 冷却警示 + 当前账号配额快照。
// 数据复用 /api/balancer/status + /api/quota（api.js 在途去重，消除旧版重复请求）。

import { apiGet, apiPost } from '../assets/api.js';
import { esc, formatNum, renderError, showToast, isDirty, clearDirty } from '../assets/ui.js';
import { icon } from '../assets/icons.js';

const REFRESH_MS = 15000;

function statCard(label, value, iconName, hint = '') {
  return (
    '<div class="stat"><div class="label">' + icon(iconName) + esc(label) + '</div>' +
    '<div class="value num">' + esc(value) + '</div>' +
    (hint ? '<div class="hint">' + esc(hint) + '</div>' : '') +
    '</div>'
  );
}

function progressRow(label, used, total) {
  const pct = total > 0 ? Math.min(100, Math.round((used / total) * 100)) : 0;
  const cls = pct > 90 ? 'danger' : pct > 70 ? 'warn' : '';
  return (
    '<div class="field"><label>' + esc(label) +
    '<span class="num label-aux">' + esc(formatNum(used)) + ' / ' + esc(formatNum(total)) + '（' + pct + '%）</span></label>' +
    '<div class="progress"><div class="fill ' + cls + '" data-pct="' + pct + '"></div></div></div>'
  );
}

function applyProgressFills(root) {
  // CSP 就绪：宽度经 CSSOM 赋值，不用内联 style 属性
  root.querySelectorAll('.progress .fill[data-pct]').forEach((el) => {
    el.style.width = el.dataset.pct + '%';
  });
}

function statusBadge(a) {
  if (a.status === 'cooling') {
    const secs = Math.ceil((a.cooldownRemainingMs || 0) / 1000);
    return '<span class="badge badge-danger">冷却 ' + secs + 's</span>';
  }
  if (a.status === 'disabled') return '<span class="badge badge-muted">已禁用</span>';
  return '<span class="badge badge-success">活跃</span>';
}

export const page = {
  polls: [
    { fn: () => load(), ms: REFRESH_MS },
  ],

  mount(el) {
    el.innerHTML =
      '<div class="page-head"><div><h1>总览</h1><div class="sub">账号池与额度聚合状态</div></div>' +
      '<div class="page-actions"><button type="button" class="btn" id="ov-refresh">' + icon('refresh', 'icon icon-sm') + '刷新额度</button></div></div>' +
      '<div id="ov-body"></div>';
    el.querySelector('#ov-refresh').addEventListener('click', async () => {
      try {
        await apiPost('/api/quota?force=1');
        showToast('已强制刷新额度快照');
        load();
      } catch (e) {
        showToast(e.message || '刷新失败', 'err');
      }
    });
    load();
  },

  unmount() {},
};

async function load() {
  const body = document.getElementById('ov-body');
  if (!body) return;

  let st, q;
  try {
    const [a, b] = await Promise.all([
      apiGet('/api/balancer/status'),
      apiGet('/api/quota'),
    ]);
    st = a;
    q = b && b.data;
  } catch (e) {
    renderError(body, e.message, load);
    return;
  }
  if (!body.isConnected) return;

  const agg = st.aggregatedQuota || {};
  const accounts = st.accounts || [];
  const cooling = accounts.filter((a) => a.status === 'cooling');
  const cur = (q && q.account) || {};

  // 冷却警示条
  let banner = '';
  if (cooling.length) {
    banner =
      '<div class="banner banner-warning">' + icon('alert', 'icon icon-sm') +
      '<div>' + esc(cooling.length) + ' 个账号冷却中：' +
      esc(cooling.map((a) => a.slug).join('、')) + '（额度类失败自动冷却，到期自动恢复）</div></div>';
  }

  // 账号池统计
  const statsHtml =
    '<div class="stats-grid">' +
    statCard('账号总数', String(st.totalAccounts ?? 0), 'users') +
    statCard('活跃账号', String(st.activeAccounts ?? 0), 'check') +
    statCard('冷却账号', String(st.coolingAccounts ?? 0), 'clock') +
    statCard('负载模式', st.mode === 'quota' ? '额度优先' : '轮询', 'layers') +
    '</div>';

  // 聚合额度（池合计只列数值：与单账号窗口不可比，不做进度条）
  let aggHtml = '';
  if (agg && (agg.dailyRemaining != null || agg.billingRemaining != null)) {
    aggHtml =
      '<div class="card"><div class="card-title">' + icon('gauge') + '池聚合额度</div>' +
      '<div class="field"><label>今日剩余额度（池合计）</label><div class="val-lg num">' + esc(formatNum(agg.dailyRemaining)) + '</div></div>' +
      '<div class="field"><label>充值余额（池合计，点数）</label><div class="val-lg num">' + esc(formatNum(agg.billingRemaining)) + '</div></div>' +
      '</div>';
  }

  // 当前账号配额快照
  let quotaHtml = '';
  if (q) {
    const da = q.dailyAllowance || {};
    const bill = q.billing || {};
    quotaHtml =
      '<div class="card"><div class="card-title">' + icon('star') + '当前账号：' + esc(cur.name || '-') +
      (cur.teamName ? ' <span class="badge badge-info">' + esc(cur.teamName) + '</span>' : '') + '</div>' +
      progressRow('每日赠送额度', da.used_points || 0, da.quota_points || 0) +
      '<div class="field"><label>充值可用点数</label><div class="val-lg num">' +
      esc(formatNum(bill.effective_available_points)) + '</div></div>' +
      (q.rateLimit && q.rateLimit.rpm_limit
        ? '<div class="field"><label>RPM 上限</label><div class="num">' + esc(formatNum(q.rateLimit.rpm_limit)) + '</div></div>'
        : '') +
      '<div class="field"><label>快照时间</label><div>' + esc((q.fetchedAt || '').replace('T', ' ').slice(0, 19) || '-') + '</div></div>' +
      '</div>';
  }

  // 池内账号明细
  let poolHtml = '';
  if (accounts.length) {
    poolHtml =
      '<div class="card"><div class="card-title">' + icon('layers') + '池内账号</div>' +
      '<div class="table-wrap"><table><thead><tr><th>账号</th><th>状态</th><th>今日剩余</th><th>余额</th><th>请求</th></tr></thead><tbody>' +
      accounts.map((a) => {
        const m = a.metrics || {};
        return (
          '<tr><td data-label="账号">' + esc(a.slug) +
          (a.isCurrent ? ' <span class="badge badge-info">激活</span>' : '') +
          (a.inPool ? '' : ' <span class="badge badge-muted">池外</span>') + '</td>' +
          '<td data-label="状态">' + statusBadge(a) + '</td>' +
          '<td data-label="今日剩余" class="num">' + esc(formatNum(a.dailyRemaining)) + '</td>' +
          '<td data-label="余额" class="num">' + esc(formatNum(a.billingRemaining)) + '</td>' +
          '<td data-label="请求" class="num">' + esc(formatNum(m.total || 0)) + ' / 失败 ' + esc(formatNum(m.fail || 0)) + '</td></tr>'
        );
      }).join('') +
      '</tbody></table></div></div>';
  }

  // 表单未被用户编辑才覆写（脏检查惯例；本页只读，防御性保留）
  if (isDirty(body)) return;
  body.innerHTML = banner + statsHtml + aggHtml + quotaHtml + poolHtml;
  applyProgressFills(body);
  clearDirty(body);
}