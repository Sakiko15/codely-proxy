// UI 工具（WebUI 重构 C4）：转义/Toast/确认框/复制/格式化/错误态/表单脏检查。

// HTML 转义（所有动态插值必经；不使用 innerHTML 注入原始数据）
export function esc(s) {
  return String(s == null ? '' : s).replace(/[&<>"']/g, (c) => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
  }[c]));
}

/* ---- Toast（右下堆叠 ≤3） ---- */

function toastStack() {
  return document.getElementById('toast-stack');
}

export function showToast(message, kind = 'ok') {
  const stack = toastStack();
  if (!stack) return;
  while (stack.children.length >= 3) stack.firstChild.remove();

  const el = document.createElement('div');
  el.className = 'toast' + (kind === 'err' ? ' toast-err' : ' toast-ok');
  const icon = kind === 'err'
    ? '<svg class="icon icon-sm" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><circle cx="12" cy="12" r="9"/><path d="M12 8v5"/><path d="M12 16.5v.01"/></svg>'
    : '<svg class="icon icon-sm" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><circle cx="12" cy="12" r="9"/><path d="m8.5 12.5 2.5 2.5 4.5-5"/></svg>';
  el.innerHTML = icon + '<span>' + esc(message) + '</span>';
  stack.appendChild(el);

  setTimeout(() => {
    el.classList.add('toast-out');
    setTimeout(() => el.remove(), 320);
  }, 3200);
}

/* ---- 确认框（替代原生 confirm；返回 Promise<boolean>） ---- */

export function confirmDialog({ title, text, confirmText = '确认', danger = false }) {
  return new Promise((resolve) => {
    const scrim = document.createElement('div');
    scrim.className = 'modal-scrim';
    scrim.innerHTML =
      '<div class="modal-box" role="dialog" aria-modal="true" aria-label="' + esc(title) + '">' +
      '<div class="modal-title">' + esc(title) + '</div>' +
      '<div class="modal-text">' + esc(text) + '</div>' +
      '<div class="modal-actions">' +
      '<button type="button" class="btn" data-act="cancel">取消</button>' +
      '<button type="button" class="btn ' + (danger ? 'btn-danger' : 'btn-primary') + '" data-act="ok">' + esc(confirmText) + '</button>' +
      '</div></div>';
    document.body.appendChild(scrim);

    const close = (val) => {
      scrim.remove();
      document.removeEventListener('keydown', onKey);
      resolve(val);
    };
    const onKey = (e) => {
      if (e.key === 'Escape') close(false);
    };
    document.addEventListener('keydown', onKey);
    scrim.addEventListener('click', (e) => {
      if (e.target === scrim) close(false);
      else if (e.target.dataset.act === 'ok') close(true);
      else if (e.target.dataset.act === 'cancel') close(false);
    });
  });
}

/* ---- 复制（clipboard API + execCommand 降级，非 HTTPS 环境可用） ---- */

export async function copyText(text) {
  if (navigator.clipboard && window.isSecureContext) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch { /* 落入降级 */ }
  }
  return fallbackCopy(text);
}

function fallbackCopy(text) {
  const ta = document.createElement('textarea');
  ta.value = text;
  ta.style.position = 'fixed';
  ta.style.opacity = '0';
  document.body.appendChild(ta);
  ta.select();
  let ok = false;
  try {
    ok = document.execCommand('copy');
  } catch {
    ok = false;
  }
  ta.remove();
  return ok;
}

/* ---- 数字格式化 ---- */

export function formatNum(n) {
  if (n == null || isNaN(n)) return '-';
  return Number(n).toLocaleString('zh-CN');
}

/* ---- 页面加载错误态（含重试，F5 契约） ---- */

export function renderError(el, message, retryFn) {
  el.innerHTML =
    '<div class="error-state">' +
    '<svg class="icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><circle cx="12" cy="12" r="9"/><path d="M12 8v5"/><path d="M12 16.5v.01"/></svg>' +
    '<div>' + esc(message || '加载失败') + '</div>' +
    (retryFn ? '<button type="button" class="btn btn-sm">重试</button>' : '') +
    '</div>';
  if (retryFn) {
    el.querySelector('.btn').addEventListener('click', retryFn);
  }
}

/* ---- 表单脏检查（防轮询覆写正在编辑的表单） ----
   委托捕获 input/change，给最近的 [data-form] 容器标 data-dirty；
   渲染函数写 DOM 前用 isDirty(form) 判断，保存/重载成功后 clearDirty。 */

export function initDirtyTracking(root) {
  const mark = (e) => {
    const box = e.target.closest('[data-form]');
    if (box) box.setAttribute('data-dirty', '1');
  };
  root.addEventListener('input', mark, true);
  root.addEventListener('change', mark, true);
}

export function isDirty(form) {
  return !!form && form.getAttribute('data-dirty') === '1';
}

export function clearDirty(form) {
  if (form) form.removeAttribute('data-dirty');
}