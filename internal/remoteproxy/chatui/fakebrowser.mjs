// A browser small enough to run chat.js under `node --test`: the few DOM,
// fetch, EventSource and timer calls the page makes, against a scripted hub.
// Test support only; it is not embedded in the binary.

const realSetTimeout = globalThis.setTimeout;
export const tick = (ms = 0) => new Promise((r) => realSetTimeout(r, ms));

class FakeNode {
  constructor(tag) {
    Object.assign(this, { tag, children: [], hidden: false, disabled: false, className: '', style: {}, listeners: {}, text: '', value: '', attrs: {} });
    Object.assign(this, { scrollHeight: 0, scrollTop: 0, clientHeight: 0 });
  }
  set textContent(v) {
    this.text = String(v);
    this.children = [];
  }
  get textContent() {
    return this.text + this.children.map((c) => c.textContent).join('');
  }
  append(...nodes) {
    for (const n of nodes) this.children.push(...(n.tag === '#fragment' ? n.children : [n]));
  }
  replaceChildren(...nodes) {
    this.children = [];
    this.append(...nodes);
  }
  addEventListener(type, f) {
    (this.listeners[type] ||= []).push(f);
  }
  dispatch(type, ev = {}) {
    for (const f of this.listeners[type] || []) f({ preventDefault() {}, ...ev });
  }
  setAttribute(k, v) {
    this.attrs[k] = v;
  }
  focus() {}
}

// load starts one page against hub, a function (method, path, body) → {status,
// body} (or {down: true} for a network fault). It returns the page's parts.
let loads = 0;
export async function load(hub, opts = {}) {
  const env = { els: {}, calls: [], streams: [], intervals: [], timers: [], reloads: 0, store: { ...opts.store }, log: [], pointerFine: true };
  const doc = new FakeNode('#document');
  doc.getElementById = (id) => (env.els[id] ||= new FakeNode(id));
  doc.createElement = (tag) => new FakeNode(tag);
  doc.createDocumentFragment = () => new FakeNode('#fragment');
  doc.visibilityState = 'visible';
  doc.title = '';
  // The state chat.html starts in.
  for (const id of ['older', 'notice', 'error', 'terminal', 'console']) doc.getElementById(id).hidden = true;
  // fieldsEnabled: what a browser that restores form state over a reload leaves.
  for (const id of ['text', 'send']) doc.getElementById(id).disabled = !opts.fieldsEnabled;
  globalThis.document = doc;
  globalThis.window = { matchMedia: () => ({ matches: env.pointerFine }), addEventListener() {} };
  globalThis.location = { origin: 'https://mac.ts.net', reload: () => env.reloads++ };
  globalThis.sessionStorage = { getItem: (k) => env.store[k] ?? null, setItem: (k, v) => (env.store[k] = String(v)) };
  globalThis.EventSource = class {
    static CLOSED = 2;
    constructor(url) {
      Object.assign(this, { url, readyState: 1, listeners: {} });
      env.streams.push(this);
      env.log.push('stream');
    }
    addEventListener(type, f) {
      (this.listeners[type] ||= []).push(f);
    }
    emit(type, ev = {}) {
      for (const f of this.listeners[type] || []) f(ev);
    }
    close() {
      this.readyState = 2;
    }
  };
  globalThis.fetch = async (path, init = {}) => {
    const method = init.method || 'GET';
    env.calls.push({ method, path, body: init.body ? JSON.parse(init.body) : undefined });
    env.log.push(`${method} ${path}`);
    const r = await hub(method, path, init.body ? JSON.parse(init.body) : undefined);
    if (r.down) throw new Error('network');
    if (r.redirect) {
      // As a browser does: an unfollowed redirect is opaque, a followed one
      // ends in whatever the target serves (here the hub's login page, 200).
      if (init.redirect === 'manual') return { ok: false, status: 0, type: 'opaqueredirect', text: async () => '' };
      if (init.redirect === 'error') throw new Error('redirect');
      return { ok: true, status: 200, type: 'basic', text: async () => '<!doctype html><title>Sign in</title>' };
    }
    const text = typeof r.body === 'string' ? r.body : JSON.stringify(r.body);
    return { ok: r.status >= 200 && r.status < 300, status: r.status, text: async () => text };
  };
  globalThis.setInterval = (f, ms) => env.intervals.push({ f, ms });
  // Timers are held, not run: a test fires the ones it wants (runTimers).
  globalThis.setTimeout = (f, ms) => env.timers.push({ f, ms });
  env.runTimers = async () => {
    for (const t of env.timers.splice(0)) t.f();
    await tick(5);
  };
  env.poll = async () => {
    for (const i of env.intervals) i.f();
    await tick(5);
    await env.runTimers();
  };
  env.rows = () => env.els.list.children.map((r) => `${r.className}: ${r.children.map((c) => c.textContent).join(' / ')}`);
  env.count = (method, prefix) => env.calls.filter((c) => c.method === method && c.path.startsWith(prefix)).length;
  await import(`./chat.js?load=${++loads}`);
  await tick(5);
  return env;
}
