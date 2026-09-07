// API Keys 页（WebUI 重构 C4）：客户端鉴权状态与 key 管理。
// 注意：security.Status.FirstKey 明文返回是 GO_PORT §17.8 已知待办——前端刻意不展示该字段。

import { apiGet, apiPost } from '../assets/api.js';
import { esc, showToast, isDirty, clearDirty, renderError } from '../assets/ui.js';
import { icon } from '../assets/icons.js';

const REFRESH_MS = 15000;

export const page = {
  polls: [{ fn: () => load(), ms: REFRESH_MS }],

  mount(el) {
    el.innerHTML =
      '<div class="page-head"><div><h1>API Keys</h1><div class="sub">客户端调用鉴权</div></div></div>' +
      '<div id="keys-body"><div class="skeleton skeleton-block"></div></div>' +
      '<div class="card" id="keys-form" data-form><div class="card-title">' + icon('key') + '设置客户端 Key</div>' +
      '<div class="field"><label for="keys-input">API Key（多个用逗号分隔；留空恢复免密）</label>' +
      '<input class="input" id="keys-input" placeholder="sk-...">' +
      '<span class="desc" id="keys-source-desc"></span></div>' +
      '<button type="button" class="btn btn-primary" id="keys-save">保存</button>' +
      '<div class="field"><span class="desc">客户端以 Authorization: Bearer 或 X-Api-Key 携带；WebUI 管理端登录与这是两套域。</span></div></div>';

    document.getElementById('keys-save').addEventListener('click', saveKeys);
    load();
  },

  unmount() {},
};

function maskedHtml(st) {
  const keys = st.maskedKeys || [];
  if (!keys.length) return '';
  return (
    '<div class="field"><label>已配置的 Key</label>' +
    keys.map((k) => '<div class="num">' + esc(k) + '</div>').join('') +
    '</div>'
  );
}

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

  const form = document.getElementById('keys-form');
  const on = !!st.authRequired;
  const stateBadge = on
    ? '<span class="badge badge-success">已启用鉴权</span>'
    : '<span class="badge badge-warning">免密模式</span>';

  body.innerHTML =
    '<div class="card"><div class="card-title">' + icon('shield') + '当前状态 ' + stateBadge + '</div>' +
    '<div class="field"><label>已配置 Key 数量</label><div class="num">' + (st.configuredKeysCount ?? 0) + '</div></div>' +
    maskedHtml(st) +
    (on
      ? ''
      : '<div class="banner banner-info">' + icon('alert', 'icon icon-sm') + '<div>未配置客户端 key 时所有 /v1 请求放行（trust mode 是设计如此）。公网部署务必配置 key 或置于反向代理之后。</div></div>') +
    '</div>';

  // env 来源提示：来源为 env 时输入框占位符提示保存将覆盖 env
  const input = document.getElementById('keys-input');
  const desc = document.getElementById('keys-source-desc');
  if (input && desc) {
    if (st.source === 'env') {
      input.placeholder = '（当前来自 CODELY_PROXY_API_KEY）';
      desc.textContent = 'Key 当前来自环境变量；在此保存将写入数据目录并覆盖 env 值。';
    } else {
      input.placeholder = 'sk-...';
      desc.textContent = '保存后写入 proxy-key.txt，立即生效。';
    }
  }
  clearDirty(form);
}

async function saveKeys() {
  const input = document.getElementById('keys-input');
  if (!input) return;
  const val = input.value.trim();
  try {
    await apiPost('/api/security/config', { apiKey: val });
    if (val) showToast('已启用鉴权保护');
    else showToast('已恢复免密');
    clearDirty(document.getElementById('keys-form'));
    load();
  } catch (e) {
    showToast(e.message || '保存失败', 'err');
  }
}