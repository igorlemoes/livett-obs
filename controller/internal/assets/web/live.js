// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

// Read-only "Ao vivo" view: stats cards + chat. All user text goes through textContent only.
const $ = id => document.getElementById(id);
const params = new URLSearchParams(location.search);
if (params.get('compact') === '1') document.body.classList.add('compact');
if (params.get('transparent') === '1') document.body.classList.add('transparent');
if (params.get('fs')) document.documentElement.style.fontSize = Math.min(24, Math.max(10, Number(params.get('fs')) || 14)) + 'px';

const ICONS = {
  eye: '<path d="M2 12s3.5-7 10-7 10 7 10 7-3.5 7-10 7S2 12 2 12z"/><circle cx="12" cy="12" r="3"/>',
  users: '<path d="M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2"/><circle cx="9" cy="7" r="4"/><path d="M22 21v-2a4 4 0 0 0-3-3.9M16 3.1a4 4 0 0 1 0 7.8"/>',
  heart: '<path d="M20.8 4.6a5.5 5.5 0 0 0-7.8 0L12 5.7l-1-1.1a5.5 5.5 0 0 0-7.8 7.8l1 1.1L12 21l7.8-7.5 1-1.1a5.5 5.5 0 0 0 0-7.8z"/>',
  chat: '<path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z"/>',
  plus: '<path d="M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2"/><circle cx="9" cy="7" r="4"/><path d="M19 8v6M22 11h-6"/>',
  share: '<circle cx="18" cy="5" r="3"/><circle cx="6" cy="12" r="3"/><circle cx="18" cy="19" r="3"/><path d="M8.6 13.5l6.8 4M15.4 6.5l-6.8 4"/>',
  gift: '<rect x="3" y="8" width="18" height="4"/><path d="M12 8v13M19 12v9H5v-9M7.5 8a2.5 2.5 0 0 1 0-5C11 3 12 8 12 8s1-5 4.5-5a2.5 2.5 0 0 1 0 5"/>',
};
const CARDS = [
  ['viewers', 'Agora', 'eye', true], ['total', 'Total', 'users'], ['likes', 'Curtidas', 'heart'],
  ['comments', 'Comentários', 'chat'], ['follows', 'Seguidores', 'plus'], ['shares', 'Compart.', 'share'], ['gifts', 'Presentes', 'gift'],
];
const cardEls = {};
for (const [key, label, icon, hero] of CARDS) {
  const card = document.createElement('div');
  card.className = 'card' + (hero ? ' hero' : '');
  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  svg.setAttribute('viewBox', '0 0 24 24');
  svg.innerHTML = ICONS[icon];  // static, trusted markup
  const body = document.createElement('div');
  const value = document.createElement('span'); value.className = 'v'; value.textContent = '–';
  const name = document.createElement('span'); name.className = 'k'; name.textContent = label;
  if (hero) { card.append(svg, body); body.append(value, name); } else { card.append(svg, value, name); }
  $('stats').append(card);
  cardEls[key] = { card, value, last: null };
}

function setStat(key, raw) {
  const el = cardEls[key];
  const text = raw == null ? '–' : Number(raw).toLocaleString('pt-BR');
  if (el.value.textContent !== text) {
    el.value.textContent = text;
    if (el.last != null && raw != null && raw > el.last) { el.card.classList.add('bump'); setTimeout(() => el.card.classList.remove('bump'), 500); }
  }
  el.last = raw;
}

const PHASES = { live: 'Ao vivo', ending: 'Encerrando', creating: 'Iniciando', closed: 'Encerrada', ready: 'Pronto', login: 'Entrar no TikTok', uncertain: 'Verificar', recovery: 'Recuperação' };
let startedAt = null, renderedUpTo = 0, stick = true, lastNames = new Set();

function hue(name) { let h = 0; for (const c of name) h = (h * 31 + c.codePointAt(0)) % 360; return h; }

function makeAvatar(m) {
  const av = document.createElement('div');
  av.className = 'av';
  const initial = (m.name || '?').trim().charAt(0).toUpperCase() || '?';
  av.textContent = initial;
  if (m.avatar && /^https:\/\//.test(m.avatar)) {
    const img = document.createElement('img');
    img.alt = ''; img.referrerPolicy = 'no-referrer'; img.decoding = 'async';  // NOT lazy: a detached <img> never loads
    img.addEventListener('load', () => { av.textContent = ''; av.classList.add('photo'); av.append(img); });
    img.addEventListener('error', () => img.remove());  // keep the colored initial
    img.src = m.avatar;
  }
  return av;
}

function addMessage(m) {
  const li = document.createElement('li');
  const h = hue(m.name || '?');
  if (m.k === 'chat') {
    li.className = 'msg'; li.style.setProperty('--h', h);
    const av = makeAvatar(m);
    const bubble = document.createElement('div'); bubble.className = 'bubble';
    const who = document.createElement('b'); who.textContent = m.name || 'Espectador';
    const text = document.createElement('span'); text.textContent = m.text;
    bubble.append(who, text); li.append(av, bubble);
  } else if (m.k === 'follow' || m.k === 'share' || m.k === 'fan') {
    li.className = 'event'; li.style.setProperty('--h', h);
    const av = makeAvatar(m);
    const body = document.createElement('div'); body.className = 'giftbody';
    const who = document.createElement('b'); who.textContent = m.name || 'Alguém';
    const what = document.createElement('span');
    what.textContent = m.k === 'follow' ? ' começou a seguir você' : m.k === 'share' ? ' compartilhou a LIVE' : ' entrou no clube de fãs' + (m.rank ? ' (nº ' + m.rank + ')' : '');
    body.append(who, what); li.append(av, body);
  } else if (m.k === 'gift') {
    li.className = 'gift'; li.style.setProperty('--h', h);
    const av = makeAvatar(m);
    const body = document.createElement('div'); body.className = 'giftbody';
    const who = document.createElement('b'); who.textContent = m.name || 'Espectador';
    const what = document.createElement('span');
    what.textContent = ' enviou ' + (m.gift || 'um presente') + (m.count > 1 ? ' × ' + m.count : '') + (m.coins ? ' · ' + m.coins + ' moedas' : '');
    body.append(who, what); li.append(av, body);
  } else {
    li.className = 'join';
    const who = document.createElement('b'); who.textContent = m.name || 'Alguém';
    li.append(who, document.createTextNode(' entrou'));
  }
  $('feed').append(li);
}

function fmtClock(sec) {
  const h = Math.floor(sec / 3600), m = Math.floor(sec % 3600 / 60), s = sec % 60;
  return (h ? h + ':' : '') + String(m).padStart(h ? 2 : 1, '0') + ':' + String(s).padStart(2, '0');
}

function tickClock(state) {
  if (!startedAt || !['live', 'ending'].includes(state.phase)) { $('clock').textContent = ''; return; }
  let text = '⏱ ' + fmtClock(Math.max(0, Math.round(Date.now() / 1000 - startedAt)));
  if (state.remainingSec != null) text += ' · restam ' + fmtClock(state.remainingSec);
  $('clock').textContent = text;
}

let lastState = {};
function render(state) {
  lastState = state;
  const live = ['live', 'ending'].includes(state.phase);
  $('app').dataset.state = state.phase === 'live' ? 'live' : state.phase === 'ending' ? 'ending' : 'idle';
  $('badgeText').textContent = PHASES[state.phase] || 'Offline';
  startedAt = state.startedAt || null;
  const view = state.view;
  const stats = (view && view.stats) || {};
  setStat('viewers', view ? (view.viewers ?? stats.viewers) : null);
  setStat('total', view ? (view.total ?? stats.total) : null);
  for (const k of ['likes', 'comments', 'follows', 'shares', 'gifts']) setStat(k, view ? stats[k] ?? null : null);
  const feed = (view && view.feed) || [];
  $('emptyText').textContent = live ? 'Nenhuma mensagem ainda. Os comentários aparecem aqui.' : 'Aguardando a LIVE começar…';
  $('feed').parentElement.classList.toggle('has', feed.length > 0);
  if (feed.length && feed[feed.length - 1].n < renderedUpTo) { $('feed').replaceChildren(); renderedUpTo = 0; }  // new broadcast
  const fresh = feed.filter(m => m.n > renderedUpTo);
  if (fresh.length) {
    const box = $('feed');
    stick = box.scrollTop + box.clientHeight >= box.scrollHeight - 30;
    fresh.forEach(addMessage);
    renderedUpTo = fresh[fresh.length - 1].n;
    while (box.children.length > 120) box.firstChild.remove();
    if (stick) box.scrollTop = box.scrollHeight; else $('more').hidden = false;
  }
  tickClock(state);
  syncSendBox();
}

// Host reply box: posts to the same /api/chat the panel and MCP use (live only, rate limited, no links, no repeats).
let panelToken = null, sending = false, noteTimer = null;
function note(text, bad) {
  const el = $('note');
  el.textContent = text; el.classList.toggle('bad', !!bad);
  clearTimeout(noteTimer);
  if (text) noteTimer = setTimeout(() => { el.textContent = ''; }, 6000);
}
function syncSendBox() {
  const live = lastState.phase === 'live';
  $('msg').disabled = !live; $('go').disabled = !live || sending;
  $('msg').placeholder = live ? 'Responder no chat da LIVE…' : 'Disponível durante a LIVE';
}
$('send').addEventListener('submit', async event => {
  event.preventDefault();
  const text = $('msg').value.trim();
  if (!text || sending) return;
  sending = true; syncSendBox();
  try {
    if (!panelToken) panelToken = (await (await fetch('/api/session', { cache: 'no-store' })).json()).token;
    const r = await fetch('/api/chat', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-Panel-Token': panelToken }, body: JSON.stringify({ text }) });
    const v = await r.json().catch(() => ({}));
    if (r.status === 403) panelToken = null;
    if (!r.ok) throw Error(v.error || 'Não foi possível enviar.');
    $('msg').value = ''; note('Enviada.');
  } catch (e) { note(e.message, true); }
  sending = false; syncSendBox(); if (!$('msg').disabled) $('msg').focus();
});

$('more').onclick = () => { const box = $('feed'); box.scrollTop = box.scrollHeight; $('more').hidden = true; };
$('feed').addEventListener('scroll', () => { const box = $('feed'); if (box.scrollTop + box.clientHeight >= box.scrollHeight - 30) $('more').hidden = true; });

async function poll() {
  try {
    const response = await fetch('/api/state', { cache: 'no-store' });
    if (response.ok) render(await response.json());
  } catch { $('badgeText').textContent = 'Sem conexão'; }
}
poll(); setInterval(poll, 1500); setInterval(() => tickClock(lastState), 1000);

// Self-refresh: if this page's own code changes on the server (an update), reload instead of staying stale.
(async function watchForUpdates() {
  const sign = async () => (await Promise.all(['/live.js', '/live.css'].map(u => fetch(u, { cache: 'no-store' }).then(r => r.text())))).join('\u0000');
  try {
    const first = await sign();
    setInterval(async () => { try { if (await sign() !== first) location.reload(); } catch { /* offline: try again later */ } }, 20000);
  } catch { /* ignore */ }
})();
