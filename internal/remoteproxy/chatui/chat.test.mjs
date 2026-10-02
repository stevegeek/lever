import test from 'node:test';
import assert from 'node:assert/strict';
import { load, tick } from './fakebrowser.mjs';

const KEY = 'dm:agent:a1:user:u1';
const HISTORY = `/api/v1/chat/conversations/${encodeURIComponent(KEY)}/messages`;
const bootBody = (over = {}) => ({ login: 'op', userId: 'u1', agent: { name: 'boss', id: 'a1' }, conversation: KEY, terminal: '/agents/a1/terminal', console: '/agents', ...over });
const msg = (i, over = {}) => ({ id: `m${String(i).padStart(3, '0')}`, msg: `msg ${i}`, senderId: 'a1', type: 'instruction', createdAt: new Date(1.7e12 + i * 1000).toISOString(), ...over });
// A newest-first page of n messages ending at number `last`, as the hub sends.
const page = (last, n = 50) => Array.from({ length: n }, (_, k) => msg(last - k));

// hubWith builds a scripted hub; each part can be replaced by a test.
function hubWith(parts = {}) {
  const h = {
    boot: () => ({ status: 200, body: bootBody() }),
    agent: () => ({ status: 200, body: { id: 'a1', phase: 'running', activity: 'working' } }),
    history: () => ({ status: 200, body: { messages: [], totalCount: 0 } }),
    post: (body) => ({ status: 201, body: { id: 'sent1', content: body.content, senderId: 'u1', type: 'instruction', createdAt: new Date(1.8e12).toISOString(), dispatchState: 'dispatched' } }),
    ...parts,
  };
  const fn = (method, path, body) => {
    if (path === '/lever/api/chat') return h.boot();
    if (path.startsWith('/api/v1/agents/')) return h.agent(path);
    if (method === 'POST') return h.post(body, path);
    return h.history(path);
  };
  fn.parts = h;
  return fn;
}

async function type(env, text) {
  env.els.text.value = text;
  env.els.text.dispatch('input');
}

test('start: shows the agent, its state, the links and the history, oldest first', async () => {
  const env = await load(hubWith({ history: () => ({ status: 200, body: { messages: [msg(2), msg(1, { senderId: 'u1' })] } }) }));
  assert.equal(env.els.agent.textContent, 'boss');
  assert.equal(env.els.state.textContent, 'working');
  assert.equal(env.els.state.className, 'state ok');
  assert.equal(env.els.terminal.attrs.href, '/agents/a1/terminal');
  assert.equal(env.els.console.attrs.href, '/agents');
  assert.equal(env.els.text.disabled, false);
  assert.equal(env.rows().length, 2);
  assert.match(env.rows()[0], /^msg mine: You .* \/ msg 1$/);
  assert.match(env.rows()[1], /^msg agent: boss .* \/ msg 2$/);
  assert.equal(env.streams.length, 1);
  assert.equal(env.streams[0].url, '/events?sub=user.u1.chat.%3E');
  assert.equal(env.els.notice.hidden, true);
  // The stream opens before the history is read, so nothing stored between
  // the two is missed.
  const order = env.calls.map((c) => c.path);
  assert.ok(order.indexOf('/lever/api/chat') < order.findIndex((p) => p.startsWith(HISTORY)));
});

test('network text is only ever text, and a link never leaves the origin', async () => {
  const evil = '<img src=x onerror=alert(1)><script>alert(2)</script>';
  for (const terminal of ['//evil.test/x', '/\t/evil.test/x', '/\\evil.test', 'https://evil.test', 'javascript:alert(1)', 7, null]) {
    const env = await load(hubWith({
      boot: () => ({ status: 200, body: bootBody({ agent: { name: evil, id: 'a1' }, terminal }) }),
      history: () => ({ status: 200, body: { messages: [msg(1, { msg: evil }), msg(2, { msg: evil, type: 'state-change' })] } }),
      agent: () => ({ status: 200, body: { phase: 'running', activity: evil } }),
    }));
    assert.equal(env.els.terminal.hidden, true, `terminal ${String(terminal)} must not become a link`);
    assert.equal(env.els.terminal.attrs.href, undefined);
    // Every node the page made is a div holding text: nothing was parsed.
    for (const row of env.els.list.children) {
      assert.equal(row.tag, 'div');
      for (const c of row.children) assert.deepEqual([c.tag, c.children.length], ['div', 0]);
    }
    assert.ok(env.rows()[0].endsWith(evil));
    // A state line names the agent, so it cannot pass for a lever notice.
    assert.equal(env.rows()[1], `msg system: ${evil}: ${evil}`);
    assert.ok(env.els.state.textContent.length <= 40);
  }
});

test('a failed first read is repaired by a later one', async () => {
  const hub = hubWith({ history: () => ({ status: 502, body: 'bad gateway\n' }) });
  const env = await load(hub);
  assert.match(env.els.notice.textContent, /Cannot read the conversation: bad gateway/);
  hub.parts.history = () => ({ status: 200, body: { messages: page(100), nextCursor: 'C1' } });
  await env.poll();
  assert.equal(env.els.notice.hidden, true);
  assert.equal(env.rows().length, 50);
  assert.equal(env.els.older.hidden, false, 'Load earlier must show once a read succeeds');
});

test('an empty conversation says so, also after a failed first read', async () => {
  const hub = hubWith({ history: () => ({ status: 500, body: '' }) });
  const env = await load(hub);
  hub.parts.history = () => ({ status: 200, body: { messages: [] } });
  await env.poll();
  assert.equal(env.els.notice.textContent, 'No messages yet.');
});

test('a history answer of any shape does not stop the page', async () => {
  for (const body of ['null', 'not json', '[]', '{"messages":"x"}', '{"messages":[null,7,{"id":7}]}']) {
    const env = await load(hubWith({ history: () => ({ status: 200, body }) }));
    assert.equal(env.streams.length, 1, body);
    assert.equal(env.intervals.length, 1, body);
    assert.equal(env.rows().length, 0, body);
  }
});

test('more than a page of new messages restarts the list instead of leaving a hole', async () => {
  const hub = hubWith({ history: () => ({ status: 200, body: { messages: page(100), nextCursor: 'C1' } }) });
  const env = await load(hub);
  assert.equal(env.rows().length, 50);
  hub.parts.history = (path) => (path.includes('cursor=C2')
    ? { status: 200, body: { messages: page(250), nextCursor: 'C3' } }
    : { status: 200, body: { messages: page(300), nextCursor: 'C2' } });
  await env.poll();
  assert.equal(env.rows().length, 50);
  assert.match(env.rows()[0], /msg 251$/);
  assert.match(env.rows()[49], /msg 300$/);
  // "Load earlier" continues from the new page, so the rows between are reachable.
  env.els.older.dispatch('click');
  await tick(5);
  assert.equal(env.rows().length, 100);
  assert.match(env.rows()[0], /msg 201$/);
});

test('an ordinary new message is merged, not a restart', async () => {
  const hub = hubWith({ history: () => ({ status: 200, body: { messages: page(100), nextCursor: 'C1' } }) });
  const env = await load(hub);
  hub.parts.history = () => ({ status: 200, body: { messages: page(101), nextCursor: 'C1b' } });
  await env.poll();
  assert.equal(env.rows().length, 51);
  assert.match(env.rows()[0], /msg 51$/);
});

test('an event for this user reads the history again; any other does not', async () => {
  const env = await load(hubWith());
  const reads = () => env.count('GET', HISTORY);
  const before = reads();
  env.streams[0].emit('update', { data: JSON.stringify({ subject: 'user.u2.chat.dm', data: {} }) });
  env.streams[0].emit('update', { data: 'not json' });
  env.streams[0].emit('update', { data: JSON.stringify({ subject: 'user.u1.notification' }) });
  await env.runTimers();
  assert.equal(reads(), before);
  // A burst is one read now or one held read, never one per event.
  for (let i = 0; i < 20; i++) env.streams[0].emit('update', { data: JSON.stringify({ subject: 'user.u1.chat.dm', data: { msg: '<b>' } }) });
  await env.runTimers();
  assert.ok(reads() - before >= 1 && reads() - before <= 2, `${reads() - before} reads for a burst of 20`);
});

test('send: posts the hub\'s field names, shows the message, clears the draft', async () => {
  const env = await load(hubWith());
  await type(env, '  hello  ');
  assert.equal(env.store['lever-chat-draft'], '  hello  ');
  env.els.composer.dispatch('submit');
  await tick(5);
  const posts = env.calls.filter((c) => c.method === 'POST');
  assert.equal(posts.length, 1);
  assert.equal(posts[0].path, HISTORY);
  assert.deepEqual(Object.keys(posts[0].body).sort(), ['content', 'idempotency_key']);
  assert.equal(posts[0].body.content, 'hello');
  assert.ok(posts[0].body.idempotency_key.length >= 32);
  assert.match(env.rows()[0], /^msg mine: You .* \/ hello$/);
  assert.equal(env.els.text.value, '');
  assert.equal(env.store['lever-chat-draft'], '');
  assert.equal(env.els.send.disabled, false);
});

test('send: text typed while a send is under way is kept', async () => {
  let release;
  const env = await load(hubWith({ post: (body) => new Promise((r) => (release = () => r({ status: 201, body: { id: 's', content: body.content, senderId: 'u1', createdAt: new Date().toISOString() } }))) }));
  await type(env, 'first');
  env.els.composer.dispatch('submit');
  await tick(5);
  assert.equal(env.els.send.disabled, true);
  env.els.composer.dispatch('submit'); // a second submit while sending does nothing
  await type(env, 'second thought');
  release();
  await tick(5);
  assert.equal(env.count('POST', HISTORY), 1);
  assert.equal(env.els.text.value, 'second thought');
  assert.equal(env.store['lever-chat-draft'], 'second thought');
});

test('send: a refusal shows the hub\'s reason and keeps the draft', async () => {
  const env = await load(hubWith({ post: () => ({ status: 400, body: { error: { code: 'validation', message: 'message exceeds 16000 character limit' } } }) }));
  await type(env, 'keep me');
  env.els.composer.dispatch('submit');
  await tick(5);
  assert.equal(env.els.error.textContent, 'Not sent: message exceeds 16000 character limit (HTTP 400)');
  assert.equal(env.els.error.hidden, false);
  assert.equal(env.els.text.value, 'keep me');
  assert.equal(env.rows().length, 0);
});

test('send: a forgotten session is retried once under the same key', async () => {
  let n = 0;
  const env = await load(hubWith({ post: (body) => (++n === 1 ? { status: 401, body: { error: 'authentication required' } } : { status: 201, body: { id: 's', content: body.content, senderId: 'u1', createdAt: new Date().toISOString() } }) }));
  await type(env, 'again');
  env.els.composer.dispatch('submit');
  await tick(10);
  const posts = env.calls.filter((c) => c.method === 'POST');
  assert.equal(posts.length, 2);
  assert.equal(posts[0].body.idempotency_key, posts[1].body.idempotency_key);
  assert.equal(env.els.error.hidden, true);
  assert.match(env.rows()[0], /again$/);
});

test('send: a replay answer (200) is not drawn from its thin body', async () => {
  const hub = hubWith({ post: (body) => ({ status: 200, body: { id: 'r1', content: body.content, sender: 'user:op' } }) });
  const env = await load(hub);
  hub.parts.history = () => ({ status: 200, body: { messages: [msg(1, { id: 'r1', msg: 'replayed', senderId: 'u1' })] } });
  await type(env, 'replayed');
  env.els.composer.dispatch('submit');
  await tick(5);
  await env.runTimers();
  assert.deepEqual(env.rows().map((r) => r.split(':')[0]), ['msg mine']);
  assert.equal(env.els.text.value, '');
});

test('send: the limit counts characters as the hub does', async () => {
  const env = await load(hubWith());
  await type(env, '😀'.repeat(16000));
  env.els.composer.dispatch('submit');
  await tick(5);
  assert.equal(env.count('POST', HISTORY), 1, '16000 emoji are 16000 characters');
  await type(env, 'x'.repeat(16001));
  env.els.composer.dispatch('submit');
  await tick(5);
  assert.equal(env.count('POST', HISTORY), 1);
  assert.match(env.els.error.textContent, /16001 characters; the limit is 16000/);
});

test('a failed delivery shows under the message', async () => {
  const env = await load(hubWith({ history: () => ({ status: 200, body: { messages: [msg(1, { senderId: 'u1', dispatchState: 'failed', dispatchFailureReason: 'agent is suspended' })] } }) }));
  assert.match(env.rows()[0], /\/ msg 1 \/ Not delivered: agent is suspended$/);
});

test('the manager gets a new hub record: the page reloads onto it', async () => {
  const hub = hubWith();
  const env = await load(hub);
  hub.parts.agent = () => ({ status: 404, body: { error: { message: 'not found' } } });
  await env.poll();
  assert.equal(env.reloads, 0, 'the same record in the bootstrap is no reason to reload');
  assert.equal(env.els.state.textContent, 'no hub record');
  hub.parts.boot = () => ({ status: 200, body: bootBody({ agent: { name: 'boss', id: 'a2' }, conversation: 'dm:agent:a2:user:u1' }) });
  await env.poll();
  assert.equal(env.reloads, 1);
});

test('no hub record at start: the composer stays off until one appears', async () => {
  const hub = hubWith({ boot: () => ({ status: 200, body: { login: 'op', userId: 'u1', agent: { name: 'boss', id: '' }, console: '/agents' } }) });
  const env = await load(hub);
  assert.equal(env.els.text.disabled, true);
  assert.equal(env.els.send.disabled, true);
  assert.match(env.els.notice.textContent, /boss has no record on the hub yet/);
  assert.equal(env.streams.length, 0);
  assert.equal(env.count('GET', HISTORY), 0);
  env.els.composer.dispatch('submit');
  await tick(5);
  assert.equal(env.count('POST', ''), 0);
  hub.parts.boot = () => ({ status: 200, body: bootBody() });
  await env.poll();
  assert.equal(env.reloads, 1);
});

test('the hub is away at start: the page keeps trying', async () => {
  const hub = hubWith({ boot: () => ({ status: 502, body: 'cannot resolve your hub user\n' }) });
  const env = await load(hub);
  assert.match(env.els.notice.textContent, /cannot start yet: cannot resolve your hub user \(HTTP 502\)/);
  assert.equal(env.els.state.className, 'state bad');
  assert.equal(env.intervals.length, 0);
  hub.parts.boot = () => ({ down: true });
  await env.runTimers();
  assert.match(env.els.notice.textContent, /cannot reach the server/);
  hub.parts.boot = () => ({ status: 200, body: bootBody() });
  await env.runTimers();
  assert.equal(env.els.agent.textContent, 'boss');
  assert.equal(env.els.text.disabled, false);
  assert.equal(env.intervals.length, 1, 'one poll, however many attempts it took');
});

test('a hidden page does not poll; a closed stream is reopened', async () => {
  const env = await load(hubWith());
  const before = env.calls.length;
  document.visibilityState = 'hidden';
  await env.poll();
  assert.equal(env.calls.length, before);
  document.visibilityState = 'visible';
  env.streams[0].close();
  await env.poll();
  assert.equal(env.streams.length, 2);
  await env.poll();
  assert.equal(env.streams.length, 2, 'an open stream is left alone');
});

test('a failed read after a good one leaves the list and shows no notice', async () => {
  const hub = hubWith({ history: () => ({ status: 200, body: { messages: [msg(1)] } }) });
  const env = await load(hub);
  hub.parts.history = () => ({ status: 502, body: 'bad gateway\n' });
  await env.poll();
  assert.equal(env.rows().length, 1);
  assert.equal(env.els.notice.hidden, true);
});

test('an older answer that arrives late does not replace a newer one', async () => {
  const hub = hubWith({ history: () => ({ status: 200, body: { messages: [msg(1, { senderId: 'u1', dispatchState: 'pending' })] } }) });
  const env = await load(hub);
  // Read A is slow and carries the old state; read B is fast and newer.
  let releaseA;
  hub.parts.history = () => new Promise((r) => (releaseA = () => r({ status: 200, body: { messages: [msg(1, { senderId: 'u1', dispatchState: 'pending' })] } })));
  env.streams[0].emit('update', { data: JSON.stringify({ subject: 'user.u1.chat.dm' }) });
  await tick(5);
  hub.parts.history = () => ({ status: 200, body: { messages: [msg(1, { senderId: 'u1', dispatchState: 'failed', dispatchFailureReason: 'agent is stopped' })] } });
  env.streams[0].emit('update', { data: JSON.stringify({ subject: 'user.u1.chat.dm' }) });
  await env.runTimers();
  assert.match(env.rows()[0], /Not delivered: agent is stopped$/);
  releaseA();
  await tick(5);
  assert.match(env.rows()[0], /Not delivered: agent is stopped$/, 'the late, older answer must be dropped');
});
