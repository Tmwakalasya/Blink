// Blink's page: start a VM, watch it boot, then use its shell.
const $ = (id) => document.getElementById(id);
const STEPS = ['requested', 'creating', 'booting', 'ssh'];

const state = {
  status: null, // from /api/status
  view: 'idle',
  vm: null, // the VM in the shell
  ws: null,
  term: null,
  fit: null,
  clock: 0,
  countdown: 0,
  ending: false,
  expired: false,
  expiredName: '',
};

init();

async function init() {
  $('start').addEventListener('click', start);
  $('retry').addEventListener('click', start);
  $('back').addEventListener('click', () => show('idle'));
  $('recheck').addEventListener('click', recheck);
  $('s-copy').addEventListener('click', () => state.vm?.ssh && copy(state.vm.ssh));
  $('s-end').addEventListener('click', end);
  for (const btn of document.querySelectorAll('.command .copy')) {
    btn.addEventListener('click', () => copy(btn.previousElementSibling.textContent));
  }
  try {
    renderStatus(await api('GET', '/api/status'));
  } catch {
    return renderStatus({ ready: false, problem: 'Blink isn’t responding. Check that it’s still running.' });
  }
  if (state.status.ready) resume();
}

// ---------- before starting ----------

function renderStatus(st) {
  state.status = st;
  $('where').textContent = st.project ? `${st.project} / ${st.zone}` : 'No project';
  $('dot').classList.toggle('off', !st.ready);
  if (st.machine) {
    $('spec-machine').replaceChildren(st.machine, small(`${st.diskGb} GB disk`));
    $('spec-image').textContent = st.image;
    $('spec-zone').replaceChildren(st.zone, small(st.place));
    for (const el of document.querySelectorAll('[data-ttl]')) el.textContent = ttlText(st.ttlSeconds);
  }
  const link = $('console');
  link.hidden = !st.project;
  if (st.project) link.href = `https://console.cloud.google.com/compute/instances?project=${encodeURIComponent(st.project)}`;
  $('start').disabled = !st.ready;
  $('recheck').hidden = st.ready;
  const message = st.problem || st.warning;
  $('notice').hidden = !message;
  if (message) {
    $('notice-label').textContent = st.problem ? 'Setup needed' : 'Warning';
    $('notice-text').textContent = message;
    setFix($('fix'), $('fix-text'), st.fix);
  }
}

async function recheck() {
  const btn = $('recheck');
  btn.disabled = true;
  btn.textContent = 'Checking…';
  try {
    renderStatus(await api('POST', '/api/status'));
  } catch {}
  btn.disabled = false;
  btn.textContent = 'Check again';
  if (state.status.ready) resume();
}

// resume reconnects to a VM that's already up, e.g. after a reload.
async function resume() {
  let vms = await listVMs();
  const ready = vms.find((v) => v.ready);
  if (ready) return openShell(ready, null);
  const starting = vms.find((v) => v.hasKey && ['PROVISIONING', 'STAGING', 'RUNNING'].includes(v.status));
  if (!starting) return;

  // The page was reloaded while the server was still starting this VM.
  showProgress();
  $('p-name').textContent = starting.name;
  for (let i = 0; i < 120; i++) {
    await wait(2000);
    vms = await listVMs();
    const vm = vms.find((v) => v.name === starting.name);
    if (!vm) return fail(`${starting.name} failed to start and was deleted.`);
    if (vm.ready) {
      stopClock();
      return openShell(vm, null);
    }
  }
  fail(`${starting.name} is taking too long to start.`);
}

async function listVMs() {
  try {
    return (await api('GET', '/api/vms')).vms || [];
  } catch {
    return [];
  }
}

// ---------- starting a VM ----------

async function start() {
  showProgress();
  let res;
  try {
    res = await fetch('/api/vms', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}' });
  } catch {
    return fail('Can’t reach Blink. Check that it’s still running.');
  }
  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    if (body.vm?.ready && body.vm.name !== state.expiredName) {
      stopClock();
      return openShell(body.vm, null);
    }
    return fail(body.error || `Blink returned ${res.status}.`, body.fix);
  }

  // Keep reading after the last event until the server ends the stream.
  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let buffered = '';
  let finished = false;
  for (;;) {
    let chunk;
    try {
      chunk = await reader.read();
    } catch {
      if (!finished) fail('Lost the connection to Blink while starting the VM.');
      return;
    }
    if (chunk.done) break;
    buffered += decoder.decode(chunk.value, { stream: true });
    let newline;
    while ((newline = buffered.indexOf('\n')) >= 0) {
      const line = buffered.slice(0, newline).trim();
      buffered = buffered.slice(newline + 1);
      if (line && !finished) finished = onEvent(JSON.parse(line));
    }
  }
  if (!finished) fail('The server stopped before the VM was ready.');
}

// onEvent applies one step of the stream and reports whether it was the last.
function onEvent(ev) {
  if (ev.error) {
    fail(ev.error, ev.fix, ev.step);
    return true;
  }
  if (ev.vm) {
    stopClock(ev.at);
    setTimeout(() => openShell(ev.vm, ev.at), 400);
    return true;
  }
  const li = stepEl(ev.step);
  li.className = 'done';
  li.querySelector('time').textContent = `${ev.at.toFixed(1)} s`;
  if (ev.note) li.querySelector('.step-note').textContent = ev.note;
  if (ev.step === 'requested' && ev.note) $('p-name').textContent = ev.note;
  const next = STEPS[STEPS.indexOf(ev.step) + 1];
  if (next) stepEl(next).className = 'active';
  return false;
}

function showProgress() {
  for (const li of document.querySelectorAll('#steps li')) {
    li.className = '';
    li.querySelector('time').textContent = '';
    li.querySelector('.step-note').textContent = '';
  }
  stepEl('requested').className = 'active';
  const st = state.status;
  $('p-name').textContent = 'New VM';
  $('p-machine').textContent = st.machine;
  $('p-image').textContent = st.image;
  $('p-ttl').textContent = ttlText(st.ttlSeconds);
  $('failure').hidden = true;
  show('progress');
  startClock();
}

function fail(message, fix, step) {
  stopClock();
  const li = step ? stepEl(step) : document.querySelector('#steps .active');
  for (const active of document.querySelectorAll('#steps .active')) active.className = '';
  if (li) li.className = 'failed';
  $('failure-text').textContent = message;
  setFix($('failure-fix'), $('failure-fix-text'), fix);
  $('failure').hidden = false;
  show('progress');
}

function startClock() {
  const began = performance.now();
  cancelAnimationFrame(state.clock);
  const tick = () => {
    $('clock').textContent = ((performance.now() - began) / 1000).toFixed(1);
    state.clock = requestAnimationFrame(tick);
  };
  tick();
}

function stopClock(at) {
  cancelAnimationFrame(state.clock);
  if (at != null) $('clock').textContent = at.toFixed(1);
}

// ---------- the shell ----------

async function openShell(vm, readyIn) {
  state.vm = vm;
  state.ending = false;
  state.expired = false;
  $('s-name').textContent = vm.name;
  $('s-ip').textContent = vm.ip || '—';
  $('s-ready').textContent = readyIn != null ? `${readyIn.toFixed(1)} s` : '—';
  $('s-copy').hidden = !vm.ssh;
  $('s-end').hidden = false;
  resetEnd();
  $('curtain').hidden = true;
  show('shell');
  await ensureTerminal();
  state.term.reset();
  startCountdown(vm.expiresAt);
  connect();
}

async function ensureTerminal() {
  if (state.term) {
    state.fit.fit();
    return;
  }
  // xterm measures the font once, so it has to be loaded first.
  try {
    await document.fonts.load('400 14px "Geist Mono"');
  } catch {}
  const term = new Terminal({
    cursorBlink: true,
    fontFamily: '"Geist Mono", ui-monospace, Menlo, monospace',
    fontSize: 14,
    lineHeight: 1.2,
    scrollback: 5000,
    theme: {
      background: '#131715', foreground: '#ecebe4', cursor: '#bc4228', cursorAccent: '#131715',
      selectionBackground: 'rgba(188, 66, 40, 0.35)',
      black: '#2a2f2b', red: '#d9674d', green: '#9dbb8a', yellow: '#dcc07a',
      blue: '#8aa7c4', magenta: '#bf9bbd', cyan: '#8fbdb4', white: '#dcdccd',
      brightBlack: '#646961', brightRed: '#e8866f', brightGreen: '#b6d1a3', brightYellow: '#ead59a',
      brightBlue: '#a9c1da', brightMagenta: '#d4b7d2', brightCyan: '#abd3cb', brightWhite: '#f7f7f2',
    },
  });
  const fit = new FitAddon.FitAddon();
  term.loadAddon(fit);
  term.open($('term'));
  fit.fit();
  new ResizeObserver(() => {
    try { fit.fit(); } catch {}
  }).observe($('term'));
  term.onData((data) => send({ type: 'stdin', data }));
  term.onResize(({ cols, rows }) => send({ type: 'resize', cols, rows }));
  state.term = term;
  state.fit = fit;
}

function connect() {
  const { vm, term } = state;
  state.ws?.close();
  const size = new URLSearchParams({ cols: term.cols, rows: term.rows });
  const scheme = location.protocol === 'https:' ? 'wss' : 'ws';
  const ws = new WebSocket(`${scheme}://${location.host}/api/vms/${vm.zone}/${vm.name}/terminal?${size}`);
  ws.binaryType = 'arraybuffer';
  ws.onopen = () => {
    $('curtain').hidden = true;
    term.focus();
  };
  ws.onmessage = (e) => term.write(typeof e.data === 'string' ? e.data : new Uint8Array(e.data));
  ws.onclose = (e) => {
    if (state.ws !== ws || state.ending || state.expired) return;
    curtain(e.code === 1000 ? 'Session closed.' : e.reason || 'Connection lost.', 'Reconnect', connect);
  };
  state.ws = ws;
}

function send(msg) {
  if (state.ws?.readyState === WebSocket.OPEN) state.ws.send(JSON.stringify(msg));
}

function startCountdown(expiresAt) {
  clearInterval(state.countdown);
  const end = expiresAt ? Date.parse(expiresAt) : Date.now() + state.status.ttlSeconds * 1000;
  const tick = () => {
    const left = Math.ceil((end - Date.now()) / 1000);
    const el = $('s-left');
    if (left <= 0) {
      el.textContent = '0:00';
      clearInterval(state.countdown);
      return expire();
    }
    el.textContent = `${Math.floor(left / 60)}:${String(left % 60).padStart(2, '0')}`;
    el.classList.toggle('soon', left <= 60);
  };
  tick();
  state.countdown = setInterval(tick, 1000);
}

function expire() {
  state.expired = true;
  state.expiredName = state.vm.name;
  state.ws?.close();
  $('s-copy').hidden = true;
  $('s-end').hidden = true;
  curtain(`${state.vm.name} was deleted after ${ttlText(state.status.ttlSeconds)}.`, 'Start new VM', () => {
    show('idle');
    start();
  });
}

let confirmTimer = 0;
async function end() {
  const btn = $('s-end');
  if (!btn.classList.contains('confirm')) {
    btn.classList.add('confirm');
    btn.textContent = 'Confirm delete';
    confirmTimer = setTimeout(resetEnd, 3000);
    return;
  }
  resetEnd();
  const { vm } = state;
  state.ending = true;
  state.ws?.close();
  btn.disabled = true;
  try {
    await api('DELETE', `/api/vms/${vm.zone}/${vm.name}`);
  } catch (e) {
    btn.disabled = false;
    state.ending = false;
    toast(`Couldn’t delete ${vm.name}: ${e.message}`);
    return connect();
  }
  btn.disabled = false;
  show('idle');
  toast(`Deleted ${vm.name}`);
}

function resetEnd() {
  clearTimeout(confirmTimer);
  const btn = $('s-end');
  btn.classList.remove('confirm');
  btn.textContent = 'Delete VM';
}

function curtain(text, label, action) {
  $('curtain-text').textContent = text;
  const btn = $('curtain-btn');
  btn.textContent = label;
  btn.onclick = action;
  $('curtain').hidden = false;
}

// ---------- helpers ----------

function show(view) {
  $('home').hidden = view === 'shell';
  $('home').classList.toggle('compact', view === 'progress');
  $('idle').hidden = view !== 'idle';
  $('progress').hidden = view !== 'progress';
  $('shell').hidden = view !== 'shell';
  document.body.classList.toggle('in-shell', view === 'shell');
  if (view !== 'shell') {
    clearInterval(state.countdown);
    state.vm = null;
  }
  if (view !== state.view) {
    const el = view === 'shell' ? $('shell') : state.view === 'shell' ? $('home') : $(view);
    el.classList.remove('enter');
    void el.offsetWidth; // restart the animation
    el.classList.add('enter');
    state.view = view;
  }
}

function stepEl(step) {
  return document.querySelector(`#steps li[data-step="${step}"]`);
}

function small(text) {
  const el = document.createElement('small');
  el.textContent = text;
  return el;
}

function setFix(box, code, fix) {
  box.hidden = !fix;
  if (!fix) return;
  code.replaceChildren();
  if (/^https?:\/\//.test(fix)) {
    const a = document.createElement('a');
    a.href = fix;
    a.target = '_blank';
    a.rel = 'noreferrer';
    a.textContent = fix;
    code.append(a);
  } else {
    code.textContent = fix;
  }
}

function ttlText(seconds) {
  return seconds % 3600 === 0 ? `${seconds / 3600} hr` : `${Math.round(seconds / 60)} min`;
}

async function api(method, path) {
  const res = await fetch(path, {
    method,
    headers: method === 'GET' ? {} : { 'Content-Type': 'application/json' },
  });
  const text = await res.text();
  let body = null;
  try {
    body = text ? JSON.parse(text) : null;
  } catch {}
  if (!res.ok) throw new Error(body?.error || `HTTP ${res.status}`);
  return body;
}

async function copy(text) {
  try {
    await navigator.clipboard.writeText(text);
    toast('Copied');
  } catch {
    toast('The browser blocked the clipboard.');
  }
}

let toastTimer = 0;
function toast(text) {
  const el = $('toast');
  el.textContent = text;
  el.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => (el.hidden = true), 2600);
}

function wait(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
