// 应用装配（WebUI 重构 C4）：导航注册/登录流/主题切换/时钟/会话过期接管。
// 页面路由表在此集中注册（C5 的模型/日志页也加在这里）。

import { apiGet, apiPost } from './api.js';
import { Poller } from './poller.js';
import { esc, showToast, initDirtyTracking } from './ui.js';
import { icon } from './icons.js';
import { register as registerRoute, start as startRouter, rerender } from './router.js';
import { page as overview } from '../pages/overview.js';
import { page as accounts } from '../pages/accounts.js';
import { page as balancer } from '../pages/balancer.js';
import { page as keys } from '../pages/keys.js';
import { page as models } from '../pages/models.js';
import { page as logs } from '../pages/logs.js';

/* ---- 导航注册表 ---- */

const NAV = [
  { route: 'overview', label: '总览', icon: 'gauge' },
  { route: 'accounts', label: '账号', icon: 'users' },
  { route: 'balancer', label: '负载均衡', icon: 'layers' },
  { route: 'keys', label: 'API Keys', icon: 'key' },
  { route: 'models', label: '模型', icon: 'cpu' },
  { route: 'logs', label: '日志', icon: 'list' },
];

function buildNav() {
  const item = (n, extra = '') =>
    '<button type="button" class="nav-item ' + extra + '" data-route="' + n.route + '" data-href="#/' + n.route + '">' +
    icon(n.icon) + '<span>' + esc(n.label) + '</span></button>';

  const nav = document.getElementById('nav');
  const tabbar = document.getElementById('tabbar');
  if (nav) nav.innerHTML = NAV.map((n) => item(n)).join('');
  if (tabbar) tabbar.innerHTML = NAV.map((n) => item(n)).join('');

  document.querySelectorAll('.nav-item[data-href]').forEach((el) => {
    el.addEventListener('click', () => {
      location.hash = el.dataset.href;
    });
  });
}

/* ---- 路由注册 ---- */

registerRoute('overview', overview);
registerRoute('accounts', accounts);
registerRoute('balancer', balancer);
registerRoute('keys', keys);
registerRoute('models', models);
registerRoute('logs', logs);

/* ---- 主题：三态循环 auto → light → dark → auto ---- */

const THEME_KEY = 'codely-theme';

function themeMode() {
  try {
    const m = localStorage.getItem(THEME_KEY);
    if (m === 'light' || m === 'dark' || m === 'auto') return m;
  } catch { /* 存储被禁用则恒 auto */ }
  return 'auto';
}

function applyTheme(mode) {
  const resolved = mode === 'light' || mode === 'dark'
    ? mode
    : (matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark');
  document.documentElement.setAttribute('data-theme', resolved);
  return resolved;
}

function cycleTheme() {
  const order = ['auto', 'light', 'dark'];
  const next = order[(order.indexOf(themeMode()) + 1) % order.length];
  try {
    localStorage.setItem(THEME_KEY, next);
  } catch { /* ignore */ }
  renderTheme(next);
}

let mediaHandler = null;

function renderTheme(mode) {
  const resolved = applyTheme(mode);
  const btn = document.getElementById('theme-btn');
  if (!btn) return;
  const meta = {
    auto: { icon: 'contrast', label: '跟随系统' },
    light: { icon: 'sun', label: '亮色' },
    dark: { icon: 'moon', label: '深色' },
  }[mode];
  btn.innerHTML = icon(meta.icon, 'icon icon-sm') + '<span>' + meta.label + '（' + resolved + '）</span>';

  // auto 模式跟随系统切换；手动模式挂起监听
  if (mediaHandler) {
    matchMedia('(prefers-color-scheme: light)').removeEventListener('change', mediaHandler);
    mediaHandler = null;
  }
  if (mode === 'auto') {
    mediaHandler = () => applyTheme('auto');
    matchMedia('(prefers-color-scheme: light)').addEventListener('change', mediaHandler);
  }
}

/* ---- 登录流 ---- */

function showLogin(hint) {
  const modal = document.getElementById('login-modal');
  if (!modal) return;
  modal.hidden = false;
  // F1：未改密前的生成密码首屏提示（后端在首次成功登录后收回该字段）
  const hintBox = modal.querySelector('#login-hint');
  if (hintBox) {
    hintBox.innerHTML = hint
      ? '初始管理密码：<span class="num">' + esc(hint) + '</span>（登录成功一次后此提示消失，请尽快修改）'
      : '';
    hintBox.hidden = !hint;
  }
  const userInput = modal.querySelector('#login-user');
  if (userInput) userInput.focus();
}

function hideLogin() {
  const modal = document.getElementById('login-modal');
  if (modal) modal.hidden = true;
}

let appStarted = false;

function enterApp() {
  hideLogin();
  document.getElementById('app-root').hidden = false;
  buildNav();
  renderTheme(themeMode());
  startRouter(document.getElementById('outlet'));
  if (appStarted) rerender();
  appStarted = true;
}

async function checkAuth() {
  try {
    const r = await apiGet('/api/auth-status');
    if (r.authed) {
      enterApp();
      return;
    }
    showLogin(r.generatedPassword ? r.password : '');
  } catch {
    // auth-status 拉不到（服务重启中等）也展示登录框
    showLogin('');
  }
}

function wireLogin() {
  const form = document.getElementById('login-form');
  if (!form) return;
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    const btn = form.querySelector('#login-submit');
    const errEl = form.querySelector('#login-err');
    btn.disabled = true;
    errEl.hidden = true;
    try {
      await apiPost('/api/login', {
        username: form.querySelector('#login-user').value.trim(),
        password: form.querySelector('#login-pass').value,
      });
      enterApp();
    } catch (err) {
      errEl.textContent = err.message === '未登录' ? '用户名或密码错误' : (err.message || '登录失败');
      errEl.hidden = false;
    } finally {
      btn.disabled = false;
    }
  });

  const logoutBtn = document.getElementById('logout-btn');
  if (logoutBtn) {
    logoutBtn.addEventListener('click', async () => {
      try {
        await apiPost('/api/logout');
      } catch { /* 会话可能已失效，仍回登录页 */ }
      location.reload();
    });
  }

  // 会话过期（api.js 401 派发）：停一切轮询并弹登录框（F4 全局接管）
  window.addEventListener('session:expired', () => {
    Poller.stop();
    showLogin('');
  });
}

/* ---- 启动 ---- */

function startClock() {
  const el = document.getElementById('sidebar-clock');
  if (!el) return;
  const tick = () => {
    el.textContent = new Date().toLocaleString('zh-CN', { hour12: false });
  };
  tick();
  setInterval(tick, 1000);
}

initDirtyTracking(document.body);
wireLogin();
startClock();
document.getElementById('theme-btn').addEventListener('click', cycleTheme);
checkAuth();