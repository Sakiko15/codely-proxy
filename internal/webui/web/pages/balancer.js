// 负载均衡页（WebUI 重构 C4）：调度开关/模式/池内禁用，patch 即时生效。
// 表单脏检查：用户编辑期间轮询不覆写控件；保存失败清脏+重载+Toast。

import { apiGet, apiPost } from '../assets/api.js';
import { esc, showToast, isDirty, clearDirty, renderError } from '../assets/ui.js';
import { icon } from '../assets/icons.js';

const REFRESH_MS = 15000;

function poolBadge(a) {
  if (a.status === 'cooling') {
    const secs = Math.ceil((a.cooldownRemainingMs || 0) / 1000);
    return '<span class="badge badge-danger">冷却 ' + secs + 's</span>';
  }
  if (a.status === 'disabled') return '<span class="badge badge-muted">已禁用</span>';
  return '<span class="badge badge-success">活跃</span>';
}

export const page = {
  polls: [{ fn: () => load(), ms: REFRESH_MS }],

  mount(el) {
    el.innerHTML =
      '<div class="page-head"><div><h1>负载均衡</h1><div class="sub">调度模式与账号池</div></div></div>' +
      '<div id="bal-body"><div class="skeleton skeleton-block"></div></div>';

    // 委托：池内开关（toggleSlug 即时生效，不整表提交）
    const body = document.getElementById('bal-body');
    body.addEventListener('change', async (e) => {
      const toggle = e.target.closest('input[data-toggle-slug]');
      if (!toggle) return;
      try {
        await apiPost('/api/balancer/config', { toggleSlug: toggle.dataset.toggleSlug });
        showToast('已更新：' + toggle.dataset.toggleSlug);
      } catch (err) {
        showToast(err.message || '更新失败', 'err');
      }
      load(); // 回读真实状态（成功失败都刷新）
    });

    // 全局开关/模式变更：即时 applyPatch
    body.addEventListener('change', async (e) => {
      if (e.target.id !== 'bal-enabled' && e.target.id !== 'bal-mode') return;
      const patch = {};
      if (e.target.id === 'bal-enabled') patch.enabled = e.target.checked;
      else patch.mode = e.target.value;
      try {
        await apiPost('/api/balancer/config', patch);
        showToast('配置已生效');
      } catch (err) {
        showToast(err.message || '保存失败', 'err');
        clearDirty(body);
        load();
      }
    });

    load();
  },

  unmount() {},
};

async function load() {
  const body = document.getElementById('bal-body');
  if (!body) return;

  let st;
  try {
    st = await apiGet('/api/balancer/status');
  } catch (e) {
    if (e.message === '未登录') return;
    renderError(body, e.message, load);
    return;
  }
  if (!body.isConnected) return;

  const accounts = st.accounts || [];
  const inPool = accounts.filter((a) => a.inPool);

  // 脏检查：用户正在编辑表单时不覆写（修"轮询覆写正在编辑的表单"缺陷）
  const form = body.querySelector('[data-form]');
  if (isDirty(form)) return;

  body.innerHTML =
    '<div class="card" data-form>' +
    '<div class="card-title">' + icon('layers') + '调度配置</div>' +
    '<div class="switch-row"><div><label for="bal-enabled">启用负载均衡</label>' +
    '<div class="field"><span class="desc">关闭后请求按注册表顺序取账号。</span></div></div>' +
    '<label class="switch"><input type="checkbox" id="bal-enabled"' + (st.enabled ? ' checked' : '') + '><span class="track"></span></label></div>' +
    '<div class="field"><label for="bal-mode">调度模式</label>' +
    '<select class="input" id="bal-mode">' +
    '<option value="quota-first"' + (st.mode === 'quota-first' ? ' selected' : '') + '>额度优先（并行查额度，分层选择）</option>' +
    '<option value="round-robin"' + (st.mode === 'round-robin' ? ' selected' : '') + '>纯轮询</option>' +
    '</select>' +
    '<span class="desc">改动即时生效，写入 balancer.json。</span></div>' +
    '</div>' +
    '<div class="card"><div class="card-title">' + icon('users') + '账号池（' + inPool.length + '）</div>' +
    (inPool.length
      ? '<div class="table-wrap"><table><thead><tr><th>账号</th><th>状态</th><th>今日剩余</th><th>余额</th><th>池内</th></tr></thead><tbody>' +
        inPool.map((a) => (
          '<tr>' +
          '<td data-label="账号">' + esc(a.slug) + (a.isCurrent ? ' <span class="badge badge-info">激活</span>' : '') + '</td>' +
          '<td data-label="状态">' + poolBadge(a) + '</td>' +
          '<td data-label="今日剩余" class="num">' + fmt(a.dailyRemaining) + '</td>' +
          '<td data-label="余额" class="num">' + fmt(a.billingRemaining) + '</td>' +
          '<td data-label="池内"><label class="switch"><input type="checkbox" data-toggle-slug="' + esc(a.slug) + '"' + (a.status === 'disabled' ? '' : ' checked') + '><span class="track"></span></label></td>' +
          '</tr>'
        )).join('') +
        '</tbody></table></div>'
      : '<div class="empty">' + icon('users') + '池内暂无账号，先到「账号」页添加</div>') +
    '</div>';
  clearDirty(form);
}

function fmt(n) {
  return n == null || isNaN(n) ? '-' : Number(n).toLocaleString('zh-CN');
}