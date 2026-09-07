// hash 路由（WebUI 重构 C4）：#/overview|accounts|balancer|keys|models|logs。
// handleIndex 只注册在 GET /{$}，History API 深链接会 404，hash 路由零后端改动；
// 刷新/分享行为正确。路由表由 main.js 注册（保持 C4/C5 页面文件解耦）。

import { Poller } from './poller.js';

const routes = new Map();

export function register(name, page) {
  routes.set(name, page);
}

function currentRoute() {
  const h = location.hash.replace(/^#\/?/, '');
  return routes.has(h) ? h : 'overview';
}

function markActive(name) {
  document.querySelectorAll('.nav-item[data-route]').forEach((el) => {
    el.classList.toggle('active', el.dataset.route === name);
  });
}

let outlet = null;
let started = false;
let currentPage = null;

export function start(el) {
  outlet = el;
  if (started) return;
  started = true;
  window.addEventListener('hashchange', () => render());
  render();
}

// 重新渲染当前页（登录后进入应用时调用）
export function rerender() {
  if (started) render();
}

function render() {
  if (!outlet) return;
  const name = currentRoute();
  const page = routes.get(name);
  if (!page) return;

  // 旧页卸载：先停轮询，再清 DOM（unmount 可清理页内计时器）
  if (currentPage && currentPage.unmount) {
    try {
      currentPage.unmount();
    } catch { /* 卸载失败不阻塞切页 */ }
  }
  Poller.stop();
  outlet.innerHTML = '';

  markActive(name);
  currentPage = page;
  page.mount(outlet);
  if (page.polls && page.polls.length) Poller.set(page.polls);
}