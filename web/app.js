// Blink's page: sign in, pick a size, watch the VM boot, then work in it.
const $ = (id) => document.getElementById(id);
const STEPS = ['requested', 'creating', 'booting', 'ssh'];
// What each step says in the boot log, and what the status pill says after it.
const STEP_LOG = {
  requested: { key: 'request', after: 'Allocating your workspace' },
  creating: { key: 'provision', after: 'Starting Linux' },
  booting: { key: 'boot', after: 'Connecting securely' },
  ssh: { key: 'ssh', after: 'Ready' },
};

const WORKSPACES = {
  small: { name: 'Terminal', description: 'Quick commands and Linux experiments.' },
  medium: { name: 'Development', description: 'Your everyday coding workspace.' },
  large: { name: 'Compute', description: 'More room for builds and heavier tasks.' },
};

function workspaceName(size) { return WORKSPACES[size?.id]?.name || size?.label || 'Workspace'; }

const state = {
  status: null, // from /api/status
  view: '',
  size: '', // chosen size id
  ttl: 0, // chosen lifetime, seconds
  vm: null, // the VM on screen
  ws: null,
  retries: 0,
  term: null,
  fit: null,
  clock: 0,
  countdown: 0,
  editorPoll: null,
  credential: '',
  ending: false,
  expired: false,
  expiredName: '',
  checkingVMs: false,
};

init();

async function init() {
  $('start').addEventListener('click', start);
  $('retry').addEventListener('click', start);
  $('back').addEventListener('click', backToIdle);
  $('recheck').addEventListener('click', recheck);
  $('signout').addEventListener('click', signOut);
  $('save-roster').addEventListener('click', saveRoster);
  $('request-btn').addEventListener('click', requestAccess);
  $('s-copy').addEventListener('click', () => state.vm?.ssh && copy(state.vm.ssh, 'SSH command copied'));
  $('s-ip').addEventListener('click', () => state.vm?.ip && copy(state.vm.ip, 'IP copied'));
  $('s-end').addEventListener('click', end);
  $('end-dialog').addEventListener('close', () => {
    if ($('end-dialog').returnValue === 'confirm') deleteWorkspace();
  });
  $('save-help').addEventListener('click', () => $('save-dialog').showModal());
  $('session-save-help').addEventListener('click', () => $('save-dialog').showModal());
  $('tab-editor').addEventListener('click', () => selectTab('editor'));
  $('tab-terminal').addEventListener('click', () => selectTab('terminal'));
  $('tabs').addEventListener('keydown', (e) => {
    if (!['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(e.key)) return;
    e.preventDefault();
    const tab = e.key === 'Home' ? 'editor' : e.key === 'End' ? 'terminal' :
      $('tab-editor').getAttribute('aria-selected') === 'true' ? 'terminal' : 'editor';
    selectTab(tab);
    $(`tab-${tab}`).focus();
  });
  for (const btn of document.querySelectorAll('.command .copy')) {
    btn.addEventListener('click', () => copy(btn.previousElementSibling.textContent, 'Copied'));
  }
  let st;
  try {
    st = await api('GET', '/api/status');
  } catch {
    show('idle');
    return renderNotice('Blink isn’t responding. Check that it’s still running.', 'Offline');
  }
  render(st);
  if (st.signIn && !st.user) return showSignIn(st);
  show('idle');
  if (st.user?.admin && st.signIn) loadClass();
  if (st.ready) await resume();
}

// ---------- the create page ----------

function render(st) {
  state.status = st;
  const u = st.user;
  $('who').hidden = $('signout').hidden = !u?.email;
  $('who').textContent = u?.email || '';
  const link = $('console');
  link.hidden = !(u?.admin && st.project);
  if (!link.hidden) link.href = `https://console.cloud.google.com/compute/instances?project=${encodeURIComponent(st.project)}`;
  $('where-line').hidden = !u || !st.project;
  $('where').textContent = st.project ? `${st.project} / ${st.zone}` : '';
  $('conn').hidden = !u;
  $('dot').classList.toggle('off', !st.ready);
  $('conn-text').textContent = st.ready ? 'connected' : 'not connected';
  if (!u) return;

  const sizes = st.sizes || [];
  if (!sizes.some((s) => s.id === state.size)) {
    const saved = remembered('size');
    state.size = sizes.some((s) => s.id === saved) ? saved : sizes.find((s) => s.id === 'medium')?.id || sizes[0]?.id;
  }
  $('sizes').replaceChildren(...sizes.map(sizeOption));
  const lifetimes = st.lifetimes || [];
  if (!lifetimes.includes(state.ttl)) {
    const saved = Number(remembered('ttl'));
    state.ttl = lifetimes.includes(saved) ? saved : lifetimes.includes(3600) ? 3600 : lifetimes[0];
  }
  $('lifetimes').replaceChildren(...lifetimes.map(lifetimeOption));

  renderMeters(st);
  renderNotice(st.problem || st.warning, st.problem ? 'Setup needed' : 'Warning', st.fix);
  $('recheck').hidden = st.ready || !u.admin;
  updateSummary();
}

// refresh re-reads the status, e.g. after a VM ends, to update the meters.
async function refresh() {
  try {
    render(await api('GET', '/api/status'));
  } catch (e) {
    renderNotice(`Couldn't refresh usage: ${e.message}. Check the connection before starting another workspace.`, 'Connection interrupted');
    $('start').disabled = true;
  }
}

function sizeOption(s) {
  const row = el('label', 'size-row');
  const heading = el('span', 'size-heading');
  heading.append(el('span', 'size-name', workspaceName(s)), el('span', 'radio'));
  const foot = el('span', 'size-foot');
  foot.append(el('span', 'size-price', `${money(s.hourly)}/hr`));
  if (s.id === 'medium') foot.append(el('span', 'recommended', 'Recommended'));
  row.append(
    choice('size', s.id, s.id === state.size, () => {
      state.size = s.id;
      remember('size', s.id);
      updateSummary();
    }),
    heading,
    el('span', 'size-description', WORKSPACES[s.id]?.description || 'A fresh Linux workspace.'),
    specLine(s),
    foot,
  );
  return row;
}

function specLine(s) {
  const spec = el('span', 'size-spec');
  spec.append(
    el('span', '', `${s.cpu.replace(' vCPUs', ' vCPU')} · ${s.memory}`),
    el('span', 'tag', s.editor ? 'VS Code + terminal' : 'terminal'),
  );
  return spec;
}

function lifetimeOption(seconds) {
  const seg = el('label', 'seg');
  seg.append(
    choice('ttl', seconds, seconds === state.ttl, () => {
      state.ttl = seconds;
      remember('ttl', seconds);
      updateSummary();
    }),
    ttlText(seconds),
  );
  return seg;
}

function choice(name, value, checked, onChange) {
  const input = document.createElement('input');
  input.type = 'radio';
  input.name = name;
  input.value = value;
  input.checked = checked;
  input.addEventListener('change', onChange);
  return input;
}

function updateSummary() {
  const st = state.status;
  const size = st.sizes?.find((s) => s.id === state.size);
  if (!size || !state.ttl) {
    $('start').disabled = true;
    return;
  }
  const hours = state.ttl / 3600;
  const cost = size.hourly * hours;
  $('sum-machine').textContent = `${size.machine} (${size.label.toLowerCase()})`;
  $('sum-image').textContent = st.image || '—';
  $('sum-zone').textContent = st.zone || '—';
  $('sum-ttl').textContent = ttlText(state.ttl);
  $('sum-cost').textContent = money(cost);
  $('sum-description').textContent = `${workspaceName(size)} · ${ttlText(state.ttl)} · ${size.editor ? 'VS Code + terminal' : 'Terminal'}`;

  let limit = '';
  if (st.budget && st.budget.used + cost > st.budget.limit) {
    limit = `That would go over the ${usd(st.budget.limit)} budget. Try a smaller size or a shorter lifetime.`;
  } else if (st.week && st.week.used + hours > st.week.limit) {
    limit = `That would go over your ${hoursText(st.week.limit)} this week. Try a shorter lifetime.`;
  } else if (st.maxVMs && st.running >= st.maxVMs) {
    limit = `All ${st.maxVMs} VMs are in use right now. Try again when one frees up.`;
  }
  $('limit-note').hidden = !limit;
  $('limit-note').textContent = limit;
  $('start').disabled = !st.ready || Boolean(limit) || state.checkingVMs;
}

function renderMeters(st) {
  $('meters').hidden = !(st.budget || st.week || st.signIn);
  meter('budget', st.budget, (m) => `${usd(m.used)} / ${usd(m.limit)}`);
  meter('week', st.week, (m) => `${hoursText(m.used)} / ${hoursText(m.limit)}`);
  meter('vms', st.signIn ? { used: st.running, limit: st.maxVMs } : null, (m) => `${m.used} / ${m.limit}`);
}

function meter(id, m, text) {
  $(`${id}-meter`).hidden = !m;
  if (!m) return;
  $(`${id}-fill`).style.width = `${Math.min(100, (m.used / m.limit) * 100)}%`;
  $(`${id}-text`).textContent = text(m);
}

function renderNotice(message, label = 'Setup needed', fix = '') {
  $('notice').hidden = !message;
  if (!message) return;
  $('notice-label').textContent = label;
  $('notice-text').textContent = message;
  setFix($('fix'), $('fix-text'), fix);
}

async function recheck() {
  const btn = $('recheck');
  btn.disabled = true;
  btn.textContent = 'Checking…';
  try {
    render(await api('POST', '/api/status'));
  } catch (e) {
    renderNotice(e.message, 'Could not check connection');
  }
  btn.disabled = false;
  btn.textContent = 'Check again';
  if (state.status.ready) resume();
}

// resume reconnects to the user's VM if it's already up, e.g. after a reload.
async function resume() {
  state.checkingVMs = true;
  updateSummary();
  try {
    let vms = await listVMs();
    const ready = vms.find((v) => v.ready);
    if (ready) return openShell(ready, null);
    const starting = vms.find((v) => v.hasKey && ['PROVISIONING', 'STAGING', 'RUNNING'].includes(v.status));
    if (!starting) return;

    // The server also resumes unfinished SSH verification after a restart.
    showProgress(starting.size);
    setTitle(starting.name);
    log(0, 'resume', `${starting.name} is still starting`);
    for (let i = 0; i < 120; i++) {
      await wait(2000);
      vms = await listVMs();
      const vm = vms.find((v) => v.name === starting.name);
      if (!vm) return fail(`${starting.name} is no longer available.`);
      if (vm.ready) {
        stopClock();
        return openShell(vm, null);
      }
    }
    fail(`${starting.name} is taking too long to start.`);
  } catch (e) {
    if (state.view === 'progress') fail(`Couldn't check your workspace: ${e.message}. Reload to reconnect; your VM may still be running.`);
    else renderNotice(`Couldn't check existing workspaces: ${e.message}. Reload to reconnect.`, 'Connection interrupted');
  } finally {
    state.checkingVMs = false;
    updateSummary();
  }
}

async function listVMs() {
  const vms = (await api('GET', '/api/vms')).vms || [];
  // Admins can list everyone's VMs, but automatic resume is personal.
  return state.status?.signIn && state.status.user?.admin
    ? vms.filter((vm) => vm.email === state.status.user.email) : vms;
}

// ---------- signing in ----------

function showSignIn(st) {
  show('signin');
  loadScript('https://accounts.google.com/gsi/client')
    .then(() => {
      google.accounts.id.initialize({ client_id: st.signIn.clientId, callback: onCredential, ux_mode: 'popup' });
      google.accounts.id.renderButton($('gsi'), { theme: 'outline', size: 'large', shape: 'rectangular', text: 'signin_with' });
    })
    .catch(() => signInError('Couldn’t load Google sign-in. Check your connection and reload.'));
}

async function onCredential({ credential }) {
  state.credential = credential;
  $('signin-error').hidden = $('request').hidden = true;
  try {
    await api('POST', '/api/login', { credential });
    location.reload();
  } catch (e) {
    if (!e.data?.canRequest) return signInError(e.message);
    $('request-text').textContent = `${e.message} Ask to be let in, then sign in again once you are.`;
    $('request-btn').hidden = false;
    $('request').hidden = false;
  }
}

async function requestAccess() {
  const btn = $('request-btn');
  btn.disabled = true;
  try {
    const res = await api('POST', '/api/access', { credential: state.credential });
    if (res?.allowed) return location.reload();
    $('request-text').textContent = 'Request sent. Sign in again once you’ve been let in.';
    btn.hidden = true;
  } catch (e) {
    $('request-text').textContent = e.message;
  }
  btn.disabled = false;
}

function signInError(message) {
  $('signin-error').textContent = message;
  $('signin-error').hidden = false;
}

async function signOut() {
  try {
    await api('POST', '/api/logout');
  } catch {}
  if (typeof google !== 'undefined') google.accounts.id.disableAutoSelect();
  location.reload();
}

// ---------- admins ----------

async function loadClass() {
  $('admin').hidden = false;
  try {
    const data = await api('GET', '/api/class');
    $('roster').value = data.roster || '';
    renderRequests(data.requests || []);
    renderUsage(data.week || []);
  } catch {}
}

async function saveRoster() {
  const btn = $('save-roster');
  btn.disabled = true;
  try {
    const { count } = await api('PUT', '/api/class', { roster: $('roster').value });
    $('roster-status').textContent = `Saved · ${count} ${count === 1 ? 'entry' : 'entries'}`;
  } catch (e) {
    $('roster-status').textContent = e.message;
  }
  btn.disabled = false;
}

function renderRequests(requests) {
  $('requests').hidden = !requests.length;
  $('request-list').replaceChildren(
    ...requests.map((r) => {
      const approve = el('button', 'btn btn-primary btn-xs', 'Let in');
      const dismiss = el('button', 'btn btn-secondary btn-xs', 'Dismiss');
      approve.onclick = () => answer(r.email, true);
      dismiss.onclick = () => answer(r.email, false);
      const actions = el('span', 'request-actions');
      actions.append(approve, dismiss);
      const li = el('li');
      li.append(el('span', 'request-email', r.email), el('span', 'request-when', ago(r.at)), actions);
      return li;
    }),
  );
}

async function answer(email, approve) {
  try {
    await api('POST', '/api/class/answer', { email, approve });
    toast(approve ? `${email} can sign in now` : `Dismissed ${email}`);
  } catch (e) {
    toast(e.message);
  }
  loadClass();
}

function renderUsage(week) {
  const rows = week.map((p) => row([p.email, p.vms, hoursText(p.hours), money(p.cost)]));
  if (!rows.length) {
    const td = el('td', 'empty', 'No VMs in the last 7 days.');
    td.colSpan = 4;
    const tr = el('tr');
    tr.append(td);
    rows.push(tr);
  }
  $('usage').querySelector('tbody').replaceChildren(...rows);
}

function row(cells) {
  const tr = el('tr');
  for (const c of cells) tr.append(el('td', '', String(c)));
  return tr;
}

function ago(when) {
  const minutes = Math.round((Date.now() - Date.parse(when)) / 60000);
  if (minutes < 60) return `${minutes} min ago`;
  if (minutes < 48 * 60) return `${Math.round(minutes / 60)} h ago`;
  return `${Math.round(minutes / 1440)} days ago`;
}

// ---------- creating a VM ----------

async function start() {
  if (!state.size || !state.ttl) return;
  showProgress(state.size);
  const size = state.status.sizes?.find((s) => s.id === state.size);
  log(0, 'create', `${size?.machine} · ${ttlText(state.ttl)} · ${state.status.zone}`);
  let res;
  try {
    res = await fetch('/api/vms', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ size: state.size, ttlSeconds: state.ttl }),
    });
  } catch {
    return fail('Can’t reach Blink. Check that it’s still running.');
  }
  if (res.status === 401) return location.reload();
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
      if (!finished) fail('Lost the connection to Blink while creating the VM.');
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
    fail(ev.error, ev.fix, ev.step, ev.at);
    return true;
  }
  if (ev.vm) {
    stopClock(ev.at);
    log(ev.at, 'ready', `${ev.vm.name} · ${ev.vm.ip}`, 'ok');
    setStatus('Ready', 'done');
    setTimeout(() => openShell(ev.vm, ev.at), 500);
    return true;
  }
  const li = stepEl(ev.step);
  li.className = 'done';
  li.querySelector('time').textContent = `${ev.at.toFixed(1)}s`;
  const info = STEP_LOG[ev.step];
  log(ev.at, info.key, (ev.note || 'done').toLowerCase());
  if (ev.step === 'requested' && ev.note) setTitle(ev.note);
  setStatus(info.after);
  const next = STEPS[STEPS.indexOf(ev.step) + 1];
  if (next) stepEl(next).className = 'active';
  return false;
}

function showProgress(sizeId) {
  for (const li of document.querySelectorAll('#steps li')) {
    li.className = '';
    li.querySelector('time').textContent = '';
  }
  stepEl('requested').className = 'active';
  $('log').replaceChildren();
  const size = state.status.sizes?.find((s) => s.id === sizeId);
  $('p-name').textContent = 'new workspace';
  $('p-title').textContent = 'Preparing your workspace.';
  $('p-description').textContent = "We'll open it here as soon as it's ready.";
  $('progress-details').open = false;
  $('p-size').textContent = workspaceName(size);
  $('p-machine').textContent = size?.machine || '—';
  $('p-ttl').textContent = ttlText(state.ttl);
  setStatus('Requesting workspace');
  $('failure').hidden = true;
  show('progress');
  startClock();
}

function setTitle(name) {
  $('p-name').textContent = name;
}

function setStatus(text, kind = '') {
  $('p-status-text').textContent = text;
  $('p-status').className = `pill ${kind}`;
}

function log(at, key, value, kind = '') {
  const line = el('div', `log-line ${kind}`);
  line.append(el('span', 't', clockText(at)), el('span', 'k', key), el('span', 'v', value));
  $('log').append(line);
}

function fail(message, fix, step, at) {
  const elapsed = stopClock();
  const li = step ? stepEl(step) : document.querySelector('#steps .active');
  for (const active of document.querySelectorAll('#steps .active')) active.className = '';
  if (li) li.className = 'failed';
  log(at ?? elapsed, 'error', message, 'err');
  setStatus('Failed', 'failed');
  $('p-title').textContent = 'We couldn’t finish setup.';
  $('p-description').textContent = 'Check the message below before trying again.';
  $('failure-text').textContent = message;
  setFix($('failure-fix'), $('failure-fix-text'), fix);
  $('failure').hidden = false;
  show('progress');
}

function backToIdle() {
  show('idle');
  refresh();
}

function startClock() {
  state.clockStart = performance.now();
  cancelAnimationFrame(state.clock);
  const tick = () => {
    $('clock').textContent = `${((performance.now() - state.clockStart) / 1000).toFixed(1)}s`;
    state.clock = requestAnimationFrame(tick);
  };
  tick();
}

// stopClock freezes the timer at at (or now) and returns the elapsed seconds.
function stopClock(at) {
  cancelAnimationFrame(state.clock);
  const elapsed = at ?? (performance.now() - (state.clockStart || performance.now())) / 1000;
  $('clock').textContent = `${elapsed.toFixed(1)}s`;
  return elapsed;
}

// ---------- the VM ----------

async function openShell(vm, readyIn) {
  state.vm = vm;
  state.ending = false;
  state.expired = false;
  state.retries = 0;
  const size = state.status.sizes?.find((s) => s.id === vm.size);
  $('s-title').textContent = workspaceName(size);
  $('workspace-details').open = false;
  $('expiry-notice').hidden = true;
  $('s-name').textContent = vm.name;
  $('s-size').textContent = size ? `${size.label} · ${size.machine}` : '—';
  $('s-ip').textContent = vm.ip || '—';
  $('s-ready').textContent = readyIn != null ? `${readyIn.toFixed(1)}s` : '—';
  $('s-copy').hidden = !vm.ssh;
  $('s-end').hidden = false;
  $('s-end').disabled = false;
  $('s-end').textContent = 'End session';
  $('curtain').hidden = true;
  $('tabs').hidden = !vm.editor;
  show('shell');
  selectTab('terminal'); // xterm has to be visible when it first opens
  try {
    await ensureTerminal();
  } catch {
    connectionStatus('Terminal unavailable', 'failed');
    curtain('The terminal could not load. Your workspace may still be running. Check your connection and reload to reconnect.', 'Reload', () => location.reload());
    return;
  }
  startCountdown(vm.expiresAt);
  if (state.expired) return;
  connect();
  if (vm.editor) {
    startEditor(vm);
    selectTab('editor');
  }
}

function editorURL(vm) {
  return `/editor/${vm.zone}/${vm.name}/`;
}

// startEditor waits for VS Code to start on the VM, then shows it.
async function startEditor(vm) {
  stopEditor();
  const frame = $('editor-frame');
  $('editor-wait').hidden = false;
  $('s-open').href = editorURL(vm);
  const began = Date.now();
  const poll = (state.editorPoll = {});
  while (state.editorPoll === poll && state.vm === vm) {
    try {
      const res = await fetch(`${editorURL(vm)}healthz`, { cache: 'no-store' });
      if (res.ok) break;
    } catch {}
    const waited = Math.round((Date.now() - began) / 1000);
    $('editor-wait-time').textContent = `${Math.floor(waited / 60)}:${String(waited % 60).padStart(2, '0')}`;
    if (waited > 600) {
      $('editor-wait-time').textContent = 'VS Code didn’t start. The terminal still works.';
      return;
    }
    await wait(waited < 60 ? 1000 : 3000); // quick at first: with the pre-built image it takes seconds
  }
  if (state.editorPoll !== poll || state.vm !== vm) return;
  frame.src = editorURL(vm);
  frame.hidden = false;
  $('editor-wait').hidden = true;
  $('s-open').hidden = false;
}

function stopEditor() {
  state.editorPoll = null;
  const frame = $('editor-frame');
  frame.hidden = true;
  frame.removeAttribute('src');
  $('s-open').hidden = true;
  $('editor-wait-time').textContent = '';
}

function selectTab(tab) {
  $('tab-editor').setAttribute('aria-selected', String(tab === 'editor'));
  $('tab-terminal').setAttribute('aria-selected', String(tab === 'terminal'));
  $('tab-editor').tabIndex = tab === 'editor' ? 0 : -1;
  $('tab-terminal').tabIndex = tab === 'terminal' ? 0 : -1;
  $('editor').hidden = tab !== 'editor';
  $('term').hidden = tab !== 'terminal';
  if (tab === 'terminal' && state.term) {
    state.fit.fit();
    state.term.focus();
  }
}

async function ensureTerminal() {
  if (state.term) {
    state.fit.fit();
    return;
  }
  // xterm measures the font once, so it has to be loaded first.
  try {
    await document.fonts.load('400 13px "Geist Mono"');
  } catch {}
  const term = new Terminal({
    cursorBlink: true,
    fontFamily: '"Geist Mono", ui-monospace, Menlo, monospace',
    fontSize: 13,
    lineHeight: 1.25,
    scrollback: 5000,
    theme: {
      background: '#101111', foreground: '#f0f1e9', cursor: '#ffa475', cursorAccent: '#101111',
      selectionBackground: 'rgba(226, 100, 63, 0.3)',
      black: '#242a26', red: '#d9674d', green: '#9dbb8a', yellow: '#dcc07a',
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

// connect opens the terminal socket. The server replays the shell's recent
// output, so the screen starts blank each time.
function connect() {
  const { vm, term } = state;
  if (!vm || state.expired || state.ending) return;
  connectionStatus('Connecting');
  state.ws?.close();
  term.reset();
  const size = new URLSearchParams({ cols: term.cols, rows: term.rows });
  const scheme = location.protocol === 'https:' ? 'wss' : 'ws';
  const ws = new WebSocket(`${scheme}://${location.host}/api/vms/${vm.zone}/${vm.name}/terminal?${size}`);
  ws.binaryType = 'arraybuffer';
  ws.onopen = () => {
    if (state.ws !== ws) return;
    connectionStatus('Connected', 'pill-ok');
    $('curtain').hidden = true;
    send({ type: 'resize', cols: term.cols, rows: term.rows });
    if (!$('term').hidden) term.focus();
  };
  ws.onmessage = (e) => {
    state.retries = 0;
    term.write(typeof e.data === 'string' ? e.data : new Uint8Array(e.data));
  };
  ws.onclose = (e) => {
    if (state.ws !== ws || state.ending || state.expired) return;
    connectionStatus('Disconnected', 'failed');
    if (e.code === 1000) return curtain('Session closed.', 'Reconnect', reconnect);
    if (e.code === 4000) return curtain(e.reason || 'Can’t connect to the VM.', 'Try again', reconnect);
    if (e.code === 4001) return curtain('This terminal is open in another tab.', 'Use it here', reconnect);
    // Dropped: a network blip, a server restart, or Cloud Run's hourly cut.
    // The shell is still there, so reattach quietly.
    if (state.retries < 5) {
      connectionStatus('Reconnecting');
      state.retries++;
      setTimeout(() => state.ws === ws && connect(), 800 * state.retries);
      return;
    }
    curtain('Connection lost.', 'Reconnect', reconnect);
  };
  state.ws = ws;
}

function connectionStatus(text, kind = '') {
  $('s-status-text').textContent = text;
  $('s-status').className = `pill ${kind}`;
}

function reconnect() {
  state.retries = 0;
  selectTab('terminal');
  connect();
}

function send(msg) {
  if (state.ws?.readyState === WebSocket.OPEN) state.ws.send(JSON.stringify(msg));
}

function startCountdown(expiresAt) {
  clearInterval(state.countdown);
  const end = Date.parse(expiresAt);
  if (!Number.isFinite(end)) {
    $('s-left').textContent = 'Unavailable';
    $('s-left').classList.remove('soon');
    $('expiry-title').textContent = 'Expiry time is unavailable.';
    $('expiry-notice').hidden = false;
    return;
  }
  const tick = () => {
    const left = Math.ceil((end - Date.now()) / 1000);
    const cell = $('s-left');
    if (left <= 0) {
      cell.textContent = '0:00';
      clearInterval(state.countdown);
      return expire();
    }
    const h = Math.floor(left / 3600);
    const m = Math.floor((left % 3600) / 60);
    const s = String(left % 60).padStart(2, '0');
    cell.textContent = h ? `${h}:${String(m).padStart(2, '0')}:${s}` : `${m}:${s}`;
    cell.classList.toggle('soon', left <= 300);
    $('expiry-notice').hidden = left > 300;
    $('expiry-title').textContent = left <= 60 ? 'Less than a minute left. Save your work now.' : 'Your workspace expires in less than 5 minutes.';
  };
  state.countdown = setInterval(tick, 1000);
  tick();
}

function expire() {
  state.expired = true;
  state.expiredName = state.vm.name;
  state.ws?.close();
  stopEditor();
  selectTab('terminal');
  $('s-copy').hidden = true;
  $('s-end').hidden = true;
  $('expiry-notice').hidden = true;
  connectionStatus('Expired', 'failed');
  curtain('This session has reached its scheduled end. Google Cloud is scheduled to delete the workspace and its files.', 'Back to workspaces', backToIdle);
}

function end() {
  $('end-dialog').returnValue = 'cancel';
  $('end-dialog').showModal();
}

async function deleteWorkspace() {
  const btn = $('s-end');
  const { vm } = state;
  if (!vm || state.ending || state.expired) return;
  state.ending = true;
  connectionStatus('Ending session');
  btn.textContent = 'Deleting…';
  state.ws?.close();
  btn.disabled = true;
  try {
    await api('DELETE', `/api/vms/${vm.zone}/${vm.name}`);
  } catch (e) {
    btn.disabled = false;
    btn.textContent = 'End session';
    state.ending = false;
    toast(`Couldn’t delete ${vm.name}: ${e.message}`);
    return connect();
  }
  btn.disabled = false;
  backToIdle();
  toast('Workspace deleted.');
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
  $('signin').hidden = view !== 'signin';
  $('idle').hidden = view !== 'idle';
  $('progress').hidden = view !== 'progress';
  $('shell').hidden = view !== 'shell';
  document.body.classList.toggle('in-shell', view === 'shell');
  if (view !== 'shell') {
    state.ws?.close();
    state.ws = null;
    clearInterval(state.countdown);
    stopEditor();
    state.vm = null;
  }
  if (view !== state.view) {
    const target = view === 'shell' ? $('shell') : $(view);
    target.classList.remove('enter');
    void target.offsetWidth; // restart the animation
    target.classList.add('enter');
    state.view = view;
  }
}

function stepEl(step) {
  return document.querySelector(`#steps li[data-step="${step}"]`);
}

function el(tag, className = '', text = '') {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text) node.textContent = text;
  return node;
}

function setFix(box, code, fix) {
  box.hidden = !fix;
  if (!fix) return;
  code.replaceChildren();
  if (/^https?:\/\//.test(fix)) {
    const a = el('a', '', fix);
    a.href = fix;
    a.target = '_blank';
    a.rel = 'noreferrer';
    code.append(a);
  } else {
    code.textContent = fix;
  }
}

function clockText(seconds) {
  const m = Math.floor(seconds / 60);
  return `${String(m).padStart(2, '0')}:${(seconds - m * 60).toFixed(1).padStart(4, '0')}`;
}

function ttlText(seconds) {
  return seconds % 3600 === 0 ? `${seconds / 3600} hr` : `${Math.round(seconds / 60)} min`;
}

function hoursText(h) {
  return `${Number.isInteger(h) ? h : h.toFixed(1)} h`;
}

function money(v) {
  if (v < 0.1) return `${(v * 100).toFixed(1).replace(/\.0$/, '')}¢`;
  if (v < 1) return `${Math.round(v * 100)}¢`;
  return usd(v);
}

function usd(v) {
  return `$${v.toFixed(2)}`;
}

function remember(key, value) {
  try {
    localStorage.setItem(`blink.${key}`, String(value));
  } catch {}
}

function remembered(key) {
  try {
    return localStorage.getItem(`blink.${key}`);
  } catch {
    return null;
  }
}

function loadScript(src) {
  return new Promise((resolve, reject) => {
    const s = document.createElement('script');
    s.src = src;
    s.async = true;
    s.onload = resolve;
    s.onerror = reject;
    document.head.append(s);
  });
}

async function api(method, path, body) {
  const res = await fetch(path, {
    method,
    headers: body === undefined ? {} : { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await res.text();
  let data = null;
  try {
    data = text ? JSON.parse(text) : null;
  } catch {}
  if (res.status === 401 && state.status?.signIn && path !== '/api/login') location.reload();
  if (!res.ok) {
    const err = new Error(data?.error || `HTTP ${res.status}`);
    err.data = data;
    throw err;
  }
  return data;
}

async function copy(text, message) {
  try {
    await navigator.clipboard.writeText(text);
    toast(message);
  } catch {
    toast('The browser blocked the clipboard.');
  }
}

let toastTimer = 0;
function toast(text) {
  const t = $('toast');
  t.textContent = text;
  t.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => (t.hidden = true), 2600);
}

function wait(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
