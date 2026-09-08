// API Keys 页（WebUI 重构 C4 → 2026-09-08 多 key 管理）：自动生成 / 手动添加 /
// 逐把复制 / 逐把删除。status 常态只回脱敏 key（FirstKey 明文是 GO_PORT §17.8
// 已知待办，前端刻意不展示该字段）；明文仅在显式动作后短暂出现——生成响应、
// 复制按钮按需 GET /api/security/keys/reveal?index=N。

import { apiGet, apiPost } from '../assets/api.js';
import { esc, showToast, confirmDialog, copyText, renderError } from '../assets/ui.js';
import { icon } from '../assets/icons.js';

const REFRESH_MS = 15000;

export const page = {
  polls: [{ fn: () => load(), ms: REFRESH_MS }],

  mount(el) {
    el.innerHTML =
      '<div class="page-head"><div><h1>API Keys</h1><div class="sub">客户端调用鉴权</div></div></div>' +
      '<div id="keys-body"><div class="skeleton skeleton-block"></div></div>' +
      '<div class="card" id="keys-form" data-form><div class="card-title">' + icon('plus') + '新增 Key</div>' +
      '<div class="copy-field">' +
      '<button type="button" class="btn btn-primary" id="keys-gen">' + icon('key', 'icon icon-sm') + '自动生成 Key</button>' +
      '<input class="input" id="keys-new" placeholder="或手动粘贴 key（sk-...）" autocomplete="off" spellcheck="false">' +
      '<button type="button" class="btn" id="keys-add">添加</button>' +
      '</div>' +
      '<div class="field"><span class="desc" id="keys-source-desc"></span></div>' +
      '<div class="field"><span class="desc">客户端以 Authorization: Bearer 或 X-Api-Key 携带；WebUI 管理端登录与这是两套域。</span></div></div>';

    document.getElementById('keys-gen').addEventListener('click', generateKey);
    document.getElementById('keys-add').addEventListener('click', addManualKey);

    // 列表操作委托（body 元素跨轮询常驻，只绑一次；load() 只换 innerHTML）
    const body = document.getElementById('keys-body');
    body.addEventListener('click', onListAction);

    load();
  },

  unmount() {},
};

async function load() {
  const body = document.getElementById('keys-body');
  if (!body) return;

  let st;
  try {
    st = await apiGet('/api/security/status');
  } catch (e) {
    if (e.message === '未登录') return;
    renderError(body, e.message, load);
    return;
  }
  if (!body.isConnected) return;

  const on = !!st.authRequired;
  const stateBadge = on
    ? '<span class="badge badge-success">已启用鉴权</span>'
    : '<span class="badge badge-warning">免密模式</span>';
  const keys = st.maskedKeys || [];
  const rows = keys
    .map((k, i) =>
      '<tr>' +
      '<td data-label="Key"><span class="num">' + esc(k) + '</span></td>' +
      '<td data-label="操作">' +
      '<button type="button" class="btn btn-sm" data-act="copy" data-i="' + i + '">' + icon('copy', 'icon icon-sm') + '复制</button> ' +
      '<button type="button" class="btn btn-sm btn-danger" data-act="del" data-i="' + i + '">' + icon('trash', 'icon icon-sm') + '删除</button>' +
      '</td></tr>')
    .join('');

  body.innerHTML =
    '<div class="card"><div class="card-title">' + icon('shield') + '当前状态 ' + stateBadge + '</div>' +
    '<div class="field"><label>已配置 Key 数量</label><div class="num">' + (st.configuredKeysCount ?? 0) + '</div></div>' +
    (keys.length
      ? '<div class="table-wrap"><table><thead><tr><th>Key（脱敏）</th><th>操作</th></tr></thead><tbody>' + rows + '</tbody></table></div>'
      : '') +
    (on
      ? ''
      : '<div class="banner banner-info">' + icon('alert', 'icon icon-sm') + '<div>未配置客户端 key 时所有 /v1 请求放行（trust mode 是设计如此）。公网部署务必配置 key 或置于反向代理之后。</div></div>') +
    '</div>';

  // env 来源：文件配置不生效（审查记录 P2 #38），禁用在线增删并说明
  const gen = document.getElementById('keys-gen');
  const add = document.getElementById('keys-add');
  const input = document.getElementById('keys-new');
  const desc = document.getElementById('keys-source-desc');
  if (gen && add && input && desc) {
    if (st.source === 'env') {
      gen.disabled = true;
      add.disabled = true;
      input.disabled = true;
      desc.textContent = 'Key 当前由环境变量 CODELY_PROXY_API_KEY 管理；在线增删文件配置不会生效。';
    } else {
      desc.textContent = st.source === 'file'
        ? '保存于 proxy-key.txt（逗号分隔多 key），立即生效。'
        : '自动生成或手动添加后写入 proxy-key.txt，立即生效。';
    }
  }
}

// onListAction：复制（按需取回明文）/ 删除（确认后按索引删）。
async function onListAction(e) {
  const cp = e.target.closest('[data-act="copy"]');
  const del = e.target.closest('[data-act="del"]');
  if (cp) {
    const i = cp.dataset.i;
    try {
      const r = await apiGet('/api/security/keys/reveal?index=' + encodeURIComponent(i));
      const ok = await copyText(r.key || '');
      showToast(ok ? '已复制第 ' + (Number(i) + 1) + ' 把 key' : '复制失败，请手动复制', ok ? 'ok' : 'err');
    } catch (err) {
      showToast(err.message || '取回 key 失败', 'err');
    }
    return;
  }
  if (del) {
    const i = Number(del.dataset.i);
    const ok = await confirmDialog({
      title: '删除 Key',
      text: '确定删除第 ' + (i + 1) + ' 把 key？使用该 key 的客户端将立即 401，不可恢复。',
      confirmText: '删除',
      danger: true,
    });
    if (!ok) return;
    try {
      const r = await apiPost('/api/security/keys/delete', { index: i });
      showToast(r.authRequired ? '已删除该 key' : '已删除全部 key，恢复免密模式');
      load();
    } catch (err) {
      showToast(err.message || '删除失败', 'err');
    }
  }
}

// generateKey：自动生成（免手动输入）→ 立即复制到剪贴板 → 刷新列表。
async function generateKey() {
  const btn = document.getElementById('keys-gen');
  if (!btn || btn.disabled) return;
  btn.disabled = true;
  try {
    const r = await apiPost('/api/security/keys/add', {});
    const ok = await copyText(r.key || '');
    showToast(ok ? '已生成 key 并复制到剪贴板' : '已生成 key，复制失败请用列表内复制按钮', ok ? 'ok' : 'err');
    load();
  } catch (e) {
    showToast(e.message || '生成失败', 'err');
  } finally {
    btn.disabled = false;
  }
}

// addManualKey：手动粘贴添加（一次一把；多把请逐个添加）。
async function addManualKey() {
  const input = document.getElementById('keys-new');
  if (!input) return;
  const val = input.value.trim();
  if (!val) {
    showToast('请输入 key，或点「自动生成 Key」', 'err');
    return;
  }
  try {
    await apiPost('/api/security/keys/add', { key: val });
    showToast('已添加 key');
    input.value = '';
    load();
  } catch (e) {
    showToast(e.message || '添加失败', 'err');
  }
}