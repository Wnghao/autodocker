'use strict';

/* ---------- 基础工具 ---------- */

const $ = (sel) => document.querySelector(sel);

const state = {
  stats: null,
  containers: [],
  images: [],
  tasks: [],
  notifications: [],
  settings: null,
  tab: 'containers',
  containerSearch: '',
  imageSearch: '',
  // 镜像页勾选的镜像 ID。清理是批量操作，靠它决定删哪些。
  picked: new Set(),
};

/* ---------- 登录态 ---------- */

// 页面走自己的登录表单，而不是让浏览器弹原生框：飞牛桌面用 iframe 加载页面，
// 原生弹框在里面体验很差，部分壳还会直接判定页面不可用变成白屏。
// 凭据以 Basic 形式随请求发送，存在 localStorage 里以便刷新后免登录。

const AUTH_KEY = 'autodocker.auth';
const USER_KEY = 'autodocker.user';
let authToken = readAuth();
let pollTimer = null;
// 上一轮轮询还没结束就不再开一轮，免得请求叠在一起越堆越多
let pollBusy = false;
// 空闲状态下每多少轮做一次整份重载
let idleTicks = 0;
// 登录层还挂着的时候不要打接口：会 401，还会弹一串没用的提示
let uiReady = false;

function readAuth() {
  try { return localStorage.getItem(AUTH_KEY) || ''; } catch (_) { return ''; }
}

function writeAuth(v) {
  authToken = v || '';
  try {
    if (authToken) localStorage.setItem(AUTH_KEY, authToken);
    else localStorage.removeItem(AUTH_KEY);
  } catch (_) { /* 无痕模式下写不了，本次会话内依然可用 */ }
}

// 安装时可以自定义登录用户名，这里记住上一次成功登录的那个，
// 免得每次都默认填 admin 让人以为密码错了。
function rememberedUser() {
  try { return localStorage.getItem(USER_KEY) || ''; } catch (_) { return ''; }
}

function rememberUser(u) {
  try {
    if (u) localStorage.setItem(USER_KEY, u);
  } catch (_) { /* 同上 */ }
}

// 凭据是 base64("用户名:密码")，顺手把用户名解出来
function userFromToken() {
  try {
    const raw = atob(authToken);
    const i = raw.indexOf(':');
    return i > 0 ? raw.slice(0, i) : '';
  } catch (_) { return ''; }
}

/* ---------- 主题 ---------- */

// data-theme 属性在 index.html 的 <head> 里已经由内联脚本设好了
// （那段必须同步执行，否则深色主题下会先闪一下白底）。
// 这里只负责：把按钮的图标/提示语对上当前主题、以及切换后落库。
const THEME_KEY = 'autodocker.theme';

function currentTheme() {
  return document.documentElement.getAttribute('data-theme') === 'light' ? 'light' : 'dark';
}

function applyTheme(name) {
  const t = name === 'light' ? 'light' : 'dark';
  document.documentElement.setAttribute('data-theme', t);
  // 认按钮上写着「切到哪个主题」，不是当前是什么主题。
  const next = t === 'dark' ? '浅色' : '深色';
  document.querySelectorAll('#btn-theme, #btn-theme-login').forEach((btn) => {
    btn.title = '切换到' + next + '主题';
    btn.setAttribute('aria-label', '切换到' + next + '主题');
  });
  try { localStorage.setItem(THEME_KEY, t); } catch (_) { /* 无痕模式下写不了，本次会话内依然生效 */ }
}

function toggleTheme() {
  applyTheme(currentTheme() === 'dark' ? 'light' : 'dark');
}

function b64(s) {
  const bytes = new TextEncoder().encode(s);
  let bin = '';
  bytes.forEach((b) => { bin += String.fromCharCode(b); });
  return btoa(bin);
}

function showLogin(message) {
  const mask = $('#login');
  if (!mask) return;
  mask.hidden = false;

  const err = $('#login-error');
  if (message) {
    err.textContent = message;
    err.hidden = false;
  } else {
    err.hidden = true;
  }

  const user = $('#login-user');
  if (user) user.value = rememberedUser() || 'admin';

  const pass = $('#login-pass');
  if (pass) {
    // 只在凭据失效重新弹出时清空；登录失败要留着让用户改
    if (!message) pass.value = '';
    setTimeout(() => pass.focus(), 30);
  }
}

function hideLogin() {
  const mask = $('#login');
  if (mask) mask.hidden = true;
}

async function api(path, opts = {}) {
  const headers = { 'Content-Type': 'application/json', ...(opts.headers || {}) };
  if (authToken) headers['Authorization'] = 'Basic ' + authToken;

  const res = await fetch(path, { ...opts, headers });

  if (res.status === 401) {
    writeAuth('');
    stopPolling();
    showLogin('');
    const err = new Error('需要登录');
    err.unauthorized = true;
    throw err;
  }
  if (!res.ok) {
    let msg = res.status + ' ' + res.statusText;
    try {
      const data = await res.json();
      if (data && data.error) msg = data.error;
    } catch (_) { /* 忽略解析失败 */ }
    throw new Error(msg);
  }
  if (res.status === 204) return null;
  return res.json();
}

let toastTimer = null;
function toast(msg, kind) {
  const el = $('#toast');
  el.textContent = msg;
  el.className = 'toast ' + (kind || 'ok');
  el.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { el.hidden = true; }, 3200);
}

function esc(s) {
  return String(s == null ? '' : s)
    .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
}

function fmtTime(t) {
  if (!t) return '—';
  const d = new Date(t);
  if (isNaN(d.getTime()) || d.getFullYear() < 2000) return '—';
  const p = (n) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}

function fmtDuration(from, to) {
  if (!from) return '';
  const a = new Date(from), b = to ? new Date(to) : new Date();
  if (isNaN(a.getTime()) || isNaN(b.getTime())) return '';
  const sec = Math.max(0, Math.round((b - a) / 1000));
  if (sec < 60) return sec + ' 秒';
  return Math.floor(sec / 60) + ' 分 ' + (sec % 60) + ' 秒';
}

function fmtSize(n) {
  if (!n || n <= 0) return '—';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let v = n, i = 0;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return (v >= 100 || i === 0 ? Math.round(v) : v.toFixed(1)) + ' ' + units[i];
}

// 镜像 ID 太长，只留 sha256: 加前 12 位，足够区分又看得下
function shortImageId(id) {
  const s = String(id || '');
  const i = s.indexOf(':');
  if (i >= 0) return s.slice(0, i + 1) + s.slice(i + 1, i + 13);
  return s.length > 12 ? s.slice(0, 12) : s;
}

// 悬空镜像拿短 ID 当标题时，去掉 sha256: 前缀 —— `docker images` 的 IMAGE ID
// 列也是这么显示的（形如 d825b9c26134），用户能直接拿去命令行里对。
function bareImageId(id) {
  const s = String(id || '');
  const i = s.indexOf(':');
  const hex = i >= 0 ? s.slice(i + 1) : s;
  return hex.length > 12 ? hex.slice(0, 12) : hex;
}

/* ---------- 状态字典 ---------- */

// 状态徽章只服务于「容器」页。镜像页不再展示检测状态 —— 见 renderImages 的说明。
const STATUS = {
  up_to_date: { text: '已是最新', cls: 'ok' },
  update_available: { text: '有更新', cls: 'warn' },
  new_version_tag: { text: '可选更新', cls: 'info' },
  unknown: { text: '未知', cls: 'muted' },
  ignored: { text: '已忽略', cls: 'muted' },
};

const MODE = { auto: '自动', 'digest-only': '摘要比对', 'pin-watch': '版本监听' };

const TASK = {
  pending: { text: '排队中', cls: 'muted' },
  running: { text: '执行中', cls: 'info' },
  success: { text: '成功', cls: 'ok' },
  failed: { text: '失败', cls: 'danger' },
  rolled_back: { text: '已回滚', cls: 'warn' },
};

const CARD = {
  pending: { text: '待处理', cls: 'warn' },
  updating: { text: '更新中', cls: 'info' },
  done: { text: '已完成', cls: 'ok' },
  failed: { text: '失败', cls: 'danger' },
  rollback: { text: '已回滚', cls: 'warn' },
  ignored: { text: '已忽略', cls: 'muted' },
  snoozed: { text: '已稍后', cls: 'muted' },
};

const KIND = { new_version: '有新版本', new_tag: '可选更新' };

function badge(map, key) {
  const it = map[key] || { text: key || '—', cls: 'muted' };
  return `<span class="badge ${it.cls}">${esc(it.text)}</span>`;
}

/* ---------- 渲染 ---------- */

// 首页这排数字说的是**容器**，不是镜像。
// 镜像页讲「磁盘上有什么、能不能清」，容器页讲「在跑的东西该不该更新」，
// 顶部这条跟着「该不该更新」走。标签也写明白，免得看的人以为数错了。
function renderStats() {
  const s = state.stats || {};
  const items = [
    { label: '监控容器', value: s.total || 0, cls: '' },
    { label: '有更新', value: s.update_available || 0, cls: 'warn' },
    { label: '可选更新', value: s.new_version_tag || 0, cls: 'info' },
    { label: '未读通知', value: s.unread || 0, cls: 'accent' },
    { label: '已是最新', value: s.up_to_date || 0, cls: 'ok' },
  ];
  $('#stats').innerHTML = items.map((it) => `
    <div class="stat ${it.cls}">
      <div class="stat-value">${it.value}</div>
      <div class="stat-label">${it.label}</div>
    </div>
  `).join('');

  const statusEl = $('#global-status');
  if (s.updating) {
    statusEl.textContent = '更新执行中';
    statusEl.className = 'pill info';
  } else if (s.scanning) {
    statusEl.textContent = '扫描中';
    statusEl.className = 'pill info';
  } else if (!s.feishu_configured) {
    statusEl.textContent = '飞书未配置';
    statusEl.className = 'pill warn';
  } else {
    statusEl.textContent = '空闲';
    statusEl.className = 'pill idle';
  }
}

// 把后端返回的失败原因压成一行，完整内容挂在 title 上。
// 列表里全是「未知」却看不到原因，是最让人抓瞎的情形。
function shortErr(s, n) {
  const t = String(s || '').replace(/\s+/g, ' ').trim();
  const max = n || 64;
  return t.length > max ? t.slice(0, max) + '…' : t;
}

// 用户在飞书卡片上点过「忽略本版本」：远端摘要没再变过，这一版就不再提醒。
//
// 这一行必须显式标出来。否则页面上写着「有更新」、卡片却再也不来，
// 只会让人以为通知坏了或者按钮没生效 —— 其实是自己点过的放行。
function ackIgnored(img) {
  return !img.ignored && !!img.ignored_digest && img.ignored_digest === img.remote_digest;
}

/* 容器列表：一条记录对应一个真实容器，「更新」等于按原配置重建它。 */
function renderContainers() {
  const kw = state.containerSearch.trim().toLowerCase();
  const rows = state.containers.filter((img) => {
    if (!kw) return true;
    return (img.container_name || '').toLowerCase().includes(kw) ||
      (img.reference || '').toLowerCase().includes(kw);
  });

  if (!rows.length) {
    $('#containers-body').innerHTML = `<div class="empty">${state.containers.length ? '没有匹配的容器' : '还没有容器。挂载 docker.sock 后扫描一次即可自动发现。'}</div>`;
    return;
  }

  const html = rows.map((img) => {
    // 「更新」只在该出现的时候出现，不给灰着的占位：
    // 检测模式是自动判定的，「已是最新」根本没有新镜像可拉，摆一个点不动的按钮
    // 只会让人反复去点、点完没反应；「未知」「已忽略」同理 —— 前者是这次没查到，
    // 后者是你自己说了先不动。检测失败的原因已经写在容器名下方的小字里，
    // 不需要再用一个灰按钮来占位提示。跟镜像页是同一条原则。
    const canUpdate = !!img.container_id &&
      (img.status === 'update_available' || img.status === 'new_version_tag');
    const err = img.status === 'unknown' ? (img.last_error || '') : '';
    const acked = ackIgnored(img);

    // 「可用」列的含义是「仓库里有没有更高的版本号」，不是「能拉到什么」。
    // 三种情况要分开写，否则会让人以为没取到数：
    // · 有更高版本 → 显示那个 tag；
    // · 没有更高版本但摘要变了（同一个 tag 被作者重推了内容）→ 写一句「内容已更新」。
    //   这类镜像的 tag 里带的数字是架构而不是版本号（v3-386 / v3-arm64 这种并列发布），
    //   永远不可能有「更高版本」，点更新拉到的还是同一个 tag，只是它的最新内容；
    // · 其余（已是最新 / 未知）→ 留横杠。
    const avail = img.latest_tag
      ? `<div class="cell-main mono">${esc(img.latest_tag)}</div>`
      : (img.status === 'update_available'
        ? `<div class="cell-main hint-text" title="仓库里 ${esc(img.reference)} 这个 tag 已被重新推送，点「更新」拉到它的最新内容，版本号不变">内容已更新</div>`
        : '<div class="cell-main mono">—</div>');
    return `
    <div class="row" data-id="${img.id}">
      <div class="cell grow">
        <div class="cell-main">${esc(img.container_name || '—')}</div>
        <div class="cell-sub">${esc(img.reference)}</div>
        ${err ? `<div class="cell-err" title="${esc(err)}">${esc(shortErr(err))}</div>` : ''}
      </div>
      <div class="cell col-tag">
        <div class="cell-sub">当前</div>
        <div class="cell-main mono">${esc(img.local_tag || '—')}</div>
      </div>
      <div class="cell col-tag">
        <div class="cell-sub">可用</div>
        ${avail}
      </div>
      <div class="cell col-mode">
        <div class="cell-sub">检测模式</div>
        <select class="mode-select" data-mode="${img.id}"
                title="自动：版本号 tag（1.25）走版本监听，latest 等浮动 tag 走摘要比对">
          ${['auto', 'digest-only', 'pin-watch'].map((m) =>
            `<option value="${m}"${(img.mode || 'auto') === m ? ' selected' : ''}>${MODE[m]}</option>`).join('')}
        </select>
      </div>
      <div class="cell col-status center"${err ? ` title="${esc(err)}"` : ''}>${
        acked
          ? '<span class="badge muted" title="你在飞书卡片上点过「忽略本版本」，这版内容不会再提醒；仓库一旦推了新内容，会照常通知">已忽略本版本</span>'
          : badge(STATUS, img.status)}</div>
      <div class="cell actions">
        ${canUpdate ? '<button class="btn small primary" data-act="update">更新</button>' : ''}
        <button class="btn small ghost" data-act="ignore"${
          acked ? ' title="清除「已忽略本版本」，下次扫描若仍是这一版会重新提醒"' : ''}>${
          img.ignored ? '恢复' : (acked ? '恢复提醒' : '忽略')}</button>
        <button class="btn small ghost danger" data-act="remove">移除</button>
      </div>
    </div>`;
  }).join('');

  $('#containers-body').innerHTML = html;
}

/* 镜像列表：本机真实存在的镜像（含没被任何容器使用的），
   外加只在远端盯梢的监控项。这里关心的是占了多少磁盘、能不能清掉。

   这一页刻意**不展示检测状态**（可选更新 / 未检测 / 未知）：
   镜像有没有新版，是「容器」页的问题 —— 那边才有「更新」这个动作，
   点下去会重建容器。镜像页上摆一串状态徽章，加上一个点了只会重新拉取的
   「更新」按钮，等于把两件事混在一起，看的人只会更糊涂。
   磁盘上这一层归谁用、能不能清掉，才是这一页要回答的。 */
function renderImages() {
  const kw = state.imageSearch.trim().toLowerCase();
  const rows = state.images.filter((img) => {
    if (!kw) return true;
    return (img.name || '').toLowerCase().includes(kw) ||
      (img.reference || '').toLowerCase().includes(kw) ||
      (img.id || '').toLowerCase().includes(kw);
  });

  if (!rows.length) {
    $('#images-body').innerHTML = `<div class="empty">${state.images.length ? '没有匹配的镜像' : '本机还没有镜像，或服务尚未连上 Docker。'}</div>`;
    return;
  }

  const html = rows.map((img) => {
    // 统一成「第一行镜像名、第二行镜像 ID」，跟 `docker images` 一眼就能对上号。
    // 悬空镜像（被新版本顶下来的旧层）在 Docker 里没有 tag，但后端已经尽力从
    // 镜像自带的 OCI 标签里挖过名字；实在挖不到才退回显示短 ID —— 一屏好几个
    // <none> 谁也分不清谁，短 ID 至少能唯一指认，也能拿去命令行里对。
    const named = !!img.name;
    const title = named ? img.name
      : (img.remote_only ? (img.reference || '—') : bareImageId(img.id));
    const sub = img.remote_only ? '本机无此镜像，仅盯远端仓库'
      : (img.dangling && !named ? '历史残留镜像层' : shortImageId(img.id));
    const fullRef = img.dangling ? img.id : (img.reference || img.name || '');
    const err = img.last_error || '';

    let usage;
    if (img.remote_only) {
      usage = '<span class="badge muted">仅监控</span>';
    } else if (img.in_used) {
      const names = (img.used_by || []).join('、');
      usage = `<span class="badge ok" title="${esc(names)}">使用中</span>`;
    } else {
      // 悬空镜像一并归到「未使用」：从「能不能清理」的角度看，它和一个没容器
      // 在用的普通镜像没有区别 —— 单摆一个「无 TAG」的徽章反而像是另一类东西。
      // 它没有 tag 这件事，放在副标题里说明一句就够了。
      usage = '<span class="badge warn">未使用</span>';
    }

    // 这一页只剩「删除」一个动作，不多摆按钮占位：
    // 使用中的镜像不给删 —— 删了它，容器下次重启就起不来。这里直接不渲染，
    // 摆个灰按钮仍然会让人去点。复选框同步禁用，免得被「删除所选」批量带走。
    const canDelete = !img.remote_only && !img.in_used;
    const btns = canDelete
      ? ['<button class="btn small ghost danger" data-act="del">删除</button>']
      : [];

    return `
    <div class="row" data-imgid="${esc(img.id)}">
      <div class="cell col-pick">
        <input type="checkbox" data-pick="${esc(img.id)}"
               ${state.picked.has(img.id) ? 'checked' : ''}
               ${canDelete ? '' : 'disabled'}
               title="${canDelete ? '选中后可批量删除' : (img.remote_only ? '纯远端监控项，本机没有可删的镜像' : '镜像正在被容器使用，不能删除')}">
      </div>
      <div class="cell grow">
        <div class="cell-main${!named && !img.remote_only ? ' mono' : ''}" title="${esc(fullRef)}">${esc(title)}</div>
        <div class="cell-sub mono" title="${esc(img.id)}">${esc(sub)}</div>
        ${err ? `<div class="cell-err" title="${esc(err)}">${esc(shortErr(err))}</div>` : ''}
      </div>
      <div class="cell col-usage">${usage}</div>
      <div class="cell col-tag">
        <div class="cell-sub">TAG</div>
        <div class="cell-main mono">${esc(img.tag || '—')}</div>
      </div>
      <div class="cell col-size">
        <div class="cell-sub">大小</div>
        <div class="cell-main">${esc(fmtSize(img.size_bytes))}</div>
      </div>
      <div class="cell col-created">
        <div class="cell-sub">创建时间</div>
        <div class="cell-main">${esc(fmtTime((img.created || 0) * 1000))}</div>
      </div>
      <div class="cell actions">${btns.length ? btns.join('') : '<span class="no-action">—</span>'}</div>
    </div>`;
  }).join('');

  $('#images-body').innerHTML = html;
}

function renderTasks() {
  if (!state.tasks.length) {
    $('#tasks-body').innerHTML = '<div class="empty">还没有更新记录。</div>';
    return;
  }
  $('#tasks-body').innerHTML = state.tasks.map((t) => `
    <div class="row clickable" data-task="${t.id}">
      <div class="cell narrow mono">#${t.id}</div>
      <div class="cell grow">
        <div class="cell-main">${esc(t.container_name || '—')}</div>
        <div class="cell-sub">${esc(t.image_ref)}</div>
      </div>
      <div class="cell col-status center">${badge(TASK, t.status)}</div>
      <div class="cell col-src">
        <div class="cell-sub">来源</div>
        <div class="cell-main">${esc(t.triggered_by || '—')}</div>
      </div>
      <div class="cell col-cost">
        <div class="cell-sub">耗时</div>
        <div class="cell-main">${esc(fmtDuration(t.started_at, t.finished_at) || '—')}</div>
      </div>
      <div class="cell col-time">
        <div class="cell-sub">时间</div>
        <div class="cell-main">${esc(fmtTime(t.created_at))}</div>
      </div>
    </div>
  `).join('');
}

function renderNotifications() {
  if (!state.notifications.length) {
    $('#notifications-body').innerHTML = '<div class="empty">暂无通知。</div>';
    return;
  }
  $('#notifications-body').innerHTML = state.notifications.map((n) => `
    <div class="row">
      <div class="cell grow">
        <div class="cell-main">${n.read ? '' : '<span class="dot"></span>'}${esc(n.title || n.image_ref)}</div>
        <div class="cell-sub">${esc(n.container_name || '（远端监控）')} · ${esc(n.image_ref)}</div>
      </div>
      <div class="cell col-kind">
        <div class="cell-sub">类型</div>
        <div class="cell-main">${esc(KIND[n.kind] || n.kind)}</div>
      </div>
      <div class="cell col-tag">
        <div class="cell-sub">版本</div>
        ${n.latest_tag
          ? `<div class="cell-main mono">${esc(n.latest_tag)}</div>`
          : (n.kind === 'new_version'
            ? '<div class="cell-main hint-text" title="这条通知说的是：仓库里同一个 tag 的内容变了，没有新的版本号">内容已更新</div>'
            : '<div class="cell-main mono">—</div>')}
      </div>
      <div class="cell col-status center">${badge(CARD, n.card_state)}</div>
      <div class="cell col-time">
        <div class="cell-sub">时间</div>
        <div class="cell-main">${esc(fmtTime(n.created_at))}</div>
      </div>
      <div class="cell actions">
        ${n.read ? '' : `<button class="btn small ghost" data-read="${n.id}">标为已读</button>`}
      </div>
    </div>
  `).join('');
}

function renderSettings() {
  const s = state.settings;
  if (!s) return;
  const configured = !!(s.feishu_app_id && s.feishu_app_secret);
  const live = state.stats && state.stats.feishu_longconn;
  const statusText = !configured
    ? '未配置，发现新版本只会记录在「通知」标签里'
    : (live ? '已配置，按钮回调长连接正常' : '已配置，但长连接尚未就绪（保存后稍等片刻，或查看日志）');
  $('#settings-form').innerHTML = `
    <div class="field">
      <label for="scan_interval">扫描间隔（秒，0 表示关闭定时扫描）</label>
      <input id="scan_interval" name="scan_interval" type="number" min="0" value="${s.scan_interval}">
      <p class="field-hint">最小 30 秒。修改后无需重启即可生效。</p>
    </div>
    <div class="field">
      <label for="health_check_wait">更新后健康检查观察窗口（秒）</label>
      <input id="health_check_wait" name="health_check_wait" type="number" min="0" max="600" value="${s.health_check_wait}">
      <p class="field-hint">新容器启动后最多等待这么久，未通过则按下方策略处理。</p>
    </div>

    <h3 class="form-section">飞书通知</h3>
    <p class="field-hint">
      右侧一栏列了完整的配置步骤，第一次配置照着走一遍即可；
      三个值一并留空，即完全关闭飞书通知。
    </p>

    <details class="setup-guide" open>
      <summary>配置步骤（第一次配置照着做，共 8 步）</summary>
      <ol class="setup-steps">
        <li>
          <b>建应用</b>：打开
          <a href="https://open.feishu.cn/app" target="_blank" rel="noreferrer">飞书开放平台 · 开发者后台</a>，
          点「创建企业自建应用」，名字随意。
        </li>
        <li>
          <b>加机器人能力</b>：左侧「应用能力 → 机器人」→ 添加应用能力，
          确认「消息卡片」开关是打开的。
        </li>
        <li>
          <b>取凭据</b>：左侧「凭证与基础信息」，把 <code>App ID</code> 和
          <code>App Secret</code> 填到左边的设置项里。
        </li>
        <li>
          <b>开权限</b>：左侧「开发配置 → 权限管理 → API 权限」，搜到下面两项并开通，
          权限类型都选<b>「应用身份」</b>：
          <ul class="scope-list">
            <li><code>im:message:send_as_bot</code></li>
            <li><code>im:chat:readonly</code></li>
          </ul>
        </li>
        <li>
          <b>配回调</b>：左侧「开发配置 → 事件与回调」→ 切到<b>「回调配置」标签页</b>：
          <ul class="scope-list">
            <li>「订阅方式」选<b>「使用长连接接收回调」</b>，点保存</li>
            <li>「已订阅的回调」→ 添加回调 → 搜<b>「卡片回传交互」</b>
              （<code>card.action.trigger</code>）→ 确认添加</li>
          </ul>
        </li>
        <li>
          <b>发布版本</b>：左侧「应用发布 → 版本管理与发布」→ 创建版本 → 提交发布。
        </li>
        <li>
          <b>把机器人拉进群</b>：飞书里打开目标群 → 右上角设置 → 群机器人 → 添加机器人，
          选中你刚建的这个应用。
        </li>
        <li>
          <b>取群 ID</b>：打开
          <a href="https://open.feishu.cn/api-explorer" target="_blank" rel="noreferrer">飞书 API 调试台</a>，
          搜「获取机器人所在的群列表」，以<b>应用身份</b>调用，
          返回里的 <code>chat_id</code> 填到左边的设置项里。
        </li>
      </ol>
    </details>

    <div class="field">
      <label for="feishu_app_id">App ID</label>
      <input id="feishu_app_id" name="feishu_app_id" type="text" value="${esc(s.feishu_app_id)}" placeholder="cli_xxxxxxxxxxxx">
    </div>
    <div class="field">
      <label for="feishu_app_secret">App Secret</label>
      <input id="feishu_app_secret" name="feishu_app_secret" type="password" value="${esc(s.feishu_app_secret)}" placeholder="留空则关闭飞书通知">
      <p class="field-hint">仅保存在本机数据库里，不会外传。</p>
    </div>
    <div class="field">
      <label for="feishu_chat_id">接收群 chat_id</label>
      <input id="feishu_chat_id" name="feishu_chat_id" type="text" value="${esc(s.feishu_chat_id)}" placeholder="oc_xxxxxxxx">
      <p class="field-hint">上面三个值一并留空，即完全关闭飞书通知。</p>
    </div>

    <h3 class="form-section">注册表加速</h3>
    <p class="field-hint">
      国内直连 Docker Hub 大多不通，这会表现为镜像列表里一片「未知」。
      填上加速地址即可解决；本服务查 digest 是自己发请求，不会自动继承
      Docker 守护进程的 <code>registry-mirrors</code>，所以这里需要单独配一次。
    </p>
    <div class="field">
      <label for="registry_mirror">镜像加速地址（可填多个）</label>
      <input id="registry_mirror" name="registry_mirror" type="text" value="${esc(s.registry_mirror)}"
             placeholder="例如 docker.1ms.run, docker.xuanyuan.me">
      <p class="field-hint">
        多个地址用逗号分隔，按顺序回退，全部失败才走直连。仅作用于 <b>Docker Hub</b>；
        ghcr.io / quay.io / 私有仓库仍直连，不会被带偏。留空则先沿用 Docker 守护进程已配的加速器，
        仍为空时使用内置默认（<code>docker.1ms.run</code> / <code>docker.xuanyuan.me</code>）。
      </p>
    </div>
    <div class="field">
      <button type="button" id="btn-test-registry" class="btn ghost">检测注册表连通性</button>
      <div id="registry-test-result" class="registry-test" hidden></div>
    </div>

    <h3 class="form-section">访问控制</h3>
    <p class="field-hint">
      默认<b>不启用登录</b>，打开页面即可使用。本应用能重建任意容器（等同于系统最高权限），
      如果端口会暴露到内网之外，建议在这里设一个访问密码。
    </p>
    <div class="field">
      <label for="admin_user">登录用户名</label>
      <input id="admin_user" name="admin_user" type="text" value="${esc(s.admin_user)}" placeholder="admin">
    </div>
    <div class="field">
      <label for="admin_password">访问密码</label>
      <input id="admin_password" name="admin_password" type="password" value="${esc(s.admin_password)}" placeholder="留空表示不需要登录">
      <p class="field-hint">保存后立即生效，无需重启；留空即关闭登录。改动后本页会自动改用新凭据，不用手动重登。</p>
    </div>

    <h3 class="form-section">更新策略</h3>
    <div class="checks">
      <label class="check"><input type="checkbox" id="rollback_on_failure" ${s.rollback_on_failure ? 'checked' : ''}> 更新失败时自动回滚到原容器</label>
      <label class="check"><input type="checkbox" id="keep_old_container" ${s.keep_old_container ? 'checked' : ''}> 更新成功后保留旧容器（便于手动回退）</label>
      <label class="check"><input type="checkbox" id="registry_insecure" ${s.registry_insecure ? 'checked' : ''}> 允许 http 明文注册表（本地地址自动走 http，无需勾选）</label>
    </div>
    <p class="field-hint">更新动作始终由人确认：飞书卡片点「立即更新」，或在此页「容器」标签点「更新」。
      环境变量 <code>AUTO_UPDATE=true</code> 可改为发现即更新，该开关不在本页展示，保存设置不会改动它。</p>
    <div class="form-actions">
      <button type="submit" class="btn primary">保存设置</button>
      <button type="button" id="btn-test-feishu" class="btn ghost">发送飞书测试卡片</button>
    </div>
    <p class="field-hint">测试卡片上带一个按钮 —— 点一下、卡片变成「回调已收到」，
      说明按钮交互这条链路也通了（它和「收得到消息」是两回事）。</p>
    <div id="feishu-callback-state" class="callback-state" hidden></div>
  `;

  // 飞书那套指引有八步，挤在 720px 的表单里读起来很局促。整块搬到右侧栏 ——
  // 直接搬 DOM 节点，省得把那一大段 HTML 复制两份、改一处漏一处。
  const guide = $('#settings-guide');
  guide.innerHTML = `
    <div class="guide-card">
      <h3>飞书通知配置指引</h3>
      <p class="field-hint">
        配好之后，发现新版本会往飞书群推一张卡片，点卡片上的按钮就能直接重建容器 ——
        手机上也能操作，不必打开这个后台页面。本应用用<b>长连接</b>接收按钮回调，
        不需要公网地址、不需要域名备案、不需要反向代理。
      </p>
      <p class="field-hint">当前状态：<b>${esc(statusText)}</b></p>
    </div>`;
  const guideBlock = $('#settings-form .setup-guide');
  if (guideBlock) guide.querySelector('.guide-card').appendChild(guideBlock);
}

/* ---------- 数据加载 ---------- */

async function loadStats() {
  state.stats = await api('/api/stats');
  renderStats();
}

async function loadContainers() {
  const data = await api('/api/containers');
  state.containers = data.items || [];
  renderContainers();
}

async function loadImages() {
  const data = await api('/api/images');
  state.images = data.items || [];
  // 丢掉已经不存在的勾选，免得删完之后按钮还指着空气
  const alive = new Set(state.images.map((it) => it.id));
  state.picked.forEach((id) => { if (!alive.has(id)) state.picked.delete(id); });
  renderImages();
}

async function loadTasks() {
  const data = await api('/api/tasks');
  state.tasks = data.items || [];
  renderTasks();
}

async function loadNotifications() {
  const data = await api('/api/notifications');
  state.notifications = data.items || [];
  renderNotifications();
}

async function loadSettings() {
  state.settings = await api('/api/settings');
  renderSettings();
}

async function refresh(opts) {
  const quiet = !!(opts && opts.quiet);
  try {
    await Promise.all([loadStats(), loadContainers(), loadImages(), loadTasks(), loadNotifications()]);
  } catch (e) {
    // 轮询触发的刷新失败不提示：局域网抖一下是常事，下一轮自己会好，
    // 每隔几秒弹一次红条反而比数据旧一会儿更烦人。
    if (!quiet) toast('加载失败：' + e.message, 'danger');
  }
}

/* ---------- 交互 ---------- */

async function doScan(force) {
  const btn = force ? $('#btn-force') : $('#btn-scan');
  btn.disabled = true;
  btn.textContent = '扫描中…';
  try {
    const res = await api('/api/scan' + (force ? '?force=1' : ''), { method: 'POST' });
    if (res.result === 'scan already running') {
      toast('已有扫描在执行', 'warn');
    } else {
      toast('扫描完成：共 ' + (res.scan ? res.scan.total : 0) + ' 项，发现 ' + (res.scan ? res.scan.with_update : 0) + ' 项更新');
    }
  } catch (e) {
    toast('扫描失败：' + e.message, 'danger');
  } finally {
    btn.disabled = false;
    btn.textContent = force ? '强制扫描' : '立即扫描';
    refresh();
  }
}

async function triggerUpdate(imageId) {
  try {
    await api('/api/update', { method: 'POST', body: JSON.stringify({ image_id: imageId }) });
    toast('已开始更新，进度可在「更新历史」查看');
    refresh();
  } catch (e) {
    toast('无法更新：' + e.message, 'danger');
  }
}

async function toggleIgnore(img) {
  // 三种状态必须分开算，不能一律发 !img.ignored：
  // · 正常 → 点「忽略」：ignored=true，整条不再检测；
  // · 已忽略 → 点「恢复」：ignored=false；
  // · 已忽略本版本 → 点「恢复提醒」：此时 ignored 本来就是 false，要清的是
  //   ignored_digest，再发 !ignored(=true) 会变成「整条忽略」，跟按钮文字正好相反。
  //   后端 SetImageIgnored(false) 会把 ignored_digest 一并清掉，所以这里发 false 即可。
  const acked = ackIgnored(img);
  const next = acked ? false : !img.ignored;
  try {
    await api(`/api/images/${img.id}/ignored`, {
      method: 'PUT',
      body: JSON.stringify({ ignored: next }),
    });
    toast(next ? '已忽略该镜像' : (acked ? '已恢复提醒，下次扫描若仍是这一版会重新通知' : '已恢复监控'));
    loadContainers();
  } catch (e) {
    toast('操作失败：' + e.message, 'danger');
  }
}

// 切换检测模式。override=false 表示恢复「自动判定」。
async function setMode(id, mode) {
  try {
    await api(`/api/images/${id}/mode`, {
      method: 'PUT',
      body: JSON.stringify({ mode, override: mode !== 'auto' }),
    });
    toast(mode === 'auto' ? '已恢复自动判定' : '检测模式已固定为「' + MODE[mode] + '」');
  } catch (e) {
    toast('设置失败：' + e.message, 'danger');
  }
  loadContainers();
}

// 移除一条容器监控记录（不动容器本身）
async function removeMonitorItem(id) {
  if (!confirm('确定移除这条监控记录？不会影响正在运行的容器。')) return;
  try {
    await api('/api/images/' + id, { method: 'DELETE' });
    toast('已移除');
    loadContainers();
    loadStats();
  } catch (e) {
    toast('移除失败：' + e.message, 'danger');
  }
}

/* ---------- 镜像操作 ---------- */

// 删本机镜像：不可逆，需要时只能重新拉
async function doRemoveImages(ids, okMsg) {
  try {
    const res = await api('/api/images/remove', {
      method: 'POST',
      body: JSON.stringify({ ids, force: false }),
    });
    const failed = (res.results || []).filter((r) => !r.ok);
    if (failed.length && !res.removed) {
      toast('删除失败：' + failed[0].error, 'danger');
    } else if (failed.length) {
      toast(`已删除 ${res.removed} 个，${failed.length} 个跳过（${failed[0].error}）`, 'warn');
    } else {
      toast(okMsg || `已删除 ${res.removed} 个镜像`);
    }
  } catch (e) {
    toast('删除失败：' + e.message, 'danger');
  }
  refresh();
}

async function removeLocalImage(img) {
  const label = img.dangling ? '（无 TAG 镜像 ' + shortImageId(img.id) + '）' : img.reference;
  if (!confirm(`确认删除镜像 ${label}？\n\n删除后本机就没有这份了，需要时得重新拉取。`)) return;
  await doRemoveImages([img.id], `已删除 ${label}`);
}

// 清理候选。按镜像 ID 去重：同一个镜像可能挂着多个 tag，
// 不去重会对着同一个镜像删两次，第二次必然报错。
function cleanCandidates(mode) {
  const seen = new Set();
  return state.images.filter((img) => {
    if (img.remote_only || img.in_used) return false;
    if (mode === 'dangling' && !img.dangling) return false;
    if (seen.has(img.id)) return false;
    seen.add(img.id);
    return true;
  });
}

function renderCleanPreview() {
  const box = $('#clean-preview');
  if (!box) return;
  const picked = document.querySelector('input[name="clean-mode"]:checked');
  const mode = picked ? picked.value : 'dangling';
  const items = cleanCandidates(mode);
  if (!items.length) {
    box.innerHTML = `<div class="preview-empty">${mode === 'dangling' ? '没有无 TAG 镜像' : '没有未使用的镜像'}，无需清理。</div>`;
    return;
  }
  const total = items.reduce((a, b) => a + (b.size_bytes || 0), 0);
  box.innerHTML = items.map((img) => `
    <div class="preview-row">
      <span>${esc(img.dangling ? '（无 TAG） ' + shortImageId(img.id) : img.reference)}</span>
      <span class="mono">${esc(fmtSize(img.size_bytes))}</span>
    </div>`).join('') +
    `<div class="preview-row"><span>共 ${items.length} 个</span><span class="mono">${esc(fmtSize(total))}</span></div>`;
}

function openCleanModal() {
  const modal = $('#clean-modal');
  if (!modal) return;
  modal.hidden = false;
  renderCleanPreview();
}

function closeCleanModal() {
  const modal = $('#clean-modal');
  if (modal) modal.hidden = true;
}

async function doClean() {
  const picked = document.querySelector('input[name="clean-mode"]:checked');
  const mode = picked ? picked.value : 'dangling';
  const items = cleanCandidates(mode);
  if (!items.length) {
    toast('没有需要清理的镜像', 'warn');
    return;
  }
  const total = items.reduce((a, b) => a + (b.size_bytes || 0), 0);
  if (!confirm(`确认删除 ${items.length} 个镜像（约 ${fmtSize(total)}）？\n\n删除后需要时得重新拉取。`)) return;

  const btn = $('#btn-clean-ok');
  btn.disabled = true;
  const label = btn.textContent;
  btn.textContent = '清理中…';
  try {
    // 一次交太多会把 Docker 和这台机器一起拖住，分批发
    let removed = 0, failed = 0;
    for (let i = 0; i < items.length; i += 10) {
      const chunk = items.slice(i, i + 10).map((it) => it.id);
      const res = await api('/api/images/remove', {
        method: 'POST',
        body: JSON.stringify({ ids: chunk, force: false }),
      });
      removed += res.removed || 0;
      failed += (res.results || []).filter((r) => !r.ok).length;
    }
    closeCleanModal();
    toast(failed
      ? `已清理 ${removed} 个，${failed} 个跳过（仍被容器占用）`
      : `已清理 ${removed} 个镜像，释放约 ${fmtSize(total)}`);
    refresh();
  } catch (e) {
    toast('清理失败：' + e.message, 'danger');
  } finally {
    btn.disabled = false;
    btn.textContent = label;
  }
}

async function removePicked() {
  if (!state.picked.size) {
    toast('先勾选要删除的镜像', 'warn');
    return;
  }
  const items = state.images.filter((it) => state.picked.has(it.id));
  const total = items.reduce((a, b) => a + (b.size_bytes || 0), 0);
  if (!confirm(`确认删除勾选的 ${items.length} 个镜像（约 ${fmtSize(total)}）？`)) return;

  const ids = items.map((it) => it.id);
  state.picked.clear();
  await doRemoveImages(ids, `已删除 ${ids.length} 个镜像`);
}

async function openTaskLog(id) {
  try {
    const t = await api('/api/tasks/' + id);
    $('#drawer-title').textContent = `任务 #${t.id} · ${t.container_name || ''} · ${TASK[t.status] ? TASK[t.status].text : t.status}`;
    const head = [
      `镜像      ${t.image_ref}`,
      `来源      ${t.triggered_by || '—'}`,
      `开始      ${fmtTime(t.started_at)}`,
      `结束      ${fmtTime(t.finished_at)}`,
      `耗时      ${fmtDuration(t.started_at, t.finished_at) || '—'}`,
      `新容器    ${t.new_container_id || '—'}`,
      `备份名    ${t.backup_name || '—'}`,
      t.error ? `错误      ${t.error}` : '',
      '──────────── 执行日志 ────────────',
      '',
    ].filter(Boolean).join('\n');
    $('#drawer-body').textContent = head + (t.log || '（无日志）');
    $('#drawer').hidden = false;
  } catch (e) {
    toast('读取日志失败：' + e.message, 'danger');
  }
}

async function saveSettings(ev) {
  ev.preventDefault();
  const prev = state.settings || {};
  const payload = {
    scan_interval: Number($('#scan_interval').value),
    health_check_wait: Number($('#health_check_wait').value),
    feishu_app_id: $('#feishu_app_id').value.trim(),
    feishu_app_secret: $('#feishu_app_secret').value.trim(),
    feishu_chat_id: $('#feishu_chat_id').value.trim(),
    registry_mirror: $('#registry_mirror').value.trim(),
    admin_user: ($('#admin_user').value || '').trim(),
    admin_password: $('#admin_password').value || '',
    rollback_on_failure: $('#rollback_on_failure').checked,
    keep_old_container: $('#keep_old_container').checked,
    registry_insecure: $('#registry_insecure').checked,
    // 这两个开关页面上没有，原样回传即可。
    // 早先这里硬编码成 false/true，会把用户通过环境变量开的自动更新悄悄关掉。
    auto_update: !!prev.auto_update,
    require_confirm: prev.require_confirm !== undefined ? prev.require_confirm : true,
  };

  try {
    state.settings = await api('/api/settings', { method: 'PUT', body: JSON.stringify(payload) });
  } catch (e) {
    toast('保存失败：' + e.message, 'danger');
    return;
  }

  // 凭据变了就立刻换掉本页手里的那一份，
  // 否则下一个请求就会被自己刚设的密码挡在门外。
  if (payload.admin_password) {
    const user = payload.admin_user || 'admin';
    writeAuth(b64(user + ':' + payload.admin_password));
    rememberUser(user);
  } else {
    writeAuth('');
  }

  renderSettings();

  const mirrorChanged = (prev.registry_mirror || '') !== payload.registry_mirror;
  toast(mirrorChanged ? '已保存，加速地址已生效，点「立即扫描」验证' : '设置已保存并生效');
}

async function testFeishu() {
  const btn = $('#btn-test-feishu');
  btn.disabled = true;

  // 先取一次回调计数当基线，之后只要它变大，就说明回调真的到了。
  // 用增量而不是绝对时间，是为了不被上一次测试留下的旧记录干扰。
  let base = 0;
  try {
    const st = await api('/api/feishu/status');
    base = st.callback_count || 0;
  } catch (_) { /* 状态读不到不影响发卡片 */ }

  try {
    await api('/api/feishu/test', { method: 'POST' });
    toast('测试卡片已发送，请到飞书群点一下卡片上的按钮');
  } catch (e) {
    toast('发送失败：' + e.message, 'danger');
    btn.disabled = false;
    return;
  }
  btn.disabled = false;
  waitFeishuCallback(base);
}

// waitFeishuCallback 轮询等待按钮回调到达，最多 90 秒。
//
// 卡片发出去只证明「消息通道」通；按钮点击走的是长连接回调，是另一条链路。
// 等一个确定的信号回来，用户就不必去翻容器日志猜到底配没配对。
function waitFeishuCallback(base) {
  const el = $('#feishu-callback-state');
  if (!el) return;
  el.hidden = false;
  el.className = 'callback-state waiting';
  el.textContent = '⏳ 等待按钮回调…请在飞书群里点一下卡片上的按钮';

  let left = 45; // 45 次 × 2 秒 = 90 秒
  const timer = setInterval(async () => {
    left -= 1;
    if (left <= 0) {
      clearInterval(timer);
      el.className = 'callback-state bad';
      el.innerHTML = '未收到按钮回调。请检查「回调配置」里是否加了 <code>card.action.trigger</code>，' +
        '以及改完权限/回调后是否<b>发布了新版本</b>。';
      return;
    }
    try {
      const st = await api('/api/feishu/status');
      if ((st.callback_count || 0) > base) {
        clearInterval(timer);
        el.className = 'callback-state ok';
        const t = st.last_callback_at ? fmtTime(Date.parse(st.last_callback_at)) : '';
        el.textContent = '✅ 已收到按钮回调' + (t ? '（' + t + '）' : '') + '，按钮链路正常';
      }
    } catch (_) { /* 单次失败忽略，下一轮重试 */ }
  }, 2000);
}

function renderRegistryTest(res) {
  const rows = (res.results || []).map((r) => `
    <div class="test-line ${r.ok ? 'ok' : 'bad'}">
      <span class="test-host mono">${esc(r.label)}${r.mirror ? '<i class="tag-mini">加速</i>' : ''}</span>
      <span class="test-res">${r.ok
        ? '连通 · ' + r.elapsed_ms + ' ms'
        : '失败 · ' + esc(r.error_brief || r.error || '未知原因')}</span>
    </div>`).join('');

  const dm = res.daemon_mirrors || [];
  const hint = dm.length
    ? `<p class="field-hint">Docker 守护进程已配加速器：<code>${esc(dm.join(', '))}</code>，加速地址留空时会自动沿用。</p>`
    : `<p class="field-hint">Docker 守护进程未配置 registry-mirrors。</p>`;

  const allBad = (res.results || []).length > 0 && res.results.every((r) => !r.ok);
  const advice = allBad
    ? `<p class="field-hint warn-text">全部不通。若网络能正常上网，多半是 Docker Hub 在国内不可达，
       请在上一栏填入可用的加速地址（例如 <code>docker.1ms.run</code> 或 <code>docker.xuanyuan.me</code>）后重试。</p>`
    : '';

  return `
    <p class="field-hint">探针镜像：<code>${esc(res.reference)}</code>${res.docker_hub ? '（Docker Hub）' : ''}</p>
    ${rows}
    ${hint}
    ${advice}
  `;
}

// 逐个探测直连与各加速源。镜像全是「未知」时，这一步能把
// 「到底是网络不通、还是 tag 不在」直接摊开，不用去猜。
async function testRegistry() {
  const btn = $('#btn-test-registry');
  const box = $('#registry-test-result');
  if (!btn || !box) return;

  btn.disabled = true;
  const label = btn.textContent;
  btn.textContent = '检测中…';
  box.hidden = false;
  box.innerHTML = '<p class="field-hint">正在逐个探测，可能要十几秒…</p>';

  try {
    const res = await api('/api/registry/test', {
      method: 'POST',
      body: JSON.stringify({ reference: '' }),
    });
    box.innerHTML = renderRegistryTest(res);
  } catch (e) {
    box.innerHTML = `<p class="test-line bad">检测失败：${esc(e.message)}</p>`;
  } finally {
    btn.disabled = false;
    btn.textContent = label;
  }
}

const TABS = ['containers', 'images', 'tasks', 'notifications', 'settings'];

// 标签页写进 hash，刷新后还能停在原来那一页
function initialTab() {
  const h = (location.hash || '').replace(/^#/, '');
  return TABS.includes(h) ? h : 'containers';
}

function switchTab(name) {
  state.tab = name;
  document.querySelectorAll('.tab').forEach((el) => {
    el.classList.toggle('active', el.dataset.tab === name);
  });
  document.querySelectorAll('.panel').forEach((el) => {
    el.classList.toggle('active', el.id === 'panel-' + name);
  });
  try {
    const want = name === 'containers' ? location.pathname : '#' + name;
    const cur = location.hash || location.pathname;
    if (cur !== want) history.replaceState(null, '', want);
  } catch (_) { /* 某些内嵌浏览器禁用 history，忽略即可 */ }
}

/* ---------- 事件绑定 ---------- */

document.addEventListener('click', (ev) => {
  const tab = ev.target.closest('.tab');
  if (tab) { switchTab(tab.dataset.tab); return; }

  if (ev.target.closest('[data-close]')) {
    $('#drawer').hidden = true;
    return;
  }

  if (ev.target.closest('[data-close-modal]')) {
    closeCleanModal();
    return;
  }

  const row = ev.target.closest('.row');
  if (row && row.dataset.task && !ev.target.closest('button')) {
    openTaskLog(row.dataset.task);
    return;
  }

  const btn = ev.target.closest('button[data-act]');
  if (btn) {
    const rowEl = btn.closest('.row');
    const act = btn.dataset.act;

    // 镜像页只剩「删除」一个动作；更新容器请到「容器」页
    if (act === 'del') {
      const img = state.images.find((i) => i.id === rowEl.dataset.imgid);
      if (!img) return;
      removeLocalImage(img);
      return;
    }

    // 容器页：更新 = 拉新镜像 + 重建容器
    const id = Number(rowEl.dataset.id);
    const img = state.containers.find((i) => i.id === id);
    if (!img) return;
    if (act === 'update') triggerUpdate(id);
    if (act === 'ignore') toggleIgnore(img);
    if (act === 'remove') removeMonitorItem(id);
    return;
  }

  const readBtn = ev.target.closest('button[data-read]');
  if (readBtn) {
    api('/api/notifications/' + readBtn.dataset.read + '/read', { method: 'POST' })
      .then(() => { loadNotifications(); loadStats(); })
      .catch((e) => toast('操作失败：' + e.message, 'danger'));
    return;
  }

  if (ev.target.id === 'btn-scan') doScan(false);
  if (ev.target.id === 'btn-force') doScan(true);
  if (ev.target.id === 'btn-test-feishu') testFeishu();
  if (ev.target.id === 'btn-test-registry') testRegistry();
  if (ev.target.id === 'btn-clean') openCleanModal();
  if (ev.target.id === 'btn-clean-ok') doClean();
  if (ev.target.id === 'btn-remove-picked') removePicked();
  if (ev.target.id === 'btn-read-all') {
    api('/api/notifications/read-all', { method: 'POST' })
      .then(() => { loadNotifications(); loadStats(); toast('已全部标记为已读'); })
      .catch((e) => toast('操作失败：' + e.message, 'danger'));
  }
  if (ev.target.id === 'btn-clear-read') {
    if (!confirm('清空全部已读通知？此操作不可撤销。')) return;
    api('/api/notifications/read', { method: 'DELETE' })
      .then((r) => { loadNotifications(); loadStats(); toast('已清空 ' + (r.deleted || 0) + ' 条'); })
      .catch((e) => toast('操作失败：' + e.message, 'danger'));
  }
  if (ev.target.id === 'btn-add') {
    const ref = prompt('输入要监控的镜像引用，例如 nginx:latest 或 ghcr.io/foo/bar:1.2.3');
    if (!ref) return;
    api('/api/images', { method: 'POST', body: JSON.stringify({ reference: ref.trim() }) })
      .then(() => { toast('已添加，下次扫描开始监控'); loadImages(); })
      .catch((e) => toast('添加失败：' + e.message, 'danger'));
  }
});

document.addEventListener('input', (ev) => {
  if (ev.target.id === 'container-search') {
    state.containerSearch = ev.target.value;
    renderContainers();
  }
  if (ev.target.id === 'image-search') {
    state.imageSearch = ev.target.value;
    renderImages();
  }
});

document.addEventListener('change', (ev) => {
  const sel = ev.target.closest('select[data-mode]');
  if (sel) {
    setMode(Number(sel.dataset.mode), sel.value);
    return;
  }

  const pick = ev.target.closest('input[data-pick]');
  if (pick) {
    const id = pick.dataset.pick;
    if (pick.checked) state.picked.add(id);
    else state.picked.delete(id);
    return;
  }

  if (ev.target.name === 'clean-mode') renderCleanPreview();
});

document.addEventListener('submit', (ev) => {
  if (ev.target.id === 'settings-form') saveSettings(ev);
});

/* ---------- 启动 ---------- */

// 轮询间隔。列表内容只有两个来源会变：本机扫描，以及**在飞书里点了卡片按钮**。
// 后者完全绕开页面 —— 页面不主动回查，就会一直停在旧数据上：
// 容器早就重建完了，列表里还写着「有更新」，看着像根本没生效。
const POLL_MS = 5000;
// 空闲时也按轮数定期整份重载。飞书那边的操作不经过页面，
// 只靠「忙 → 闲」的状态跳变会漏掉（比如页面停在别的标签时）。
const IDLE_FULL_EVERY = 6;

async function pollTick() {
  if (pollBusy || !uiReady) return;
  pollBusy = true;
  try {
    const wasBusy = !!(state.stats && (state.stats.updating || state.stats.scanning));
    await loadStats();
    const busy = !!(state.stats && (state.stats.updating || state.stats.scanning));

    // 任务在跑 → 整份重载看进度；刚跑完 → 再整份重载一次，把最终结果取回来。
    // 这一次是必须的：更新结束的那一刻数据才真正落定，只刷统计会漏掉列表变化。
    if (busy || wasBusy) {
      idleTicks = 0;
      await refresh({ quiet: true });
      return;
    }

    idleTicks = (idleTicks + 1) % IDLE_FULL_EVERY;
    if (idleTicks === 0) await refresh({ quiet: true });
  } catch (_) {
    // 网络抖一下就跳过这一轮，下一轮自己会好
  } finally {
    pollBusy = false;
  }
}

function startPolling() {
  if (pollTimer) return;
  pollTimer = setInterval(pollTick, POLL_MS);
}

// 页面被切走再切回来、或者重新拿到焦点时补一次整份刷新。
// 飞牛桌面是用 iframe 常驻加载页面的，切来切去并不会重新执行 boot()，
// 数据就停在切走的那一刻 —— 重新看到页面时它应该是当下的事实。
document.addEventListener('visibilitychange', () => {
  if (document.visibilityState === 'visible' && uiReady) refresh({ quiet: true });
});
window.addEventListener('focus', () => {
  if (uiReady) refresh({ quiet: true });
});

function stopPolling() {
  if (pollTimer) {
    clearInterval(pollTimer);
    pollTimer = null;
  }
}

async function doLogin(ev) {
  ev.preventDefault();

  const user = ($('#login-user').value || '').trim() || 'admin';
  const pass = $('#login-pass').value || '';
  const prev = authToken;

  writeAuth(b64(user + ':' + pass));

  try {
    await loadStats();
  } catch (err) {
    writeAuth(prev);
    if (err && err.unauthorized) {
      showLogin('用户名或密码不正确');
    } else {
      showLogin((err && err.message) || '登录失败');
    }
    return;
  }

  hideLogin();
  uiReady = true;
  switchTab(initialTab());
  toast('已登录');
  rememberUser(user);
  loadSettings().catch(() => {});
  refresh();
  startPolling();
}

async function boot() {
  try {
    await loadStats();
  } catch (err) {
    if (err && err.unauthorized) {
      // 登录层已经显示，等用户输入
      return;
    }
    toast((err && err.message) || '无法连接到服务', 'danger');
    return;
  }

  hideLogin();
  uiReady = true;
  switchTab(initialTab());
  rememberUser(userFromToken());
  // 打开页面先整份取一次，不要先摆上一屏旧数据再等轮询慢慢补
  loadSettings().catch(() => {});
  refresh();
  startPolling();
}

document.addEventListener('submit', (ev) => {
  if (ev.target.id === 'login-form') doLogin(ev);
});

// 主题按钮有两个（顶栏一个、登录层一个），绑同一个切换动作。
// data-theme 已由 <head> 里的内联脚本设好，这里只把按钮的提示语对齐。
document.querySelectorAll('#btn-theme, #btn-theme-login').forEach((btn) => {
  btn.addEventListener('click', toggleTheme);
});
applyTheme(currentTheme());

boot();
