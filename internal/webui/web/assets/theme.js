// 主题首帧解析（阻塞加载，防 FOUC）：读 localStorage 主题模式（auto/light/dark），
// auto 跟随系统。与 main.js 的切换逻辑共用同一存储键 'codely-theme'。
(function () {
  var mode = 'auto';
  try { mode = localStorage.getItem('codely-theme') || 'auto'; } catch (e) {}
  var resolved = mode;
  if (resolved !== 'light' && resolved !== 'dark') {
    resolved = window.matchMedia && matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark';
  }
  document.documentElement.setAttribute('data-theme', resolved);
})();