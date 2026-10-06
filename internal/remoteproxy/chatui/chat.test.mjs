import test from 'node:test';
import assert from 'node:assert/strict';
import { load, tick } from './fakebrowser.mjs';

const KEY = 'dm:agent:a1:user:u1';
const HISTORY = `/api/v1/chat/conversations/${encodeURIComponent(KEY)}/messages`;
const msg = (i, over = {}) => ({ id: `m${String(i).padStart(3, '0')}`, msg: `msg ${i}`, senderId: 'a1', type: 'instruction', createdAt: new Date(1.7e12 + i * 1000).toISOString(), ...over });
// A newest-first page of n messages ending at number `last`, as the hub sends.
const page = (last, n = 50) => Array.from({ length: n }, (_, k) => msg(last - k));
// A list answer from /lever/api/agents, and one agent row in it.
const roster = (agents, over = {}) => ({ status: 200, body: { login: 'op', tier: 'operator', userId: 'u1', console: '/agents', agents, ...over } });
const A = (name, over = {}) => ({ name, role: 'worker', label: '', access: 'message', state: 'running', id: `${name}id`,
  conversation: `dm:agent:${name}id:user:u1`, terminal: `/agents/${name}id/terminal`, ...over });
const BOSS = (over = {}) => A('boss', { role: 'manager', activity: 'working', id: 'a1', conversation: KEY, terminal: '/agents/a1/terminal', ...over });
const defaultAgents = () => roster([BOSS(), A('w1')]);

// hubWith builds a scripted hub; each part can be replaced by a test.
function hubWith(parts = {}) {
  const h = {
    agents: defaultAgents,
    history: () => ({ status: 200, body: { messages: [], totalCount: 0 } }),
    post: (body) => ({ status: 201, body: { id: 'sent1', content: body.content, senderId: 'u1', type: 'instruction', createdAt: new Date(1.8e12).toISOString(), dispatchState: 'dispatched' } }),
    read: () => ({ status: 200, body: { status: 'ok' } }),
    wake: () => ({ status: 202, body: { state: 'starting' } }),
    contacts: () => ({ status: 200, body: { contacts: [] } }),
    transcript: () => ({ status: 200, body: { contact: 'c@x', agent: 'w1', matched: true, messages: [] } }),
    ...parts,
  };
  const fn = (method, path, body) => {
    if (path === '/lever/api/contacts') return h.contacts();
    if (path.startsWith('/lever/api/contacts/')) return h.transcript(path);
    if (path === '/lever/api/agents') return h.agents();
    if (path.startsWith('/lever/api/agents/') && path.endsWith('/wake')) return h.wake(path);
    if (method === 'POST' && path.endsWith('/read')) return h.read(body, path);
    if (method === 'POST') return h.post(body, path);
    return h.history(path);
  };
  fn.parts = h;
  return fn;
}

// loadChat starts the page with the manager's chat open, as a reload of the
// page with that chat open does.
const loadChat = (hub, opts = {}) => load(hub, { ...opts, store: { 'lever-chat-open': 'boss', ...opts.store } });
// sends: the message posts the page made (read marks are not sends).
const sends = (env) => env.calls.filter((c) => c.method === 'POST' && c.path.endsWith('/messages'));

async function type(env, text) {
  env.els.text.value = text;
  env.els.text.dispatch('input');
}

test('start: shows the agent, its state, the links and the history, oldest first', async () => {
  const env = await loadChat(hubWith({ history: () => ({ status: 200, body: { messages: [msg(2), msg(1, { senderId: 'u1' })] } }) }));
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
      agents: () => roster([BOSS({ name: evil, label: evil, activity: evil, terminal })], { console: terminal }),
      history: () => ({ status: 200, body: { messages: [msg(1, { msg: evil }), msg(2, { msg: evil, type: 'state-change' })] } }),
    }), { store: { 'lever-chat-open': evil } });
    assert.equal(env.els.terminal.hidden, true, `terminal ${String(terminal)} must not become a link`);
    assert.equal(env.els.terminal.attrs.href, undefined);
    assert.equal(env.els.console.hidden, true, `console ${String(terminal)} must not become a link`);
    // Every node the page made holds text: nothing was parsed.
    for (const row of env.els.list.children) {
      assert.equal(row.tag, 'div');
      for (const c of row.children) assert.deepEqual([c.tag, c.children.length], ['div', 0]);
    }
    for (const li of env.els.agents.children) {
      assert.equal(li.tag, 'li');
      for (const c of li.children[0].children) assert.deepEqual([c.tag, c.children.length], ['span', 0]);
    }
    assert.ok(env.rows()[0].endsWith(evil));
    // A state line names its sender, so it cannot pass for a lever notice.
    assert.equal(env.rows()[1], `msg system: ${evil}: ${evil}`);
    assert.equal(env.els.state.textContent, 'running', 'an activity that is not a page word is not shown');
    assert.equal(env.agentRows()[0], `${evil} · <img src=x onerror=alert(1)><script>alert(2)</script> | running | `);
  }
});

test('a failed first read is repaired by a later one', async () => {
  const hub = hubWith({ history: () => ({ status: 502, body: 'bad gateway\n' }) });
  const env = await loadChat(hub);
  assert.match(env.els.notice.textContent, /Cannot read the conversation: bad gateway/);
  hub.parts.history = () => ({ status: 200, body: { messages: page(100), nextCursor: 'C1' } });
  await env.poll();
  assert.equal(env.els.notice.hidden, true);
  assert.equal(env.rows().length, 50);
  assert.equal(env.els.older.hidden, false, 'Load earlier must show once a read succeeds');
});

test('an empty conversation says so, also after a failed first read', async () => {
  const hub = hubWith({ history: () => ({ status: 500, body: '' }) });
  const env = await loadChat(hub);
  hub.parts.history = () => ({ status: 200, body: { messages: [] } });
  await env.poll();
  assert.equal(env.els.notice.textContent, 'No messages yet.');
});

test('a history answer of any shape does not stop the page', async () => {
  for (const body of ['null', 'not json', '[]', '{"messages":"x"}', '{"messages":[null,7,{"id":7}]}']) {
    const env = await loadChat(hubWith({ history: () => ({ status: 200, body }) }));
    assert.equal(env.streams.length, 1, body);
    assert.equal(env.intervals.filter((i) => i.ms === 15000).length, 1, body);
    assert.equal(env.rows().length, 0, body);
  }
});

test('more than a page of new messages restarts the list instead of leaving a hole', async () => {
  const hub = hubWith({ history: () => ({ status: 200, body: { messages: page(100), nextCursor: 'C1' } }) });
  const env = await loadChat(hub);
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
  const env = await loadChat(hub);
  hub.parts.history = () => ({ status: 200, body: { messages: page(101), nextCursor: 'C1b' } });
  await env.poll();
  assert.equal(env.rows().length, 51);
  assert.match(env.rows()[0], /msg 51$/);
});

test('an event for this user reads the history again; any other does not', async () => {
  const env = await loadChat(hubWith());
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
  const env = await loadChat(hubWith());
  await type(env, '  hello  ');
  assert.equal(env.store['lever-chat-draft:boss'], '  hello  ');
  env.els.composer.dispatch('submit');
  await tick(5);
  const posts = sends(env);
  assert.equal(posts.length, 1);
  assert.equal(posts[0].path, HISTORY);
  assert.deepEqual(Object.keys(posts[0].body).sort(), ['content', 'idempotency_key']);
  assert.equal(posts[0].body.content, 'hello');
  assert.ok(posts[0].body.idempotency_key.length >= 32);
  assert.match(env.rows()[0], /^msg mine: You .* \/ hello$/);
  assert.equal(env.els.text.value, '');
  assert.equal(env.store['lever-chat-draft:boss'], '');
  assert.equal(env.els.send.disabled, false);
});

test('send: text typed while a send is under way is kept', async () => {
  let release;
  const env = await loadChat(hubWith({ post: (body) => new Promise((r) => (release = () => r({ status: 201, body: { id: 's', content: body.content, senderId: 'u1', createdAt: new Date().toISOString() } }))) }));
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
  assert.equal(env.store['lever-chat-draft:boss'], 'second thought');
});

test('send: a refusal shows the hub\'s reason and keeps the draft', async () => {
  const env = await loadChat(hubWith({ post: () => ({ status: 400, body: { error: { code: 'validation', message: 'message exceeds 16000 character limit' } } }) }));
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
  const env = await loadChat(hubWith({ post: (body) => (++n === 1 ? { status: 401, body: { error: 'authentication required' } } : { status: 201, body: { id: 's', content: body.content, senderId: 'u1', createdAt: new Date().toISOString() } }) }));
  await type(env, 'again');
  env.els.composer.dispatch('submit');
  await tick(10);
  const posts = sends(env);
  assert.equal(posts.length, 2);
  assert.equal(posts[0].body.idempotency_key, posts[1].body.idempotency_key);
  assert.equal(env.els.error.hidden, true);
  assert.match(env.rows()[0], /again$/);
});

test('send: the limit counts characters as the hub does', async () => {
  const env = await loadChat(hubWith());
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

test('a failed delivery shows under the message, its reason as one plain line', async () => {
  const env = await loadChat(hubWith({ history: () => ({ status: 200, body: { messages: [msg(1, { senderId: 'u1', dispatchState: 'failed', dispatchFailureReason: 'agent is\u202E suspended\n\nDelivered.' })] } }) }));
  assert.match(env.rows()[0], /\/ msg 1 \/ Not delivered: agent is suspended Delivered\.$/);
  const plain = await loadChat(hubWith({ history: () => ({ status: 200, body: { messages: [msg(1, { senderId: 'u1', dispatchState: 'failed', dispatchFailureReason: { no: 'text' } })] } }) }));
  assert.match(plain.rows()[0], /\/ msg 1 \/ Not delivered$/);
});

test('the open agent gets a new record: the chat moves to the new conversation, nothing goes to the old one', async () => {
  const hub = hubWith({ history: (path) => ({ status: 200, body: { messages: path.includes('a2') ? [msg(5, { senderId: 'a2', msg: 'new one' })] : [msg(1)] } }) });
  const env = await loadChat(hub);
  await type(env, 'for the new record');
  hub.parts.agents = () => roster([BOSS({ id: 'a2', conversation: 'dm:agent:a2:user:u1' })]);
  await env.poll();
  assert.equal(env.reloads, 0, 'the page never reloads itself');
  assert.ok(env.count('GET', `/api/v1/chat/conversations/${encodeURIComponent('dm:agent:a2:user:u1')}/messages`) >= 1);
  assert.deepEqual(env.rows().map((r) => r.split(' / ')[1]), ['new one']);
  assert.equal(env.els.text.value, 'for the new record', 'the draft is the agent\'s, and stays');
  env.els.composer.dispatch('submit');
  await tick(10);
  assert.equal(sends(env).length, 1);
  assert.equal(sends(env)[0].path, `/api/v1/chat/conversations/${encodeURIComponent('dm:agent:a2:user:u1')}/messages`);
});

test('no hub record at start: the composer stays off until one appears', async () => {
  const hub = hubWith({ agents: () => roster([BOSS({ state: 'no-record', id: undefined, conversation: undefined, terminal: undefined, activity: undefined })]) });
  const env = await loadChat(hub);
  assert.equal(env.els.text.disabled, true);
  assert.equal(env.els.send.disabled, true);
  assert.match(env.els.notice.textContent, /boss has no record on the hub yet/);
  assert.equal(env.els.note.textContent, 'the assistant is offline');
  assert.equal(env.els.ask.hidden, true, 'the manager is not asked to start itself');
  assert.equal(env.count('GET', '/api/v1/chat/'), 0);
  env.els.composer.dispatch('submit');
  await tick(5);
  assert.equal(sends(env).length, 0);
  hub.parts.agents = defaultAgents;
  await env.poll();
  assert.equal(env.els.text.disabled, false);
  assert.ok(env.count('GET', HISTORY) >= 1);
  assert.equal(env.reloads, 0);
});

test('the hub is away at start: the page keeps trying', async () => {
  const hub = hubWith({ agents: () => ({ status: 502, body: 'cannot resolve your hub user\n' }) });
  const env = await loadChat(hub);
  assert.match(env.els.listnote.textContent, /cannot be read yet: cannot resolve your hub user \(HTTP 502\)\. Trying again\./);
  assert.equal(env.els.listnote.hidden, false);
  assert.equal(env.intervals.length, 0);
  hub.parts.agents = () => ({ down: true });
  await env.runTimers();
  assert.match(env.els.listnote.textContent, /cannot reach the server/);
  hub.parts.agents = defaultAgents;
  await env.runTimers();
  assert.equal(env.els.listnote.hidden, true);
  assert.equal(env.els.agent.textContent, 'boss');
  assert.equal(env.els.text.disabled, false);
  assert.equal(env.intervals.filter((i) => i.ms === 15000).length, 1, 'one poll, however many attempts it took');
});

test('a list the fence refuses is not asked for again and again', async () => {
  const env = await loadChat(hubWith({ agents: () => ({ status: 403, body: { error: 'not allowed' } }) }));
  assert.match(env.els.listnote.textContent, /may not use the chat page: not allowed \(HTTP 403\)/);
  await env.runTimers();
  assert.equal(env.count('GET', '/lever/api/agents'), 1);
});

test('a hidden page does not poll; a closed stream is reopened', async () => {
  const env = await loadChat(hubWith());
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
  const env = await loadChat(hub);
  hub.parts.history = () => ({ status: 502, body: 'bad gateway\n' });
  await env.poll();
  assert.equal(env.rows().length, 1);
  assert.equal(env.els.notice.hidden, true);
});

test('reads run one at a time, in order, and none is lost', async () => {
  const pending = (state) => ({ status: 200, body: { messages: [msg(1, { senderId: 'u1', dispatchState: state, dispatchFailureReason: 'agent is stopped' })] } });
  const hub = hubWith({ history: () => pending('pending') });
  const env = await loadChat(hub);
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
  const env = await loadChat(hubWith({ history: () => ({ status: 200, body: { messages: [msg(1, { type: 'state-change', senderId: '' }), msg(2, { type: 'system', senderId: 'a1' })] } }) }));
  assert.deepEqual(env.rows(), ['msg system: hub: msg 1', 'msg system: boss: msg 2']);
});

test('send: only a stored message counts as sent', async () => {
  // What the hub's login page, an empty success or a stray 2xx look like.
  for (const answer of [{ redirect: true }, { status: 200, body: '<!doctype html><title>Sign in</title>' }, { status: 204, body: '' }, { status: 200, body: {} }, { status: 200, body: { id: 7 } }, { status: 202, body: { id: 'x' } }]) {
    const env = await loadChat(hubWith({ post: () => answer }));
    await type(env, 'do not lose me');
    env.els.composer.dispatch('submit');
    await tick(10);
    assert.equal(env.els.text.value, 'do not lose me', JSON.stringify(answer));
    assert.equal(env.store['lever-chat-draft:boss'], 'do not lose me');
    assert.match(env.els.error.textContent, /^(Not sent: |No clear answer )/, JSON.stringify(answer));
    assert.equal(env.rows().length, 0);
  }
});

test('send: a redirect to the hub\'s login is treated as a lost session and retried once', async () => {
  let n = 0;
  const env = await loadChat(hubWith({ post: (body) => (++n === 1 ? { redirect: true } : { status: 201, body: { id: 's', content: body.content, senderId: 'u1', createdAt: new Date().toISOString() } }) }));
  await type(env, 'after sign-in');
  env.els.composer.dispatch('submit');
  await tick(10);
  const posts = sends(env);
  assert.equal(posts.length, 2);
  assert.equal(posts[0].body.idempotency_key, posts[1].body.idempotency_key);
  assert.match(env.rows()[0], /after sign-in$/);
  assert.equal(env.els.error.hidden, true);
});

test('send: the same text sent again after an unclear answer keeps its key', async () => {
  let n = 0;
  const env = await loadChat(hubWith({ post: (body) => (++n === 1 ? { down: true } : { status: 201, body: { id: `s${n}`, content: body.content, senderId: 'u1', createdAt: new Date().toISOString() } }) }));
  await type(env, 'once only');
  env.els.composer.dispatch('submit');
  await tick(10);
  assert.equal(env.els.error.textContent, 'No clear answer (cannot reach the server). The message may have arrived: look at the conversation first. A repeat within a few minutes is stored only once.');
  env.els.composer.dispatch('submit');
  await tick(10);
  const posts = sends(env);
  assert.equal(posts.length, 2);
  assert.equal(posts[0].body.idempotency_key, posts[1].body.idempotency_key, 'a resend of the same text must not be a second message');
  // Once it is stored, the same words sent again are a new message.
  await type(env, 'once only');
  env.els.composer.dispatch('submit');
  await tick(10);
  const again = sends(env)[2];
  assert.notEqual(again.body.idempotency_key, posts[0].body.idempotency_key);
  // Another message gets its own key.
  await type(env, 'a new one');
  env.els.composer.dispatch('submit');
  await tick(10);
  const last = sends(env)[3];
  assert.notEqual(last.body.idempotency_key, again.body.idempotency_key);
});

test('send: changed text after a failed send is a new message', async () => {
  let n = 0;
  const env = await loadChat(hubWith({ post: (body) => (++n === 1 ? { down: true } : { status: 201, body: { id: `s${n}`, content: body.content, senderId: 'u1', createdAt: new Date().toISOString() } }) }));
  await type(env, 'first wording');
  env.els.composer.dispatch('submit');
  await tick(10);
  await type(env, 'second wording');
  env.els.composer.dispatch('submit');
  await tick(10);
  const posts = sends(env);
  assert.notEqual(posts[0].body.idempotency_key, posts[1].body.idempotency_key);
});

test('send: a failed send reads the agent state again', async () => {
  // The list as the page start and the send's own check see it, then after.
  let n = 0;
  const hub = hubWith({
    post: () => ({ status: 500, body: 'boom' }),
    agents: () => (++n <= 2 ? roster([BOSS()]) : roster([BOSS({ state: 'stopped', activity: undefined })])),
  });
  const env = await loadChat(hub);
  await type(env, 'x');
  env.els.composer.dispatch('submit');
  await tick(10);
  await env.runTimers();
  assert.ok(n >= 3, 'the list is read again after the failure');
  assert.equal(env.els.state.textContent, 'stopped');
  assert.equal(env.els.note.textContent, 'the assistant is offline');
  assert.equal(env.els.text.disabled, true);
});
test('send: the agent has a new hub record: nothing is posted to the old one', async () => {
  const hub = hubWith();
  const env = await loadChat(hub);
  hub.parts.agents = () => roster([BOSS({ id: 'a2', conversation: 'dm:agent:a2:user:u1' })]);
  await type(env, 'for the new manager');
  env.els.composer.dispatch('submit');
  await tick(10);
  assert.equal(sends(env).length, 0);
  assert.equal(env.reloads, 0);
  assert.match(env.els.error.textContent, /^Not sent: boss has a new hub record, and the chat now shows it\./);
  assert.equal(env.els.text.value, 'for the new manager', 'the draft stays');
  assert.equal(env.store['lever-chat-draft:boss'], 'for the new manager');
  assert.equal(env.els.send.disabled, false);
  // The next press goes to the new conversation.
  env.els.composer.dispatch('submit');
  await tick(10);
  assert.equal(sends(env)[0].path, `/api/v1/chat/conversations/${encodeURIComponent('dm:agent:a2:user:u1')}/messages`);
});

test('send: the list cannot be read: nothing is posted, the draft stays', async () => {
  const hub = hubWith();
  const env = await loadChat(hub);
  hub.parts.agents = () => ({ status: 502, body: 'cannot resolve your hub user\n' });
  await type(env, 'hold on');
  env.els.composer.dispatch('submit');
  await tick(10);
  assert.equal(sends(env).length, 0);
  assert.equal(env.els.error.textContent, 'Not sent: cannot resolve your hub user (HTTP 502)');
  assert.equal(env.els.text.value, 'hold on');
  assert.equal(env.els.send.disabled, false);
});

test('a sent message does not hide a hole behind it', async () => {
  const hub = hubWith({ history: () => ({ status: 200, body: { messages: page(100), nextCursor: 'C1' } }) });
  const env = await loadChat(hub);
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
  const env = await loadChat(hub);
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
  const env = await loadChat(hubWith());
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
  const env = await loadChat(hubWith(), { store: { 'lever-chat-draft:boss': 'half a thought' } });
  assert.equal(env.els.text.value, 'half a thought');
});

test('the stream coming back after an error reads the history again', async () => {
  const env = await loadChat(hubWith());
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
  const env = await loadChat(hubWith());
  await env.runTimers();
  const before = env.count('GET', HISTORY);
  document.dispatch('visibilitychange');
  await tick(5);
  await env.runTimers();
  assert.ok(env.count('GET', HISTORY) > before);
});

test('no hub record: the fields are off even if the browser restored them on', async () => {
  const env = await loadChat(hubWith({ agents: () => roster([BOSS({ state: 'no-record', id: undefined, conversation: undefined })]) }), { fieldsEnabled: true });
  assert.equal(env.els.text.disabled, true);
  assert.equal(env.els.send.disabled, true);
});

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

test('send: a stored record that is junk, or for another conversation, is ignored and removed', async () => {
  for (const junk of ['{', 'null', '7', '[]', '{"text":1,"key":2}', '{"text":"a","key":"k"}', '{"text":"a","key":"k","conversation":"dm:agent:OLD:user:u1"}',
    '{"text":"a","key":"k","conversation":7}', '{"__proto__":{"text":"a","key":"k"}}']) {
    const env = await loadChat(hubWith(), { store: { 'lever-chat-unsent:boss': junk, 'lever-chat-draft:boss': 'a' } });
    assert.equal(env.els.text.disabled, false, junk);
    env.els.composer.dispatch('submit');
    await tick(10);
    assert.notEqual(sends(env)[0].body.idempotency_key, 'k', junk);
  }
  // A good one is used.
  const good = JSON.stringify({ text: 'a', key: 'k', conversation: KEY });
  const env = await loadChat(hubWith(), { store: { 'lever-chat-unsent:boss': good, 'lever-chat-draft:boss': 'a' } });
  env.els.composer.dispatch('submit');
  await tick(10);
  assert.equal(sends(env)[0].body.idempotency_key, 'k');
});

test('send: a refused message sent again keeps its key', async () => {
  let n = 0;
  const env = await loadChat(hubWith({ post: (body) => (++n === 1 ? { status: 500, body: 'boom' } : { status: 201, body: { id: 's', content: body.content, senderId: 'u1', createdAt: new Date().toISOString() } }) }));
  await type(env, 'retry me');
  env.els.composer.dispatch('submit');
  await tick(10);
  env.els.composer.dispatch('submit');
  await tick(10);
  const posts = sends(env);
  assert.equal(posts[0].body.idempotency_key, posts[1].body.idempotency_key);
});

test('a request that never answers is given up on, and the page goes on', async () => {
  const hub = hubWith();
  const env = await loadChat(hub);
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
  // A send whose list check hangs does not lock the composer for ever.
  hub.parts.agents = never;
  await type(env, 'hello');
  env.els.composer.dispatch('submit');
  await tick(5);
  assert.equal(env.els.send.disabled, true);
  await env.runTimers(Infinity);
  assert.equal(env.els.send.disabled, false);
  assert.equal(env.els.error.textContent, 'Not sent: no answer in time');
  assert.equal(env.els.text.value, 'hello');
  // A hung "Load earlier" gives the button back too.
  hub.parts.agents = defaultAgents;
  hub.parts.history = (path) => (path.includes('cursor=') ? never() : { status: 200, body: { messages: page(100), nextCursor: 'C1' } });
  await env.poll();
  env.els.older.dispatch('click');
  await tick(5);
  assert.equal(env.els.older.disabled, true);
  await env.runTimers(Infinity);
  assert.equal(env.els.older.disabled, false);
});

test('a finished request leaves no time limit behind', async () => {
  const env = await loadChat(hubWith());
  await env.runTimers();
  assert.deepEqual(env.timers.filter((t) => t.ms > 10000), []);
});

test('a redirect on a read is never taken for an answer', async () => {
  // The front's own sign-in lapsed: every request is redirected.
  for (const part of ['agents', 'history']) {
    for (const answer of [{ redirect: true }, { status: 302, body: '' }]) {
      const env = await loadChat(hubWith({ [part]: () => answer }));
      if (part === 'agents') {
        assert.equal(env.els.listnote.textContent, 'The agent list cannot be read yet: sign-in is needed again: reload the page. Trying again.');
        assert.equal(env.els.text.disabled, true);
      } else {
        assert.match(env.els.notice.textContent, /Cannot read the conversation: sign-in is needed again: reload the page$/);
        assert.equal(env.rows().length, 0);
      }
    }
  }
});

test('the list failing after a good read: rows stay, chips read unknown, inputs off, retry', async () => {
  const hub = hubWith({ history: () => ({ status: 200, body: { messages: [msg(1)] } }) });
  const env = await loadChat(hub);
  for (const fail of [() => ({ status: 502, body: 'x' }), () => ({ down: true }), () => ({ redirect: true }), () => ({ status: 200, body: 'null' })]) {
    hub.parts.agents = fail;
    await env.poll();
    assert.deepEqual(env.agentRows(), ['boss | unknown | ', 'w1 | unknown | ']);
    assert.equal(env.els.text.disabled, true);
    assert.equal(env.els.send.disabled, true);
    assert.equal(env.els.note.textContent, 'state unknown – retrying');
    assert.match(env.els.listnote.textContent, /^The agent list cannot be read: .*\. Trying again\.$/);
    assert.equal(env.rows().length, 1, 'the conversation stays');
  }
  assert.equal(env.reloads, 0);
  hub.parts.agents = defaultAgents;
  await env.poll();
  assert.deepEqual(env.agentRows(), ['boss | working | ', 'w1 | running | ']);
  assert.equal(env.els.text.disabled, false);
  assert.equal(env.els.listnote.hidden, true);
});

test('send: a key is not reused for another conversation', async () => {
  // The hub keeps a key per user, not per conversation: reused after the
  // manager got a new record, it would answer for the message in the old one.
  const record = JSON.stringify({ text: 'yes', key: 'old-key', conversation: 'dm:agent:OLD:user:u1', known: [] });
  const env = await loadChat(hubWith(), { store: { 'lever-chat-unsent:boss': record, 'lever-chat-draft:boss': 'yes' } });
  assert.equal(env.store['lever-chat-unsent:boss'], undefined);
  env.els.composer.dispatch('submit');
  await tick(10);
  const post = sends(env)[0];
  assert.notEqual(post.body.idempotency_key, 'old-key');
});

const UNCLEAR = /^No clear answer \(.*\)\. The message may have arrived: look at the conversation first\. A repeat within a few minutes is stored only once\.$/;
const STORED_AGAIN = 'Stored now. An earlier attempt had no clear answer: if the message shows twice above, both arrived.';
const REPLAY = /^The earlier attempt did arrive: the message is in the conversation, and nothing new was stored\./;

test('send: an answer that leaves the outcome open says so; a refusal says not sent', async () => {
  for (const [answer, want] of [
    [{ down: true }, UNCLEAR],
    [{ status: 502, body: 'bad gateway\n' }, UNCLEAR],
    [{ status: 504, body: '' }, UNCLEAR],
    [{ redirect: true }, UNCLEAR],
    [{ status: 400, body: { error: { message: 'too long' } } }, /^Not sent: too long \(HTTP 400\)$/],
    [{ status: 403, body: { error: { message: 'forbidden' } } }, /^Not sent: forbidden \(HTTP 403\)$/],
    [{ status: 429, body: { error: { message: 'slow down' } } }, /^Not sent: slow down \(HTTP 429\)$/],
  ]) {
    const env = await loadChat(hubWith({ post: () => answer }));
    await type(env, 'yes');
    env.els.composer.dispatch('submit');
    await tick(10);
    assert.match(env.els.error.textContent, want, JSON.stringify(answer));
    assert.equal(env.els.text.value, 'yes', 'the draft always stays without a stored message');
  }
});

test('send: stored but the answer was lost: the retry is told so, and nothing is stored twice', async () => {
  const hub = storingHub((n) => n === 1);
  const env = await loadChat(hub);
  await type(env, 'yes');
  env.els.composer.dispatch('submit');
  await tick(10);
  assert.match(env.els.error.textContent, UNCLEAR);
  env.els.composer.dispatch('submit');
  await tick(10);
  assert.equal(hub.stored.length, 1);
  assert.match(env.els.error.textContent, REPLAY);
  assert.match(env.els.error.textContent, /If the agent treats the message as unverified, send it in other words\.$/);
  assert.equal(env.els.text.value, 'yes', 'the draft stays: only the operator knows if this was a retry');
  assert.equal(env.store['lever-chat-unsent:boss'], undefined);
  // Send rests after the note: a double tap is not the deliberate press.
  assert.equal(env.els.send.disabled, true);
  env.els.composer.dispatch('submit');
  env.els.text.dispatch('keydown', { key: 'Enter' });
  await tick(10);
  assert.equal(hub.stored.length, 1);
  assert.match(env.els.error.textContent, REPLAY, 'the note is still there to read');
  await env.runTimers();
  assert.equal(env.els.send.disabled, false);
  // The row comes from the history, never from the replay answer's thin body.
  assert.deepEqual(env.rows().map((r) => r.split(':')[0]), ['msg mine']);
  // One more press is a deliberate second message.
  env.els.composer.dispatch('submit');
  await tick(10);
  assert.equal(hub.stored.length, 2);
  assert.equal(env.els.text.value, '');
  assert.equal(env.els.error.hidden, true);
});

test('send: not stored the first time: the retry stores it once and clears the draft', async () => {
  let n = 0;
  const env = await loadChat(hubWith({ post: (body) => (++n === 1 ? { status: 502, body: '' } : { status: 201, body: { id: 's', content: body.content, senderId: 'u1', createdAt: new Date().toISOString() } }) }));
  await type(env, 'yes');
  env.els.composer.dispatch('submit');
  await tick(10);
  env.els.composer.dispatch('submit');
  await tick(10);
  assert.equal(env.els.text.value, '');
  assert.equal(env.rows().length, 1);
  // The hub did not know the key. Either the first attempt never arrived, or
  // it did and the hub has forgotten: the page cannot tell, and says so.
  assert.equal(env.els.error.textContent, STORED_AGAIN);
  assert.equal(env.store['lever-chat-unsent:boss'], undefined);
});

test('send: the history never settles a send: only the hub\'s answer to the key does', async () => {
  // Whatever the history shows after a failed send, the page concludes
  // nothing: the error and the draft stay. Each of these once looked like
  // "it arrived" to a rule that guessed, and none of them is the failed send.
  const own = (over) => msg(1, { msg: 'yes', senderId: 'u1', ...over });
  for (const [name, firstRead, laterRead] of [
    ['an old message with the same words, first read failed', { status: 502, body: '' }, { status: 200, body: { messages: [own()] } }],
    ['a first read that was not a history at all', { status: 200, body: '<html>front</html>' }, { status: 200, body: { messages: [own()] } }],
    ['the same words from another tab', { status: 200, body: { messages: [] } }, { status: 200, body: { messages: [own({ id: 'other-tab' })] } }],
    ['the hub\'s hidden mention copy', { status: 200, body: { messages: [] } }, { status: 200, body: { messages: [own({ type: 'mention' })] } }],
    ['a row with no id', { status: 200, body: { messages: [] } }, { status: 200, body: { messages: [own({ id: undefined })] } }],
    ['the manager saying the same words', { status: 200, body: { messages: [] } }, { status: 200, body: { messages: [own({ senderId: 'a1' })] } }],
  ]) {
    const hub = hubWith({ history: () => firstRead, post: () => ({ down: true }) });
    const env = await loadChat(hub);
    await type(env, 'yes');
    env.els.composer.dispatch('submit');
    await tick(10);
    hub.parts.history = () => laterRead;
    await env.poll();
    await env.poll();
    assert.match(env.els.error.textContent, UNCLEAR, name);
    assert.equal(env.els.text.value, 'yes', name);
    assert.ok(env.store['lever-chat-unsent:boss'], name);
  }
});

test('send: a message stored a moment ago does not answer for the next one', async () => {
  // "yes" is stored (201). A second "yes" fails without reaching the hub. It
  // gets its own key, so the retry stores it: two messages, as sent.
  const hub = storingHub((n) => false);
  const env = await loadChat(hub);
  await type(env, 'yes');
  env.els.composer.dispatch('submit');
  await tick(10);
  const store = hub.parts.post;
  hub.parts.post = () => ({ down: true });
  await type(env, 'yes');
  env.els.composer.dispatch('submit');
  await tick(10);
  assert.match(env.els.error.textContent, UNCLEAR);
  hub.parts.post = store;
  env.els.composer.dispatch('submit');
  await tick(10);
  assert.equal(hub.stored.length, 2);
  assert.equal(env.els.text.value, '');
});

test('send: after a reload the retry still goes under the first key', async () => {
  const hub = storingHub((n) => n === 1);
  const first = await loadChat(hub);
  await type(first, 'yes');
  first.els.composer.dispatch('submit');
  await tick(10);
  const again = await loadChat(hub, { store: first.store });
  assert.equal(again.els.text.value, 'yes');
  assert.equal(again.rows().length, 1, 'the stored message shows; the page still concludes nothing');
  assert.equal(again.els.error.hidden, true);
  again.els.composer.dispatch('submit');
  await tick(10);
  assert.equal(hub.stored.length, 1);
  assert.match(again.els.error.textContent, REPLAY);
});

test('send: a reload while the post is under way keeps the key with the draft', async () => {
  // The hub stores the message; the page reloads before the answer. The
  // draft comes back, and it must come back with its key, or Send stores a
  // second copy without a word.
  const hub = storingHub(() => false);
  let release;
  const store = hub.parts.post;
  hub.parts.post = (body) => {
    const answer = store(body);
    return new Promise((r) => (release = () => r(answer)));
  };
  const first = await loadChat(hub);
  await type(first, 'yes');
  first.els.composer.dispatch('submit');
  await tick(10);
  assert.equal(hub.stored.length, 1);
  assert.ok(first.store['lever-chat-unsent:boss'], 'the record is written before the post');
  hub.parts.post = store;
  const again = await loadChat(hub, { store: { ...first.store } });
  assert.equal(again.els.text.value, 'yes');
  again.els.composer.dispatch('submit');
  await tick(10);
  assert.equal(hub.stored.length, 1, 'the same key: the hub answers with the message it has');
  assert.match(again.els.error.textContent, REPLAY);
  release();
});

test('send: the hub forgot the key: the second copy is stored, and said', async () => {
  // An unclear answer, the message did arrive, and by the retry the hub no
  // longer knows the key (it keeps keys in memory for a few minutes).
  let n = 0;
  const env = await loadChat(hubWith({ post: (body) => (++n === 1 ? { status: 504, body: '' } : { status: 201, body: { id: `s${n}`, content: body.content, senderId: 'u1', createdAt: new Date().toISOString() } }) }));
  await type(env, 'yes');
  env.els.composer.dispatch('submit');
  await tick(10);
  env.els.composer.dispatch('submit');
  await tick(10);
  assert.equal(env.els.error.textContent, STORED_AGAIN);
  assert.equal(env.els.text.value, '');
  // A first send that is simply stored says nothing.
  await type(env, 'next');
  env.els.composer.dispatch('submit');
  await tick(10);
  assert.equal(env.els.error.hidden, true);
});

test('send: a retry the hub refuses does not say the earlier attempt failed', async () => {
  let n = 0;
  const env = await loadChat(hubWith({ post: () => (++n === 1 ? { down: true } : { status: 429, body: { error: { message: 'slow down' } } }) }));
  await type(env, 'yes');
  env.els.composer.dispatch('submit');
  await tick(10);
  env.els.composer.dispatch('submit');
  await tick(10);
  assert.equal(env.els.error.textContent, 'This attempt was refused (slow down (HTTP 429)). An earlier attempt had no clear answer and may have arrived: look at the conversation.');
  assert.equal(env.els.text.value, 'yes');
  assert.ok(env.store['lever-chat-unsent:boss'], 'the key stays for the next press');
});

test('send: a first attempt the hub refuses is done with: its key is not kept', async () => {
  const env = await loadChat(hubWith({ post: () => ({ status: 400, body: { error: { message: 'no' } } }) }));
  await type(env, 'yes');
  env.els.composer.dispatch('submit');
  await tick(10);
  assert.equal(env.els.error.textContent, 'Not sent: no (HTTP 400)');
  assert.equal(env.store['lever-chat-unsent:boss'], undefined);
});

test('send: a success that is no stored message leaves the outcome open and keeps the key', async () => {
  // The hub answers 200 only to a key it has seen. A first attempt that gets
  // one was answered by something else (a front that replayed the post, a
  // cache): the hub may hold the message. Never "the earlier attempt did
  // arrive", never "not sent", and the other party's body is not shown.
  let n = 0;
  const hub = hubWith({ post: (body) => (++n === 1 ? { status: 200, body: { id: 'x', content: '<b>front page</b>' } } : { status: 200, body: { id: 'x', content: body.content, sender: 'user:op' } }) });
  const env = await loadChat(hub);
  await type(env, 'yes');
  env.els.composer.dispatch('submit');
  await tick(10);
  assert.equal(env.els.error.textContent, 'No clear answer (unexpected answer (HTTP 200)). The message may have arrived: look at the conversation first. A repeat within a few minutes is stored only once.');
  assert.equal(env.els.text.value, 'yes');
  assert.equal(env.rows().length, 0);
  assert.ok(env.store['lever-chat-unsent:boss']);
  // The retry goes under the same key, and the hub's replay is told.
  env.els.composer.dispatch('submit');
  await tick(10);
  const posts = sends(env);
  assert.equal(posts[0].body.idempotency_key, posts[1].body.idempotency_key);
  assert.match(env.els.error.textContent, REPLAY);
});

test('send: a retry answered with a success that is no message does not show its body', async () => {
  let n = 0;
  const env = await loadChat(hubWith({ post: () => (++n === 1 ? { down: true } : { status: 200, body: '<html>Sent. All good.</html>' }) }));
  await type(env, 'yes');
  env.els.composer.dispatch('submit');
  await tick(10);
  env.els.composer.dispatch('submit');
  await tick(10);
  assert.match(env.els.error.textContent, /^No clear answer \(unexpected answer \(HTTP 200\)\)\./);
  assert.equal(env.els.text.value, 'yes');
});

test('send: a replay after two attempts with no clear answer says there may be two copies', async () => {
  // Attempt 1 arrives, answer lost. The hub forgets the key. Attempt 2
  // arrives, answer lost. Attempt 3 gets the replay of attempt 2.
  let n = 0;
  const env = await loadChat(hubWith({ post: (body) => (++n < 3 ? { status: 504, body: '' } : { status: 200, body: { id: 's2', content: body.content, sender: 'user:op' } }) }));
  await type(env, 'yes');
  for (let i = 0; i < 3; i++) {
    env.els.composer.dispatch('submit');
    await tick(10);
  }
  assert.match(env.els.error.textContent, REPLAY);
  assert.match(env.els.error.textContent, / If it shows twice above, two attempts arrived\.$/);
  // A refused attempt did not arrive: it does not count toward "twice".
  let r = 0;
  const refused = await loadChat(hubWith({ post: (body) => (++r === 1 ? { down: true } : r === 2 ? { status: 400, body: 'no' } : { status: 200, body: { id: 's1', content: body.content, sender: 'user:op' } }) }));
  await type(refused, 'yes');
  for (let i = 0; i < 3; i++) {
    refused.els.composer.dispatch('submit');
    await tick(10);
  }
  assert.match(refused.els.error.textContent, REPLAY);
  assert.doesNotMatch(refused.els.error.textContent, /shows twice/);
  // After one unclear attempt the replay is of that attempt: one copy.
  let m = 0;
  const one = await loadChat(hubWith({ post: (body) => (++m < 2 ? { status: 504, body: '' } : { status: 200, body: { id: 's1', content: body.content, sender: 'user:op' } }) }));
  await type(one, 'yes');
  for (let i = 0; i < 2; i++) {
    one.els.composer.dispatch('submit');
    await tick(10);
  }
  assert.match(one.els.error.textContent, REPLAY);
  assert.doesNotMatch(one.els.error.textContent, /shows twice/);
  // The count survives a reload with the record.
  const stored = JSON.stringify({ text: 'yes', key: 'k', conversation: KEY, tries: 2 });
  const again = await loadChat(hubWith({ post: (body) => ({ status: 200, body: { id: 's2', content: body.content, sender: 'user:op' } }) }), { store: { 'lever-chat-unsent:boss': stored, 'lever-chat-draft:boss': 'yes' } });
  again.els.composer.dispatch('submit');
  await tick(10);
  assert.match(again.els.error.textContent, /shows twice/);
});

test('a held Enter is one send', async () => {
  const env = await loadChat(hubWith());
  await type(env, 'once');
  let prevented = 0;
  for (let i = 0; i < 5; i++) env.els.text.dispatch('keydown', { key: 'Enter', repeat: i > 0, preventDefault: () => prevented++ });
  await tick(10);
  assert.equal(env.count('POST', HISTORY), 1);
  assert.equal(prevented, 5, 'the repeats add no new lines to the draft either');
  await type(env, 'twice');
  env.els.text.dispatch('keydown', { key: 'Enter', repeat: true });
  await tick(10);
  assert.equal(env.count('POST', HISTORY), 1, 'a repeat alone sends nothing');
});

// The agent list (multi-agent page).

test('start shows the list in the server\'s order with chips and badges', async () => {
  const env = await load(hubWith({ agents: () => roster([BOSS({ activity: 'waiting', unread: 2 }), A('w1', { state: 'suspended', label: 'Via Roma 12' }), A('w2', { access: 'see', state: 'stopped' })]) }));
  assert.deepEqual(env.agentRows(), ['boss | waiting | 2', 'w1 · Via Roma 12 | asleep | ', 'w2 | stopped |  | view only']);
  assert.equal(env.els.agents.children[2].className, 'view');
  assert.equal(env.els.console.hidden, false);
  assert.equal(env.els.console.attrs.href, 'https://mac.ts.net/agents');
  assert.equal(env.document.body.classList.contains('chatting'), false);
  assert.equal(env.els.composer.hidden, true);
  assert.equal(env.count('GET', '/api/v1/chat/conversations/'), 0, 'nothing opened yet');
  assert.equal(env.streams.length, 1, 'one stream for the page');
  assert.equal(env.streams[0].url, '/events?sub=user.u1.chat.%3E');
});

test('a tap opens the chat: name, label, state, history', async () => {
  const env = await load(hubWith({ agents: () => roster([BOSS(), A('w1', { label: 'Via Roma 12' })]), history: () => ({ status: 200, body: { messages: [msg(1, { senderId: 'w1id' })] } }) }));
  env.els.agents.children[1].children[0].dispatch('click');
  await tick(5);
  assert.equal(env.document.body.classList.contains('chatting'), true);
  assert.equal(env.els.agent.textContent, 'w1');
  assert.equal(env.els.label.textContent, 'Via Roma 12');
  assert.equal(env.els.label.hidden, false);
  assert.equal(env.els.state.textContent, 'running');
  assert.equal(env.els.terminal.attrs.href, 'https://mac.ts.net/agents/w1id/terminal');
  assert.equal(env.count('GET', `/api/v1/chat/conversations/${encodeURIComponent('dm:agent:w1id:user:u1')}/messages`), 1);
  assert.match(env.rows()[0], /^msg agent: w1 .* \/ msg 1$/);
  assert.equal(env.store['lever-chat-open'], 'w1');
  assert.equal(env.els.agents.children[1].className, 'current');
});

test('a contact gets no console and no terminal link', async () => {
  const env = await load(hubWith({ agents: () => roster([A('w1', { terminal: '/agents/w1id/terminal' })], { tier: 'contact', console: '/agents' }) }), { store: { 'lever-chat-open': 'w1' } });
  // Even links in the answer are not used for a contact.
  assert.equal(env.els.console.hidden, true);
  assert.equal(env.els.terminal.hidden, true);
  assert.equal(env.els.console.attrs.href, undefined);
  assert.equal(env.els.terminal.attrs.href, undefined);
  assert.equal(env.els.text.disabled, false);
});

test('a see-only agent shows name, label and state; no history, no input', async () => {
  const env = await load(hubWith({ agents: () => roster([A('w2', { access: 'see', state: 'running', label: 'scout', id: 'leak', conversation: 'dm:agent:leak:user:u1' })]) }));
  env.els.agents.children[0].children[0].dispatch('click');
  await tick(5);
  assert.equal(env.els.agent.textContent, 'w2');
  assert.equal(env.els.label.textContent, 'scout');
  assert.equal(env.els.state.textContent, 'running');
  assert.equal(env.els.viewonly.hidden, false);
  assert.equal(env.els.composer.hidden, true);
  assert.equal(env.els.terminal.hidden, true);
  assert.equal(env.count('GET', '/api/v1/chat/conversations/'), 0);
  assert.equal(env.streams.length, 1, 'the one page stream, nothing for this agent');
  await env.poll();
  assert.equal(env.count('GET', '/api/v1/chat/conversations/'), 0, 'nor on a poll');
  assert.equal(sends(env).length, 0);
});

test('a hostile label is text', async () => {
  const evil = '<img src=x onerror=alert(1)>';
  const env = await load(hubWith({ agents: () => roster([A('w1', { label: `${evil}‮\n${'x'.repeat(100)}` })]) }), { store: { 'lever-chat-open': 'w1' } });
  const title = env.agentRows()[0].split(' | ')[0];
  assert.ok(title.startsWith(`w1 · ${evil}`));
  assert.equal([...title].length, 'w1 · '.length + 60, 'a label is cut to 60 characters');
  assert.equal(/[‮\n]/.test(title), false);
  assert.equal(env.els.agent.textContent, 'w1', 'the name comes first, by itself');
  // FakeNode has no markup sink: textContent is all there is.
});

test('the state table drives the composer', async () => {
  for (const [state, disabled, note, ask] of [
    ['running', false, '', false],
    ['starting', false, 'starting – your message waits until it runs', false],
    ['suspended', false, 'asleep – your message wakes it', false],
    ['stopped', false, 'asleep – your message wakes it', false],
    ['error', true, '', true],
    ['no-record', true, '', true],
    ['not-fresh', true, '', true],
    ['unknown', true, 'state unknown – retrying', false],
  ]) {
    const env = await load(hubWith({ agents: () => roster([BOSS(), A('w1', { state })]) }), { store: { 'lever-chat-open': 'w1' } });
    assert.equal(env.els.text.disabled, disabled, state);
    assert.equal(env.els.send.disabled, disabled, state);
    assert.equal(env.els.note.textContent, note, state);
    assert.equal(env.els.note.hidden, !note, state);
    assert.equal(env.els.ask.hidden, !ask, state);
    assert.equal(env.els.state.className, disabled ? 'state bad' : 'state ok', state);
  }
  for (const state of ['suspended', 'stopped', 'error', 'no-record', 'not-fresh']) {
    const env = await loadChat(hubWith({ agents: () => roster([BOSS({ state, activity: undefined })]) }));
    assert.equal(env.els.text.disabled, true, `manager ${state}`);
    assert.equal(env.els.note.textContent, 'the assistant is offline', `manager ${state}`);
  }
});

test('a contact sees a stopped worker with input off and the ask button; an operator may wake it', async () => {
  const contact = (agents) => roster(agents, { login: 'c', tier: 'contact', console: undefined });
  let env = await load(hubWith({ agents: () => contact([BOSS({ terminal: undefined }), A('w1', { state: 'stopped', terminal: undefined })]) }), { store: { 'lever-chat-open': 'w1' } });
  assert.equal(env.els.text.disabled, true);
  assert.equal(env.els.ask.hidden, false);
  assert.equal(env.els.ask.textContent, 'Ask the manager to start w1');
  env = await load(hubWith({ agents: () => contact([BOSS({ terminal: undefined }), A('w1', { state: 'suspended', terminal: undefined })]) }), { store: { 'lever-chat-open': 'w1' } });
  assert.equal(env.els.text.disabled, false);
  assert.equal(env.els.note.textContent, 'asleep – your message wakes it');
  env = await load(hubWith({ agents: () => roster([BOSS(), A('w1', { state: 'stopped' })]) }), { store: { 'lever-chat-open': 'w1' } });
  assert.equal(env.els.text.disabled, false);
  assert.equal(env.els.ask.hidden, true);
});

test('Ask the manager opens the manager chat with an editable draft, sending nothing', async () => {
  const env = await load(hubWith({ agents: () => roster([BOSS(), A('deal-3', { state: 'no-record', id: undefined, conversation: undefined })]) }), { store: { 'lever-chat-open': 'deal-3' } });
  assert.equal(env.els.ask.hidden, false);
  assert.equal(env.els.ask.textContent, 'Ask the manager to start deal-3');
  env.els.ask.dispatch('click');
  await tick(5);
  assert.equal(env.els.agent.textContent, 'boss');
  assert.equal(env.els.text.value, 'Please start deal-3 for me.');
  assert.equal(env.store['lever-chat-draft:boss'], 'Please start deal-3 for me.');
  assert.equal(sends(env).length, 0);
  // A draft the manager chat already had is kept; the ask goes after it.
  const kept = await load(hubWith({ agents: () => roster([BOSS(), A('deal-3', { state: 'error' })]) }), { store: { 'lever-chat-open': 'deal-3', 'lever-chat-draft:boss': 'hello' } });
  kept.els.ask.dispatch('click');
  await tick(5);
  assert.equal(kept.els.text.value, 'hello\nPlease start deal-3 for me.');
});

test('a contact who may not message the manager gets no ask button', async () => {
  const env = await load(hubWith({ agents: () => roster([A('deal-3', { state: 'no-record', id: undefined, conversation: undefined })], { tier: 'contact', console: undefined }) }), { store: { 'lever-chat-open': 'deal-3' } });
  assert.equal(env.els.ask.hidden, true);
  assert.equal(env.els.text.disabled, true);
  assert.equal(env.els.state.textContent, 'no record');
});

test('opening a chat marks it read and the badge drops on the next list', async () => {
  let unread = 2;
  const env = await load(hubWith({
    agents: () => roster([BOSS({ unread })]),
    history: () => ({ status: 200, body: { messages: [msg(2), msg(1)] } }),
    read: () => {
      unread = 0;
      return { status: 200, body: { status: 'ok' } };
    },
  }), { store: { 'lever-chat-open': 'boss' } });
  const marks = () => env.calls.filter((c) => c.method === 'POST' && c.path.endsWith('/read'));
  assert.equal(marks().length, 1);
  assert.equal(marks()[0].path, `/api/v1/chat/conversations/${encodeURIComponent(KEY)}/read`);
  assert.deepEqual(marks()[0].body, { messageId: 'm002' });
  await env.poll();
  assert.equal(env.agentRows()[0], 'boss | working | ');
  assert.equal(marks().length, 1, 'the same newest message is not marked twice');
});

test('a hidden page marks nothing read', async () => {
  const hub = hubWith({ history: () => ({ status: 200, body: { messages: [msg(1)] } }) });
  const env = await loadChat(hub);
  const marks = () => env.calls.filter((c) => c.method === 'POST' && c.path.endsWith('/read')).length;
  assert.equal(marks(), 1);
  hub.parts.history = () => ({ status: 200, body: { messages: [msg(2), msg(1)] } });
  document.visibilityState = 'hidden';
  env.streams[0].emit('update', { data: JSON.stringify({ subject: 'user.u1.chat.dm' }) });
  await env.runTimers();
  assert.equal(marks(), 1);
});

test('a chat event reloads the list and the open history', async () => {
  const env = await loadChat(hubWith());
  await env.runTimers();
  const lists = env.count('GET', '/lever/api/agents');
  const reads = env.count('GET', HISTORY);
  env.streams[0].emit('update', { data: JSON.stringify({ subject: 'user.u1.chat.dm' }) });
  await env.runTimers();
  assert.equal(env.count('GET', '/lever/api/agents'), lists + 1);
  assert.equal(env.count('GET', HISTORY), reads + 1);
  // Someone else's subject is nothing to this page.
  env.streams[0].emit('update', { data: JSON.stringify({ subject: 'user.u2.chat.dm' }) });
  await env.runTimers();
  assert.equal(env.count('GET', '/lever/api/agents'), lists + 1);
});

test('the list is read every 15 s while the page shows', async () => {
  const env = await load(hubWith());
  // An operator's page also reads its contacts every 30 s.
  assert.deepEqual(env.intervals.map((i) => i.ms), [30000, 15000]);
  const lists = env.count('GET', '/lever/api/agents');
  await env.poll();
  assert.equal(env.count('GET', '/lever/api/agents'), lists + 1);
  document.visibilityState = 'hidden';
  await env.poll();
  assert.equal(env.count('GET', '/lever/api/agents'), lists + 1);
});

test('back returns to the list', async () => {
  const env = await loadChat(hubWith());
  assert.equal(env.document.body.classList.contains('chatting'), true);
  env.els.back.dispatch('click');
  await tick(5);
  assert.equal(env.document.body.classList.contains('chatting'), false);
  assert.equal(env.store['lever-chat-open'], undefined);
  assert.equal(env.els.composer.hidden, true);
  assert.equal(env.rows().length, 0);
  // Its draft waits for it.
  await type(env, 'ignored without a chat');
  env.els.agents.children[0].children[0].dispatch('click');
  await tick(5);
  assert.equal(env.els.agent.textContent, 'boss');
});

test('drafts are kept per agent', async () => {
  const env = await loadChat(hubWith());
  await type(env, 'for boss');
  env.els.agents.children[1].children[0].dispatch('click');
  await tick(5);
  assert.equal(env.els.text.value, '');
  await type(env, 'for w1');
  env.els.agents.children[0].children[0].dispatch('click');
  await tick(5);
  assert.equal(env.els.text.value, 'for boss');
  assert.equal(env.store['lever-chat-draft:w1'], 'for w1');
});

test('an open chat that left the list goes back to the list', async () => {
  const hub = hubWith();
  const env = await load(hub, { store: { 'lever-chat-open': 'w1' } });
  assert.equal(env.els.agent.textContent, 'w1');
  hub.parts.agents = () => roster([BOSS()]);
  await env.poll();
  assert.equal(env.document.body.classList.contains('chatting'), false);
  assert.deepEqual(env.agentRows(), ['boss | working | ']);
  // A stored name the list does not have opens nothing.
  const gone = await load(hubWith(), { store: { 'lever-chat-open': 'nope' } });
  assert.equal(gone.document.body.classList.contains('chatting'), false);
  assert.equal(gone.count('GET', '/api/v1/chat/'), 0);
});

test('a send to one agent stays with it when another chat opens meanwhile', async () => {
  let release;
  const hub = hubWith({ post: (body) => new Promise((r) => (release = () => r({ status: 201, body: { id: 's', content: body.content, senderId: 'u1', createdAt: new Date().toISOString() } }))) });
  const env = await loadChat(hub);
  await type(env, 'to boss');
  env.els.composer.dispatch('submit');
  await tick(5);
  env.els.agents.children[1].children[0].dispatch('click');
  await tick(5);
  await type(env, 'to w1');
  release();
  await tick(5);
  assert.equal(sends(env)[0].path, HISTORY);
  assert.equal(env.els.text.value, 'to w1', 'the other chat\'s draft is not touched');
  assert.equal(env.store['lever-chat-draft:boss'], '', 'the sent draft is cleared where it belongs');
  assert.equal(env.store['lever-chat-unsent:boss'], undefined);
  assert.equal(env.rows().length, 0, 'the stored message is not shown in w1\'s chat');
});

test('the one-agent page\'s draft and unsent record are adopted once', async () => {
  const record = JSON.stringify({ text: 'yes', key: 'old-key', conversation: KEY, tries: 1 });
  const env = await loadChat(hubWith(), { store: { 'lever-chat-unsent': record, 'lever-chat-draft': 'yes' } });
  assert.equal(env.store['lever-chat-unsent'], undefined);
  assert.equal(env.store['lever-chat-draft'], undefined);
  assert.equal(env.els.text.value, 'yes');
  env.els.composer.dispatch('submit');
  await tick(10);
  assert.equal(sends(env)[0].body.idempotency_key, 'old-key', 'an unclear send keeps its key across the upgrade');
});

// The wake flow: a message to an asleep worker wakes it first.

const W1_KEY = 'dm:agent:w1id:user:u1';
const W1_SENDS = `/api/v1/chat/conversations/${encodeURIComponent(W1_KEY)}/messages`;

test('wake: a message to an asleep worker wakes it, then goes under its first key', async () => {
  let state = 'suspended';
  const env = await load(hubWith({
    agents: () => roster([BOSS(), A('w1', { state })]),
    wake: () => {
      state = 'starting';
      return { status: 202, body: { state: 'starting' } };
    },
  }), { store: { 'lever-chat-open': 'w1' } });
  await type(env, 'hello');
  env.els.composer.dispatch('submit');
  await tick(5);
  assert.equal(env.count('POST', '/lever/api/agents/w1/wake'), 1);
  assert.equal(env.calls.find((c) => c.path === '/lever/api/agents/w1/wake').body, undefined, 'the wake carries nothing');
  assert.equal(env.els.note.textContent, 'waking w1…');
  assert.equal(env.els.send.disabled, true);
  assert.equal(sends(env).length, 0, 'nothing is posted to an agent that is not running');
  const record = JSON.parse(env.store['lever-chat-unsent:w1']);
  assert.deepEqual([record.text, record.conversation, record.tries], ['hello', W1_KEY, 0], 'the text is kept before the wake');
  // Still starting: the page waits.
  await env.runTimers(3000);
  assert.equal(sends(env).length, 0);
  assert.equal(env.els.note.textContent, 'waking w1…', 'the list read does not hide the wake');
  state = 'running';
  await env.runTimers(3000);
  await tick(5);
  assert.equal(sends(env).length, 1);
  assert.equal(sends(env)[0].path, W1_SENDS);
  assert.equal(sends(env)[0].body.idempotency_key, record.key);
  assert.equal(env.els.text.value, '');
  assert.equal(env.els.error.hidden, true, 'a first post is no "earlier attempt"');
  assert.equal(env.els.note.hidden, true);
  assert.equal(env.els.send.disabled, false);
  assert.equal(env.store['lever-chat-unsent:w1'], undefined);
});

test('wake: no running in 90 s keeps the text and says so', async () => {
  const env = await load(hubWith({ agents: () => roster([A('w1', { state: 'suspended' })]), wake: () => ({ status: 202, body: {} }) }), { store: { 'lever-chat-open': 'w1' } });
  await type(env, 'hello');
  env.els.composer.dispatch('submit');
  await tick(5);
  for (let i = 0; i < 29; i++) await env.runTimers(3000);
  assert.equal(env.els.error.hidden, true, 'not before 90 s');
  await env.runTimers(3000);
  assert.equal(env.els.error.textContent, 'Not sent: w1 did not wake in time. Your message is kept; send it again later.');
  assert.equal(env.els.text.value, 'hello');
  assert.ok(env.store['lever-chat-unsent:w1']);
  assert.equal(sends(env).length, 0);
  assert.equal(env.count('POST', '/lever/api/agents/w1/wake'), 1, 'one wake, then only list reads');
  assert.equal(env.els.send.disabled, false);
  assert.equal(env.els.note.textContent, 'asleep – your message wakes it');
});

test('wake: a refusal shows fixed text and keeps the message', async () => {
  for (const [status, body, re] of [
    [429, { error: 'rate-limited' }, /a minute/],
    [403, { error: 'not-allowed' }, /not allowed/],
    [503, { error: 'unavailable' }, /not available/],
    [409, { error: 'refused' }, /lever refused to wake it/],
    [502, '<b>proxy page</b>', /^Not sent: w1 could not be woken \(HTTP 502\)\.$/],
  ]) {
    const env = await load(hubWith({ agents: () => roster([A('w1', { state: 'suspended' })]), wake: () => ({ status, body }) }), { store: { 'lever-chat-open': 'w1' } });
    await type(env, 'hello');
    env.els.composer.dispatch('submit');
    await tick(10);
    assert.match(env.els.error.textContent, re, String(status));
    assert.equal(env.els.text.value, 'hello');
    assert.ok(env.store['lever-chat-unsent:w1']);
    assert.equal(sends(env).length, 0);
    assert.equal(env.count('GET', '/lever/api/agents'), 2, 'no polling after a refusal');
    assert.equal(env.els.send.disabled, false);
  }
});

test('wake: a network fault on the wake keeps the message', async () => {
  const env = await load(hubWith({ agents: () => roster([A('w1', { state: 'stopped' })]), wake: () => ({ down: true }) }), { store: { 'lever-chat-open': 'w1' } });
  await type(env, 'hello');
  env.els.composer.dispatch('submit');
  await tick(10);
  assert.equal(env.els.error.textContent, 'Not sent: w1 could not be woken (HTTP no answer).');
  assert.equal(env.els.text.value, 'hello');
});

test('wake: already starting (409 not-asleep) waits and sends', async () => {
  let state = 'suspended';
  const env = await load(hubWith({
    agents: () => roster([A('w1', { state })]),
    wake: () => {
      state = 'starting';
      return { status: 409, body: { error: 'not-asleep', state: 'starting' } };
    },
  }), { store: { 'lever-chat-open': 'w1' } });
  await type(env, 'hello');
  env.els.composer.dispatch('submit');
  await tick(5);
  assert.equal(env.els.note.textContent, 'waking w1…');
  state = 'running';
  await env.runTimers(3000);
  await tick(5);
  assert.equal(sends(env).length, 1);
  assert.equal(env.els.text.value, '');
});

test('wake: an agent that goes to error instead says so and keeps the text', async () => {
  let state = 'suspended';
  const env = await load(hubWith({ agents: () => roster([A('w1', { state })]), wake: () => ({ status: 202, body: {} }) }), { store: { 'lever-chat-open': 'w1' } });
  await type(env, 'hello');
  env.els.composer.dispatch('submit');
  await tick(5);
  state = 'error';
  await env.runTimers(3000);
  assert.equal(env.els.error.textContent, 'Not sent: w1 did not wake (error).');
  assert.equal(env.els.text.value, 'hello');
  assert.equal(sends(env).length, 0);
});

test('wake: a list that cannot be read during the wake is waited out', async () => {
  let n = 0;
  const env = await load(hubWith({
    agents: () => {
      n++;
      if (n <= 2) return roster([A('w1', { state: 'suspended' })]);
      return n === 3 ? { status: 502, body: '' } : roster([A('w1')]);
    },
    wake: () => ({ status: 202, body: {} }),
  }), { store: { 'lever-chat-open': 'w1' } });
  await type(env, 'hello');
  env.els.composer.dispatch('submit');
  await tick(5);
  await env.runTimers(3000);
  assert.equal(sends(env).length, 0);
  await env.runTimers(3000);
  await tick(5);
  assert.equal(sends(env).length, 1);
});

test('wake: a reload during the wake keeps the text and its key', async () => {
  const record = JSON.stringify({ text: 'hello', key: 'k-wake', conversation: W1_KEY, tries: 0 });
  // Still asleep after the reload: the same key goes with the wake and the post.
  let state = 'suspended';
  const env = await load(hubWith({
    agents: () => roster([A('w1', { state })]),
    wake: () => {
      state = 'running';
      return { status: 202, body: {} };
    },
  }), { store: { 'lever-chat-open': 'w1', 'lever-chat-unsent:w1': record, 'lever-chat-draft:w1': 'hello' } });
  assert.equal(env.els.text.value, 'hello');
  env.els.composer.dispatch('submit');
  await tick(5);
  await env.runTimers(3000);
  await tick(5);
  assert.equal(sends(env)[0].body.idempotency_key, 'k-wake');
  assert.equal(env.els.error.hidden, true, 'nothing was posted before: no "earlier attempt" note');
  // Awake by the reload: posted at once under the same key.
  const awake = await load(hubWith({ agents: () => roster([A('w1')]) }), { store: { 'lever-chat-open': 'w1', 'lever-chat-unsent:w1': record, 'lever-chat-draft:w1': 'hello' } });
  awake.els.composer.dispatch('submit');
  await tick(10);
  assert.equal(awake.count('POST', '/lever/api/agents/w1/wake'), 0);
  assert.equal(sends(awake)[0].body.idempotency_key, 'k-wake');
  assert.equal(awake.els.error.hidden, true);
});

test('wake: an earlier unclear send keeps its key and its count through a wake', async () => {
  const record = JSON.stringify({ text: 'hello', key: 'k-old', conversation: W1_KEY, tries: 1 });
  let state = 'suspended';
  const env = await load(hubWith({
    agents: () => roster([A('w1', { state })]),
    wake: () => {
      state = 'running';
      return { status: 202, body: {} };
    },
  }), { store: { 'lever-chat-open': 'w1', 'lever-chat-unsent:w1': record, 'lever-chat-draft:w1': 'hello' } });
  env.els.composer.dispatch('submit');
  await tick(5);
  await env.runTimers(3000);
  await tick(5);
  assert.equal(sends(env)[0].body.idempotency_key, 'k-old');
  assert.equal(env.els.error.textContent, 'Stored now. An earlier attempt had no clear answer: if the message shows twice above, both arrived.');
});

test('wake: never for the manager, a see-only agent, or a running worker', async () => {
  for (const [agents, open] of [
    [[BOSS({ state: 'suspended', activity: undefined })], 'boss'],
    [[A('w1')], 'w1'],
  ]) {
    const env = await load(hubWith({ agents: () => roster(agents) }), { store: { 'lever-chat-open': open } });
    await type(env, 'hello');
    env.els.composer.dispatch('submit');
    await tick(10);
    assert.equal(env.count('POST', '/lever/api/agents/'), 0, open);
  }
  const see = await load(hubWith({ agents: () => roster([A('w2', { access: 'see', state: 'suspended' })]) }), { store: { 'lever-chat-open': 'w2' } });
  see.els.composer.dispatch('submit');
  await tick(10);
  assert.equal(see.count('POST', ''), 0);
});

test('with no chat open the chat pane says to choose an agent (a wide screen shows it beside the list)', async () => {
  const env = await load(hubWith({ agents: () => roster([BOSS(), A('w1')]) }));
  assert.equal(env.els.notice.textContent, 'Choose an agent from the list.');
  assert.equal(env.els.notice.hidden, false);
  assert.equal(env.els.composer.hidden, true);
});

const CONTACTS = () => ({ status: 200, body: { contacts: [
  { login: 'c@x', signedIn: true, agents: [{ name: 'w1', label: 'Via Roma', state: 'running' }] },
  { login: 'd@x', signedIn: false, agents: [{ name: 'w1', label: '', state: 'suspended' }] },
] } });
const T = (i, from, text, shown) => ({ id: `t${i}`, from, text, createdAt: new Date(1.7e12 + i * 1000).toISOString(), shownToContact: shown });
const TPATH = '/lever/api/contacts/c%40x/agents/w1/messages';

test('operator: a Contacts section; a contact gets none and never asks', async () => {
  const env = await load(hubWith({ contacts: CONTACTS }));
  assert.equal(env.els['contacts-title'].hidden, false);
  assert.deepEqual(env.contactRows(), ['c@x | ', 'd@x | not signed in yet']);
  await env.clickContact(0);
  assert.deepEqual(env.contactRows(), ['c@x | ', '  w1 · Via Roma | running', 'd@x | not signed in yet']);

  const contact = await load(hubWith({ agents: () => roster([A('w1')], { tier: 'contact', console: '' }), contacts: CONTACTS }));
  assert.equal(contact.count('GET', '/lever/api/contacts'), 0);
  assert.equal(contact.els['contacts-title'].hidden, true);
  assert.equal(contact.els.contacts.hidden, true);
});

test('a transcript is read-only, marks what the contact is not shown, and writes nothing', async () => {
  const env = await load(hubWith({
    contacts: CONTACTS,
    transcript: () => ({ status: 200, body: { matched: true, messages: [T(2, 'agent', 'CANARY', false), T(1, 'contact', 'hello', true), T(3, 'system', 'started', true),
      { ...T(4, 'agent', 'soon', false), pending: true }] } }),
  }));
  await env.clickContact(0);
  await env.clickContactAgent(0, 0);
  assert.equal(env.count('GET', TPATH), 1);
  assert.equal(env.els.agent.textContent, 'c@x · w1');
  assert.equal(env.els.state.textContent, 'read only');
  assert.equal(env.els.composer.hidden, true);
  assert.equal(env.els.readonly.hidden, false);
  assert.equal(env.els.refresh.hidden, false);
  const rows = env.rows();
  assert.match(rows[0], /^msg contact: c@x .* \/ hello$/);
  assert.match(rows[1], /^msg agent unshown: w1 .* \/ CANARY \/ not shown to the contact$/);
  assert.match(rows[2], /^msg system: hub .* \/ started$/);
  assert.match(rows[3], /^msg agent pending: w1 .* \/ soon \/ not yet read by the contact$/);
  await env.poll();
  assert.equal(env.calls.filter((c) => c.method !== 'GET').length, 0, 'no post, no read marker');
});

test('transcript text is only text', async () => {
  const evil = '<img src=x onerror=alert(1)>';
  const env = await load(hubWith({
    contacts: () => ({ status: 200, body: { contacts: [{ login: evil, signedIn: true, agents: [{ name: 'w1', label: evil, state: evil }] }] } }),
    transcript: () => ({ status: 200, body: { matched: true, messages: [{ id: 't1', from: evil, text: evil, createdAt: evil, shownToContact: false }] } }),
  }));
  await env.clickContact(0);
  await env.clickContactAgent(0, 0);
  for (const row of env.els.list.children) {
    assert.equal(row.tag, 'div');
    for (const c of row.children) assert.deepEqual([c.tag, c.children.length], ['div', 0]);
  }
  assert.ok(env.rows()[0].startsWith('msg agent unshown: w1'));
  assert.equal(env.contactRows()[1], `  w1 · ${evil} | unknown`);
});

test('a contact that never signed in reads nothing', async () => {
  const env = await load(hubWith({ contacts: CONTACTS }));
  await env.clickContact(1);
  await env.clickContactAgent(1, 0);
  assert.equal(env.els.notice.textContent, 'd@x has not signed in yet.');
  assert.equal(env.count('GET', '/lever/api/contacts/'), 0);
});

test('a refusal shows its words', async () => {
  const env = await load(hubWith({ contacts: CONTACTS, transcript: () => ({ status: 409, body: { error: 'not-signed-in' } }) }));
  await env.clickContact(0);
  await env.clickContactAgent(0, 0);
  assert.equal(env.els.notice.textContent, 'Cannot read the conversation: has not signed in yet');
});

test('a transcript the broker could not match says so', async () => {
  const env = await load(hubWith({ contacts: CONTACTS, transcript: () => ({ status: 200, body: { matched: false, messages: [T(1, 'agent', 'x', false)] } }) }));
  await env.clickContact(0);
  await env.clickContactAgent(0, 0);
  assert.equal(env.els.notice.textContent, 'The broker did not answer: which agent messages the contact sees is not known.');
});

test('the transcript refreshes every 30 s and on Refresh, and stops when closed', async () => {
  const env = await load(hubWith({ contacts: CONTACTS }));
  await env.clickContact(0);
  await env.clickContactAgent(0, 0);
  const every = env.intervals.find((i) => i.ms === 30000);
  assert.ok(every, 'a 30 s interval');
  every.f();
  await tick(5);
  assert.equal(env.count('GET', TPATH), 2);
  env.els.refresh.dispatch('click');
  await tick(5);
  assert.equal(env.count('GET', TPATH), 3);
  env.els.back.dispatch('click');
  every.f();
  await tick(5);
  assert.equal(env.count('GET', TPATH), 3, 'no read once closed');
  assert.equal(env.els.readonly.hidden, true);
});

test('Load earlier reads with the cursor', async () => {
  const env = await load(hubWith({ contacts: CONTACTS, transcript: (p) => ({ status: 200, body: { matched: true, nextCursor: p.includes('cursor=') ? '' : 'C1', messages: [T(p.includes('cursor=') ? 1 : 2, 'agent', 'x', true)] } }) }));
  await env.clickContact(0);
  await env.clickContactAgent(0, 0);
  assert.equal(env.els.older.hidden, false);
  env.els.older.dispatch('click');
  await tick(5);
  assert.equal(env.count('GET', `${TPATH}?limit=50&cursor=C1`), 1);
  assert.equal(env.rows().length, 2);
  assert.equal(env.els.older.hidden, true);
});

test('opening an agent chat leaves the transcript', async () => {
  const env = await load(hubWith({ contacts: CONTACTS }));
  await env.clickContact(0);
  await env.clickContactAgent(0, 0);
  env.els.agents.children[0].children[0].dispatch('click');
  await tick(5);
  assert.equal(env.els.readonly.hidden, true);
  assert.equal(env.els.refresh.hidden, true);
  assert.equal(env.els.composer.hidden, false);
  assert.equal(env.els.agent.textContent, 'boss');
});

const VAPID = 'BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4';
// pushHub: the default hub plus the push routes.
function pushHub(parts = {}) {
  const h = hubWith(parts);
  const subs = [];
  const fn = (method, path, body) => {
    if (path === '/lever/api/push/key') return (parts.pushKey || (() => ({ status: 200, body: { key: VAPID } })))();
    if (path === '/lever/api/push/subscriptions') {
      subs.push({ method, body });
      return (parts.subs || (() => ({ status: method === 'POST' ? 201 : 200, body: { ok: 'true' } })))(method, body);
    }
    return h(method, path, body);
  };
  fn.subs = subs;
  return fn;
}

test('push: a browser without push asks nothing and shows no button', async () => {
  const hub = pushHub();
  const env = await load(hub);
  assert.equal(env.count('GET', '/lever/api/push'), 0);
  assert.equal(env.els.push.hidden, true);
});

test('push: server off hides the button and removes an old worker', async () => {
  const env = await load(pushHub({ pushKey: () => ({ status: 404, body: 'not found' }) }), { push: { registered: true } });
  assert.equal(env.els.push.hidden, true);
  assert.equal(env.push.registered, null);
});

test('push: no worker is registered until the login turns notifications on', async () => {
  const hub = pushHub();
  const env = await load(hub, { push: {} });
  assert.equal(env.els.push.hidden, false);
  assert.equal(env.els.push.textContent, 'Turn on notifications');
  assert.equal(env.push.registerCalls.length, 0);
  env.els.push.dispatch('click');
  await tick(20);
  assert.deepEqual(env.push.registerCalls, [{ url: '/lever/sw.js', scope: '/lever/' }]);
  assert.equal(env.push.subscribeOpts.userVisibleOnly, true);
  assert.equal(env.push.subscribeOpts.applicationServerKey.length, 65);
  assert.deepEqual(hub.subs.at(-1), { method: 'POST', body: { endpoint: env.push.sub.endpoint, keys: env.push.sub.toJSON().keys } });
  assert.equal(env.els.push.textContent, 'Turn off notifications');
});

test('push: a refused permission registers nothing and says why', async () => {
  const hub = pushHub();
  const env = await load(hub, { push: { grant: 'denied' } });
  env.els.push.dispatch('click');
  await tick(20);
  assert.equal(env.push.registerCalls.length, 0);
  assert.equal(env.els.push.hidden, true);
  assert.match(env.els.pushnote.textContent, /blocked/);
  assert.equal(hub.subs.length, 0);
});

test('push: a subscription the server refuses is undone', async () => {
  const hub = pushHub({ subs: () => ({ status: 400, body: { error: 'endpoint' } }) });
  const env = await load(hub, { push: {} });
  env.els.push.dispatch('click');
  await tick(20);
  assert.equal(env.push.sub, null);
  assert.equal(env.els.push.textContent, 'Turn on notifications');
  assert.ok(env.els.pushnote.textContent.length > 0);
});

test('push: an existing subscription is sent again at load and can be turned off', async () => {
  const hub = pushHub();
  const env = await load(hub, { push: { registered: true, existing: true, permission: 'granted' }, local: { 'lever-push-optin:op': '1' } });
  assert.equal(env.els.push.textContent, 'Turn off notifications');
  assert.equal(hub.subs[0].method, 'POST');
  env.els.push.dispatch('click');
  await tick(20);
  assert.deepEqual(hub.subs.at(-1), { method: 'DELETE', body: { endpoint: 'https://fcm.googleapis.com/fcm/send/fake' } });
  assert.equal(env.push.sub, null);
  assert.equal(env.push.registered, null);
  assert.equal(env.els.push.textContent, 'Turn on notifications');
});

test('a notification tap opens the agent named in the hash, only if listed', async () => {
  let env = await load(hubWith(), { hash: '#agent=w1' });
  assert.equal(env.els.agent.textContent, 'w1');
  assert.equal(env.replaced.length, 1);
  env = await load(hubWith(), { hash: '#agent=nobody' });
  assert.equal(env.els.agent.textContent, 'Chat');
  env = await load(hubWith(), { hash: '#agent=../w1' });
  assert.equal(env.els.agent.textContent, 'Chat');
});

test('a message from the worker opens a listed agent', async () => {
  const env = await load(pushHub(), { push: {} });
  env.swMessage({ agent: 'w1' });
  await tick(5);
  assert.equal(env.els.agent.textContent, 'w1');
  env.swMessage({ agent: 'nobody' });
  env.swMessage({ agent: 7 });
  env.swMessage(null);
  await tick(5);
  assert.equal(env.els.agent.textContent, 'w1');
});

test('push: turning on marks this login, turning off clears the mark', async () => {
  const hub = pushHub();
  const env = await load(hub, { push: {} });
  env.els.push.dispatch('click');
  await tick(20);
  assert.equal(env.local['lever-push-optin:op'], '1');
  env.els.push.dispatch('click');
  await tick(20);
  assert.equal(env.local['lever-push-optin:op'], undefined);
});

test('push: another login on a shared device does not take the subscription', async () => {
  // Login op turned notifications on here; contact c now uses the page.
  const c = pushHub({ agents: () => roster([A('w1')], { login: 'c', tier: 'contact', console: undefined }) });
  const envC = await load(c, { push: { registered: true, existing: true, permission: 'granted' }, local: { 'lever-push-optin:op': '1' } });
  assert.equal(c.subs.length, 0, 'the subscription was posted for a login that never opted in');
  assert.equal(envC.els.push.textContent, 'Turn on notifications');
});

test('push: under Trusted Types the worker URL comes from the lever-sw policy', async () => {
  const hub = pushHub();
  const env = await load(hub, { push: {}, trustedTypes: true });
  env.els.push.dispatch('click');
  await tick(20);
  assert.deepEqual(env.push.registerCalls, [{ url: '/lever/sw.js', scope: '/lever/', trusted: true }]);
  assert.equal(env.els.push.textContent, 'Turn off notifications');
  const policy = env.tt.policies['lever-sw'];
  for (const bad of ['/lever/sw.js?x', '/lever/other.js', '/api/agent.js', 'https://evil.test/sw.js', '']) {
    assert.throws(() => policy.createScriptURL(bad), /refused/, bad);
  }
  // Off, then on again: the policy is made once (a second one would be
  // refused by the CSP).
  env.els.push.dispatch('click');
  await tick(20);
  env.els.push.dispatch('click');
  await tick(20);
  assert.equal(env.push.registerCalls.length, 2);
  assert.equal(env.tt.refused, 0);
});
