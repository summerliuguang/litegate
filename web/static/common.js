/* LiteGate 管理台共享 JS：布局注入、token 管理、API 封装、格式化 */
'use strict';

const TOKEN_KEY = 'lg_token';

function getToken() { return localStorage.getItem(TOKEN_KEY) || ''; }
function setToken(t) { localStorage.setItem(TOKEN_KEY, t); }
function clearToken() { localStorage.removeItem(TOKEN_KEY); }

// 统一 API 封装：自动带 Bearer；401 时跳登录页（login 页自身不跳）
async function api(path, opts = {}) {
  const headers = Object.assign({}, opts.headers || {});
  if (opts.body !== undefined) headers['Content-Type'] = 'application/json';
  const tok = getToken();
  if (tok) headers['Authorization'] = 'Bearer ' + tok;
  const resp = await fetch(path, Object.assign({}, opts, { headers }));
  if (resp.status === 401 && !location.pathname.endsWith('/login.html')) {
    clearToken();
    location.href = '/login.html';
    throw new Error('未登录或会话已过期');
  }
  let data = null;
  try { data = await resp.json(); } catch (_) { /* 空响应 */ }
  if (!resp.ok) {
    const msg = (data && data.error) ? data.error : ('HTTP ' + resp.status);
    throw new Error(msg);
  }
  return data;
}

// 侧边栏 + 手机折叠菜单（data-page 标记当前页）
const NAV_ITEMS = [
  { id: 'dashboard', href: '/',            icon: '', label: '仪表盘' },
  { id: 'channels',  href: '/channels.html', icon: '', label: '渠道管理' },
  { id: 'keys',      href: '/keys.html',     icon: '', label: '虚拟密钥' },
  { id: 'logs',      href: '/logs.html',     icon: '', label: '请求日志' },
  { id: 'prices',    href: '/prices.html',   icon: '', label: '模型价格' },
];

function renderNav() {
  const page = document.body.dataset.page || '';
  const link = (it, cls) =>
    `<li><a href="${it.href}" class="${cls}${page === it.id ? ' on' : ''}">${it.label}</a></li>`;
  const aside = document.createElement('aside');
  aside.className = 'side';
  aside.innerHTML = `
    <div class="logo">LiteGate</div>
    <aside class="menu">
      <p class="menu-label">管理</p>
      <ul class="menu-list">${NAV_ITEMS.map(i => link(i, '')).join('')}</ul>
      <p class="menu-label">其他</p>
      <ul class="menu-list">
        <li><a href="#" id="navLogout">退出登录</a></li>
      </ul>
    </aside>
    <div class="foot">LiteGate AI 网关<br>数据面与管理共用入口</div>`;
  document.body.prepend(aside);

  const det = document.createElement('details');
  det.className = 'mnav';
  det.innerHTML = `<summary>菜单</summary>
    <ul class="menu-list" style="margin-top:.5rem">${NAV_ITEMS.map(i => link(i, '')).join('')}</ul>`;
  // 插到 .wrap 之前
  const wrap = document.querySelector('.wrap');
  wrap.parentNode.insertBefore(det, wrap);

  aside.querySelector('#navLogout').addEventListener('click', e => {
    e.preventDefault();
    clearToken();
    location.href = '/login.html';
  });
}

// 格式化辅助
function fmtInt(n) { return Number(n || 0).toLocaleString('zh-CN'); }
function fmtUSD(n) {
  n = Number(n || 0);
  return n >= 100 ? ('$' + n.toFixed(1)) : n >= 1 ? ('$' + n.toFixed(2)) : ('$' + n.toFixed(4));
}
function fmtTok(n) {
  n = Number(n || 0);
  if (n >= 1e6) return (n / 1e6).toFixed(2) + 'M';
  if (n >= 1e3) return (n / 1e3).toFixed(1) + 'k';
  return String(n);
}
// "YYYY-MM-DD HH:MM:SS"(UTC 文本) → 本地显示
function fmtTs(ts) {
  if (!ts) return '--';
  const d = new Date(ts.replace(' ', 'T') + 'Z');
  if (isNaN(d)) return ts;
  const p = x => String(x).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}
function esc(s) {
  return String(s == null ? '' : s).replace(/[&<>"']/g,
    c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}
function toast(msg, ok = true) {
  const el = document.createElement('div');
  el.className = 'notification ' + (ok ? 'is-success' : 'is-danger');
  el.style.cssText = 'position:fixed;top:1rem;right:1rem;z-index:50;max-width:380px;padding:.8rem 1rem';
  el.textContent = msg;
  document.body.appendChild(el);
  setTimeout(() => el.remove(), 3000);
}
async function copyText(text) {
  try { await navigator.clipboard.writeText(text); toast('已复制'); }
  catch (_) {
    const ta = document.createElement('textarea');
    ta.value = text; document.body.appendChild(ta); ta.select();
    document.execCommand('copy'); ta.remove(); toast('已复制');
  }
}
