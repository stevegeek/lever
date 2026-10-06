// A browser small enough to run chat.js under `node --test`: the few DOM,
// fetch, EventSource and timer calls the page makes, against a scripted hub.
// Test support only; it is not embedded in the binary.

const realSetTimeout = globalThis.setTimeout;
export const tick = (ms = 0) => new Promise((r) => realSetTimeout(r, ms));

class FakeNode {
  constructor(tag) {
    Object.assign(this, { tag, children: [], hidden: false, disabled: false, className: '', style: {}, listeners: {}, text: '', value: '', attrs: {} });
    Object.assign(this, { scrollHeight: 0, scrollTop: 0, clientHeight: 0 });
    const classes = new Set();
    this.classList = { add: (c) => classes.add(c), remove: (c) => classes.delete(c), contains: (c) => classes.has(c) };
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
  doc.body = new FakeNode('body');
  // The state chat.html starts in.
  for (const id of ['older', 'error', 'terminal', 'console', 'composer', 'ask', 'viewonly', 'listnote', 'note', 'label', 'contacts', 'contacts-title', 'refresh', 'readonly', 'push', 'pushnote']) doc.getElementById(id).hidden = true;
  // fieldsEnabled: what a browser that restores form state over a reload leaves.
  for (const id of ['text', 'send']) doc.getElementById(id).disabled = !opts.fieldsEnabled;
  globalThis.document = doc;
  env.document = doc;
  globalThis.window = { matchMedia: () => ({ matches: env.pointerFine }), addEventListener() {} };
  // Trusted Types as the push-on CSP sets it (opts.trustedTypes): only the
  // policy name lever-sw, once; a script-URL sink refuses a plain string.
  if (opts.trustedTypes) {
    env.tt = { policies: {}, refused: 0 };
    globalThis.window.trustedTypes = {
      createPolicy: (name, rules) => {
        if (name !== 'lever-sw' || env.tt.policies[name]) {
          env.tt.refused++;
          throw new TypeError(`policy ${name} refused by the CSP`);
        }
        const p = { name, createScriptURL: (u) => ({ trusted: 'TrustedScriptURL', url: rules.createScriptURL(u) }) };
        env.tt.policies[name] = p;
        return p;
      },
    };
  }
  globalThis.location = { origin: 'https://mac.ts.net', reload: () => env.reloads++ };
  // Push: present only when a test asks (opts.push), as in a browser
  // without it (iOS outside a Home Screen app).
  env.replaced = [];
  globalThis.history = { replaceState: (...a) => env.replaced.push(a) };
  globalThis.location.hash = opts.hash || '';
  let swListeners = {};
  env.swMessage = (data) => (swListeners.message || []).forEach((f) => f({ data }));
  if (opts.push) {
    const p = (env.push = { permission: opts.push.permission || 'default', registerCalls: [], subscribeOpts: null, sub: null, registered: null });
    const fakeSub = () => ({
      endpoint: 'https://fcm.googleapis.com/fcm/send/fake',
      toJSON: () => ({ endpoint: 'https://fcm.googleapis.com/fcm/send/fake', expirationTime: null, keys: { p256dh: 'P', auth: 'A' } }),
      unsubscribe: async () => { p.sub = null; return true; },
    });
    const reg = {
      pushManager: {
        getSubscription: async () => p.sub,
        subscribe: async (o) => { if (opts.push.subscribeFails) throw new Error('no'); p.subscribeOpts = o; p.sub = fakeSub(); return p.sub; },
      },
      unregister: async () => { p.registered = null; return true; },
    };
    if (opts.push.registered) p.registered = reg;
    if (opts.push.existing) p.sub = fakeSub();
    globalThis.window.PushManager = class {};
    globalThis.Notification = { get permission() { return p.permission; }, requestPermission: async () => (p.permission = opts.push.grant || 'granted') };
    Object.defineProperty(globalThis, 'navigator', { configurable: true, writable: true, value: { serviceWorker: {
      register: async (url, o) => {
        // As Chrome does under require-trusted-types-for 'script'.
        if (globalThis.window.trustedTypes && typeof url === 'string') throw new TypeError("This document requires 'TrustedScriptURL' assignment.");
        const trusted = typeof url === 'object';
        p.registerCalls.push(trusted ? { url: url.url, scope: o && o.scope, trusted } : { url, scope: o && o.scope });
        p.registered = reg;
        return reg;
      },
      get ready() { return Promise.resolve(reg); },
      getRegistration: async () => p.registered || undefined,
      addEventListener: (t, f) => (swListeners[t] ||= []).push(f),
    } } });
  } else {
    delete globalThis.Notification;
    Object.defineProperty(globalThis, 'navigator', { configurable: true, writable: true, value: {} });
  }
  env.local = { ...opts.local };
  globalThis.localStorage = { getItem: (k) => env.local[k] ?? null, setItem: (k, v) => (env.local[k] = String(v)), removeItem: (k) => delete env.local[k] };
  globalThis.sessionStorage = { getItem: (k) => env.store[k] ?? null, setItem: (k, v) => (env.store[k] = String(v)), removeItem: (k) => delete env.store[k] };
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
    // As a browser does: an aborted request rejects, however far it got.
    const aborted = new Promise((_, reject) => init.signal?.addEventListener('abort', () => reject(new Error('aborted'))));
    const r = await Promise.race([hub(method, path, init.body ? JSON.parse(init.body) : undefined), aborted]);
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
  // Timers are held, not run: a test fires the ones it wants. runTimers
  // fires the short ones (the page's own pacing); a request's time limit is
  // long, and runs only when a test asks for it (runTimers(Infinity)).
  let timerIds = 0;
  globalThis.setTimeout = (f, ms) => {
    env.timers.push({ f, ms, id: ++timerIds });
    return timerIds;
  };
  globalThis.clearTimeout = (id) => {
    env.timers = env.timers.filter((t) => t.id !== id);
  };
  env.runTimers = async (upTo = 10000) => {
    const due = env.timers.filter((t) => t.ms <= upTo);
    env.timers = env.timers.filter((t) => t.ms > upTo);
    for (const t of due) t.f();
    await tick(5);
  };
  env.poll = async () => {
    for (const i of env.intervals) i.f();
    await tick(5);
    await env.runTimers();
  };
  env.rows = () => env.els.list.children.map((r) => `${r.className}: ${r.children.map((c) => c.textContent).join(' / ')}`);
  // agentRows: each list row as "title | chip | badge[ | view only]".
  env.agentRows = () => env.els.agents.children.map((li) => li.children[0].children.map((c) => c.textContent).join(' | '));
  // contactRows: each contact row as "login | note", then its open agents
  // as "  title | chip".
  env.contactRows = () => env.els.contacts.children.flatMap((li) => [
    li.children[0].children.map((c) => c.textContent).join(' | '),
    ...(li.children[1] ? li.children[1].children.map((a) => `  ${a.children[0].children.map((c) => c.textContent).join(' | ')}`) : []),
  ]);
  env.clickContact = async (i) => {
    env.els.contacts.children[i].children[0].dispatch('click');
    await tick(5);
  };
  env.clickContactAgent = async (i, j) => {
    env.els.contacts.children[i].children[1].children[j].children[0].dispatch('click');
    await tick(5);
  };
  env.count = (method, prefix) => env.calls.filter((c) => c.method === method && c.path.startsWith(prefix)).length;
  await import(`./chat.js?load=${++loads}`);
  await tick(5);
  return env;
}
