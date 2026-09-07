// 账号页（WebUI 重构 C4）：账号列表 + 主激活切换/删除 + 设备码登录状态机。
// 保留旧版修复契约：F2 删除 warning 透传、F3 发起按钮防双击、F4 轮询遇会话过期终止、
// F5 错误态重试、F7+P2#34 备注名预校验、S2 授权链接 scheme 加固、轮询 setTimeout 链防在途叠加。

import { apiGet, apiPost } from '../assets/api.js';
import {
  esc, showToast, confirmDialog, copyText, renderError, isDirty, clearDirty,
} from '../assets/ui.js';
import { icon } from '../assets/icons.js';

const REFRESH_MS = 15000;

// 页内轮询句柄（模块级：关窗/卸载都能 clearTimeout(pollTimer)）
let pollTimer = null;

function clearPoll() {
  if (pollTimer) {
    clearTimeout(pollTimer);
    pollTimer = null;
  }
}

// 弹窗常驻按钮是否已绑定（modal 常驻 shell，跨页面存续；重复绑定会在每次进账号页时
// 累积一份监听器 → N 次访问后一次复制弹 N 个 toast，审查记录 2026-09-07 P2-C）
let devModalWired = false;

export function wireDevLogin() {
  const btn = document.getElementById('dev-start-btn');
  const nameInput = document.getElementById('dev-name');
  const modal = document.getElementById('dev-login-modal');
  if (!btn || !modal) return;

  // 复制/外开/关闭按钮随 modal 常驻，只绑一次（点击时再读当前链接值）
  if (!devModalWired) {
    modal.querySelector('#dev-copy-btn').addEventListener('click', async () => {
      const url = modal.querySelector('#dev-url-text').value;
      if (!url) return;
      const ok = await copyText(url);
      showToast(ok ? '已复制授权链接' : '复制失败，请手动复制', ok ? 'ok' : 'err');
    });
    modal.querySelector('#dev-open-btn').addEventListener('click', () => {
      const url = modal.querySelector('#dev-url-text').value;
      if (url) window.open(url, '_blank', 'noopener');
    });
    modal.querySelectorAll('[data-close]').forEach((el) => {
      el.addEventListener('click', () => {
        closeDevModal(modal);
      });
    });
    devModalWired = true;
  }

  // 发起按钮在 outlet 内，每次 mount 重建，须随 mount 重绑
  btn.addEventListener('click', async () => {
    const name = (nameInput.value || '').trim();

    // F7+P2#34：备注名预校验（与后端 Slugify 规则一致：字母数字/点号/连字符/下划线，
    // 保留字与长度）；备注名可选，留空不校验（复审 2026-09-07：空串必败导致无法发起登录）
    const vErr = name ? validateName(name) : '';
    if (vErr) {
      showToast(vErr, 'err');
      return;
    }

    clearPoll(); // F3：发起前先清旧轮询句柄，防旧链在途叠加
    btn.disabled = true; // F3：防双击

    try {
      const r = await apiPost('/api/account/login/start', { name });
      openDevModal(modal, r.login);
      pollDevLogin();
    } catch (e) {
      showToast(e.message || '发起授权失败', 'err');
    } finally {
      btn.disabled = false;
    }
  });
}

function validateName(name) {
  // 字符集对齐后端 Slugify（registry.go slugSepRe：._- 均保留，21ea933）
  if (!/^[A-Za-z0-9._-]{1,64}$/.test(name)) {
    return '备注名仅支持字母数字、点号、连字符与下划线（≤64 字符）';
  }
  if (name === 'index') {
    return '备注名 index 为保留字，请换一个';
  }
  return '';
}

function openDevModal(modal, login) {
  // S2：授权链接只接受 http(s)，防 start 异常注入其他 scheme
  const rawUrl = (login && login.verification_uri_complete) || '';
  const safeUrl = /^https?:/i.test(rawUrl) ? rawUrl : '';

  modal.querySelector('#dev-url-text').value = safeUrl;
  modal.querySelector('#dev-user-code').textContent = (login && login.user_code) || '-';
  modal.querySelector('#dev-status-text').textContent = '等待授权中...';
  modal.hidden = false;
}

function closeDevModal(modal) {
  modal.hidden = true;
  clearPoll();
  // best-effort 取消：失败静默（会话可能已终结）
  apiPost('/api/account/login/cancel').catch(() => {});
}

async function pollDevLogin() {
  const modal = document.getElementById('dev-login-modal');
  if (!modal || modal.hidden) return;

  const statusEl = modal.querySelector('#dev-status-text');
  let r;
  try {
    r = await apiGet('/api/account/login/status');
  } catch (e) {
    if (e.message === '未登录') return; // F4：会话过期，api.js 已触发全局过期流程，终止轮询
    statusEl.textContent = e.message || '轮询失败';
    pollTimer = setTimeout(pollDevLogin, 3000); // 瞬时网络错误不终局，降频续轮
    return;
  }

  if (r.status === 'authorized') {
    statusEl.textContent = '授权成功';
    showToast('账号已加入：' + ((r.account && r.account.name) || ''), 'ok');
    closeDevModal(modal);
    loadAccounts();
    return;
  }
  if (r.status === 'denied' || r.status === 'expired' || r.status === 'error') {
    // 终态：透传原因，允许关窗后重新发起
    statusEl.textContent = r.message || r.error || r.status;
    return;
  }

  // pending / idle：消费后端 message（不吞 slow_down 等提示）
  statusEl.textContent = r.message || '等待授权中...';
  pollTimer = setTimeout(pollDevLogin, 3000);
}

/* ---- 列表与操作 ---- */

export const page = {
  polls: [{ fn: () => loadAccounts(), ms: REFRESH_MS }],

  mount(el) {
    el.innerHTML =
      '<div class="page-head"><div><h1>账号</h1><div class="sub">池内账号与设备码登录</div></div></div>' +
      '<div id="acc-body"></div>' +
      '<div class="card" id="acc-add-card"><div class="card-title">' + icon('plus') + '添加账号（设备码登录）</div>' +
      '<div class="copy-field">' +
      '<input class="input" id="dev-name" placeholder="备注名（字母数字，可选）" maxlength="64">' +
      '<button type="button" class="btn btn-primary" id="dev-start-btn">发起授权</button>' +
      '</div><div class="field"><span class="desc">发起后将在新窗口打开 Codely 授权页，确认后账号自动入池。</span></div></div>';

    wireDevLogin();
    loadAccounts();

    // 委托：切换/删除
    const body = document.getElementById('acc-body');
    body.addEventListener('click', async (e) => {
      const sw = e.target.closest('[data-act="switch"]');
      const del = e.target.closest('[data-act="delete"]');
      if (sw) {
        const name = sw.dataset.name;
        if (isDirty(body)) clearDirty(body);
        try {
          await apiPost('/api/account/switch', { name });
          showToast('已切换主账号：' + name);
          loadAccounts();
        } catch (err) {
          showToast(err.message || '切换失败', 'err');
        }
      } else if (del) {
        const name = del.dataset.name;
        const ok = await confirmDialog({
          title: '删除账号',
          text: `确定删除账号「${name}」？其凭据与 key 将一并移除，不可恢复。`,
          confirmText: '删除',
          danger: true,
        });
        if (!ok) return;
        try {
          const r = await apiPost('/api/account/delete', { name });
          if (r.warning) showToast(r.warning, 'err'); // F2：删除成功但收尾失败的告警必须透传
          else showToast('已删除：' + name);
          loadAccounts();
        } catch (err) {
          showToast(err.message || '删除失败', 'err');
        }
      }
    });
  },

  unmount() {
    clearPoll();
  },
};

async function loadAccounts() {
  const body = document.getElementById('acc-body');
  if (!body) return;

  let r;
  try {
    r = await apiGet('/api/accounts');
  } catch (e) {
    if (e.message === '未登录') return; // F4：过期流程接管
    renderError(body, e.message, loadAccounts); // F5：错误态 + 重试
    return;
  }
  if (!body.isConnected) return;

  const list = r.list || [];
  if (!list.length) {
    body.innerHTML =
      '<div class="empty">' + icon('users') + '暂无账号，用下方设备码登录添加第一个账号</div>';
    return;
  }

  // 脏检查：用户正在操作时不覆写列表
  if (isDirty(body)) return;

  body.innerHTML =
    '<div class="table-wrap"><table><thead><tr><th>备注名</th><th>团队</th><th>来源</th><th>状态</th><th>操作</th></tr></thead><tbody>' +
    list.map((a) => (
      '<tr>' +
      '<td data-label="备注名">' + esc(a.name) +
      (a.isCurrent ? ' <span class="badge badge-info">主激活</span>' : '') + '</td>' +
      '<td data-label="团队">' + esc(a.teamName || '-') + '</td>' +
      '<td data-label="来源">' + esc(a.source || '-') + '</td>' +
      '<td data-label="状态">' + (a.isCurrent
        ? '<span class="badge badge-success">当前</span>'
        : '<span class="badge badge-muted">备用</span>') + '</td>' +
      '<td data-label="操作">' +
      (a.isCurrent ? '' : '<button type="button" class="btn btn-sm" data-act="switch" data-name="' + esc(a.name) + '">激活</button> ') +
      '<button type="button" class="btn btn-sm btn-danger" data-act="delete" data-name="' + esc(a.name) + '">' + icon('trash', 'icon icon-sm') + '删除</button>' +
      '</td></tr>'
    )).join('') +
    '</tbody></table></div>';
  clearDirty(body);
}