// 按页轮询（WebUI 重构 C4）：每页注册自己的加载器列表，切页即换。
// setTimeout 链式自续，续期前校验 loader 仍在册（防已切页的旧链在途叠加）；
// document.hidden 时暂停，恢复可见立即刷一轮。

const DEFAULT_MS = 15000;

// 类名不得与下方导出名相同：同模块作用域 `class Poller` + `export const Poller`
// 是重复声明，ESM 编译期 SyntaxError——main.js 起的整张静态模块图在所有浏览器
// 拒绝执行，整站白屏（2026-09-08 线上事故；此前被弹窗恒显回归掩盖）。
class PollerService {
  constructor() {
    this.loaders = [];
    this.timer = null;
    this._onVisible = () => {
      if (!document.hidden && this.loaders.length) this.runAll();
    };
    document.addEventListener('visibilitychange', this._onVisible);
  }

  // loaders: [{fn, ms}]，ms 缺省 15s
  set(loaders) {
    this.loaders = loaders || [];
    this.runAll();
  }

  // 停止一切（登出/会话过期时调用）
  stop() {
    this.loaders = [];
    if (this.timer) {
      clearTimeout(this.timer);
      this.timer = null;
    }
  }

  runAll() {
    if (this.timer) {
      clearTimeout(this.timer);
      this.timer = null;
    }
    if (document.hidden || !this.loaders.length) return;
    for (const loader of this.loaders) {
      try {
        loader.fn();
      } catch {
        /* 加载器内部自行兜错，这里防调度被单个异常打断 */
      }
    }
    this.schedule();
  }

  schedule() {
    if (!this.loaders.length) return;
    const minMs = Math.min(...this.loaders.map((l) => l.ms || DEFAULT_MS));
    this.timer = setTimeout(() => {
      // 只续当前在册的链：切页后旧 timer 到期直接放弃
      if (!this.loaders.length || document.hidden) return;
      for (const loader of this.loaders) {
        if (!this.loaders.includes(loader)) continue;
        try {
          loader.fn();
        } catch { /* 同上 */ }
      }
      this.schedule();
    }, minMs);
  }
}

export const Poller = new PollerService();