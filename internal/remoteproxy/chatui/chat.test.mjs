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
  assert.equal(env.els.terminal.attrs.href, 'https://mac.ts.net/agents/a1/terminal');
  assert.equal(env.els.console.attrs.href, 'https://mac.ts.net/agents');
  assert.equal(env.els.text.disabled, false);
  assert.equal(env.rows().length, 2);
  assert.match(env.rows()[0], /^msg mine: You .* \/ msg 1$/);
  assert.match(env.rows()[1], /^msg agent: boss .* \/ msg 2$/);
  assert.equal(env.streams.length, 1);
  assert.equal(env.streams[0].url, '/events?sub=user.u1.chat.%3E');
  assert.equal(env.els.notice.hidden, true);
  // The stream opens before the history is read, so nothing stored between
  // the two is missed.
  assert.ok(env.log.indexOf('stream') >= 0 && env.log.indexOf('stream') < env.log.findIndex((l) => l.startsWith(`GET ${HISTORY}`)), env.log.join(' | '));
});

test('network text is only ever text, and a link never leaves the origin', async () => {
  const evil = '<img src=x onerror=alert(1)><script>alert(2)</script>';
  // Each of these reads, to a browser, as a link to another host (or to no
  // path at all): after a tab is dropped or a dot segment removed, the path
  // begins with "//".
  for (const terminal of ['//evil.test/x', '/\t/evil.test/x', '/\\evil.test', '/.//evil.test/x', '/..//evil.test', '/%2e//evil.test',
    '/x/..//evil.test', '/./\\evil.test', 'https://evil.test', 'javascript:alert(1)', 'agents', '', 7, null]) {
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
    // A state line names its sender, so it cannot pass for a lever notice.
    assert.equal(env.rows()[1], `msg system: ${evil}: ${evil}`);
    assert.equal(env.els.state.textContent, 'running', 'an activity that is not a hub word is not shown');
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

test('reads run one at a time, in order, and none is lost', async () => {
  const pending = (state) => ({ status: 200, body: { messages: [msg(1, { senderId: 'u1', dispatchState: state, dispatchFailureReason: 'agent is stopped' })] } });
  const hub = hubWith({ history: () => pending('pending') });
  const env = await load(hub);
  const reads = () => env.count('GET', HISTORY);
  // Read A is slow and carries the old state. Events arrive while it runs.
  let releaseA;
  hub.parts.history = () => new Promise((r) => (releaseA = () => r(pending('pending'))));
  const event = () => env.streams[0].emit('update', { data: JSON.stringify({ subject: 'user.u1.chat.dm' }) });
  event();
  await tick(5);
  const during = reads();
  hub.parts.history = () => pending('failed');
  for (let i = 0; i < 5; i++) event();
  await env.runTimers();
  assert.equal(reads(), during, 'no second read starts while one is under way');
  // A ends; the one read that was asked for meanwhile follows, and is applied.
  releaseA();
  await tick(5);
  await env.runTimers();
  assert.equal(reads(), during + 1);
  assert.match(env.rows()[0], /Not delivered: agent is stopped$/);
});

test('a system line that is not from the agent is not given the agent\'s name', async () => {
  const env = await load(hubWith({ history: () => ({ status: 200, body: { messages: [msg(1, { type: 'state-change', senderId: '' }), msg(2, { type: 'system', senderId: 'a1' })] } }) }));
  assert.deepEqual(env.rows(), ['msg system: hub: msg 1', 'msg system: boss: msg 2']);
});

test('send: only a stored message counts as sent', async () => {
  // What the hub's login page, an empty success or a stray 2xx look like.
  for (const answer of [{ redirect: true }, { status: 200, body: '<!doctype html><title>Sign in</title>' }, { status: 204, body: '' }, { status: 200, body: {} }, { status: 200, body: { id: 7 } }, { status: 202, body: { id: 'x' } }]) {
    const env = await load(hubWith({ post: () => answer }));
    await type(env, 'do not lose me');
    env.els.composer.dispatch('submit');
    await tick(10);
    assert.equal(env.els.text.value, 'do not lose me', JSON.stringify(answer));
    assert.equal(env.store['lever-chat-draft'], 'do not lose me');
    assert.match(env.els.error.textContent, /^Not sent: /, JSON.stringify(answer));
    assert.equal(env.rows().length, 0);
  }
});

test('send: a redirect to the hub\'s login is treated as a lost session and retried once', async () => {
  let n = 0;
  const env = await load(hubWith({ post: (body) => (++n === 1 ? { redirect: true } : { status: 201, body: { id: 's', content: body.content, senderId: 'u1', createdAt: new Date().toISOString() } }) }));
  await type(env, 'after sign-in');
  env.els.composer.dispatch('submit');
  await tick(10);
  const posts = env.calls.filter((c) => c.method === 'POST');
  assert.equal(posts.length, 2);
  assert.equal(posts[0].body.idempotency_key, posts[1].body.idempotency_key);
  assert.match(env.rows()[0], /after sign-in$/);
  assert.equal(env.els.error.hidden, true);
});

test('send: the same text sent again after an unclear answer keeps its key', async () => {
  let n = 0;
  const env = await load(hubWith({ post: (body) => (++n === 1 ? { down: true } : { status: 201, body: { id: `s${n}`, content: body.content, senderId: 'u1', createdAt: new Date().toISOString() } }) }));
  await type(env, 'once only');
  env.els.composer.dispatch('submit');
  await tick(10);
  assert.equal(env.els.error.textContent, 'Not sent: cannot reach the server');
  env.els.composer.dispatch('submit');
  await tick(10);
  const posts = env.calls.filter((c) => c.method === 'POST');
  assert.equal(posts.length, 2);
  assert.equal(posts[0].body.idempotency_key, posts[1].body.idempotency_key, 'a resend of the same text must not be a second message');
  // Once it is stored, the same words sent again are a new message.
  await type(env, 'once only');
  env.els.composer.dispatch('submit');
  await tick(10);
  const again = env.calls.filter((c) => c.method === 'POST')[2];
  assert.notEqual(again.body.idempotency_key, posts[0].body.idempotency_key);
  // Another message gets its own key.
  await type(env, 'a new one');
  env.els.composer.dispatch('submit');
  await tick(10);
  const last = env.calls.filter((c) => c.method === 'POST')[3];
  assert.notEqual(last.body.idempotency_key, again.body.idempotency_key);
});

test('send: changed text after a failed send is a new message', async () => {
  let n = 0;
  const env = await load(hubWith({ post: (body) => (++n === 1 ? { down: true } : { status: 201, body: { id: `s${n}`, content: body.content, senderId: 'u1', createdAt: new Date().toISOString() } }) }));
  await type(env, 'first wording');
  env.els.composer.dispatch('submit');
  await tick(10);
  await type(env, 'second wording');
  env.els.composer.dispatch('submit');
  await tick(10);
  const posts = env.calls.filter((c) => c.method === 'POST');
  assert.notEqual(posts[0].body.idempotency_key, posts[1].body.idempotency_key);
});

test('send: a failed send reads the agent state again', async () => {
  const hub = hubWith({ post: () => ({ status: 500, body: 'boom' }) });
  const env = await load(hub);
  hub.parts.agent = () => ({ status: 200, body: { phase: 'stopped' } });
  await type(env, 'x');
  env.els.composer.dispatch('submit');
  await tick(10);
  assert.match(env.els.state.textContent, /^stopped \(not running/);
});

test('send: the manager has a new hub record: nothing is posted to the old one', async () => {
  const hub = hubWith();
  const env = await load(hub);
  hub.parts.boot = () => ({ status: 200, body: bootBody({ agent: { name: 'boss', id: 'a2' }, conversation: 'dm:agent:a2:user:u1' }) });
  await type(env, 'for the new manager');
  env.els.composer.dispatch('submit');
  await tick(10);
  assert.equal(env.count('POST', ''), 0);
  assert.equal(env.reloads, 1);
  assert.equal(env.store['lever-chat-draft'], 'for the new manager', 'the draft survives the reload');
  assert.equal(env.els.send.disabled, false);
});

test('send: the bootstrap cannot be read: nothing is posted, the draft stays', async () => {
  const hub = hubWith();
  const env = await load(hub);
  hub.parts.boot = () => ({ status: 502, body: 'cannot resolve the manager agent\n' });
  await type(env, 'hold on');
  env.els.composer.dispatch('submit');
  await tick(10);
  assert.equal(env.count('POST', ''), 0);
  assert.equal(env.els.error.textContent, 'Not sent: cannot resolve the manager agent (HTTP 502)');
  assert.equal(env.els.text.value, 'hold on');
  assert.equal(env.els.send.disabled, false);
});

test('a sent message does not hide a hole behind it', async () => {
  const hub = hubWith({ history: () => ({ status: 200, body: { messages: page(100), nextCursor: 'C1' } }) });
  const env = await load(hub);
  // 200 more arrive unseen; then the operator sends. The newest page now
  // holds the sent id, which must not count as overlap with what was shown.
  hub.parts.history = () => ({ status: 502, body: '' });
  hub.parts.post = (body) => ({ status: 201, body: msg(301, { id: 'sent', content: body.content, msg: undefined, senderId: 'u1' }) });
  await type(env, 'hello');
  env.els.composer.dispatch('submit');
  await tick(10);
  hub.parts.history = () => ({ status: 200, body: { messages: [msg(301, { id: 'sent', msg: 'hello', senderId: 'u1' }), ...page(300, 49)], nextCursor: 'C2' } });
  await env.poll();
  const rows = env.rows();
  assert.equal(rows.length, 50, 'the list restarts from the newest page');
  assert.match(rows[0], /msg 252$/);
  assert.match(rows[49], /hello$/);
});

test('a "Load earlier" answer that lands after a restart is dropped', async () => {
  const hub = hubWith({ history: () => ({ status: 200, body: { messages: page(100), nextCursor: 'C1' } }) });
  const env = await load(hub);
  let releaseOlder;
  hub.parts.history = (path) => (path.includes('cursor=C1')
    ? new Promise((r) => (releaseOlder = () => r({ status: 200, body: { messages: page(50), nextCursor: 'C0' } })))
    : { status: 200, body: { messages: page(300), nextCursor: 'C2' } });
  env.els.older.dispatch('click');
  await tick(5);
  await env.poll(); // the restart
  releaseOlder();
  await tick(5);
  assert.equal(env.els.older.disabled, false, 'the button comes back even when its answer is dropped');
  assert.equal(env.rows().length, 50);
  assert.match(env.rows()[0], /msg 251$/);
  // The cursor is the new list's, so the next "Load earlier" continues from it.
  hub.parts.history = (path) => ({ status: 200, body: { messages: path.includes('cursor=C2') ? page(250) : page(300) } });
  env.els.older.dispatch('click');
  await tick(5);
  assert.match(env.rows()[0], /msg 201$/);
});

test('Enter sends with a keyboard and mouse, and only a plain Enter', async () => {
  const env = await load(hubWith());
  const key = async (ev) => {
    let prevented = false;
    await type(env, 'typed');
    env.els.text.dispatch('keydown', { key: 'Enter', preventDefault: () => (prevented = true), ...ev });
    await tick(10);
    return prevented;
  };
  const posts = () => env.count('POST', HISTORY);
  for (const ev of [{ shiftKey: true }, { isComposing: true }, { keyCode: 229 }, { key: 'a' }]) {
    assert.equal(await key(ev), false, JSON.stringify(ev));
    assert.equal(posts(), 0, JSON.stringify(ev));
  }
  env.pointerFine = false; // a touch screen: Enter is a new line
  assert.equal(await key({}), false);
  assert.equal(posts(), 0);
  env.pointerFine = true;
  assert.equal(await key({}), true);
  assert.equal(posts(), 1);
});

test('a draft survives a reload', async () => {
  const env = await load(hubWith(), { store: { 'lever-chat-draft': 'half a thought' } });
  assert.equal(env.els.text.value, 'half a thought');
});

test('the stream coming back after an error reads the history again', async () => {
  const env = await load(hubWith());
  const reads = () => env.count('GET', HISTORY);
  await env.runTimers();
  const before = reads();
  env.streams[0].emit('open');
  await env.runTimers();
  assert.equal(reads(), before, 'a first open is no reason to read');
  env.streams[0].emit('error');
  env.streams[0].emit('open');
  await env.runTimers();
  assert.equal(reads(), before + 1);
});

test('coming back to the page reads again at once', async () => {
  const env = await load(hubWith());
  await env.runTimers();
  const before = env.count('GET', HISTORY);
  document.dispatch('visibilitychange');
  await tick(5);
  await env.runTimers();
  assert.ok(env.count('GET', HISTORY) > before);
});

test('no hub record: the fields are off even if the browser restored them on', async () => {
  const env = await load(hubWith({ boot: () => ({ status: 200, body: { login: 'op', userId: 'u1', agent: { name: 'boss', id: '' }, console: '/agents' } }) }), { fieldsEnabled: true });
  assert.equal(env.els.text.disabled, true);
  assert.equal(env.els.send.disabled, true);
});

// storingHub keeps what it is sent, as the hub does: a key it has seen gets
// the 200 replay answer. lose(n) makes the answer to post number n vanish.
function storingHub(lose) {
  const stored = [];
  let posts = 0;
  const hub = hubWith({
    post: (body) => {
      posts++;
      const known = stored.find((m) => m.key === body.idempotency_key);
      if (known) return { status: 200, body: { id: known.id, content: body.content, sender: 'user:op' } };
      stored.push({ key: body.idempotency_key, id: `s${stored.length + 1}`, text: body.content });
      if (lose(posts)) return { down: true };
      return { status: 201, body: { id: `s${stored.length}`, content: body.content, senderId: 'u1', createdAt: new Date(1.8e12 + stored.length).toISOString() } };
    },
    history: () => ({ status: 200, body: { messages: stored.map((m, i) => msg(i, { id: m.id, msg: m.text, senderId: 'u1' })).reverse() } }),
  });
  hub.stored = stored;
  return hub;
}

test('send: a message that did arrive after "Not sent" is shown as sent, and its words can be sent again', async () => {
  const hub = storingHub((n) => n === 1);
  const env = await load(hub);
  await type(env, 'yes');
  env.els.composer.dispatch('submit');
  await tick(10);
  assert.match(env.els.error.textContent, /^Not sent/);
  assert.equal(env.els.text.value, 'yes');
  // The history shows it: the page takes back the error and the draft, so
  // nothing invites a second send of the same message. It says the message
  // may be unverified: the proxy may never have seen the hub's answer.
  await env.poll();
  assert.equal(env.rows().length, 1);
  assert.match(env.els.error.textContent, /^The message did arrive\. If the manager treats it as unverified/);
  assert.equal(env.els.text.value, '');
  assert.equal(env.store['lever-chat-draft'], '');
  assert.equal(env.store['lever-chat-unsent'], undefined);
  // The same words typed now are a new answer, stored as its own message.
  await type(env, 'yes');
  env.els.composer.dispatch('submit');
  await tick(10);
  await env.runTimers();
  assert.equal(hub.stored.length, 2);
  assert.equal(env.rows().length, 2);
});

test('send: an older message with the same words does not settle a failed send', async () => {
  const hub = storingHub((n) => n === 2);
  const env = await load(hub);
  await type(env, 'yes');
  env.els.composer.dispatch('submit');
  await tick(10);
  await env.poll();
  assert.equal(env.rows().length, 1);
  // A second "yes" fails without reaching the hub.
  hub.parts.post = () => ({ down: true });
  await type(env, 'yes');
  env.els.composer.dispatch('submit');
  await tick(10);
  await env.poll();
  assert.match(env.els.error.textContent, /^Not sent/);
  assert.equal(env.els.text.value, 'yes', 'the first "yes" in the history is not this one');
});

test('send: a draft changed after a failed send is kept when the first arrives', async () => {
  const hub = storingHub((n) => n === 1);
  const env = await load(hub);
  await type(env, 'yes');
  env.els.composer.dispatch('submit');
  await tick(10);
  await type(env, 'yes, and one more thing');
  await env.poll();
  assert.equal(env.els.text.value, 'yes, and one more thing');
  assert.match(env.els.error.textContent, /^The message did arrive/);
});

test('send: a replay answer says the message may be unverified', async () => {
  const hub = storingHub((n) => n === 1);
  const env = await load(hub);
  hub.parts.history = () => ({ status: 502, body: '' }); // the page cannot see that it arrived
  await type(env, 'yes');
  env.els.composer.dispatch('submit');
  await tick(10);
  env.els.composer.dispatch('submit');
  await tick(10);
  assert.equal(hub.stored.length, 1);
  assert.match(env.els.error.textContent, /^Sent, but the first attempt had no clear answer/);
  assert.equal(env.els.text.value, '');
});

test('send: the key of an unsent message survives a reload', async () => {
  const hub = storingHub((n) => n === 1);
  const first = await load(hub);
  hub.parts.history = () => ({ status: 200, body: { messages: [] } });
  await type(first, 'yes');
  first.els.composer.dispatch('submit');
  await tick(10);
  const again = await load(hub, { store: first.store });
  assert.equal(again.els.text.value, 'yes');
  again.els.composer.dispatch('submit');
  await tick(10);
  assert.equal(hub.stored.length, 1, 'the resend after a reload must go under the first key');
  // Stored junk in its place is ignored, and removed.
  for (const junk of ['{', 'null', '7', '{"text":1,"key":2}', '{"text":"a","key":"k"}', '{"text":"a","key":"k","conversation":"dm:agent:OLD:user:u1"}']) {
    const env = await load(hubWith(), { store: { 'lever-chat-unsent': junk, 'lever-chat-draft': 'a' } });
    assert.equal(env.els.text.disabled, false, junk);
    env.els.composer.dispatch('submit');
    await tick(10);
    assert.notEqual(env.calls.find((c) => c.method === 'POST').body.idempotency_key, 'k', junk);
  }
});

test('send: a refused message sent again keeps its key', async () => {
  let n = 0;
  const env = await load(hubWith({ post: (body) => (++n === 1 ? { status: 500, body: 'boom' } : { status: 201, body: { id: 's', content: body.content, senderId: 'u1', createdAt: new Date().toISOString() } }) }));
  await type(env, 'retry me');
  env.els.composer.dispatch('submit');
  await tick(10);
  env.els.composer.dispatch('submit');
  await tick(10);
  const posts = env.calls.filter((c) => c.method === 'POST');
  assert.equal(posts[0].body.idempotency_key, posts[1].body.idempotency_key);
});

test('a request that never answers is given up on, and the page goes on', async () => {
  const hub = hubWith();
  const env = await load(hub);
  const never = () => new Promise(() => {});
  // A history read hangs: later signals must still get a read.
  hub.parts.history = never;
  await env.poll();
  const stuck = env.count('GET', HISTORY);
  await env.poll();
  assert.equal(env.count('GET', HISTORY), stuck, 'one read at a time');
  hub.parts.history = () => ({ status: 200, body: { messages: [msg(1)] } });
  await env.runTimers(Infinity); // the time limit passes
  await env.poll();
  assert.equal(env.rows().length, 1, 'reads resume after the hung one is dropped');
  // A send whose bootstrap check hangs does not lock the composer for ever.
  hub.parts.boot = never;
  await type(env, 'hello');
  env.els.composer.dispatch('submit');
  await tick(5);
  assert.equal(env.els.send.disabled, true);
  await env.runTimers(Infinity);
  assert.equal(env.els.send.disabled, false);
  assert.equal(env.els.error.textContent, 'Not sent: no answer in time');
  assert.equal(env.els.text.value, 'hello');
  // A hung "Load earlier" gives the button back too.
  hub.parts.boot = () => ({ status: 200, body: bootBody() });
  hub.parts.history = (path) => (path.includes('cursor=') ? never() : { status: 200, body: { messages: page(100), nextCursor: 'C1' } });
  await env.poll();
  env.els.older.dispatch('click');
  await tick(5);
  assert.equal(env.els.older.disabled, true);
  await env.runTimers(Infinity);
  assert.equal(env.els.older.disabled, false);
});

test('a finished request leaves no time limit behind', async () => {
  const env = await load(hubWith());
  await env.runTimers();
  assert.deepEqual(env.timers.filter((t) => t.ms > 10000), []);
});

test('a redirect on a read is never taken for an answer', async () => {
  // The front's own sign-in lapsed: every request is redirected.
  for (const part of ['boot', 'history', 'agent']) {
    for (const answer of [{ redirect: true }, { status: 302, body: '' }]) {
      const env = await load(hubWith({ [part]: () => answer }));
      if (part === 'boot') {
        assert.equal(env.els.notice.textContent, 'The chat page cannot start yet: sign-in is needed again: reload the page. Trying again.');
        assert.equal(env.els.text.disabled, true);
      } else if (part === 'history') {
        assert.match(env.els.notice.textContent, /Cannot read the conversation: sign-in is needed again: reload the page$/);
        assert.equal(env.rows().length, 0);
      } else {
        assert.equal(env.els.state.className, 'state bad');
      }
    }
  }
});

test('the bootstrap failing during a check is no reason to reload', async () => {
  const hub = hubWith();
  const env = await load(hub);
  hub.parts.agent = () => ({ status: 404, body: '' });
  for (const boot of [() => ({ status: 502, body: 'x' }), () => ({ down: true }), () => ({ redirect: true }), () => ({ status: 200, body: 'null' })]) {
    hub.parts.boot = boot;
    await env.poll();
  }
  assert.equal(env.reloads, 0);
});

test('send: a resend before the first is seen stays one message', async () => {
  const stored = [];
  const hub = hubWith({
    post: (body) => {
      const known = stored.find((m) => m.key === body.idempotency_key);
      if (known) return { status: 200, body: { id: known.id, content: body.content, sender: 'user:op' } };
      stored.push({ key: body.idempotency_key, id: `s${stored.length + 1}` });
      return { down: true };
    },
    history: () => ({ status: 200, body: { messages: [] } }),
  });
  const env = await load(hub);
  await type(env, 'yes');
  env.els.composer.dispatch('submit');
  await tick(10);
  env.els.composer.dispatch('submit');
  await tick(10);
  assert.equal(stored.length, 1, 'an immediate resend must not store the message twice');
  assert.equal(env.els.text.value, '');
});

test('send: the manager saying the same words does not settle a failed send', async () => {
  const hub = hubWith({ post: () => ({ down: true }) });
  const env = await load(hub);
  await type(env, 'yes');
  env.els.composer.dispatch('submit');
  await tick(10);
  hub.parts.history = () => ({ status: 200, body: { messages: [msg(1, { msg: 'yes', senderId: 'a1' })] } });
  await env.poll();
  assert.match(env.els.error.textContent, /^Not sent/);
  assert.equal(env.els.text.value, 'yes');
});

test('send: with no history read yet, an old message never settles a failed send', async () => {
  // The first read fails, so the page holds no ids: every message it reads
  // later would look new. A failed "yes" must not be taken for an old one.
  const hub = hubWith({ history: () => ({ status: 502, body: '' }), post: () => ({ down: true }) });
  const env = await load(hub);
  await type(env, 'yes');
  env.els.composer.dispatch('submit');
  await tick(10);
  hub.parts.history = () => ({ status: 200, body: { messages: [msg(1, { msg: 'yes', senderId: 'u1' })] } });
  await env.poll();
  assert.equal(env.rows().length, 1);
  assert.equal(env.els.error.textContent, 'Not sent: cannot reach the server');
  assert.equal(env.els.text.value, 'yes');
});

test('send: a message just sent, not yet in a history read, does not settle a second one', async () => {
  let n = 0;
  const hub = hubWith({ post: (body) => (++n === 1 ? { status: 201, body: { id: 's1', content: body.content, senderId: 'u1', createdAt: new Date(1.8e12).toISOString() } } : { down: true }) });
  const env = await load(hub);
  hub.parts.history = () => ({ status: 502, body: '' }); // s1 is known only from its 201
  await type(env, 'yes');
  env.els.composer.dispatch('submit');
  await tick(10);
  await type(env, 'yes');
  env.els.composer.dispatch('submit');
  await tick(10);
  assert.match(env.els.error.textContent, /^Not sent/);
  hub.parts.history = () => ({ status: 200, body: { messages: [msg(1, { id: 's1', msg: 'yes', senderId: 'u1' })] } });
  await env.poll();
  assert.match(env.els.error.textContent, /^Not sent/, 's1 is the first "yes", not the second');
  assert.equal(env.els.text.value, 'yes');
});

test('send: after a reload, a message that arrived is settled by the first read', async () => {
  const hub = storingHub((n) => n === 1);
  const first = await load(hub);
  const seen = hub.parts.history;
  hub.parts.history = () => ({ status: 200, body: { messages: [] } });
  await type(first, 'yes');
  first.els.composer.dispatch('submit');
  await tick(10);
  assert.match(first.els.error.textContent, /^Not sent/);
  // Reload: the record comes back with the ids it knew, so the stored
  // message in the first read is told from an old one.
  hub.parts.history = seen;
  const again = await load(hub, { store: first.store });
  assert.equal(again.rows().length, 1);
  assert.equal(again.els.text.value, '');
  assert.match(again.els.error.textContent, /^The message did arrive/);
  await type(again, 'yes');
  again.els.composer.dispatch('submit');
  await tick(10);
  assert.equal(hub.stored.length, 2, 'the same words after that are a new message');
});

test('send: a replay of a message already on screen is said, not shown as sent', async () => {
  // The page could not settle it (it had read no history when the send
  // failed), but the list shows the message when the operator presses Send.
  const hub = storingHub((n) => n === 1);
  const seen = hub.parts.history;
  hub.parts.history = () => ({ status: 502, body: '' });
  const env = await load(hub);
  await type(env, 'yes');
  env.els.composer.dispatch('submit');
  await tick(10);
  hub.parts.history = seen;
  await env.poll();
  assert.equal(env.rows().length, 1);
  assert.match(env.els.error.textContent, /^Not sent/);
  env.els.composer.dispatch('submit');
  await tick(10);
  assert.equal(hub.stored.length, 1);
  assert.match(env.els.error.textContent, /^The hub already has this message/);
  assert.equal(env.els.text.value, 'yes', 'the draft stays: the page cannot tell a retry from a second answer');
  // One more press is a deliberate second message.
  env.els.composer.dispatch('submit');
  await tick(10);
  assert.equal(hub.stored.length, 2);
  assert.equal(env.els.text.value, '');
});

test('send: a key is not reused for another conversation', async () => {
  // The hub keeps a key per user, not per conversation: reused after the
  // manager got a new record, it would answer for the message in the old one.
  const record = JSON.stringify({ text: 'yes', key: 'old-key', conversation: 'dm:agent:OLD:user:u1', known: [] });
  const env = await load(hubWith(), { store: { 'lever-chat-unsent': record, 'lever-chat-draft': 'yes' } });
  assert.equal(env.store['lever-chat-unsent'], undefined);
  env.els.composer.dispatch('submit');
  await tick(10);
  const post = env.calls.find((c) => c.method === 'POST');
  assert.notEqual(post.body.idempotency_key, 'old-key');
});
