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
  { id: 'dashboard',  href: '/',                 icon: '', label: '仪表盘' },
  { id: 'channels',   href: '/channels.html',    icon: '', label: '渠道管理' },
  { id: 'balances',   href: '/balances.html',    icon: '', label: '余额监控' },
  { id: 'keys',       href: '/keys.html',        icon: '', label: '虚拟密钥' },
  { id: 'logs',       href: '/logs.html',        icon: '', label: '请求日志' },
  { id: 'prices',     href: '/prices.html',      icon: '', label: '模型价格' },
  { id: 'playground', href: '/playground.html',  icon: '', label: '对话测试' },
  { id: 'settings',   href: '/settings.html',    icon: '', label: '设置' },
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
      <p class="menu-label">外观</p>
      <div class="theme-seg" data-theme-seg>
        <button type="button" data-t="system">跟随系统</button>
        <button type="button" data-t="light">日间</button>
        <button type="button" data-t="dark">夜间</button>
      </div>
    </aside>
    <div class="foot">LiteGate AI 网关<br>数据面与管理共用入口</div>`;
  document.body.prepend(aside);

  // 移动端固定顶栏：左侧汉堡图标、右侧当前页面名称；点图标菜单从左滑出
  // （遮罩点击/× 按钮/点任意链接即关闭，逻辑见底部 bindDrawer）
  const cur = NAV_ITEMS.find(i => i.id === page);
  const bar = document.createElement('header');
  bar.className = 'topbar';
  bar.innerHTML = `
    <div class="topbar-left">
      <button class="topbar-icon" aria-label="打开菜单">
        <svg width="21" height="21" viewBox="0 0 24 24" fill="none" stroke="currentColor"
          stroke-width="2.2" stroke-linecap="round" aria-hidden="true">
          <path d="M4 6.5h16"/><path d="M4 12h11"/><path d="M4 17.5h16"/>
        </svg>
      </button>
      <span class="topbar-brand">LiteGate</span>
    </div>
    <span class="topbar-left">
      <button class="theme-quick" id="themeQuick" aria-label="切换日间夜间">
        <svg width="17" height="17" viewBox="0 0 24 24" fill="none" stroke="currentColor"
          stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
          <path d="M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8z"/>
        </svg>
      </button>
      <span class="topbar-title">${esc(cur ? cur.label : (document.title.split(' - ')[0] || ''))}</span>
    </span>`;
  const backdrop = document.createElement('div');
  backdrop.className = 'mnav-backdrop';
  const drawer = document.createElement('aside');
  drawer.className = 'mnav-drawer';
  drawer.innerHTML = `
    <div class="mnav-head">
      <span class="mnav-logo">LiteGate</span>
      <button class="mnav-close" aria-label="关闭菜单">×</button>
    </div>
    <aside class="menu">
      <p class="menu-label">管理</p>
      <ul class="menu-list">${NAV_ITEMS.map(i => link(i, '')).join('')}</ul>
      <p class="menu-label">其他</p>
      <ul class="menu-list">
        <li><a href="#" class="mnav-logout">退出登录</a></li>
      </ul>
      <p class="menu-label">外观</p>
      <div class="theme-seg" data-theme-seg>
        <button type="button" data-t="system">跟随系统</button>
        <button type="button" data-t="light">日间</button>
        <button type="button" data-t="dark">夜间</button>
      </div>
    </aside>
    <div class="foot">LiteGate AI 网关<br>数据面与管理共用入口</div>`;
  document.body.prepend(bar);
  document.body.appendChild(backdrop);
  document.body.appendChild(drawer);

  const trigger = bar.querySelector('.topbar-icon');
  drawer.id = 'mobileNavigation';
  drawer.setAttribute('role', 'dialog');
  drawer.setAttribute('aria-modal', 'true');
  drawer.setAttribute('aria-label', '导航菜单');
  drawer.inert = true;
  trigger.setAttribute('aria-controls', drawer.id);
  trigger.setAttribute('aria-expanded', 'false');
  const background = [aside, bar, document.querySelector('.wrap')].filter(Boolean);
  let returnFocus = null;
  const setOpen = open => {
    if (open) returnFocus = document.activeElement;
    document.body.classList.toggle('mnav-open', open);
    trigger.setAttribute('aria-expanded', String(open));
    drawer.inert = !open;
    background.forEach(el => { el.inert = open; });
    if (open) drawer.querySelector('.mnav-close').focus();
    else if (returnFocus) { returnFocus.focus(); returnFocus = null; }
  };
  document.addEventListener('keydown', e => {
    if (!document.body.classList.contains('mnav-open')) return;
    if (e.key === 'Escape') { e.preventDefault(); setOpen(false); }
    if (e.key === 'Tab') {
      const controls = [...drawer.querySelectorAll('a[href], button:not([disabled])')];
      const first = controls[0], last = controls[controls.length - 1];
      if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
      else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
    }
  });
  window.matchMedia('(min-width:900px)').addEventListener('change', e => { if (e.matches) setOpen(false); });
  bar.querySelector('.topbar-icon').addEventListener('click', () => setOpen(true));
  backdrop.addEventListener('click', () => setOpen(false));
  drawer.querySelector('.mnav-close').addEventListener('click', () => setOpen(false));
  drawer.querySelectorAll('.menu-list a').forEach(a => a.addEventListener('click', () => setOpen(false)));
  drawer.querySelector('.mnav-logout').addEventListener('click', e => {
    e.preventDefault();
    clearToken();
    location.href = '/login.html';
  });

  // 外观三档(theme.js 提供 siteTheme)+顶栏日/夜快捷切换
  function syncThemeUI() {
    const mode = window.siteTheme.get();
    document.querySelectorAll('[data-theme-seg] button').forEach(b =>
      b.classList.toggle('on', b.dataset.t === mode));
    const dark = document.documentElement.getAttribute('data-theme') === 'dark';
    const q = document.getElementById('themeQuick');
    if (q) q.innerHTML = dark
      ? '<svg width="17" height="17" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="12" cy="12" r="4"/><path d="M12 2v2m0 16v2M4.9 4.9l1.4 1.4m11.4 11.4 1.4 1.4M2 12h2m16 0h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4"/></svg>'
      : '<svg width="17" height="17" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8z"/></svg>';
  }
  document.querySelectorAll('[data-theme-seg] button').forEach(b =>
    b.addEventListener('click', () => window.siteTheme.set(b.dataset.t)));
  const tQuick = document.getElementById('themeQuick');
  if (tQuick) tQuick.addEventListener('click', () => window.siteTheme.quickToggle());
  document.addEventListener('themechange', syncThemeUI);
  syncThemeUI();

  aside.querySelector('#navLogout').addEventListener('click', e => {
    e.preventDefault();
    clearToken();
    location.href = '/login.html';
  });
}

// 格式化辅助
function fmtInt(n) { return Number(n || 0).toLocaleString('zh-CN'); }
function fmtUSD(n) {
  return fmtCur(n, 'USD');
}

// 按币种格式化金额：USD→$、CNY→¥，其余用代码原样标注
function fmtCur(n, cur) {
  n = Number(n || 0);
  const s = cur === 'CNY' ? '¥' : cur === 'USD' ? '$' : (cur || '');
  return n >= 100 ? (s + n.toFixed(1)) : n >= 1 ? (s + n.toFixed(2)) : (s + n.toFixed(4));
}

// 成本聚合可能是「按币种拆分」对象（跨模型数值不可互加），渲染成 "¥x + $y"；
// 兼容旧的单一数值（按美元显示）
function fmtCost(v) {
  if (v && typeof v === 'object') {
    const parts = [];
    for (const cur of ['CNY', 'USD']) {
      if (v[cur]) parts.push(fmtCur(v[cur], cur));
    }
    for (const cur of Object.keys(v)) {
      if (cur !== 'CNY' && cur !== 'USD' && v[cur]) parts.push(fmtCur(v[cur], cur));
    }
    return parts.length ? parts.join(' + ') : '$0';
  }
  return fmtUSD(v);
}
function fmtTok(n) {
  n = Number(n || 0);
  if (n >= 1e9) return (n / 1e9).toFixed(2) + 'B';
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
  el.style.cssText = 'position:fixed;top:1rem;right:1rem;z-index:50;max-width:380px;padding:.8rem 1rem;white-space:pre-line';
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

// 手机卡片视图折叠(renderFold 设初始态:initCardFold 绑定一次交互,配合 tr.card-foldable/card-fold 与 td.cell-detail)
// 首卡默认展开;有鼠标时悬停展开、移出列表收起;触屏时点按展开、点其它卡或页面空白收起
window.renderFold = function (tbody) {
  tbody = typeof tbody === 'string' ? document.querySelector(tbody) : tbody;
  if (!tbody) return;
  [...tbody.querySelectorAll('tr.card-foldable')]
    .forEach((tr, i) => tr.classList.toggle('card-fold', i > 0));
};
window.initCardFold = function (tbody) {
  tbody = typeof tbody === 'string' ? document.querySelector(tbody) : tbody;
  if (!tbody || tbody.dataset.foldInit) return;
  tbody.dataset.foldInit = '1';
  const cards = () => [...tbody.querySelectorAll('tr.card-foldable')];
  const collapseAll = except => cards().forEach(tr => { if (tr !== except) tr.classList.add('card-fold'); });
  if (matchMedia('(hover: none)').matches) {
    document.addEventListener('click', e => {
      const tr = e.target.closest('tr.card-foldable');
      if (!tr) { collapseAll(); return; }
      if (tr.classList.contains('card-fold')) { collapseAll(tr); tr.classList.remove('card-fold'); }
      else if (!e.target.closest('button,a,input,select,textarea')) tr.classList.add('card-fold');
    });
  } else {
    tbody.addEventListener('mouseover', e => {
      const tr = e.target.closest('tr.card-foldable');
      if (tr) { collapseAll(tr); tr.classList.remove('card-fold'); }
    });
    tbody.addEventListener('mouseleave', () => collapseAll());
  }
};
