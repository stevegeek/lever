import test from 'node:test';
import assert from 'node:assert/strict';

let loads = 0;
// loadSW runs sw.js against a fake worker global.
async function loadSW({ agents = [{ name: 'w1', access: 'message' }, { name: 'w2', access: 'see' }], fetchFails = false, windows = [] } = {}) {
  const env = { shown: [], opened: [], posted: [], focused: 0, fetches: 0, listeners: {} };
  globalThis.self = {
    addEventListener: (t, f) => (env.listeners[t] = f),
    registration: { showNotification: async (title, o) => env.shown.push({ title, o }) },
    clients: {
      matchAll: async () => windows.map((url) => ({ url, postMessage: (m) => env.posted.push(m), focus: async () => env.focused++ })),
      openWindow: async (u) => env.opened.push(u),
    },
  };
  globalThis.fetch = async () => {
    env.fetches++;
    if (fetchFails) throw new Error('network');
    return { ok: true, json: async () => ({ agents }) };
  };
  await import(`./sw.js?load=${++loads}`);
  env.push = async (raw) => {
    let done;
    env.listeners.push({ data: raw === undefined ? null : { json: () => JSON.parse(raw) }, waitUntil: (p) => (done = p) });
    await done;
  };
  env.click = async (data) => {
    let done, closed = false;
    env.listeners.notificationclick({ notification: { data, close: () => (closed = true) }, waitUntil: (p) => (done = p) });
    await done;
    return closed;
  };
  return env;
}

test('a push names a listed agent and carries no text', async () => {
  const env = await loadSW();
  await env.push('{"v":1,"agent":"w1"}');
  assert.equal(env.shown[0].title, 'New message from w1');
  assert.equal(env.shown[0].o.tag, 'w1');
  assert.deepEqual(env.shown[0].o.data, { agent: 'w1' });
  assert.equal(env.shown[0].o.body, undefined);
});

test('anything else shows the plain text', async () => {
  for (const raw of ['{"v":1,"agent":"w2"}', '{"v":1,"agent":"nobody"}', '{"v":2,"agent":"w1"}', '{"v":1,"agent":"<b>"}', '{"v":1,"agent":"W1"}', 'not json', undefined]) {
    const env = await loadSW();
    await env.push(raw);
    assert.equal(env.shown[0].title, 'New message', String(raw));
    assert.deepEqual(env.shown[0].o.data, { agent: '' });
  }
  const down = await loadSW({ fetchFails: true });
  await down.push('{"v":1,"agent":"w1"}');
  assert.equal(down.shown[0].title, 'New message');
});

test('a tap focuses an open chat or opens one', async () => {
  let env = await loadSW({ windows: ['https://mac.ts.net/lever/chat'] });
  assert.equal(await env.click({ agent: 'w1' }), true);
  assert.deepEqual(env.posted, [{ agent: 'w1' }]);
  assert.equal(env.focused, 1);
  assert.equal(env.opened.length, 0);
  env = await loadSW();
  await env.click({ agent: 'w1' });
  assert.deepEqual(env.opened, ['/lever/chat#agent=w1']);
  env = await loadSW();
  await env.click({ agent: '../evil' });
  assert.deepEqual(env.opened, ['/lever/chat']);
});
