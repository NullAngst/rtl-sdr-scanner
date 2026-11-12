'use strict';

const $ = id => document.getElementById(id);

// Per-browser conveniences. Storage can throw (private windows, blocked
// site data), and the page has to work without it.
const store = {
  get(k, d) { try { const v = localStorage.getItem(k); return v === null ? d : v; } catch { return d; } },
  set(k, v) { try { localStorage.setItem(k, v); } catch {} },
};

let state = null;       // last PublicState from the server
let isAdmin = false;
let editingId = null;   // channel id in the form, null when adding
let lastError = '';

// ---------- theme ----------
const THEMES = ['crt', 'light', 'dark', 'verdigris', 'kawaii-light', 'kawaii-dark'];
function applyTheme(t) {
  if (!THEMES.includes(t)) t = 'crt';
  document.documentElement.dataset.theme = t;
  $('theme').value = t;
}
applyTheme(store.get('theme', 'crt'));
$('theme').addEventListener('change', e => { applyTheme(e.target.value); store.set('theme', e.target.value); });

// ---------- helpers ----------
let toastTimer;
function toast(msg, bad = false) {
  const t = $('toast');
  t.textContent = msg;
  t.classList.toggle('bad', bad);
  t.classList.add('show');
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => t.classList.remove('show'), 3000);
}

async function api(method, path, body) {
  const opts = { method, headers: {}, credentials: 'same-origin' };
  if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  const r = await fetch(path, opts);
  const d = await r.json().catch(() => ({}));
  if (!r.ok) {
    if (r.status === 401 && isAdmin) setAdmin(false);
    if (d.must_change) $('pw-dlg').showModal();
    const err = new Error(d.error || `HTTP ${r.status}`);
    err.status = r.status;
    throw err;
  }
  return d;
}

// Run an admin action and toast any error.
async function act(fn, okMsg) {
  try {
    await fn();
    if (okMsg) toast(okMsg);
  } catch (e) {
    toast(e.message || 'Something went wrong', true);
  }
}

function mhz(hz) { return (hz / 1e6).toFixed(4); }

// "162.400" is MHz. Anything over 3000 is taken as Hz, since the old
// version wanted Hz and the RTL-SDR tops out around 2200 MHz.
function parseFreq(text) {
  const s = String(text).trim().replace(',', '.');
  if (!/^\d+(\.\d+)?$/.test(s)) return NaN;
  const n = parseFloat(s);
  return n > 3000 ? Math.round(n) : Math.round(n * 1e6);
}

function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

// ---------- meter ----------
const segs = [];
(function buildSegs() {
  const row = $('segs');
  for (let i = 0; i < 20; i++) {
    const s = el('div', 'seg');
    row.insertBefore(s, $('thresh'));
    segs.push(s);
  }
})();

function drawLevel(db, open) {
  const pct = Math.max(0, Math.min(100, db + 100));
  const lit = Math.round(pct / 5);
  segs.forEach((s, i) => { s.className = i < lit ? 'seg ' + (i < 13 ? 'l1' : i < 17 ? 'l2' : 'l3') : 'seg'; });
  $('meter-val').textContent = db <= -100 ? '-- dBFS' : db.toFixed(1) + ' dBFS';
  const running = state && state.running && state.channel;
  const tag = $('sql-tag');
  tag.textContent = running ? (open ? 'SQL OPEN' : 'SQL CLOSED') : 'SQL --';
  tag.classList.toggle('open', !!(running && open));
  $('dot').className = 'dot' + (!running ? '' : open ? ' open' : ' scan');
}

// ---------- render ----------
function render(s) {
  state = s;
  const ch = s.channel;
  $('badge-live').classList.toggle('hidden', !s.running);

  let label = 'IDLE';
  if (s.running) label = !ch ? 'NO CHANNELS' : s.held ? 'HOLDING' : 'SCANNING';
  $('state-label').textContent = label;
  $('progress').textContent = s.running && ch && s.total ? `CH ${s.pos} / ${s.total}` : '';
  if (!s.running) $('dot').className = s.error ? 'dot err' : 'dot';

  $('freq-big').textContent = ch ? mhz(ch.freq) : '---.----';
  $('freq-name').textContent = ch ? ch.label : (s.running ? 'NOTHING TO SCAN' : 'NOT SCANNING');
  $('mode-tag').textContent = 'MODE ' + (ch ? ch.mode.toUpperCase() : '--');
  $('hold-tag').classList.toggle('hidden', !(s.running && s.held));

  const btnScan = $('btn-scan');
  btnScan.textContent = s.running ? 'Stop scan' : 'Start scan';
  btnScan.className = 'btn ' + (s.running ? 'danger' : 'primary');
  $('btn-hold').disabled = !s.running;
  $('btn-hold').textContent = s.held ? 'Resume' : 'Hold';
  $('btn-hold').classList.toggle('on', !!s.held);
  $('btn-skip').disabled = !s.running;

  const errBox = $('error');
  errBox.textContent = s.error || '';
  errBox.classList.toggle('hidden', !s.error);
  if (s.error && s.error !== lastError && isAdmin) toast(s.error, true);
  lastError = s.error || '';

  const thresh = $('thresh');
  thresh.classList.toggle('hidden', s.squelch_mode !== 'audio');
  thresh.style.left = Math.max(0, Math.min(100, s.squelch_db + 100)) + '%';

  $('sys-ver').textContent = s.version;
  $('sys-listen').textContent = s.listeners;
  $('sys-sq').textContent = { audio: 'Audio level', diff: 'Level swing', rf: 'RF carrier' }[s.squelch_mode] || s.squelch_mode;

  if (!s.running) drawLevel(-100, false);
  renderChannels(s.channels, ch && s.running ? ch.id : null);
}

function renderChannels(list, activeId) {
  const ul = $('ch-list');
  ul.replaceChildren();
  const locked = list.filter(c => c.lockout).length;
  $('ch-count').textContent = list.length ? `(${list.length}${locked ? `, ${locked} locked out` : ''})` : '';
  if (!list.length) {
    ul.append(el('li', 'empty', 'NO CHANNELS YET'));
    return;
  }
  list.forEach((c, i) => {
    const li = el('li', 'ch-item');
    li.dataset.id = c.id;
    if (c.id === activeId) li.classList.add('active');
    if (c.lockout) li.classList.add('locked');
    if (isAdmin) { li.classList.add('admin'); li.title = 'Click to tune and hold'; }

    const info = el('div', 'ch-info');
    info.append(el('div', 'ch-mhz', mhz(c.freq)), el('div', 'ch-lbl', c.label));
    li.append(info, el('span', 'tag', c.mode.toUpperCase()));

    if (isAdmin) {
      const b = el('div', 'ch-btns');
      const btn = (text, action, cls = '') => {
        const x = el('button', 'btn sm ' + cls, text);
        x.type = 'button';
        x.dataset.action = action;
        return x;
      };
      const up = btn('▲', 'up'); up.disabled = i === 0; up.title = 'Move up';
      const dn = btn('▼', 'down'); dn.disabled = i === list.length - 1; dn.title = 'Move down';
      b.append(up, dn, btn('Edit', 'edit'), btn(c.lockout ? 'Unlock' : 'Lock', 'lock', c.lockout ? 'on' : ''), btn('Del', 'del', 'danger'));
      li.append(b);
    }
    ul.append(li);
  });
}

// ---------- live updates ----------
let events;
function connect() {
  events = new EventSource('/api/events');
  events.addEventListener('state', e => { $('conn-banner').classList.add('hidden'); render(JSON.parse(e.data)); });
  events.addEventListener('level', e => { const d = JSON.parse(e.data); drawLevel(d.db, d.open); });
  events.onerror = () => {
    $('conn-banner').classList.remove('hidden');
    // EventSource retries by itself unless the server answered with an
    // error, in which case it gives up. Start over in that case.
    if (events.readyState === EventSource.CLOSED) setTimeout(connect, 3000);
  };
}
connect();

// ---------- audio ----------
const audio = new Audio();
audio.preload = 'none';
let wantAudio = false;
let retryTimer;

audio.volume = parseFloat(store.get('volume', '0.8')) || 0.8;
$('vol').value = audio.volume;
$('vol-pct').textContent = Math.round(audio.volume * 100) + '%';
$('vol').addEventListener('input', e => {
  audio.volume = parseFloat(e.target.value);
  $('vol-pct').textContent = Math.round(audio.volume * 100) + '%';
  store.set('volume', e.target.value);
});

function startAudio() {
  clearTimeout(retryTimer);
  // A fresh URL every time, so the browser never replays a stale buffer and
  // always joins at live.
  audio.src = '/stream.mp3?t=' + Date.now();
  audio.play().catch(err => {
    if (err.name === 'NotAllowedError') { stopAudio(); toast('The browser blocked playback, press Listen again', true); }
  });
}
function stopAudio() {
  wantAudio = false;
  clearTimeout(retryTimer);
  audio.pause();
  audio.removeAttribute('src');
  audio.load();
  setAudioBtn();
}
function setAudioBtn() {
  const b = $('btn-play');
  b.textContent = wantAudio ? 'Stop listening' : 'Listen';
  b.className = 'btn big ' + (wantAudio ? 'danger' : 'amber');
}
function audioDropped() {
  if (!wantAudio) return;
  $('audio-status').textContent = 'Stream dropped, reconnecting...';
  clearTimeout(retryTimer);
  retryTimer = setTimeout(startAudio, 2000);
}
audio.addEventListener('playing', () => { $('audio-status').textContent = 'Playing live.'; });
audio.addEventListener('error', audioDropped);
audio.addEventListener('ended', audioDropped);
$('btn-play').addEventListener('click', () => {
  if (wantAudio) { stopAudio(); $('audio-status').textContent = 'Stopped.'; return; }
  wantAudio = true;
  setAudioBtn();
  $('audio-status').textContent = 'Connecting...';
  startAudio();
});

// ---------- auth ----------
function setAdmin(on) {
  isAdmin = on;
  document.querySelectorAll('.admin-only').forEach(e => e.classList.toggle('hidden', !on));
  $('badge-admin').classList.toggle('hidden', !on);
  $('btn-login').classList.toggle('hidden', on);
  $('btn-logout').classList.toggle('hidden', !on);
  if (!on) closeForm();
  if (state) render(state);
}

async function checkMe() {
  try {
    const me = await api('GET', '/api/me');
    setAdmin(me.admin);
    if (me.admin) {
      if (me.must_change) $('pw-dlg').showModal();
      else await loadSettings();
    }
  } catch {}
}

$('btn-login').addEventListener('click', () => {
  $('login-err').textContent = '';
  $('l-pass').value = '';
  $('login-dlg').showModal();
  $('l-pass').focus();
});
$('login-cancel').addEventListener('click', () => $('login-dlg').close());
$('login-form').addEventListener('submit', async e => {
  e.preventDefault();
  $('login-err').textContent = '';
  try {
    const r = await api('POST', '/api/login', { username: $('l-user').value.trim(), password: $('l-pass').value });
    $('login-dlg').close();
    setAdmin(true);
    if (r.must_change) { $('pw-dlg').showModal(); $('pw-first').focus(); }
    else { await loadSettings(); toast('Logged in'); }
  } catch (err) {
    $('login-err').textContent = err.message;
  }
});

async function logout() {
  await api('POST', '/api/logout').catch(() => {});
  setAdmin(false);
  toast('Logged out');
}
$('btn-logout').addEventListener('click', logout);

// The forced password dialog cannot be dismissed, only completed or logged out of.
$('pw-dlg').addEventListener('cancel', e => e.preventDefault());
$('pw-logout').addEventListener('click', async () => { $('pw-dlg').close(); await logout(); });
$('pw-dlg-form').addEventListener('submit', async e => {
  e.preventDefault();
  const a = $('pw-first').value, b = $('pw-again').value;
  $('pw-err').textContent = '';
  if (a.length < 8) { $('pw-err').textContent = 'At least 8 characters'; return; }
  if (a !== b) { $('pw-err').textContent = 'The two do not match'; return; }
  try {
    await api('POST', '/api/password', { password: a });
    $('pw-dlg').close();
    $('pw-first').value = $('pw-again').value = '';
    await loadSettings();
    toast('Password set');
  } catch (err) { $('pw-err').textContent = err.message; }
});
$('pw-form').addEventListener('submit', e => {
  e.preventDefault();
  const pw = $('pw-new').value;
  if (pw.length < 8) { toast('At least 8 characters', true); return; }
  act(async () => { await api('POST', '/api/password', { password: pw }); $('pw-new').value = ''; }, 'Password changed');
});

// ---------- scanner controls ----------
$('btn-scan').addEventListener('click', () => {
  const running = state && state.running;
  act(() => api('POST', '/api/scanner/' + (running ? 'stop' : 'start')), running ? 'Scanner stopped' : 'Scanner started');
});
$('btn-hold').addEventListener('click', () => act(() => api('POST', '/api/scanner/hold')));
$('btn-skip').addEventListener('click', () => act(() => api('POST', '/api/scanner/skip')));

// ---------- channel form ----------
function openForm(c) {
  editingId = c ? c.id : null;
  $('ch-freq').value = c ? mhz(c.freq) : '';
  $('ch-label').value = c ? c.label : '';
  $('ch-mode').value = c ? c.mode : 'fm';
  $('ch-lockout').checked = c ? c.lockout : false;
  $('ch-save').textContent = c ? 'Save' : 'Add';
  $('ch-form').classList.remove('hidden');
  $('ch-freq').focus();
}
function closeForm() { editingId = null; $('ch-form').classList.add('hidden'); }

$('btn-add').addEventListener('click', () => {
  if (!$('ch-form').classList.contains('hidden') && !editingId) closeForm(); else openForm(null);
});
$('ch-cancel').addEventListener('click', closeForm);
$('ch-form').addEventListener('submit', e => {
  e.preventDefault();
  const freq = parseFreq($('ch-freq').value);
  if (!Number.isFinite(freq)) { toast('Enter the frequency in MHz, like 162.400', true); return; }
  const body = { freq, label: $('ch-label').value, mode: $('ch-mode').value, lockout: $('ch-lockout').checked };
  const id = editingId;
  act(async () => {
    if (id) await api('PUT', '/api/channels/' + encodeURIComponent(id), body);
    else await api('POST', '/api/channels', body);
    if (id) closeForm();
    else { $('ch-freq').value = ''; $('ch-label').value = ''; $('ch-freq').focus(); }
  }, id ? 'Channel saved' : 'Channel added');
});

// One listener for every row button.
const pendingDelete = new Map();
$('ch-list').addEventListener('click', e => {
  if (!isAdmin) return;
  const li = e.target.closest('.ch-item');
  if (!li) return;
  const id = li.dataset.id;
  const c = state.channels.find(x => x.id === id);
  if (!c) return;
  const btn = e.target.closest('button');
  const path = '/api/channels/' + encodeURIComponent(id);
  if (!btn) { act(() => api('POST', '/api/scanner/tune', { id }), 'Holding on ' + c.label); return; }
  switch (btn.dataset.action) {
    case 'up': act(() => api('POST', path + '/move', { dir: -1 })); break;
    case 'down': act(() => api('POST', path + '/move', { dir: 1 })); break;
    case 'edit': openForm(c); break;
    case 'lock': act(() => api('PUT', path, { ...c, lockout: !c.lockout })); break;
    case 'del':
      // Second click within three seconds deletes.
      if (pendingDelete.has(id)) {
        clearTimeout(pendingDelete.get(id));
        pendingDelete.delete(id);
        if (editingId === id) closeForm();
        act(() => api('DELETE', path), 'Channel removed');
      } else {
        btn.textContent = 'Sure?';
        pendingDelete.set(id, setTimeout(() => { pendingDelete.delete(id); btn.textContent = 'Del'; }, 3000));
      }
      break;
  }
});

// ---------- settings ----------
function showSquelchFields() {
  const m = $('s-mode').value;
  document.querySelectorAll('[data-sq]').forEach(e => e.classList.toggle('hidden', e.dataset.sq !== m));
}
$('s-mode').addEventListener('change', showSquelchFields);

async function loadSettings() {
  try {
    const s = await api('GET', '/api/settings');
    $('s-mode').value = s.squelch_mode;
    $('s-db').value = s.squelch_db;
    $('s-diff').value = s.diff_db;
    $('s-rf').value = s.rf_squelch;
    $('s-dwell').value = s.dwell;
    $('s-mute').checked = s.mute_closed;
    $('s-rate').value = String(s.sample_rate);
    $('s-gain').value = s.gain;
    $('s-ppm').value = s.ppm;
    $('s-dev').value = s.device;
    showSquelchFields();
  } catch {}
}

$('set-form').addEventListener('submit', e => {
  e.preventDefault();
  const num = id => parseFloat($(id).value);
  const body = {
    squelch_mode: $('s-mode').value,
    squelch_db: num('s-db'),
    diff_db: num('s-diff'),
    rf_squelch: parseInt($('s-rf').value, 10) || 0,
    dwell: num('s-dwell'),
    mute_closed: $('s-mute').checked,
    sample_rate: parseInt($('s-rate').value, 10),
    gain: $('s-gain').value.trim() || 'auto',
    ppm: parseInt($('s-ppm').value, 10) || 0,
    device: $('s-dev').value.trim(),
  };
  for (const k of ['squelch_db', 'diff_db', 'dwell']) {
    if (!Number.isFinite(body[k])) { toast('Fill in every number', true); return; }
  }
  act(async () => { await api('PUT', '/api/settings', body); await loadSettings(); }, 'Settings applied');
});

checkMe();
