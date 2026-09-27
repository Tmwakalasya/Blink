// Dependency-free UI contract checks. This supplies browser boundaries, not a
// layout engine; rendered layout still needs a browser check.
const { test } = require('node:test');
const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const vm = require('node:vm');
const html = readFileSync(`${__dirname}/web/index.html`, 'utf8');
const source = readFileSync(`${__dirname}/web/app.js`, 'utf8');

class Element {
  constructor() {
    this.children = [];
    this.events = {};
    this.attrs = {};
    this.style = {};
    this.hidden = false;
    this.textContent = '';
    this.className = '';
    this.classList = {
      add: (c) => this.classList.toggle(c, true),
      remove: (c) => this.classList.toggle(c, false),
      toggle: (c, on) => {
        const classes = new Set(this.className.split(' ').filter(Boolean));
        if (on ?? !classes.has(c)) classes.add(c); else classes.delete(c);
        this.className = [...classes].join(' ');
      },
    };
  }
  append(...children) { this.children.push(...children); }
  replaceChildren(...children) { this.children = children; }
  addEventListener(name, fn) { this.events[name] = fn; }
  setAttribute(name, value) { this.attrs[name] = value; }
  getAttribute(name) { return this.attrs[name]; }
  removeAttribute(name) { delete this.attrs[name]; }
  querySelector() { return this.child ||= new Element(); }
  focus() {}
  showModal() { this.open = true; }
}

const defaultStatus = {
  ready: true, user: { admin: true }, project: 'test', zone: 'us-central1-a', image: 'Debian 12',
  sizes: [
    { id: 'small', label: 'Small', machine: 'e2-micro', cpu: '2 shared vCPUs', memory: '1 GB', hourly: .015 },
    { id: 'medium', label: 'Medium', machine: 'e2-medium', cpu: '2 shared vCPUs', memory: '4 GB', hourly: .04, editor: true },
    { id: 'large', label: 'Large', machine: 'e2-standard-4', cpu: '4 vCPUs', memory: '16 GB', hourly: .141, editor: true },
  ], lifetimes: [1800, 3600, 7200], maxVMs: 10, running: 0,
};

async function page({ status = defaultStatus, machines = [], fetchVMs, create, remove } = {}) {
  const elements = new Map([...html.matchAll(/<[^>]+\bid="([^"]+)"[^>]*>/g)].map(([tag, id]) => {
    const element = new Element();
    element.hidden = /\bhidden\b/.test(tag);
    return [id, element];
  }));
  const steps = ['requested', 'creating', 'booting', 'ssh'].map(() => new Element());
  const intervals = new Map();
  let nextTimer = 1;
  let now = Date.now();
  class Clock extends Date { static now() { return now; } }
  const document = {
    body: new Element(), head: new Element(), fonts: { load: async () => {} },
    getElementById: (id) => elements.get(id) || null,
    createElement: () => new Element(),
    querySelectorAll: (selector) => selector === '#steps li' ? steps : [],
    querySelector: (selector) => {
      const name = selector.match(/data-step="([^"]+)"/);
      return name ? steps[['requested', 'creating', 'booting', 'ssh'].indexOf(name[1])] : null;
    },
  };
  class Terminal {
    cols = 80; rows = 24;
    loadAddon() {} open() {} onData() {} onResize() {} reset() {} focus() {} write() {}
  }
  class Socket { static OPEN = 1; close() {} send() {} }
  const json = (value, code = 200) => new Response(JSON.stringify(value), { status: code });
  const calls = [];
  const context = vm.createContext({
    document, Date: Clock, performance: { now: () => now }, Terminal, WebSocket: Socket,
    FitAddon: { FitAddon: class { fit() {} } }, ResizeObserver: class { observe() {} },
    location: { protocol: 'http:', host: 'localhost', reload() {} },
    localStorage: { getItem: () => null, setItem() {} }, navigator: {},
    URLSearchParams, TextDecoder, Uint8Array,
    requestAnimationFrame: () => 1, cancelAnimationFrame() {},
    setTimeout: () => 1, clearTimeout() {},
    setInterval: (fn) => { const id = nextTimer++; intervals.set(id, fn); return id; },
    clearInterval: (id) => intervals.delete(id),
    fetch: async (path, options = {}) => {
      calls.push([options.method || 'GET', path]);
      if (path === '/api/status') return json(status);
      if (path === '/api/vms' && options.method === 'POST') return create ? create(options) : json({ error: 'unavailable' }, 503);
      if (path === '/api/vms') return fetchVMs ? fetchVMs() : json({ vms: machines });
      if (options.method === 'DELETE') return remove ? remove() : new Response(null, { status: 204 });
      if (path === '/api/class') return json({});
      return json({ error: 'not available' }, 503);
    },
  });
  vm.runInContext(source.replace('\ninit();', '\nthis.boot = init();'), context);
  await context.boot;
  return {
    get: (id) => elements.get(id), calls,
    async click(id) { await elements.get(id).events.click(); },
    async change(id, index) { await elements.get(id).children[index].children[0].events.change(); },
    advance(ms) { now += ms; for (const tick of [...intervals.values()]) tick(); },
    intervalCount: () => intervals.size,
  };
}

function machine(expiresAt) {
  return { name: 'blink-test', zone: 'us-central1-a', size: 'small', ready: true, hasKey: true, status: 'RUNNING', ip: '127.0.0.1', expiresAt };
}

test('launch recommends Development, updates total for choices, and enforces budget', async () => {
  const p = await page({ status: { ...defaultStatus, budget: { used: 0, limit: .05 } } });
  assert.equal(p.get('sizes').children[1].children[0].checked, true);
  assert.equal(p.get('sum-cost').textContent, '4¢');
  assert.equal(p.get('start').disabled, false);
  await p.change('lifetimes', 2);
  assert.equal(p.get('sum-cost').textContent, '8¢');
  assert.equal(p.get('start').disabled, true);
  assert.match(p.get('limit-note').textContent, /over.*budget/);
  await p.change('sizes', 0);
  assert.equal(p.get('sum-cost').textContent, '3¢');
  assert.equal(p.get('start').disabled, false);
});

test('restricted menus select an offered workspace and lifetime', async () => {
  const p = await page({ status: { ...defaultStatus, sizes: [defaultStatus.sizes[0]], lifetimes: [1800] } });
  assert.equal(p.get('sizes').children[0].children[0].checked, true);
  assert.equal(p.get('lifetimes').children[0].children[0].checked, true);
  assert.match(p.get('sum-description').textContent, /Terminal · 30 min/);
});

test('a failed list reports uncertainty instead of claiming deletion', async () => {
  const p = await page({ fetchVMs: () => { throw new Error('connection lost'); } });
  assert.equal(p.get('notice').hidden, false);
  assert.match(p.get('notice-text').textContent, /Couldn't check existing workspaces/);
  assert.doesNotMatch(p.get('notice-text').textContent, /deleted/);
});

test('startup errors remain visible outside the collapsed technical log', async () => {
  const p = await page({ create: () => new Response(JSON.stringify({ error: 'Cleanup failed: deletion not confirmed', step: 'creating', at: 2 }) + '\n') });
  await p.click('start');
  assert.equal(p.get('progress').hidden, false);
  assert.equal(p.get('progress-details').open, false);
  assert.equal(p.get('failure').hidden, false);
  assert.match(p.get('failure-text').textContent, /deletion not confirmed/);
});

test('expiry warns five minutes ahead and never asserts confirmed deletion', async () => {
  const p = await page({ machines: [machine(new Date(Date.now() + 301000).toISOString())] });
  assert.equal(p.get('expiry-notice').hidden, true);
  p.advance(2000);
  assert.equal(p.get('expiry-notice').hidden, false);
  assert.match(p.get('expiry-title').textContent, /5 minutes/);
  await p.click('save-help');
  assert.equal(p.get('save-dialog').open, true);
  p.advance(300000);
  assert.equal(p.get('s-status-text').textContent, 'Expired');
  assert.match(p.get('curtain-text').textContent, /scheduled/);
  assert.doesNotMatch(p.get('curtain-text').textContent, /was deleted/);
  assert.equal(p.intervalCount(), 0);
});

test('unknown expiry is shown explicitly instead of inventing a new lifetime', async () => {
  const p = await page({ machines: [machine(undefined)] });
  assert.equal(p.get('s-left').textContent, 'Unavailable');
  assert.equal(p.get('expiry-notice').hidden, false);
  assert.equal(p.intervalCount(), 0);
});

test('ending a session needs explicit confirmation; failed deletion stays in workspace', async () => {
  const p = await page({ machines: [machine(new Date(Date.now() + 3600000).toISOString())], remove: () => new Response('{"error":"deletion pending"}', { status: 502 }) });
  await p.click('s-end');
  assert.equal(p.get('end-dialog').open, true);
  assert.equal(p.calls.some(([method]) => method === 'DELETE'), false);
  p.get('end-dialog').returnValue = 'cancel';
  await p.get('end-dialog').events.close();
  assert.equal(p.calls.some(([method]) => method === 'DELETE'), false);
  p.get('end-dialog').returnValue = 'confirm';
  await p.get('end-dialog').events.close();
  // The dialog handler starts an async operation; let its response settle.
  await new Promise(setImmediate);
  assert.equal(p.get('shell').hidden, false);
  assert.equal(p.get('s-end').disabled, false);
  assert.match(p.get('toast').textContent, /deletion pending/);
});

test('admins never automatically enter someone else’s workspace', async () => {
  const p = await page({ status: { ...defaultStatus, signIn: { clientId: 'test' }, user: { admin: true, email: 'owner@test' } }, machines: [{ ...machine(), email: 'someone-else@test' }] });
  assert.equal(p.get('idle').hidden, false);
  assert.equal(p.get('shell').hidden, true);
});
