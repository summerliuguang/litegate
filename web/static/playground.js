'use strict';
renderNav();

const chatLog = document.getElementById('chatLog');
const fInput = document.getElementById('fInput');
const fSystem = document.getElementById('fSystem');
const btnSend = document.getElementById('btnSend');
const btnStop = document.getElementById('btnStop');
const btnClear = document.getElementById('btnClear');
const btnSys = document.getElementById('btnSys');
const voiceDescRow = document.getElementById('voiceDescRow');
const fVoiceDesc = document.getElementById('fVoiceDesc');
const btnAttach = document.getElementById('btnAttach');
const fFiles = document.getElementById('fFiles');
const attachRow = document.getElementById('attachRow');
const modelChip = document.getElementById('modelChip');
const chipName = document.getElementById('chipName');
const modelMenu = document.getElementById('modelMenu');
const ctxInfo = document.getElementById('ctxInfo');
const sysModal = document.getElementById('sysModal');
const tplName = document.getElementById('tplName');
const tplSearch = document.getElementById('tplSearch');
const tplList = document.getElementById('tplList');

let models = [];
let model = '';
let turns = [];        // 每轮:{user, variants:[{reasoning,acc,url?,size?,usage?}], cur, el, kind, text/ref/desc/file/name}
let attachments = [];
let busy = false;
let ctrl = null;
let appliedTplName = ''; // 已采用的系统提示模板名(显示在主页"系统提示"旁)

const IMG_TYPES = ['image/png', 'image/jpeg', 'image/webp', 'image/gif'];
const TEXT_EXT = /\.(txt|md|json|csv|log|xml|ya?ml|py|js|ts|go|html|css|sql|sh)$/i;
const SYS_TPL_KEY = 'litegate.playground.sysTemplates';
const OLD_TPL_KEY = 'litegate.playground.templates';

const KIND_LABEL = { chat: '语言', omni: '多模态', asr: '语音转写', tts: '语音合成' };
function kindOf(m) {
  if (/tts/i.test(m)) return 'tts';
  if (/asr/i.test(m)) return 'asr';
  if (/vision|omni/i.test(m) || m === 'deepseek-flash') return 'omni';
  return 'chat';
}
function ttsVariant(m) {
  if (/voiceclone/i.test(m)) return 'clone';
  if (/voicedesign/i.test(m)) return 'design';
  return 'base';
}

// ===== 系统提示模板(带名字,存 localStorage;旧格式字符串数组自动迁移) =====
function loadSysTpls() {
  try {
    const cur = JSON.parse(localStorage.getItem(SYS_TPL_KEY) || 'null');
    if (Array.isArray(cur)) return cur.filter(t => t && t.name && t.text != null);
  } catch (_) {}
  try {
    const old = JSON.parse(localStorage.getItem(OLD_TPL_KEY) || '[]');
    if (Array.isArray(old) && old.length && typeof old[0] === 'string') {
      const mig = old.map((t, i) => ({ name: '模板' + (i + 1), text: t }));
      saveSysTpls(mig);
      return mig;
    }
  } catch (_) {}
  return [];
}
function saveSysTpls(a) { try { localStorage.setItem(SYS_TPL_KEY, JSON.stringify(a)); } catch (_) {} }
function updateSysChip() {
  btnSys.textContent = appliedTplName ? '系统提示 · ' + appliedTplName : '系统提示';
  btnSys.classList.toggle('on', !!fSystem.value.trim());
}

// ===== 模型 =====
let priceMap = {}; // model -> {input,output,currency}（对比/成本预估用）

async function loadModels() {
  try {
    const d = await api('/api/admin/playground/models');
    models = d.models || [];
    priceMap = d.prices || {};
    modelMenu.innerHTML = models.map(m => {
      const k = kindOf(m);
      return `<button type="button" data-m="${esc(m)}"><span class="name">${esc(m)}</span><span class="kind">${KIND_LABEL[k]}</span><span class="tick" style="visibility:hidden">✓</span></button>`;
    }).join('')
      || '<button type="button" disabled>（暂无启用模型）</button>';
    model = models[0] || '';
    chipName.textContent = model || '（暂无启用模型）';
    modelMenu.querySelectorAll('button[data-m]').forEach(b => {
      b.addEventListener('click', () => {
        model = b.dataset.m;
        chipName.textContent = model;
        syncMenu();
        applyKind();
        toggleMenu(modelMenu, false);
      });
    });
    syncMenu();
    applyKind();
  } catch (ex) { toast('模型列表加载失败：' + ex.message, false); }
}

function syncMenu() {
  modelMenu.querySelectorAll('button[data-m]').forEach(b => {
    const sel = b.dataset.m === model;
    b.classList.toggle('sel', sel);
    b.querySelector('.tick').style.visibility = sel ? 'visible' : 'hidden';
  });
}

function toggleMenu(menu, open) {
  menu.classList.toggle('open', open == null ? !menu.classList.contains('open') : open);
}
modelChip.addEventListener('click', e => { e.stopPropagation(); toggleMenu(modelMenu); });
document.addEventListener('click', e => {
  if (!e.target.closest('.model-menu') && !e.target.closest('.model-chip')) toggleMenu(modelMenu, false);
});

function applyKind() {
  const k = kindOf(model);
  const v = k === 'tts' ? ttsVariant(model) : null;
  btnAttach.style.display = (k === 'tts' && v === 'clone') || k === 'asr' ? 'flex' : 'none';
  fFiles.accept = (k === 'asr' || (k === 'tts' && v === 'clone'))
    ? 'audio/wav,audio/mpeg,.wav,.mp3'
    : 'image/png,image/jpeg,image/webp,image/gif,.txt,.md,.json,.csv,.log,.xml,.yaml,.yml,.py,.js,.ts,.go,.html,.css,.sql,.sh';
  if (k === 'tts' || k === 'asr') attachments = attachments.filter(a => a.kind === 'audio');
  else attachments = attachments.filter(a => a.kind !== 'audio');
  renderAttachments();
  voiceDescRow.style.display = (k === 'tts' && v === 'design') ? 'block' : 'none';
  fInput.placeholder = {
    chat: '输入消息，Enter 发送，Shift+Enter 换行',
    omni: '输入问题，可附加图片提问',
    asr: '附加音频文件（wav/mp3），发送转写',
    tts: v === 'clone' ? '输入要合成的文本，并附加参考音频（wav/mp3）'
       : v === 'design' ? '输入要合成的文本，并在上方填写声音风格描述'
       : '输入要合成的文本，发送即合成语音',
  }[k] || '';
}

function renderAttachments() {
  attachRow.innerHTML = attachments.map((a, i) => {
    let thumb;
    if (a.kind === 'image') thumb = `<img src="${a.dataUrl}" alt="">`;
    else if (a.kind === 'audio') thumb = `<svg width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" aria-hidden="true"><path d="M12 2v14"/><path d="M8 6l4-4 4 4"/><rect x="4" y="14" width="16" height="8" rx="2"/></svg>`;
    else thumb = `<svg width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" aria-hidden="true"><path d="M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8z"/><path d="M14 3v5h5"/></svg>`;
    return `<span class="att-chip">${thumb}<span class="n">${esc(a.name)}</span>
      <button type="button" data-i="${i}" aria-label="移除附件">✕</button></span>`;
  }).join('');
  attachRow.querySelectorAll('button').forEach(b =>
    b.addEventListener('click', () => { attachments.splice(+b.dataset.i, 1); renderAttachments(); }));
}

btnAttach.addEventListener('click', () => fFiles.click());
fFiles.addEventListener('change', () => {
  const k = kindOf(model);
  for (const file of fFiles.files) {
    if (k === 'asr' || (k === 'tts' && ttsVariant(model) === 'clone')) {
      if (!file.type.startsWith('audio/') && !/\.(wav|mp3|m4a|webm)$/i.test(file.name)) {
        toast('仅支持音频文件（wav/mp3）：' + file.name, false); continue;
      }
      if (file.size > 16 * 1024 * 1024) { toast(`音频 ${file.name} 超过 16MB，已跳过`, false); continue; }
      if (k === 'asr') {
        attachments = [{ name: file.name, kind: 'audio', file }];
        renderAttachments();
      } else {
        const r = new FileReader();
        r.onload = () => { attachments = [{ name: file.name, kind: 'audio', dataUrl: r.result }]; renderAttachments(); };
        r.readAsDataURL(file);
      }
    } else if (IMG_TYPES.includes(file.type)) {
      if (file.size > 8 * 1024 * 1024) { toast(`图片 ${file.name} 超过 8MB，已跳过`, false); continue; }
      const r = new FileReader();
      r.onload = () => { attachments.push({ name: file.name, kind: 'image', dataUrl: r.result }); renderAttachments(); };
      r.readAsDataURL(file);
    } else if (TEXT_EXT.test(file.name)) {
      if (file.size > 256 * 1024) { toast(`文本 ${file.name} 超过 256KB，已跳过`, false); continue; }
      const r = new FileReader();
      r.onload = () => { attachments.push({ name: file.name, kind: 'text', text: r.result }); renderAttachments(); };
      r.readAsText(file);
    } else {
      toast(`不支持的类型：${file.name}`, false);
    }
  }
  fFiles.value = '';
});

function buildContent(text) {
  let full = text;
  for (const a of attachments) {
    if (a.kind === 'text') full += `\n\n【附件：${a.name}】\n\`\`\`\n${a.text}\n\`\`\``;
  }
  const images = attachments.filter(a => a.kind === 'image');
  if (!images.length) return full;
  if (!/vision|omni/i.test(model) && model !== 'deepseek-flash') toast('图片以 base64 随请求发送，是否接受取决于模型的图像理解能力', false);
  const parts = [{ type: 'text', text: full }];
  for (const img of images) parts.push({ type: 'image_url', image_url: { url: img.dataUrl } });
  return parts;
}

function setCtx(tokens) {
  ctxInfo.textContent = tokens == null ? '—' : fmtTok(tokens);
}

function bubble(cls, text) {
  const ph = chatLog.querySelector('div.has-text-grey');
  if (ph) ph.remove();
  const el = document.createElement('div');
  el.className = 'msg ' + cls;
  if (text != null) el.textContent = text;
  chatLog.appendChild(el);
  chatLog.scrollTop = chatLog.scrollHeight;
  return el;
}

function renderStream(el, reasoning, acc) {
  let det = el.querySelector('details.reasoning');
  if (reasoning) {
    if (!det) {
      det = document.createElement('details');
      det.className = 'reasoning';
      det.open = true;
      const sum = document.createElement('summary');
      sum.textContent = '思考过程';
      const body = document.createElement('div');
      body.className = 'reasoning-body';
      det.appendChild(sum);
      det.appendChild(body);
      el.appendChild(det);
    }
    det.querySelector('.reasoning-body').textContent = reasoning;
    if (acc && !el.dataset.rc) {
      det.open = false;
      el.dataset.rc = '1';
    }
  }
  let ans = el.querySelector('span.answer');
  if (!ans) {
    ans = document.createElement('span');
    ans.className = 'answer';
    el.appendChild(ans);
  }
  ans.textContent = acc || (reasoning ? '' : '…');
  chatLog.scrollTop = chatLog.scrollHeight;
}

function setUI(streaming) {
  busy = streaming;
  btnSend.style.display = streaming ? 'none' : 'flex';
  btnStop.style.display = streaming ? 'flex' : 'none';
}

function renderActions(turn) {
  const old = turn.el.querySelector('.actions');
  if (old) old.remove();
  const actions = document.createElement('div');
  actions.className = 'actions';
  if (turn.variants.length > 1) {
    const nav = document.createElement('span');
    nav.className = 'var-nav';
    nav.innerHTML = `<button type="button" data-d="-1" ${turn.cur <= 0 ? 'disabled' : ''}>‹</button>` +
      `<span>${turn.cur + 1}/${turn.variants.length}</span>` +
      `<button type="button" data-d="1" ${turn.cur >= turn.variants.length - 1 ? 'disabled' : ''}>›</button>`;
    nav.querySelectorAll('button').forEach(b => b.addEventListener('click', () => {
      const next = turn.cur + (+b.dataset.d);
      if (next < 0 || next >= turn.variants.length) return;
      turn.cur = next;
      if (turn.kind === 'tts') renderTTSContent(turn);
      else renderTurnContent(turn);
      renderActions(turn);
    }));
    actions.appendChild(nav);
  }
  if (turn.kind !== 'tts') {
    const copy = document.createElement('button');
    copy.type = 'button';
    copy.className = 'act-btn';
    copy.textContent = '复制';
    copy.addEventListener('click', async () => {
      const v = turn.variants[turn.cur];
      const text = (v ? v.acc : '') || '';
      try { await navigator.clipboard.writeText(text); toast('已复制'); }
      catch (_) {
        const ta = document.createElement('textarea');
        ta.value = text; document.body.appendChild(ta); ta.select();
        document.execCommand('copy'); ta.remove(); toast('已复制');
      }
    });
    actions.appendChild(copy);
  }
  if (turn === turns[turns.length - 1]) {
    const regen = document.createElement('button');
    regen.type = 'button';
    regen.className = 'act-btn';
    regen.textContent = '重新生成';
    regen.addEventListener('click', () => {
      if (busy) return;
      if (turn.kind === 'tts') regenerateTTS(turn);
      else if (turn.kind === 'asr') regenerateASR(turn);
      else regenerateChat(turn);
    });
    actions.appendChild(regen);
  }
  if (actions.children.length) turn.el.appendChild(actions);
}

function renderTurnContent(turn) {
  const v = turn.variants[turn.cur] || { reasoning: '', acc: '' };
  renderStream(turn.el, v.reasoning, v.acc);
  renderMeta(turn);
  renderActions(turn);
}

function renderMeta(turn) {
  const v = turn.variants[turn.cur];
  const om = turn.el.querySelector('.meta');
  if (om) om.remove();
  if (v && v.usage) {
    const m = document.createElement('div');
    m.className = 'meta';
    m.textContent = `输入 ${v.usage.prompt_tokens || 0} tok · 输出 ${v.usage.completion_tokens || 0} tok`
      + (v.usage.cache_tokens ? ` · 缓存命中 ${v.usage.cache_tokens} tok` : '');
    turn.el.appendChild(m);
  }
  if (v && v.size != null) {
    const m = document.createElement('div');
    m.className = 'meta';
    m.textContent = `TTS 合成完成 · ${(v.size / 1024).toFixed(0)} KB`;
    turn.el.appendChild(m);
  }
}

function renderTTSContent(turn) {
  const v = turn.variants[turn.cur];
  if (!v || !v.url) return;
  turn.el.innerHTML = '';
  turn.el.appendChild(buildMiniPlayer(v.url));
  const dl = document.createElement('a');
  dl.href = v.url; dl.download = 'tts.mp3'; dl.textContent = '下载音频';
  dl.style.cssText = 'display:inline-block;margin:.3rem 0 0;font-size:.72rem;color:var(--primary,#3e7bfa)';
  turn.el.appendChild(dl);
  renderMeta(turn);
  renderActions(turn);
  chatLog.scrollTop = chatLog.scrollHeight;
}

function buildMiniPlayer(url) {
  const wrap = document.createElement('div');
  wrap.className = 'mini-player';
  const audio = new Audio(url);
  audio.preload = 'metadata';
  const pp = document.createElement('button');
  pp.type = 'button'; pp.className = 'pp';
  const playSvg = '<svg width="13" height="13" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><path d="M7 4l14 8-14 8z"/></svg>';
  const pauseSvg = '<svg width="13" height="13" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><rect x="6" y="5" width="4" height="14" rx="1"/><rect x="14" y="5" width="4" height="14" rx="1"/></svg>';
  pp.innerHTML = playSvg;
  const bar = document.createElement('div'); bar.className = 'bar';
  const fill = document.createElement('div'); fill.className = 'fill';
  bar.appendChild(fill);
  const tm = document.createElement('span'); tm.className = 'tm'; tm.textContent = '0:00';
  wrap.appendChild(pp); wrap.appendChild(bar); wrap.appendChild(tm);
  const fmt = s => isFinite(s) ? `${Math.floor(s / 60)}:${String(Math.floor(s % 60)).padStart(2, '0')}` : '0:00';
  pp.addEventListener('click', () => { audio.paused ? audio.play() : audio.pause(); });
  audio.addEventListener('play', () => pp.innerHTML = pauseSvg);
  audio.addEventListener('pause', () => { pp.innerHTML = playSvg; });
  audio.addEventListener('timeupdate', () => {
    const d = audio.duration || 0;
    fill.style.width = (d ? (audio.currentTime / d) * 100 : 0) + '%';
    tm.textContent = `${fmt(audio.currentTime)} / ${fmt(d)}`;
  });
  bar.addEventListener('click', e => {
    const r = bar.getBoundingClientRect();
    if (audio.duration) audio.currentTime = ((e.clientX - r.left) / r.width) * audio.duration;
  });
  return wrap;
}

function buildMsgs(skipLastAssistant) {
  const msgs = [];
  if (fSystem.value.trim()) msgs.push({ role: 'system', content: fSystem.value.trim() });
  turns.forEach((t, i) => {
    msgs.push({ role: 'user', content: t.user });
    const skip = skipLastAssistant && i === turns.length - 1;
    if (!skip) {
      const v = t.variants[t.cur];
      if (v && v.acc) msgs.push({ role: 'assistant', content: v.acc });
    }
  });
  return msgs;
}

function setCtx(tokens) {
  ctxInfo.textContent = tokens == null ? '—' : fmtTok(tokens);
}

function extractErr(d, resp) {
  if (d && d.error) {
    if (typeof d.error === 'string') return d.error;
    if (d.error.message) return d.error.message;
  }
  return 'HTTP ' + resp.status;
}

async function streamChat(body, turn, onDelta) {
  const resp = await fetch('/api/admin/playground/chat', {
    method: 'POST',
    headers: { 'Authorization': 'Bearer ' + getToken(), 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
    signal: ctrl.signal,
  });
  if (resp.status === 401) { clearToken(); location.href = '/login.html'; return null; }
  if (!resp.ok) {
    let d = null; try { d = await resp.json(); } catch (_) {}
    throw new Error(extractErr(d, resp));
  }
  const reader = resp.body.getReader();
  const dec = new TextDecoder();
  let buf = '';
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    buf += dec.decode(value, { stream: true });
    const lines = buf.split('\n');
    buf = lines.pop();
    for (const line of lines) {
      const s = line.trim();
      if (!s.startsWith('data:')) continue;
      const payload = s.slice(5).trim();
      if (payload === '[DONE]') continue;
      try {
        const j = JSON.parse(payload);
        if (j.usage) onDelta.usage = j.usage;
        const d = j.choices && j.choices[0] && (j.choices[0].delta || {});
        if (d.reasoning_content) onDelta.reasoning += d.reasoning_content;
        if (d.content) onDelta.acc += d.content;
      } catch (_) {}
    }
    renderStream(turn.el, onDelta.reasoning, onDelta.acc);
  }
}

async function send() {
  if (busy) return;
  const k = kindOf(model);
  const text = fInput.value.trim();
  if (k === 'tts') {
    if (!text) { toast('请输入要合成的文本', false); return; }
    await sendTTS(text);
    return;
  }
  if (k === 'asr') {
    const audio = attachments.find(a => a.kind === 'audio');
    if (!audio) { toast('请先通过 ＋ 附加音频文件（wav/mp3）', false); return; }
    await sendASR(audio);
    return;
  }
  if (!text && !attachments.length) return;
  if (!model) { toast('没有可用的模型', false); return; }
  fInput.value = '';
  fInput.style.height = 'auto';
  const userContent = buildContent(text);

  const turn = { kind: k, user: userContent, variants: [], cur: 0, el: null };
  const ub = bubble('user');
  if (typeof userContent === 'string') {
    ub.textContent = text;
  } else {
    const imgs = document.createElement('div');
    imgs.style.marginBottom = '.35rem';
    imgs.innerHTML = attachments.filter(a => a.kind === 'image')
      .map(a => `<img src="${a.dataUrl}" style="max-width:120px;max-height:90px;border-radius:10px;display:block;margin-bottom:.3rem">`).join('');
    const tx = document.createElement('span');
    tx.textContent = text;
    ub.appendChild(imgs);
    ub.appendChild(tx);
  }
  attachments = [];
  renderAttachments();
  setUI(true);
  turn.el = bubble('assistant');
  renderStream(turn.el, '', '');
  turns.push(turn);

  const onDelta = { acc: '', reasoning: '' };
  let usage = null;
  ctrl = new AbortController();
  try {
    usage = await streamChat({ model, messages: buildMsgs(false) }, turn, onDelta) || usage;
    turn.variants.push({ reasoning: onDelta.reasoning, acc: onDelta.acc, usage });
    turn.cur = turn.variants.length - 1;
    renderTurnContent(turn);
    if (usage) setCtx((usage.prompt_tokens || 0) + (usage.completion_tokens || 0));
  } catch (ex) {
    if (ex.name === 'AbortError') {
      if (onDelta.acc) { turn.variants.push({ reasoning: onDelta.reasoning, acc: onDelta.acc, usage }); turn.cur = turn.variants.length - 1; renderTurnContent(turn); }
      else { turn.el.classList.add('err'); turn.el.textContent = '（已停止）'; }
    } else {
      turn.el.classList.add('err');
      turn.el.textContent = '调用失败：' + ex.message;
    }
  } finally {
    busy = false; ctrl = null; setUI(false); fInput.focus();
  }
}

async function regenerateChat(turn) {
  if (busy) return;
  setUI(true);
  renderStream(turn.el, '', '');
  const onDelta = { acc: '', reasoning: '' };
  let usage = null;
  ctrl = new AbortController();
  try {
    usage = await streamChat({ model, messages: buildMsgs(true) }, turn, onDelta) || usage;
    turn.variants.push({ reasoning: onDelta.reasoning, acc: onDelta.acc, usage });
    turn.cur = turn.variants.length - 1;
    renderTurnContent(turn);
    if (usage) setCtx((usage.prompt_tokens || 0) + (usage.completion_tokens || 0));
  } catch (ex) {
    if (ex.name === 'AbortError') {
      if (onDelta.acc) { turn.variants.push({ reasoning: onDelta.reasoning, acc: onDelta.acc, usage }); turn.cur = turn.variants.length - 1; renderTurnContent(turn); }
      else renderStream(turn.el, onDelta.reasoning, onDelta.acc);
    } else {
      turn.el.classList.add('err');
      turn.el.textContent = '重新生成失败：' + ex.message;
    }
  } finally {
    busy = false; ctrl = null; setUI(false); fInput.focus();
  }
}

async function sendTTS(text) {
  const v = ttsVariant(model);
  const ref = attachments.find(a => a.kind === 'audio');
  const desc = fVoiceDesc ? fVoiceDesc.value.trim() : '';
  if (v === 'clone' && !ref) { toast('voiceclone 需要先附加参考音频（wav/mp3）', false); return; }
  if (v === 'design' && !desc) { toast('voicedesign 需要填写声音风格描述', false); return; }

  fInput.value = '';
  const turn = { kind: 'tts', user: text, text, ref: ref ? ref.dataUrl : null, desc, variants: [], cur: 0, el: null };
  bubble('user', text);
  setUI(true);
  turn.el = bubble('assistant');
  turn.el.textContent = '合成中…';
  turns.push(turn);
  ctrl = new AbortController();
  await ttsRequest(turn, ctrl);
  busy = false; ctrl = null; setUI(false); fInput.focus();
}

async function regenerateTTS(turn) {
  if (busy) return;
  setUI(true);
  turn.el.innerHTML = '';
  turn.el.textContent = '合成中…';
  ctrl = new AbortController();
  await ttsRequest(turn, ctrl);
  busy = false; ctrl = null; setUI(false); fInput.focus();
}

async function ttsRequest(turn, aborter) {
  const v = ttsVariant(model);
  const msgs = v === 'design'
    ? [{ role: 'user', content: '请用以下声音风格朗读：' + turn.desc }, { role: 'assistant', content: turn.text }]
    : [{ role: 'user', content: '请朗读以下内容' }, { role: 'assistant', content: turn.text }];
  const audio = v === 'clone' ? { format: 'mp3', voice: turn.ref }
    : v === 'design' ? { format: 'mp3' }
    : { format: 'mp3', voice: 'mimo_default' };
  try {
    const resp = await fetch('/api/admin/playground/chat', {
      method: 'POST',
      headers: { 'Authorization': 'Bearer ' + getToken(), 'Content-Type': 'application/json' },
      body: JSON.stringify({ model, messages: msgs, modalities: ['audio'], audio, stream: false }),
      signal: aborter.signal,
    });
    if (resp.status === 401) { clearToken(); location.href = '/login.html'; return; }
    if (!resp.ok) {
      let d = null; try { d = await resp.json(); } catch (_) {}
      throw new Error(extractErr(d, resp));
    }
    const j = await resp.json();
    const b64 = j.choices && j.choices[0] && j.choices[0].message
      && j.choices[0].message.audio && j.choices[0].message.audio.data;
    if (!b64) throw new Error('上游响应不含音频数据');
    const bytes = Uint8Array.from(atob(b64), c => c.charCodeAt(0));
    const blob = new Blob([bytes], { type: 'audio/mpeg' });
    const url = URL.createObjectURL(blob);
    turn.variants.push({ url, size: blob.size });
    turn.cur = turn.variants.length - 1;
    renderTTSContent(turn);
  } catch (ex) {
    if (ex.name === 'AbortError') { turn.el.textContent = '（已停止）'; }
    else { turn.el.classList.add('err'); turn.el.textContent = '合成失败：' + ex.message; }
  }
}

async function sendASR(audioAtt) {
  fInput.value = '';
  setUI(true);
  const turn = { kind: 'asr', file: audioAtt.file, name: audioAtt.name, variants: [{ acc: '转写中…' }], cur: 0, el: null };
  bubble('user', '〔音频转写〕' + audioAtt.name);
  turn.el = bubble('assistant');
  turn.el.textContent = '转写中…';
  turns.push(turn);
  ctrl = new AbortController();
  await asrRequest(turn, ctrl);
  busy = false; ctrl = null; setUI(false); fInput.focus();
}

async function regenerateASR(turn) {
  if (busy) return;
  setUI(true);
  turn.el.textContent = '转写中…';
  ctrl = new AbortController();
  await asrRequest(turn, ctrl);
  busy = false; ctrl = null; setUI(false); fInput.focus();
}

async function asrRequest(turn, aborter) {
  try {
    const fd = new FormData();
    fd.append('model', model);
    fd.append('file', turn.file, turn.name);
    const resp = await fetch('/api/admin/playground/audio/transcriptions', {
      method: 'POST',
      headers: { 'Authorization': 'Bearer ' + getToken() },
      body: fd,
      signal: aborter.signal,
    });
    if (resp.status === 401) { clearToken(); location.href = '/login.html'; return; }
    if (!resp.ok) {
      let d = null; try { d = await resp.json(); } catch (_) {}
      throw new Error(extractErr(d, resp));
    }
    const d = await resp.json();
    turn.variants = [{ acc: d.text || '（无转写结果）' }];
    turn.cur = 0;
    renderTurnContent(turn);
  } catch (ex) {
    if (ex.name === 'AbortError') { turn.el.textContent = '（已停止）'; }
    else { turn.el.classList.add('err'); turn.el.textContent = '转写失败：' + ex.message; }
  }
}

// ===== 系统提示卡片 =====
function openSysModal() {
  tplName.value = appliedTplName;
  tplSearch.value = '';
  renderTplList('');
  sysModal.classList.add('is-active');
  fSystem.focus();
}
function closeSysModal() { sysModal.classList.remove('is-active'); }

function renderTplList(filter) {
  const tpls = loadSysTpls();
  const f = (filter || '').toLowerCase();
  const matches = tpls.filter(t =>
    t.name.toLowerCase().includes(f) || t.text.toLowerCase().includes(f));
  tplList.innerHTML = matches.length ? '' : '<div class="has-text-grey is-size-7">暂无模板</div>';
  matches.forEach(t => {
    const item = document.createElement('div');
    item.className = 'tpl-item' + (t.name === appliedTplName && t.text === fSystem.value ? ' cur' : '');
    item.innerHTML = `<div class="t-info"><div class="t-name">${esc(t.name)}</div>` +
      `<div class="t-text">${esc(t.text)}</div></div>` +
      `<button type="button" class="del" data-name="${esc(t.name)}" title="删除模板">✕</button>`;
    item.addEventListener('click', () => {
      fSystem.value = t.text;
      tplName.value = t.name;
      item.parentElement.querySelectorAll('.tpl-item').forEach(x => x.classList.remove('cur'));
      item.classList.add('cur');
    });
    tplList.appendChild(item);
  });
  tplList.querySelectorAll('.del').forEach(d => d.addEventListener('click', e => {
    e.stopPropagation();
    const name = d.dataset.name;
    saveSysTpls(loadSysTpls().filter(t => t.name !== name));
    if (appliedTplName === name) appliedTplName = '';
    updateSysChip();
    renderTplList(tplSearch.value);
    toast('已删除模板：' + name);
  }));
}

document.getElementById('btnTplSave').addEventListener('click', () => {
  const name = tplName.value.trim();
  const text = fSystem.value.trim();
  if (!name) { toast('请先填写模板名字', false); return; }
  if (!text) { toast('提示词内容为空，没有可保存的内容', false); return; }
  const tpls = loadSysTpls();
  const exist = tpls.find(t => t.name === name);
  if (exist) exist.text = text;
  else tpls.push({ name, text });
  saveSysTpls(tpls);
  appliedTplName = name;
  updateSysChip();
  renderTplList(tplSearch.value);
  toast(exist ? '已覆盖模板：' + name : '已保存模板：' + name);
});
tplSearch.addEventListener('input', () => renderTplList(tplSearch.value));
document.getElementById('sysApply').addEventListener('click', () => {
  appliedTplName = tplName.value.trim();
  updateSysChip();
  closeSysModal();
  toast('系统提示已更新');
});
document.getElementById('sysClearPrompt').addEventListener('click', () => {
  fSystem.value = '';
  appliedTplName = '';
  tplName.value = '';
  updateSysChip();
  renderTplList(tplSearch.value);
});
btnSys.addEventListener('click', openSysModal);
document.getElementById('sysClose').addEventListener('click', closeSysModal);
document.querySelector('#sysModal .modal-background').addEventListener('click', closeSysModal);

// ===== 清空 =====
btnClear.addEventListener('click', () => {
  if (busy) ctrl && ctrl.abort();
  turns = [];
  setCtx(null);
  chatLog.innerHTML = '<div class="has-text-grey is-size-7" style="margin:auto">选个模型，发一句话试试</div>';
});
fInput.addEventListener('keydown', e => {
  if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); send(); }
});
fInput.addEventListener('input', () => {
  fInput.style.height = 'auto';
  fInput.style.height = Math.min(fInput.scrollHeight, 144) + 'px';
});

// ===== 成本预估（输入时粗估当前上下文 token 与成本） =====
function estTokens(text) {
  let cjk = 0, other = 0;
  for (const ch of text) (/[\u2e80-\u9fff\uf900-\ufaff\uff00-\uffef]/.test(ch) ? cjk++ : other++);
  return Math.round(cjk * 0.6 + other / 4);
}
function fmtEstCost(model, tok) {
  const p = priceMap[model];
  if (!p || !tok) return '';
  const cur = p.currency === 'CNY' ? '¥' : '$';
  const v = (p.input + p.output) / 1e6 * tok * 0.4; // 粗估：输入为主，乘 0.4 折算输出占比
  return ' · ≈' + cur + (v < 0.01 ? v.toFixed(4) : v.toFixed(3));
}
function updateEstimate() {
  const el = document.getElementById('estInfo');
  const msgs = buildMsgs(false);
  if (fInput.value.trim()) msgs.push({ role: 'user', content: fInput.value });
  let text = '';
  for (const m of msgs) text += typeof m.content === 'string' ? m.content : JSON.stringify(m.content);
  const tok = estTokens(text);
  el.textContent = tok ? `预估 ~${tok} tok${model ? fmtEstCost(model, tok) : ''}` : '';
}
fInput.addEventListener('input', updateEstimate);
document.getElementById('btnSys').addEventListener('click', () => setTimeout(updateEstimate, 50));
document.getElementById('sysApply').addEventListener('click', () => setTimeout(updateEstimate, 50));

// ===== 对比测试 =====
const cmpModal = document.getElementById('cmpModal');
function openCompare() {
  const opts = models.map(m => `<option value="${esc(m)}">${esc(m)}</option>`).join('');
  document.getElementById('cmpA').innerHTML = opts;
  document.getElementById('cmpB').innerHTML = opts;
  if (models.length > 1) document.getElementById('cmpB').value = models[1];
  document.getElementById('cmpGrid').style.display = 'none';
  cmpModal.classList.add('is-active');
}
document.getElementById('btnCompare').addEventListener('click', openCompare);
document.getElementById('cmpClose').addEventListener('click', () => cmpModal.classList.remove('is-active'));
document.getElementById('cmpCancel').addEventListener('click', () => cmpModal.classList.remove('is-active'));
document.querySelector('#cmpModal .modal-background').addEventListener('click', () => cmpModal.classList.remove('is-active'));

function fmtCostByUsage(m, usage) {
  const p = priceMap[m];
  if (!p || !usage) return '';
  const cur = p.currency === 'CNY' ? '¥' : '$';
  const v = ((usage.prompt_tokens || 0) * p.input + (usage.completion_tokens || 0) * p.output) / 1e6;
  return cur + (v < 0.01 ? v.toFixed(4) : v.toFixed(3));
}
async function streamCompareSide(model, prompt, sideEl) {
  const head = sideEl.querySelector('.cmp-head');
  const body = sideEl.querySelector('.cmp-body');
  const t0 = performance.now();
  let ttfb = null, acc = '', usage = null;
  const resp = await fetch('/api/admin/playground/chat', {
    method: 'POST',
    headers: { 'Authorization': 'Bearer ' + getToken(), 'Content-Type': 'application/json' },
    body: JSON.stringify({ model, messages: [{ role: 'user', content: prompt }], stream: true }),
  });
  if (!resp.ok || !resp.body) {
    let msg = 'HTTP ' + resp.status;
    try { const j = await resp.json(); if (j.error) msg = typeof j.error === 'string' ? j.error : JSON.stringify(j.error); } catch (_) {}
    head.innerHTML = `<b>${esc(model)}</b><span>失败：${esc(msg)}</span>`;
    return;
  }
  const reader = resp.body.getReader();
  const dec = new TextDecoder();
  let buf = '';
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    if (ttfb === null) ttfb = Math.round(performance.now() - t0);
    buf += dec.decode(value, { stream: true });
    const lines = buf.split('\n');
    buf = lines.pop();
    for (const line of lines) {
      const t = line.trim();
      if (!t.startsWith('data:')) continue;
      const pay = t.slice(5).trim();
      if (pay === '[DONE]') continue;
      try {
        const j = JSON.parse(pay);
        if (j.usage) usage = j.usage;
        const d = j.choices && j.choices[0] && (j.choices[0].delta || {});
        if (d.content) { acc += d.content; body.textContent = acc; }
      } catch (_) {}
    }
  }
  const total = Math.round(performance.now() - t0);
  const cost = fmtCostByUsage(model, usage);
  head.innerHTML = `<b>${esc(model)}</b><span>${ttfb ?? '-'} ms 首字 · ${total} ms 总` +
    (usage ? ` · ${usage.prompt_tokens || 0}+${usage.completion_tokens || 0} tok` : '') +
    (cost ? ` · ${cost}` : '') + '</span>';
}
document.getElementById('cmpGo').addEventListener('click', () => {
  const prompt = document.getElementById('cmpPrompt').value.trim();
  const ma = document.getElementById('cmpA').value, mb = document.getElementById('cmpB').value;
  if (!prompt) { toast('请输入提示词', false); return; }
  if (!ma || !mb) { toast('需要两个可用模型', false); return; }
  const grid = document.getElementById('cmpGrid');
  grid.style.display = 'grid';
  grid.innerHTML = `<div class="cmp-side" id="cmpSideA"><div class="cmp-head"><b>${esc(ma)}</b><span>请求中…</span></div><div class="cmp-body"></div></div>` +
    `<div class="cmp-side" id="cmpSideB"><div class="cmp-head"><b>${esc(mb)}</b><span>请求中…</span></div><div class="cmp-body"></div></div>`;
  const go = document.getElementById('cmpGo');
  go.disabled = true;
  Promise.allSettled([
    streamCompareSide(ma, prompt, document.getElementById('cmpSideA')),
    streamCompareSide(mb, prompt, document.getElementById('cmpSideB')),
  ]).finally(() => { go.disabled = false; });
});

loadModels();
updateSysChip();
fInput.focus();
