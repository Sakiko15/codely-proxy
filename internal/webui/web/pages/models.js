// 模型页（WebUI 重构 C5）：别名→上下文窗口静态表 + 后端探测（显式动作，绝不自动触发）。
// 探测成本：5 alias × 3 采样 = 15 次真实最小补全请求烧当前账号额度——确认框明示后才发起；
// 进行中重复触发 409。探测完成后刷新一次，本页不轮询。

import { apiGet, apiPost } from '../assets/api.js';
import { esc, formatNum, showToast, confirmDialog, renderError } from '../assets/ui.js';
import { icon } from '../assets/icons.js';

let probing = false;

export const page = {
  polls: [], // 不轮询：探测进行中由 triggerProbe 自行轮询一次

  mount(el) {
    el.innerHTML =
      '<div class="page-head"><div><h1>模型</h1><div class="sub">别名映射与后端探测</div></div>' +
      '<div class="page-actions"><button type="button" class="btn btn-primary" id="probe-btn">' +
      icon('cpu', 'icon icon-sm') + '探测后端</button></div></div>' +
      '<div id="models-body"><div class="skeleton skeleton-block"></div></div>' +
      '<div class="banner banner-info" id="probe-banner">' + icon('alert', 'icon icon-sm') +
      '<div>「探测后端」对每个别名采样 3 次最小补全请求（共约 15 次真实调用，计入当前账号额度），用于确认别名实际路由的后端与真实上下文窗口。窗口列的静态值来自代理侧映射表（<code>/v1/models</code> 已按它覆写上游虚标值）。</div></div>';

    document.getElementById('probe-btn').addEventListener('click', triggerProbe);
    load();
    // 页面装载时若恰有进行中的探测（他人触发），恢复按钮禁用态
    refreshProbingFlag();
  },

  unmount() {},
};

async function refreshProbingFlag() {
  try {
    const r = await apiGet('/api/models');
    setProbing(!!r.probing);
    if (r.probing) pollProbeDone();
  } catch { /* load() 会展示错误态 */ }
}

function setProbing(on) {
  probing = on;
  const btn = document.getElementById('probe-btn');
  if (btn) btn.disabled = on;
}

async function load() {
  const body = document.getElementById('models-body');
  if (!body) return;

  let r;
  try {
    r = await apiGet('/api/models');
  } catch (e) {
    if (e.message === '未登录') return;
    renderError(body, e.message, load);
    return;
  }
  if (!body.isConnected) return;

  setProbing(!!r.probing);
  const probedAt = r.probedAt
    ? '<span class="badge badge-muted">探测于 ' + esc(r.probedAt.replace('T', ' ').slice(0, 19)) + '</span>'
    : '<span class="badge badge-muted">未探测</span>';

  body.innerHTML =
    '<div class="card"><div class="card-title">' + icon('cpu') + '模型列表 ' + probedAt + '</div>' +
    '<div class="table-wrap"><table><thead><tr><th>别名</th><th>窗口（代理侧）</th><th>实际后端</th><th>后端窗口</th><th>输入</th></tr></thead><tbody>' +
    (r.models || []).map((m) => {
      const probed = !!m.backend || !!m.probeError;
      return (
        '<tr>' +
        '<td data-label="别名"><span class="num">' + esc(m.alias) + '</span></td>' +
        '<td data-label="窗口（代理侧）" class="num">' + esc(formatNum(m.contextWindow)) + '</td>' +
        '<td data-label="实际后端">' + (m.backend
          ? '<span class="badge badge-success">' + esc(m.backend) + '</span>'
          : (m.probeError
            ? '<span class="badge badge-danger" title="' + esc(m.probeError) + '">探测失败</span>'
            : '<span class="badge badge-muted">未探测</span>')) + '</td>' +
        '<td data-label="后端窗口" class="num">' + (m.backendWindow ? esc(formatNum(m.backendWindow)) : '-') + '</td>' +
        '<td data-label="输入">' + (m.input ? esc(m.input) : (probed ? '-' : '')) + '</td>' +
        '</tr>'
      );
    }).join('') +
    '</tbody></table></div></div>';
}

async function triggerProbe() {
  if (probing) return;
  const ok = await confirmDialog({
    title: '发起后端探测？',
    text: '将对 5 个别名各采样 3 次最小补全请求（约 15 次真实调用），消耗当前激活账号额度。探测约需十几秒，期间不可重复发起。',
    confirmText: '开始探测',
    danger: true,
  });
  if (!ok) return;

  try {
    await apiPost('/api/models/probe', {}, { timeoutMs: 0 }); // 202 即返回，不走超时
  } catch (e) {
    if (e.status === 409) showToast('探测进行中，请稍候', 'err');
    else showToast(e.message || '发起探测失败', 'err');
    return;
  }

  setProbing(true);
  showToast('探测已受理，完成后自动刷新');
  pollProbeDone();
}

// 探测是异步任务：仅探测期间局部轮询 /api/models，完成即刷列表并停
async function pollProbeDone() {
  const body = document.getElementById('models-body');
  for (let i = 0; i < 100; i++) {
    await new Promise((res) => setTimeout(res, 1500));
    if (!body || !body.isConnected || !probing) return; // 已切页/已复位
    try {
      const r = await apiGet('/api/models');
      if (!r.probing) {
        setProbing(false);
        if (body.isConnected) load();
        showToast('探测完成');
        return;
      }
    } catch (e) {
      if (e.message === '未登录') return;
      // 瞬时错误继续轮
    }
  }
  setProbing(false);
  showToast('探测超时，请手动刷新', 'err');
}