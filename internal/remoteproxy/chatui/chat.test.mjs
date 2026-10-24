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
const readAloudAgents = () => roster([BOSS(), A('w1')], { readAloud: true });

function hubWith(parts = {}) {
  const h = {
    agents: defaultAgents,
    history: () => ({ status: 200, body: { messages: [], totalCount: 0 } }),
    post: (body) => ({ status: 201, body: { id: 'sent1', content: body.content, senderId: 'u1', type: 'instruction', createdAt: new Date(1.8e12).toISOString(), dispatchState: 'dispatched' } }),
    read: () => ({ status: 200, body: { status: 'ok' } }),
    wake: () => ({ status: 202, body: { state: 'starting' } }),
    newConversation: () => ({ status: 202, body: { state: 'starting', id: 'op1' } }),
    newConversationStatus: () => ({ status: 200, body: { state: 'ok', backup: '20261019T054104Z' } }),
    contacts: () => ({ status: 200, body: { contacts: [] } }),
    transcript: () => ({ status: 200, body: { contact: 'c@x', agent: 'w1', matched: true, messages: [] } }),
    upload: () => ({ status: 201, body: { id: 'f'.repeat(32), name: 'report.pdf', size: 3, sha256: '0'.repeat(64) }, progress: [50, 100] }),
    files: () => ({ status: 200, body: { files: [] } }),
    viewFiles: () => ({ status: 200, body: { files: [] } }),
    transcribe: () => ({ status: 200, body: { text: 'hello from the mic' } }),
    speak: (body) => ({ status: 200, audio: `audio:${body.text}` }),
    activity: () => ({ status: 200, body: { text: 'line 1\nline 2' } }),
    ...parts,
  };
  const fn = (method, path, body) => {
    if (path === '/lever/api/contacts') return h.contacts();
    if (path.startsWith('/lever/api/contacts/') && path.endsWith('/files')) return h.viewFiles(path);
    if (path.startsWith('/lever/api/contacts/')) return h.transcript(path);
    if (path === '/lever/api/agents') return h.agents();
    if (path.startsWith('/lever/api/agents/') && path.endsWith('/wake')) return h.wake(path);
    if (path.startsWith('/lever/api/agents/') && path.endsWith('/activity')) return h.activity(path, method);
    if (path === '/lever/api/new-conversation') return h.newConversation(body);
    if (path === '/lever/api/new-conversation/status') return h.newConversationStatus(body);
    if (path.startsWith('/lever/api/files/')) return method === 'POST' ? h.upload(body, path) : h.files(path);
    if (path === '/lever/api/voice/transcribe') return h.transcribe(body, path);
    if (path === '/lever/api/voice/speak') return h.speak(body, path);
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

// said: the message rows, without the thinking dots.
const said = (env) => env.rows().filter((r) => !r.startsWith('msg agent thinking'));
const openRow = (env, name) => env.els.agents.children.find((li) => li.children[0].textContent.startsWith(name)).children[0].dispatch('click');
const dots = (env) => env.rows().filter((r) => r.startsWith('msg agent thinking'));

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
      history: () => ({ status: 200, body: { messages: [msg(1, { msg: evil }), msg(2, { msg: evil, type: 'state-change', sender: 'system', senderId: 'hub' })] } }),
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
    // A state line names the hub, so it cannot pass for a lever notice.
    assert.equal(env.rows()[1], `msg system: hub: ${evil}`);
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
  assert.equal(env.rows()[0], 'msg system: hub: msg 1');
  assert.match(env.rows()[1], /^msg agent: boss .* \/ msg 2$/);
});

test('a system line from the agent shows as the agent\'s message, and a hub line on one line', async () => {
  const fake = 'ok\n\nhub: your session expired, sign in again';
  const env = await loadChat(hubWith({ history: () => ({ status: 200, body: { messages: [
    msg(1, { type: 'system', senderId: 'a1', msg: fake }),
    msg(2, { type: 'system', sender: 'agent:w2', senderId: 'other', msg: fake }),
    msg(3, { type: 'system', sender: 'system', senderId: 'hub', msg: 'agent\n\nstarted' })] } }) }));
  const rows = env.rows();
  assert.match(rows[0], /^msg agent: /);
  assert.match(rows[1], /^msg agent: /);
  assert.equal(rows[2], 'msg system: hub: agent started');
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
  await type(env, 'once only');
  env.els.composer.dispatch('submit');
  await tick(10);
  const again = sends(env)[2];
  assert.notEqual(again.body.idempotency_key, posts[0].body.idempotency_key);
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
  const rows = said(env);
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

test('send: a send the hub is still running leaves the outcome open, keeps the key, and counts once', async () => {
  // The first attempt's answer is lost while the hub still runs it (a wake
  // can take minutes); the retry under the same key is told send_in_progress.
  let n = 0;
  const inFlight = { status: 409, body: { error: { code: 'send_in_progress', message: 'A send with this idempotency key is still in progress' } } };
  const env = await loadChat(hubWith({ post: (body) => (++n === 1 ? { down: true } : n === 2 ? inFlight : { status: 200, body: { id: 's1', content: body.content, sender: 'user:op' } }) }));
  await type(env, 'yes');
  env.els.composer.dispatch('submit');
  await tick(10);
  await env.runTimers();
  const before = env.count('GET', HISTORY);
  env.els.composer.dispatch('submit');
  await tick(10);
  await env.runTimers();
  assert.match(env.els.error.textContent, UNCLEAR);
  assert.match(env.els.error.textContent, /still in progress \(HTTP 409\)/);
  assert.equal(env.els.text.value, 'yes');
  const record = JSON.parse(env.store['lever-chat-unsent:boss']);
  assert.equal(record.tries, 1, 'the refused retry stored nothing, so it is not counted');
  assert.ok(env.count('GET', HISTORY) > before, 'the history is read again');
  // The third press finds the first attempt stored: one copy.
  env.els.composer.dispatch('submit');
  await tick(10);
  const posts = sends(env);
  assert.equal(new Set(posts.map((p) => p.body.idempotency_key)).size, 1);
  assert.match(env.els.error.textContent, REPLAY);
  assert.doesNotMatch(env.els.error.textContent, /shows twice/);
});

test('send: a first attempt answered send_in_progress is not a refusal', async () => {
  const env = await loadChat(hubWith({ post: () => ({ status: 409, body: { error: { code: 'send_in_progress', message: 'busy' } } }) }));
  await type(env, 'yes');
  env.els.composer.dispatch('submit');
  await tick(10);
  assert.match(env.els.error.textContent, UNCLEAR);
  assert.ok(env.store['lever-chat-unsent:boss'], 'the key stays for the next press');
  // Any other 409 is the hub's refusal.
  const other = await loadChat(hubWith({ post: () => ({ status: 409, body: { error: { code: 'agent_not_running', message: 'suspended' } } }) }));
  await type(other, 'yes');
  other.els.composer.dispatch('submit');
  await tick(10);
  assert.equal(other.els.error.textContent, 'Not sent: suspended (HTTP 409)');
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
  assert.deepEqual(said(env).map((r) => r.split(':')[0]), ['msg mine']);
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
  assert.equal(said(env).length, 1);
  // The hub did not know the key. Either the first attempt never arrived, or
  // it did and the hub has forgotten: the page cannot tell, and says so.
  assert.equal(env.els.error.textContent, STORED_AGAIN);
  assert.equal(env.store['lever-chat-unsent:boss'], undefined);
});

test('send: the history never settles a send: only the hub\'s answer to the key does', async () => {
  // Whatever the history shows after a failed send, the page concludes
  // nothing: the error and the draft stay. Each of these could be guessed
  // as "it arrived", and none of them is the failed send.
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
  assert.equal(said(again).length, 1, 'the stored message shows; the page still concludes nothing');
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
  assert.equal(marks().length, 0, 'nothing is marked read before the user acts');
  await env.act();
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
  await env.act();
  assert.equal(marks(), 1);
  hub.parts.history = () => ({ status: 200, body: { messages: [msg(2), msg(1)] } });
  document.visibilityState = 'hidden';
  env.streams[0].emit('update', { data: JSON.stringify({ subject: 'user.u1.chat.dm' }) });
  await env.runTimers();
  assert.equal(marks(), 1);
});

// A link from another site can open the page and choose the chat with
// #agent=, even in a window it closes again: nothing is marked read until
// the user acts on the page.
test('a page nobody has acted on marks nothing read', async () => {
  const hub = hubWith({ history: () => ({ status: 200, body: { messages: [msg(2), msg(1)] } }) });
  const marks = (env) => env.calls.filter((c) => c.method === 'POST' && c.path.endsWith('/read'));
  const env = await load(hub, { hash: '#agent=boss' });
  assert.equal(env.els.agent.textContent, 'boss', 'the hash still opens the chat');
  env.streams[0].emit('update', { data: JSON.stringify({ subject: 'user.u1.chat.dm' }) });
  await env.poll();
  assert.equal(marks(env).length, 0);
  // The first key marks the open chat read at once, and the next history read
  // with a newer message marks again.
  await env.act('keydown');
  assert.deepEqual(marks(env).map((c) => c.body), [{ messageId: 'm002' }]);
  hub.parts.history = () => ({ status: 200, body: { messages: [msg(3), msg(2), msg(1)] } });
  env.streams[0].emit('update', { data: JSON.stringify({ subject: 'user.u1.chat.dm' }) });
  await env.runTimers();
  assert.deepEqual(marks(env).map((c) => c.body), [{ messageId: 'm002' }, { messageId: 'm003' }]);
  await env.act('touchstart');
  assert.equal(marks(env).length, 2, 'only the first act marks by itself');
  // A browser's sticky activation counts as an act.
  const active = await loadChat(hub, { activation: { hasBeenActive: true } });
  assert.equal(marks(active).length, 1);
  const inactive = await loadChat(hub, { activation: { hasBeenActive: false } });
  assert.equal(marks(inactive).length, 0);
});

// A tap on one of lever's notifications reaches an open page as a message
// from its own service worker, which counts as an act.
test('a notification tap on an open page marks its chat read', async () => {
  const hub = hubWith({ agents: () => roster([BOSS(), A('w1')]), history: () => ({ status: 200, body: { messages: [msg(1)] } }) });
  const env = await load(hub, { push: { permission: 'granted' } });
  const marks = () => env.calls.filter((c) => c.method === 'POST' && c.path.endsWith('/read'));
  assert.equal(marks().length, 0);
  env.swMessage({ agent: 'w1' });
  await env.runTimers();
  assert.deepEqual(marks().map((c) => c.path), ['/api/v1/chat/conversations/dm%3Aagent%3Aw1id%3Auser%3Au1/read']);
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

const withFiles = (agents) => () => roster(agents, { files: { maxBytes: 1000, extensions: ['pdf', 'xlsm'] } });

test('files off: no paperclip and no Files button', async () => {
  const env = await loadChat(hubWith());
  assert.equal(env.els.attach.hidden, true);
  assert.equal(env.els.files.hidden, true);
});

test('files on: paperclip and Files for a message agent, none for a see-only one', async () => {
  const env = await loadChat(hubWith({ agents: withFiles([BOSS(), A('w2', { access: 'see' })]) }));
  assert.equal(env.els.attach.hidden, false);
  assert.equal(env.els.files.hidden, false);
  env.els.agents.children[1].children[0].dispatch('click');
  await tick(5);
  assert.equal(env.els.attach.hidden, true);
  assert.equal(env.els.files.hidden, true);
});

test('upload: one file field, progress, then the chat note through the normal send', async () => {
  const env = await loadChat(hubWith({ agents: withFiles([BOSS()]) }));
  env.els.attach.dispatch('click');
  assert.equal(env.els.file.clicks, 1);
  await env.pick({ name: 'report.pdf', size: 3 });
  await env.runTimers();
  const up = env.calls.find((c) => c.method === 'POST' && c.path === '/lever/api/files/boss');
  assert.deepEqual(up.body.form, [{ field: 'file', name: 'report.pdf', size: 3 }]);
  assert.equal(up.headers['X-Lever-Upload'], '1');
  assert.ok(env.uploadNotes.some((t) => /50%/.test(t)), env.uploadNotes.join(' | '));
  const s = sends(env);
  assert.equal(s.length, 1);
  assert.equal(s[0].body.content, '📎 uploaded report.pdf');
  assert.equal(env.els.upload.hidden, true);
  assert.equal(env.els.error.hidden, true);
});

test('a file over the limit or of another type is refused before any request', async () => {
  const env = await loadChat(hubWith({ agents: withFiles([BOSS()]) }));
  await env.pick({ name: 'big.pdf', size: 5000 });
  assert.match(env.els.error.textContent, /limit/);
  await env.pick({ name: 'run.exe', size: 1 });
  assert.match(env.els.error.textContent, /not accepted/);
  assert.equal(env.count('POST', '/lever/api/files/'), 0);
  assert.equal(sends(env).length, 0);
});

test('an upload refusal shows its words and posts nothing', async () => {
  const env = await loadChat(hubWith({ agents: withFiles([BOSS()]), upload: () => ({ status: 413, body: { error: 'too-large' } }) }));
  await env.pick({ name: 'a.pdf', size: 3 });
  await env.runTimers();
  assert.match(env.els.error.textContent, /^Not uploaded: .*too large/);
  assert.equal(sends(env).length, 0);
});

test('uploaded, but the chat note was refused: says so', async () => {
  const env = await loadChat(hubWith({ agents: withFiles([BOSS()]), post: () => ({ status: 403, body: 'forbidden' }) }));
  await env.pick({ name: 'a.pdf', size: 3 });
  await env.runTimers();
  assert.match(env.els.error.textContent, /^Uploaded report\.pdf, but the chat message was not sent/);
});

test('files panel: names are text, links only to lever\'s download route', async () => {
  const evil = '<img src=x onerror=alert(1)>.pdf';
  const id = 'e'.repeat(32);
  const env = await loadChat(hubWith({
    agents: withFiles([BOSS()]),
    files: () => ({ status: 200, body: { files: [
      { id, name: evil, size: 2048, at: '2026-10-07T10:00:00Z', direction: 'received' },
      { id: '../../api/v1/x', name: 'bad', size: 1, direction: 'sent' },
    ] } }),
  }));
  env.els.files.dispatch('click');
  await tick(5);
  assert.equal(env.els.filespanel.hidden, false);
  const rows = env.els.filelist.children;
  assert.equal(rows.length, 1);
  const a = rows[0].children.find((c) => c.tag === 'a');
  assert.equal(a.textContent, evil);
  assert.equal(a.attrs.href, `https://mac.ts.net/lever/api/files/boss/${id}`);
  assert.equal(a.attrs.download, '');
  assert.match(rows[0].textContent, /From boss/);
  assert.match(rows[0].textContent, /2 KB/);
});

test('files panel closes and clears when the chat changes', async () => {
  const env = await loadChat(hubWith({ agents: withFiles([BOSS(), A('w1')]),
    files: () => ({ status: 200, body: { files: [{ id: 'e'.repeat(32), name: 'a.pdf', size: 1, direction: 'sent' }] } }) }));
  env.els.files.dispatch('click');
  await tick(5);
  assert.equal(env.els.files.attrs['aria-expanded'], 'true', 'the icon button says the panel is open');
  assert.equal(env.els.files.children.length, 0, 'no label text is written over the icon');
  env.els.back.dispatch('click');
  assert.equal(env.els.filespanel.hidden, true);
  assert.equal(env.els.filelist.children.length, 0);
  assert.equal(env.els.files.attrs['aria-expanded'], 'false');
});

test('the Files button toggles the panel and its expanded state', async () => {
  const env = await loadChat(hubWith({ agents: withFiles([BOSS()]), files: () => ({ status: 200, body: { files: [] } }) }));
  env.els.files.dispatch('click');
  await tick(5);
  assert.equal(env.els.filespanel.hidden, false);
  assert.equal(env.els.files.attrs['aria-expanded'], 'true');
  env.els.files.dispatch('click');
  assert.equal(env.els.filespanel.hidden, true);
  assert.equal(env.els.files.attrs['aria-expanded'], 'false');
  assert.equal(env.els.files.textContent, '', 'the button keeps its icon, not a text label');
});

test('the chat note names the file as the server stored it', async () => {
  const env = await loadChat(hubWith({ agents: withFiles([BOSS()]),
    upload: () => ({ status: 201, body: { id: 'f'.repeat(32), name: '_bashrc.pdf', size: 3, sha256: '0'.repeat(64) } }) }));
  await env.pick({ name: '.bashrc.pdf', size: 3 });
  await env.runTimers();
  assert.equal(sends(env)[0].body.content, '📎 uploaded _bashrc.pdf');
});

test('push and files together: both buttons work', async () => {
  const hub = pushHub({ agents: withFiles([BOSS()]) });
  const env = await load(hub, { push: {}, store: { 'lever-chat-open': 'boss' } });
  assert.equal(env.els.push.hidden, false);
  assert.equal(env.els.attach.hidden, false);
  assert.equal(env.els.files.hidden, false);
  env.els.push.dispatch('click');
  await tick(20);
  assert.equal(env.els.push.textContent, 'Turn off notifications');
  await env.pick({ name: 'a.pdf', size: 3 });
  await env.runTimers();
  assert.equal(env.count('POST', '/lever/api/files/boss'), 1);
  assert.equal(sends(env).length, 1);
});

test('operator view: a contact\'s files are listed read-only, with links to lever\'s download route', async () => {
  const id = 'e'.repeat(32);
  const evil = '<img src=x onerror=alert(1)>.pdf';
  const env = await load(hubWith({
    agents: withFiles([BOSS(), A('w1')]),
    contacts: CONTACTS,
    viewFiles: () => ({ status: 200, body: { files: [
      { id, name: evil, size: 10, at: '2026-10-07T10:00:00Z', direction: 'sent' },
      { id: 'f'.repeat(32), name: 'v3.xlsm', size: 2048, direction: 'received' },
      { id: '../x', name: 'bad', size: 1, direction: 'sent' },
    ] } }),
  }));
  await env.clickContact(0);
  await env.clickContactAgent(0, 0);
  assert.equal(env.count('GET', '/lever/api/contacts/c%40x/agents/w1/files'), 1);
  assert.equal(env.els.filespanel.hidden, false);
  assert.equal(env.els.attach.hidden, true);
  assert.equal(env.els.files.hidden, true);
  const rows = env.els.filelist.children;
  assert.equal(rows.length, 2);
  const a = rows[0].children.find((c) => c.tag === 'a');
  assert.equal(a.textContent, evil);
  assert.equal(a.attrs.href, `https://mac.ts.net/lever/api/files/w1/${id}`);
  assert.match(rows[0].textContent, /From c@x/);
  assert.match(rows[1].textContent, /From w1/);
  await env.poll();
  assert.equal(env.calls.filter((c) => c.method !== 'GET').length, 0, 'the view writes nothing');
  env.els.back.dispatch('click');
  assert.equal(env.els.filespanel.hidden, true);
  assert.equal(env.els.filelist.children.length, 0);
});

test('operator view with files off: no files list and no request', async () => {
  const env = await load(hubWith({ contacts: CONTACTS }));
  await env.clickContact(0);
  await env.clickContactAgent(0, 0);
  assert.equal(env.count('GET', '/lever/api/contacts/c%40x/agents/w1/files'), 0);
  assert.equal(env.els.filespanel.hidden, true);
});

test('the view poll does not read a not-signed-in contact\'s conversation', async () => {
  const env = await load(hubWith({ agents: withFiles([BOSS(), A('w1')]), contacts: CONTACTS }));
  await env.clickContact(1);
  await env.clickContactAgent(1, 0);
  assert.equal(env.els.notice.textContent, 'd@x has not signed in yet.');
  const reads = () => env.count('GET', '/lever/api/contacts/d%40x/');
  const before = reads();
  await env.poll();
  await env.poll();
  assert.equal(reads(), before, 'no transcript or files read from the poll');
  assert.equal(env.els.notice.textContent, 'd@x has not signed in yet.');
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

test('uploads off: no paperclip, the Files panel stays', async () => {
  const env = await loadChat(hubWith({ agents: () => roster([BOSS()], { files: { maxBytes: 1000, extensions: ['pdf'], uploads: false, shares: true } }) }));
  assert.equal(env.els.attach.hidden, true);
  assert.equal(env.els.files.hidden, false);
});

test('shares off: a share is listed without a link, an upload keeps its link', async () => {
  const env = await loadChat(hubWith({
    agents: () => roster([BOSS()], { files: { maxBytes: 1000, extensions: ['pdf'], uploads: true, shares: false } }),
    files: () => ({ status: 200, body: { files: [
      { id: 'e'.repeat(32), name: 'v3.xlsm', size: 10, direction: 'received' },
      { id: 'f'.repeat(32), name: 'mine.pdf', size: 10, direction: 'sent' },
    ] } }),
  }));
  env.els.files.dispatch('click');
  await tick(5);
  const [share, mine] = env.els.filelist.children;
  assert.equal(share.children.find((c) => c.tag === 'a').attrs.href, undefined);
  assert.match(share.textContent, /downloads of shared files are off/);
  assert.equal(mine.children.find((c) => c.tag === 'a').attrs.href, `https://mac.ts.net/lever/api/files/boss/${'f'.repeat(32)}`);
});

test('operator view: a contact with files off gets no files request', async () => {
  const env = await load(hubWith({
    agents: withFiles([BOSS(), A('w1')]),
    contacts: () => ({ status: 200, body: { contacts: [{ login: 'c@x', signedIn: true, noFiles: true, agents: [{ name: 'w1', label: '', state: 'running' }] }] } }),
  }));
  await env.clickContact(0);
  await env.clickContactAgent(0, 0);
  await env.poll();
  assert.equal(env.count('GET', '/lever/api/contacts/c%40x/agents/w1/files'), 0);
  assert.equal(env.els.filespanel.hidden, true);
});

const withVoice = (agents, voice = { maxSeconds: 300 }) => () => roster(agents, { voice });
const transcribes = (env) => env.calls.filter((c) => c.path === '/lever/api/voice/transcribe');
const LOCAL = { name: 'Local', lang: 'en-GB', voiceURI: 'local-en', localService: true };
const NETWORK = { name: 'Cloud', lang: 'en-US', voiceURI: 'cloud-en', localService: false, default: true };

test('mic: shown only when the list carries voice and the browser can record', async () => {
  let env = await loadChat(hubWith({ agents: withVoice([BOSS()]) }), { media: {} });
  assert.equal(env.els.mic.hidden, false);
  assert.equal(env.els.mic.disabled, false);
  env = await loadChat(hubWith(), { media: {} });
  assert.equal(env.els.mic.hidden, true, 'no voice in the list: no mic');
  env = await loadChat(hubWith({ agents: withVoice([BOSS()]) }));
  assert.equal(env.els.mic.hidden, true, 'a browser that cannot record: no mic');
  for (const voice of [{ maxSeconds: 0 }, { maxSeconds: 601 }, { maxSeconds: '300' }, 'yes', null]) {
    env = await loadChat(hubWith({ agents: withVoice([BOSS()], voice) }), { media: {} });
    assert.equal(env.els.mic.hidden, true, JSON.stringify(voice));
  }
  // A see-only agent or no chat open: no mic.
  env = await load(hubWith({ agents: withVoice([BOSS()]) }), { media: {} });
  assert.equal(env.els.mic.hidden, true);
  // Input off (no hub record): the mic is off too.
  env = await loadChat(hubWith({ agents: withVoice([BOSS({ conversation: '', id: '', state: 'no-record' })]) }), { media: {} });
  assert.equal(env.els.mic.disabled, true);
});

test('mic: record, stop, post a 16 kHz mono WAV, put the text at the cursor, send nothing', async () => {
  const env = await loadChat(hubWith({ agents: withVoice([BOSS()]) }), { media: { decoded: { length: 96000, sampleRate: 48000 } } });
  await type(env, 'Before after');
  env.els.text.selectionStart = 6;
  env.els.text.selectionEnd = 6;
  env.els.mic.dispatch('click');
  await tick(5);
  assert.equal(env.media.recorders.length, 1);
  assert.equal(env.media.recorders[0].state, 'recording');
  assert.equal(env.els.mic.className, 'recording');
  assert.equal(env.els.mic.textContent, '■');
  assert.match(env.els.voice.textContent, /^Recording 0:00 of 5:00/);
  env.els.mic.dispatch('click');
  await tick(20);
  assert.equal(env.media.stopped, 1, 'the microphone is released');
  // 2 s at 48 kHz becomes 32000 samples at 16 kHz, one channel.
  assert.deepEqual(env.media.renders, [{ channels: 1, length: 32000, rate: 16000 }]);
  const t = transcribes(env);
  assert.equal(t.length, 1);
  assert.equal(t[0].method, 'POST');
  assert.equal(t[0].headers['X-Lever-Voice'], '1');
  assert.equal(t[0].headers['Content-Type'], 'audio/wav');
  const wav = t[0].body;
  assert.ok(wav instanceof Uint8Array);
  assert.equal(wav.length, 44 + 2 * 32000);
  assert.equal(String.fromCharCode(...wav.slice(0, 4)), 'RIFF');
  assert.equal(env.els.text.value, 'Before hello from the mic after');
  assert.equal(env.els.text.selectionStart, 'Before hello from the mic'.length);
  assert.equal(sends(env).length, 0, 'a transcript is never sent by itself');
  assert.equal(env.store['lever-chat-draft:boss'], 'Before hello from the mic after');
  assert.equal(env.els.voice.hidden, true);
  assert.equal(env.els.mic.textContent, '🎤');
  assert.equal(env.els.error.hidden, true);
});

test('mic: a clip longer than the limit is cut to it; one too short is not sent', async () => {
  let env = await loadChat(hubWith({ agents: withVoice([BOSS()], { maxSeconds: 2 }) }), { media: { decoded: { length: 48000 * 5, sampleRate: 48000 } } });
  env.els.mic.dispatch('click');
  await tick(5);
  env.els.mic.dispatch('click');
  await tick(20);
  assert.equal(env.media.renders[0].length, 32000);
  env = await loadChat(hubWith({ agents: withVoice([BOSS()]) }), { media: { decoded: { length: 100, sampleRate: 48000 } } });
  env.els.mic.dispatch('click');
  await tick(5);
  env.els.mic.dispatch('click');
  await tick(20);
  assert.equal(transcribes(env).length, 0);
  assert.equal(env.els.error.textContent, 'Nothing was recorded.');
});

test('mic: stops by itself at the clip limit', async () => {
  const env = await loadChat(hubWith({ agents: withVoice([BOSS()], { maxSeconds: 1 }) }), { media: {} });
  const realNow = Date.now;
  try {
    env.els.mic.dispatch('click');
    await tick(5);
    const start = Date.now();
    Date.now = () => start + 1500;
    for (const i of env.intervals.filter((x) => x.ms === 500)) i.f();
    await tick(20);
  } finally {
    Date.now = realNow;
  }
  assert.equal(env.media.recorders[0].state, 'inactive');
  assert.equal(transcribes(env).length, 1);
  assert.equal(env.intervals.filter((x) => x.ms === 500).length, 0, 'the timer is cleared');
});

test('mic: a refusal shows its words and leaves the draft alone', async () => {
  const env = await loadChat(hubWith({ agents: withVoice([BOSS()]), transcribe: () => ({ status: 429, body: { error: 'quota' } }) }), { media: {} });
  await type(env, 'draft');
  env.els.mic.dispatch('click');
  await tick(5);
  env.els.mic.dispatch('click');
  await tick(20);
  assert.equal(env.els.error.textContent, 'Not transcribed: too much dictation today; try again tomorrow');
  assert.equal(env.els.text.value, 'draft');
  assert.equal(env.els.mic.disabled, false);
});

test('mic: a denied microphone or an unreadable clip says so', async () => {
  let env = await loadChat(hubWith({ agents: withVoice([BOSS()]) }), { media: { denied: true } });
  env.els.mic.dispatch('click');
  await tick(5);
  assert.match(env.els.error.textContent, /microphone is not available/);
  assert.equal(env.media.recorders.length, 0);
  env = await loadChat(hubWith({ agents: withVoice([BOSS()]) }), { media: { decodeFails: true } });
  env.els.mic.dispatch('click');
  await tick(5);
  env.els.mic.dispatch('click');
  await tick(20);
  assert.match(env.els.error.textContent, /could not be read/);
  assert.equal(transcribes(env).length, 0);
});

test('mic: stays as the stop control while recording, even if the list drops voice', async () => {
  const hub = hubWith({ agents: withVoice([BOSS()]) });
  const env = await loadChat(hub, { media: {} });
  env.els.mic.dispatch('click');
  await tick(5);
  hub.parts.agents = defaultAgents;
  await env.poll();
  assert.equal(env.els.mic.hidden, false);
  env.els.mic.dispatch('click');
  await tick(20);
  assert.equal(env.media.recorders[0].state, 'inactive');
  assert.equal(env.els.mic.hidden, true, 'stopped: the list has no voice now');
});

test('mic: leaving the chat while recording drops the clip', async () => {
  const env = await loadChat(hubWith({ agents: withVoice([BOSS(), A('w1')]) }), { media: {} });
  env.els.mic.dispatch('click');
  await tick(5);
  env.els.back.dispatch('click');
  await tick(20);
  assert.equal(env.media.stopped, 1);
  assert.equal(transcribes(env).length, 0);
  assert.equal(env.els.voice.hidden, true);
});

test('read-aloud: a speaker on each agent message, on-device voices only, cleaned text, stop control', async () => {
  const hist = () => ({ status: 200, body: { messages: [msg(1, { msg: 'See **this**:\n```\nrm -rf /\n```\nand [docs](https://example.com/x)' }), msg(2, { senderId: 'u1', msg: 'mine' }), msg(3, { msg: 'second' })] } });
  const env = await loadChat(hubWith({ agents: readAloudAgents, history: hist }), { speech: { voices: [NETWORK, LOCAL] } });
  const speakers = () => env.els.list.children.map((r) => r.children.find((c) => c.className === 'speak'));
  const [first, mine, third] = speakers();
  assert.ok(first && third);
  assert.equal(mine, undefined, 'no speaker on the login\'s own message');
  first.dispatch('click');
  assert.equal(env.speech.spoken.length, 1);
  const u = env.speech.spoken[0];
  assert.equal(u.text, 'See this:\ncode block skipped.\nand docs');
  assert.equal(u.voice, LOCAL, 'never a network voice, even the default');
  assert.equal(first.textContent, '⏹');
  first.dispatch('click');
  assert.equal(env.speech.cancels, 1);
  assert.equal(env.speech.spoken.length, 1);
  assert.equal(first.textContent, '🔊');
  first.dispatch('click');
  third.dispatch('click');
  assert.equal(env.speech.spoken.length, 3);
  assert.equal(env.speech.spoken[2].text, 'second');
  assert.equal(first.textContent, '🔊');
  assert.equal(third.textContent, '⏹');
  env.endSpeech();
  assert.equal(third.textContent, '🔊');
});

test('read-aloud: no on-device voice reads nothing; no speech API shows no speaker', async () => {
  let env = await loadChat(hubWith({ agents: readAloudAgents, history: () => ({ status: 200, body: { messages: [msg(1)] } }) }), { speech: { voices: [NETWORK] } });
  env.els.list.children[0].children.find((c) => c.className === 'speak').dispatch('click');
  assert.equal(env.speech.spoken.length, 0);
  assert.match(env.els.error.textContent, /no on-device voice/);
  env = await loadChat(hubWith({ agents: readAloudAgents, history: () => ({ status: 200, body: { messages: [msg(1)] } }) }));
  assert.equal(env.els.list.children[0].children.some((c) => c.className === 'speak'), false);
});

test('read-aloud: the voice choice is offered with two local voices and kept per device', async () => {
  const other = { name: 'Other', lang: 'de-DE', voiceURI: 'local-de', localService: true };
  const env = await loadChat(hubWith({ agents: readAloudAgents, history: () => ({ status: 200, body: { messages: [msg(1)] } }) }), { speech: { voices: [LOCAL, other, NETWORK] } });
  assert.equal(env.els.readvoicerow.hidden, false);
  assert.equal(env.els.settings.hidden, false, 'the settings button that holds it shows');
  assert.deepEqual(env.els.readvoice.children.map((o) => o.value), ['local-en', 'local-de']);
  env.els.readvoice.value = 'local-de';
  env.els.readvoice.dispatch('change');
  assert.equal(env.local['lever-read-voice'], 'local-de');
  env.els.list.children[0].children.find((c) => c.className === 'speak').dispatch('click');
  assert.equal(env.speech.spoken[0].voice, other);
  const one = await loadChat(hubWith({ agents: readAloudAgents }), { speech: { voices: [LOCAL, NETWORK] } });
  assert.equal(one.els.readvoicerow.hidden, true, 'one local voice: nothing to choose');
  assert.equal(one.els.autoreadrow.hidden, false, 'reading new replies aloud is still offered');
  assert.equal(one.els.settings.hidden, false);
});
test('read-aloud: off unless the roster offers it (remote.voice off or read_aloud false)', async () => {
  const env = await loadChat(hubWith({ history: () => ({ status: 200, body: { messages: [msg(1)] } }) }), { speech: { voices: [LOCAL, { name: 'Other', lang: 'de-DE', voiceURI: 'local-de', localService: true }] } });
  assert.equal(env.els.list.children[0].children.some((c) => c.className === 'speak'), false);
  assert.equal(env.els.readvoicerow.hidden, true);
  assert.equal(env.els.autoreadrow.hidden, true);
  assert.equal(env.els.settings.hidden, true, 'no option applies: no settings button');
});


// Server voices (read_aloud: server): the roster lists lever-tool-tts's.
const SPEECH = { voice: 'bf_emma', voices: [{ name: 'bf_emma', lang: 'en-GB' }, { name: 'am_adam', lang: 'en-US' }], maxChars: 600 };
const serverAgents = (speech = SPEECH) => () => roster([BOSS(), A('w1')], { readAloud: true, speech });
const speaks = (env) => env.calls.filter((c) => c.path === '/lever/api/voice/speak');
const speaker = (env, i = 0) => env.els.list.children[i].children.find((c) => c.className === 'speak');
// LONG is a message of three parts: a first sentence under 200 characters,
// then two more that do not fit with it.
const S1 = `${'a'.repeat(149)}.`;
const S2 = `${'b'.repeat(399)}.`;
const S3 = `${'c'.repeat(399)}.`;
const LONG = `${S1} ${S2} ${S3}`;

test('read-aloud, server voice: parts asked for one at a time, played in order with Web Audio, no device voice', async () => {
  const env = await loadChat(hubWith({ agents: serverAgents(), history: () => ({ status: 200, body: { messages: [msg(1, { msg: `**${S1}** ${S2} ${S3}` })] } }) }),
    { speech: { voices: [LOCAL] }, audio: {} });
  const b = speaker(env);
  b.dispatch('click');
  assert.equal(b.textContent, '⏹');
  assert.equal(env.audio.resumes, 1, 'the audio context is resumed within the tap');
  assert.equal(globalThis.navigator.audioSession.type, 'playback');
  await tick(10);
  const first = speaks(env)[0];
  assert.deepEqual(first.body, { agent: 'boss', id: 'm001', text: S1, voice: 'bf_emma' });
  assert.equal(first.method, 'POST');
  assert.equal(first.headers['X-Lever-Voice'], '1');
  assert.equal(first.headers['Content-Type'], 'application/json');
  assert.deepEqual(env.audio.played, [`audio:${S1}`]);
  // The next part is asked for while the first plays; no third yet.
  assert.deepEqual(speaks(env).map((c) => c.body.text), [S1, S2]);
  await env.endAudio();
  assert.deepEqual(env.audio.played, [`audio:${S1}`, `audio:${S2}`]);
  assert.deepEqual(speaks(env).map((c) => c.body.text), [S1, S2, S3]);
  await env.endAudio();
  await env.endAudio();
  assert.equal(env.audio.played.length, 3);
  assert.equal(b.textContent, '🔊');
  assert.equal(env.speech.spoken.length, 0, 'the device voice was not used');
});

test('read-aloud, server voice: the first part failing falls back to the device voice for the whole message', async () => {
  for (const answer of [{ status: 503, body: { error: 'unavailable' } }, { down: true }, { status: 200, audio: 'not audio' }, { status: 429, body: { error: 'busy' } }]) {
    const env = await loadChat(hubWith({ agents: serverAgents(), speak: () => answer, history: () => ({ status: 200, body: { messages: [msg(1, { msg: 'Hello there.' })] } }) }),
      { speech: { voices: [LOCAL] }, audio: {} });
    speaker(env).dispatch('click');
    await tick(10);
    assert.equal(env.speech.spoken.length, 1, JSON.stringify(answer));
    assert.equal(env.speech.spoken[0].text, 'Hello there.');
    assert.equal(env.speech.spoken[0].voice, LOCAL);
    assert.equal(speaker(env).textContent, '⏹');
    assert.equal(env.els.error.textContent, '');
  }
});

test('read-aloud, server voice: a later part failing stops with a note', async () => {
  let n = 0;
  const env = await loadChat(hubWith({ agents: serverAgents(), speak: (body) => (n++ ? { status: 429, body: { error: 'quota' } } : { status: 200, audio: `audio:${body.text}` }),
    history: () => ({ status: 200, body: { messages: [msg(1, { msg: LONG })] } }) }), { speech: { voices: [LOCAL] }, audio: {} });
  speaker(env).dispatch('click');
  await tick(10);
  await env.endAudio();
  assert.equal(env.audio.played.length, 1);
  assert.match(env.els.error.textContent, /Reading aloud stopped: too much read aloud today/);
  assert.equal(env.speech.spoken.length, 0);
  assert.equal(speaker(env).textContent, '🔊');
  assert.equal(speaks(env).length, 2);
});

test('read-aloud, server voice: a tap stops the playing part and the request under way; hiding the page stops it too', async () => {
  const env = await loadChat(hubWith({ agents: serverAgents(), history: () => ({ status: 200, body: { messages: [msg(1, { msg: LONG }), msg(2, { msg: 'Second.' })] } }) }),
    { speech: { voices: [LOCAL] }, audio: {} });
  speaker(env).dispatch('click');
  await tick(10);
  speaker(env).dispatch('click');
  await tick(10);
  assert.equal(env.audio.stopped, 1);
  assert.equal(speaker(env).textContent, '🔊');
  assert.equal(env.audio.played.length, 1, 'nothing more plays');
  assert.equal(speaks(env).length, 2, 'no part asked for after the stop');
  // Another message: the hidden page stops it.
  speaker(env, 1).dispatch('click');
  await tick(10);
  assert.equal(speaker(env, 1).textContent, '⏹');
  await env.hide();
  assert.equal(speaker(env, 1).textContent, '🔊');
  assert.equal(env.audio.stopped, 2);
});

test('read-aloud, server voice: the picker lists the server voices and the choice is kept per device', async () => {
  const env = await loadChat(hubWith({ agents: serverAgents(), history: () => ({ status: 200, body: { messages: [msg(1, { msg: 'Hi.' })] } }) }),
    { speech: { voices: [LOCAL, { name: 'Other', lang: 'de-DE', voiceURI: 'local-de', localService: true }] }, audio: {} });
  assert.equal(env.els.readvoice.hidden, false);
  assert.deepEqual(env.els.readvoice.children.map((o) => [o.value, o.textContent]), [['bf_emma', 'bf_emma (en-GB)'], ['am_adam', 'am_adam (en-US)']]);
  assert.equal(env.els.readvoice.value, 'bf_emma');
  env.els.readvoice.value = 'am_adam';
  env.els.readvoice.dispatch('change');
  assert.equal(env.local['lever-read-server-voice'], 'am_adam');
  assert.equal(env.local['lever-read-voice'], undefined, 'the device choice is untouched');
  speaker(env).dispatch('click');
  await tick(10);
  assert.equal(speaks(env)[0].body.voice, 'am_adam');
});

test('read-aloud, server voice: offered on a device with no speech synthesis; none without Web Audio or roster voices', async () => {
  const hist = () => ({ status: 200, body: { messages: [msg(1, { msg: 'Hi.' })] } });
  let env = await loadChat(hubWith({ agents: serverAgents(), history: hist }), { audio: {} });
  speaker(env).dispatch('click');
  await tick(10);
  assert.equal(env.audio.played.length, 1);
  // The tool down: the roster lists no server voice, and the device reads.
  env = await loadChat(hubWith({ agents: readAloudAgents, history: hist }), { speech: { voices: [LOCAL] }, audio: {} });
  speaker(env).dispatch('click');
  await tick(10);
  assert.equal(speaks(env).length, 0);
  assert.equal(env.speech.spoken.length, 1);
  // No Web Audio: the device reads.
  env = await loadChat(hubWith({ agents: serverAgents(), history: hist }), { speech: { voices: [LOCAL] } });
  speaker(env).dispatch('click');
  await tick(10);
  assert.equal(speaks(env).length, 0);
  assert.equal(env.speech.spoken.length, 1);
});

test('mic: a second tap while the permission prompt is up opens no second stream; one chunk at the stop', async () => {
  const env = await loadChat(hubWith({ agents: withVoice([BOSS()]) }), { media: {} });
  env.els.mic.dispatch('click');
  env.els.mic.dispatch('click');
  await tick(5);
  assert.equal(env.media.streams, 1);
  assert.equal(env.media.recorders.length, 1);
  assert.equal(env.media.recorders[0].slice, undefined, 'no timeslice (Safari writes fragmented MP4 for chunks)');
});

test('mic: hiding the page while recording discards the clip and releases the microphone', async () => {
  const env = await loadChat(hubWith({ agents: withVoice([BOSS()]) }), { media: {} });
  env.els.mic.dispatch('click');
  await tick(5);
  assert.equal(env.els.mic.className, 'recording');
  await env.hide();
  await tick(20);
  assert.equal(env.media.recorders[0].state, 'inactive');
  assert.equal(env.media.stopped, 1, 'the microphone is released');
  assert.equal(transcribes(env).length, 0, 'the clip is not sent');
  assert.equal(env.els.mic.className, '');
  assert.equal(env.els.voice.hidden, true);
  assert.match(env.els.error.textContent, /page was hidden; nothing was sent/);
  assert.equal(env.intervals.filter((i) => i.ms === 500).length, 0, 'the tick is cleared');
});

test('mic: pagehide while recording discards the clip; with no recording it says nothing', async () => {
  const env = await loadChat(hubWith({ agents: withVoice([BOSS()]) }), { media: {} });
  await env.pagehide();
  assert.equal(env.els.error.textContent, '');
  env.els.mic.dispatch('click');
  await tick(5);
  await env.pagehide();
  await tick(20);
  assert.equal(env.media.stopped, 1);
  assert.equal(transcribes(env).length, 0);
});

test('mic: a page hidden while the permission prompt was up does not start recording', async () => {
  const env = await loadChat(hubWith({ agents: withVoice([BOSS()]) }), { media: {} });
  env.document.visibilityState = 'hidden';
  env.els.mic.dispatch('click');
  await tick(5);
  assert.equal(env.media.recorders.length, 0);
  assert.equal(env.media.stopped, 1, 'the stream is released');
});

test('read-aloud: pagehide stops reading; hiding the page alone does not', async () => {
  const env = await loadChat(hubWith({ agents: readAloudAgents, history: () => ({ status: 200, body: { messages: [msg(1)] } }) }), { speech: { voices: [LOCAL] } });
  const b = env.els.list.children[0].children.find((c) => c.className === 'speak');
  b.dispatch('click');
  assert.equal(b.textContent, '⏹');
  await env.hide();
  assert.equal(env.speech.cancels, 0, 'a backgrounded page may go on reading');
  await env.pagehide();
  assert.equal(env.speech.cancels, 1);
  assert.equal(b.textContent, '🔊');
});

test('new conversation: offered on the operator\'s manager chat only', async () => {
  const offered = () => roster([BOSS(), A('w1')], { newConversation: true });
  let env = await loadChat(hubWith({ agents: offered }));
  assert.equal(env.els.newconv.hidden, false);
  env = await load(hubWith({ agents: offered }), { store: { 'lever-chat-open': 'w1' } });
  assert.equal(env.els.newconv.hidden, true, 'a worker\'s chat');
  env = await loadChat(hubWith({ agents: () => roster([BOSS(), A('w1')], { newConversation: true, tier: 'contact' }) }));
  assert.equal(env.els.newconv.hidden, true, 'a contact');
  env = await loadChat(hubWith());
  assert.equal(env.els.newconv.hidden, true, 'not offered by the server');
});

test('new conversation: asked, posted, followed by its status until it ends; the draft stays', async () => {
  let state = 'running';
  let status = { state: 'running' };
  const statusBodies = [];
  const env = await loadChat(hubWith({
    agents: () => roster([BOSS({ state, activity: '' }), A('w1')], { newConversation: true }),
    newConversation: () => {
      state = 'suspended';
      return { status: 202, body: { state: 'starting', id: 'op1' } };
    },
    newConversationStatus: (body) => {
      statusBodies.push(body);
      return { status: 200, body: status };
    },
  }));
  await type(env, 'half typed');
  env.els.newconv.dispatch('click');
  await tick(5);
  assert.equal(env.confirms.length, 1);
  assert.match(env.confirms[0], /new conversation/);
  assert.equal(env.count('POST', '/lever/api/new-conversation'), 1);
  assert.equal(env.calls.find((c) => c.path === '/lever/api/new-conversation').body, undefined, 'no force unless asked');
  assert.equal(env.els.newconv.disabled, true);
  await env.runTimers(3000);
  assert.equal(env.els.note.textContent, 'starting boss on a new conversation…');
  state = 'stopped';
  await env.runTimers(3000);
  state = 'running';
  status = { state: 'ok', backup: '20261019T054104Z' };
  await env.runTimers(3000);
  await tick(5);
  assert.deepEqual(statusBodies, [{ id: 'op1' }, { id: 'op1' }, { id: 'op1' }]);
  assert.equal(env.els.newconv.disabled, false);
  assert.equal(env.els.error.hidden, true);
  assert.equal(env.els.note.textContent, 'boss is on a new conversation. The old one is backup 20261019T054104Z: `lever conversation restore 20261019T054104Z` on the host puts it back.');
  assert.equal(env.els.text.value, 'half typed');
  assert.equal(env.store['lever-chat-draft:boss'], 'half typed');
  assert.equal(sends(env).length, 0);
  // The note stays through list reads, and goes with the next send.
  await env.poll();
  assert.match(env.els.note.textContent, /backup 20261019T054104Z/);
  env.els.composer.dispatch('submit');
  await tick(10);
  assert.equal(sends(env).length, 1);
  assert.ok(!env.els.note.textContent.includes('backup'), env.els.note.textContent);
});

test('new conversation: a stop and start between two list reads is a success, not an error', async () => {
  // The list never shows the manager down: lever stopped and started it in
  // about a second, between two reads.
  const env = await loadChat(hubWith({ agents: () => roster([BOSS({ activity: '' })], { newConversation: true }) }));
  env.els.newconv.dispatch('click');
  await tick(5);
  for (let i = 0; i < 25; i++) await env.runTimers(3000);
  await tick(5);
  assert.equal(env.els.error.hidden, true, env.els.error.textContent);
  assert.match(env.els.note.textContent, /^boss is on a new conversation\. The old one is backup 20261019T054104Z/);
  assert.equal(env.count('POST', '/lever/api/new-conversation/status'), 1, 'one read once it ended');
  assert.equal(env.els.newconv.disabled, false);
});

test('new conversation: declined, nothing is posted', async () => {
  const env = await loadChat(hubWith({ agents: () => roster([BOSS()], { newConversation: true }) }));
  env.confirmAnswers.push(false);
  env.els.newconv.dispatch('click');
  await tick(5);
  assert.equal(env.count('POST', '/lever/api/new-conversation'), 0);
});

test('new conversation: mid-turn is asked again and only then forced', async () => {
  const bodies = [];
  const env = await loadChat(hubWith({
    agents: () => roster([BOSS()], { newConversation: true }),
    newConversation: (body) => {
      bodies.push(body);
      return body && body.force ? { status: 202, body: {} } : { status: 409, body: { error: 'busy' } };
    },
  }));
  env.els.newconv.dispatch('click');
  await tick(5);
  assert.equal(env.confirms.length, 2);
  assert.match(env.confirms[1], /middle of a turn/);
  assert.deepEqual(bodies, [undefined, { force: true }]);

  const env2 = await loadChat(hubWith({ agents: () => roster([BOSS()], { newConversation: true }), newConversation: () => ({ status: 409, body: { error: 'busy' } }) }));
  env2.confirmAnswers.push(true, false);
  env2.els.newconv.dispatch('click');
  await tick(5);
  assert.equal(env2.count('POST', '/lever/api/new-conversation'), 1, 'not forced when declined');
  assert.equal(env2.els.newconv.disabled, false);
});

test('new conversation: a refusal shows fixed text', async () => {
  const env = await loadChat(hubWith({ agents: () => roster([BOSS()], { newConversation: true }), newConversation: () => ({ status: 409, body: { error: 'in-progress' } }) }));
  env.els.newconv.dispatch('click');
  await tick(5);
  assert.equal(env.els.error.textContent, 'lever is already changing the manager (a new conversation, a restore, or lever up, apply, stop or reload). Try again when it is done.');
  assert.equal(env.els.newconv.disabled, false);
});

test('new conversation: a failure after the answer shows its fixed text', async () => {
  const env = await loadChat(hubWith({
    agents: () => roster([BOSS({ activity: '' })], { newConversation: true }),
    newConversationStatus: () => ({ status: 200, body: { state: 'failed', error: 'failed', backup: '20261019T054104Z' } }),
  }));
  env.els.newconv.dispatch('click');
  await tick(5);
  await env.runTimers(3000);
  await tick(5);
  assert.equal(env.els.error.textContent, 'lever could not start the new conversation; `lever doctor` on the host shows why.');
  assert.ok(!env.els.note.textContent.includes('backup'));
  assert.equal(env.els.newconv.disabled, false);
});

test('new conversation: a failure that ends while another chat is open shows when the manager\'s opens again', async () => {
  const env = await loadChat(hubWith({
    agents: () => roster([BOSS({ activity: '' }), A('w1')], { newConversation: true }),
    newConversationStatus: () => ({ status: 200, body: { state: 'failed', error: 'failed' } }),
  }));
  env.els.newconv.dispatch('click');
  await tick(5);
  env.els.agents.children[1].children[0].dispatch('click');
  await tick(5);
  await env.runTimers(3000);
  await tick(5);
  assert.equal(env.els.error.hidden, true, 'not on the worker\'s chat');
  env.els.agents.children[0].children[0].dispatch('click');
  await tick(5);
  assert.match(env.els.error.textContent, /could not start the new conversation/);
});

test('new conversation: a status read that fails is read again; a lost change is a note, not an error', async () => {
  const answers = [{ status: 0, body: 'cannot reach the server' }, { status: 502, body: 'bad gateway' }, { status: 404, body: { error: 'unknown' } }];
  const env = await loadChat(hubWith({
    agents: () => roster([BOSS({ activity: '' })], { newConversation: true }),
    newConversationStatus: () => answers.shift(),
  }));
  env.els.newconv.dispatch('click');
  await tick(5);
  for (let i = 0; i < 3; i++) await env.runTimers(3000);
  await tick(5);
  assert.equal(env.count('POST', '/lever/api/new-conversation/status'), 3);
  assert.equal(env.els.error.hidden, true);
  assert.match(env.els.note.textContent, /lost track of boss's new conversation/);
});

test('new conversation: a change that never ends is a note after the wait', async () => {
  const env = await loadChat(hubWith({
    agents: () => roster([BOSS({ activity: '' })], { newConversation: true }),
    newConversationStatus: () => ({ status: 200, body: { state: 'running' } }),
  }));
  env.els.newconv.dispatch('click');
  await tick(5);
  for (let i = 0; i < 139; i++) await env.runTimers(3000);
  assert.equal(env.els.note.textContent, 'starting boss on a new conversation…', 'not before 7 min');
  await env.runTimers(3000);
  await tick(5);
  assert.equal(env.els.error.hidden, true);
  assert.match(env.els.note.textContent, /lost track/);
  assert.equal(env.els.newconv.disabled, false);
});

// Device options in the settings panel: auto-send for dictation, and reading
// new replies aloud by themselves. Both are kept per login on the device.
const bothAgents = (agents = [BOSS(), A('w1')]) => () => roster(agents, { voice: { maxSeconds: 300 }, readAloud: true });
const AUTO_SEND = 'lever-auto-send:op';
const AUTO_READ = 'lever-auto-read:op';
async function dictate(env) {
  env.els.mic.dispatch('click');
  await tick(5);
  env.els.mic.dispatch('click');
  await tick(20);
}

test('settings: the panel opens and closes as a disclosure, and offers only what applies', async () => {
  let env = await loadChat(hubWith({ agents: bothAgents() }), { media: {}, speech: { voices: [LOCAL] } });
  assert.equal(env.els.settings.hidden, false);
  assert.equal(env.els.settingspanel.hidden, true);
  assert.equal(env.els.readvoicerow.hidden, true, 'one voice: nothing to choose');
  assert.equal(env.els.autosendrow.hidden, false);
  assert.equal(env.els.autoreadrow.hidden, false);
  env.els.settings.dispatch('click');
  assert.equal(env.els.settingspanel.hidden, false);
  assert.equal(env.els.settings.attrs['aria-expanded'], 'true');
  assert.equal(env.focused(), env.els.autosend, 'focus moves to the first option shown');
  env.document.dispatch('keydown', { key: 'Escape' });
  assert.equal(env.els.settingspanel.hidden, true);
  assert.equal(env.els.settings.attrs['aria-expanded'], 'false');
  assert.equal(env.focused(), env.els.settings, 'Escape gives focus back to the button');
  env.els.settings.dispatch('click');
  env.document.dispatch('pointerdown', { target: env.els.autosend });
  assert.equal(env.els.settingspanel.hidden, true, 'the panel in the fake holds no children: any target is outside');
  env.els.settings.dispatch('click');
  env.els.settingspanel.append(env.els.autosend);
  env.document.dispatch('pointerdown', { target: env.els.autosend });
  assert.equal(env.els.settingspanel.hidden, false, 'a tap inside keeps it open');
  env.document.dispatch('pointerdown', { target: env.els.list });
  assert.equal(env.els.settingspanel.hidden, true, 'a tap outside closes it');
  env.els.settings.dispatch('click');
  env.els.settings.dispatch('click');
  assert.equal(env.els.settingspanel.hidden, true, 'a second tap on the button closes it');
  // No dictation for the login, or a browser that cannot record: no auto-send.
  env = await loadChat(hubWith({ agents: readAloudAgents }), { media: {}, speech: { voices: [LOCAL] } });
  assert.equal(env.els.autosendrow.hidden, true);
  assert.equal(env.els.autoreadrow.hidden, false);
  env = await loadChat(hubWith({ agents: bothAgents() }));
  assert.equal(env.els.settings.hidden, true, 'no recording and no speech here: nothing to set');
  // A see-only agent: nothing applies, and an open panel closes.
  env = await loadChat(hubWith({ agents: bothAgents([BOSS(), A('w1', { access: 'see' })]) }), { media: {}, speech: { voices: [LOCAL] } });
  env.els.settings.dispatch('click');
  env.els.agents.children[1].children[0].dispatch('click');
  assert.equal(env.els.settings.hidden, true);
  assert.equal(env.els.settingspanel.hidden, true);
});

test('settings: kept per login on the device, and working for the visit when storage throws', async () => {
  let env = await loadChat(hubWith({ agents: bothAgents() }), { media: {}, speech: { voices: [LOCAL] } });
  env.els.autosend.checked = true;
  env.els.autosend.dispatch('change');
  env.els.autoread.checked = true;
  env.els.autoread.dispatch('change');
  assert.equal(env.local[AUTO_SEND], '1');
  assert.equal(env.local[AUTO_READ], '1');
  assert.match(env.els.mic.className, /\bautosend\b/, 'the mic shows auto-send is on');
  assert.match(env.els.mic.attrs['aria-label'], /^Dictate and send/);
  env.els.autosend.checked = false;
  env.els.autosend.dispatch('change');
  assert.equal(env.local[AUTO_SEND], '0');
  assert.doesNotMatch(env.els.mic.className, /autosend/);
  assert.match(env.els.mic.attrs['aria-label'], /^Dictate: tap/);
  // A reload keeps them; another login on the device does not see them.
  env = await loadChat(hubWith({ agents: bothAgents() }), { media: {}, speech: { voices: [LOCAL] }, local: { [AUTO_SEND]: '1', [AUTO_READ]: '1' } });
  assert.equal(env.els.autosend.checked, true);
  assert.equal(env.els.autoread.checked, true);
  env = await loadChat(hubWith({ agents: () => ({ ...bothAgents()(), body: { ...bothAgents()().body, login: 'c@x', tier: 'contact' } }) }),
    { media: {}, speech: { voices: [LOCAL] }, local: { [AUTO_SEND]: '1', [AUTO_READ]: '1' } });
  assert.equal(env.els.autosend.checked, false);
  assert.equal(env.els.autoread.checked, false);
  // Storage off: the page works, and a choice lasts for the visit.
  env = await loadChat(hubWith({ agents: bothAgents() }), { media: {}, speech: { voices: [LOCAL] }, localThrows: true });
  assert.equal(env.els.autosend.checked, false);
  env.els.autosend.checked = true;
  env.els.autosend.dispatch('change');
  assert.match(env.els.mic.className, /autosend/);
  await type(env, 'typed');
  await dictate(env);
  assert.equal(sends(env).length, 1);
});

test('auto-send off: the transcript waits in the box', async () => {
  const env = await loadChat(hubWith({ agents: bothAgents() }), { media: {} });
  await type(env, 'typed');
  await dictate(env);
  assert.equal(env.els.text.value, 'typed hello from the mic');
  assert.equal(sends(env).length, 0);
});

test('auto-send on: the transcript goes after the typed text and the whole box is sent', async () => {
  const env = await loadChat(hubWith({ agents: bothAgents() }), { media: {}, local: { [AUTO_SEND]: '1' } });
  await type(env, 'Look at this:  ');
  env.els.text.selectionStart = 0;
  env.els.text.selectionEnd = 0;
  await dictate(env);
  assert.equal(sends(env).length, 1);
  assert.equal(sends(env)[0].body.content, 'Look at this: hello from the mic', 'appended, not put at the cursor');
  assert.equal(sends(env)[0].path, `${HISTORY}`);
  assert.equal(env.els.text.value, '', 'sent: the box is clear');
  assert.equal(env.store['lever-chat-draft:boss'], '');
  // With an empty box only the dictation goes.
  await dictate(env);
  assert.equal(sends(env)[1].body.content, 'hello from the mic');
});

test('auto-send on: an empty or failed transcription sends nothing and says why', async () => {
  for (const [answer, words] of [[{ status: 200, body: { text: '' } }, 'Nothing was heard in the recording.'],
    [{ status: 429, body: { error: 'quota' } }, 'Not transcribed: too much dictation today; try again tomorrow'], [{ down: true }, /^Not transcribed: /]]) {
    const env = await loadChat(hubWith({ agents: bothAgents(), transcribe: () => answer }), { media: {}, local: { [AUTO_SEND]: '1' } });
    await type(env, 'typed');
    await dictate(env);
    assert.equal(sends(env).length, 0, JSON.stringify(answer));
    if (typeof words === 'string') assert.equal(env.els.error.textContent, words);
    else assert.match(env.els.error.textContent, words);
    assert.equal(env.els.text.value, 'typed');
  }
  // A clip too short to hold a word is not even transcribed.
  const env = await loadChat(hubWith({ agents: bothAgents() }), { media: { decoded: { length: 100, sampleRate: 48000 } }, local: { [AUTO_SEND]: '1' } });
  await dictate(env);
  assert.equal(transcribes(env).length, 0);
  assert.equal(sends(env).length, 0);
});

test('auto-send on: a chat changed while transcribing sends nothing; the text goes to the first chat\'s draft', async () => {
  let release;
  const slow = () => new Promise((r) => { release = () => r({ status: 200, body: { text: 'for boss only' } }); });
  // Another chat opened meanwhile.
  let env = await loadChat(hubWith({ agents: bothAgents(), transcribe: slow }), { media: {}, local: { [AUTO_SEND]: '1' } });
  await type(env, 'boss draft');
  await dictate(env);
  env.els.agents.children[1].children[0].dispatch('click');
  await tick(5);
  assert.equal(env.els.agent.textContent, 'w1');
  release();
  await tick(20);
  assert.equal(sends(env).length, 0);
  assert.equal(env.els.text.value, '', 'w1\'s box is untouched');
  assert.equal(env.store['lever-chat-draft:boss'], 'boss draft for boss only');
  // The same chat left and opened again is not the chat the recording
  // started in: the text waits in the box.
  env = await loadChat(hubWith({ agents: bothAgents(), transcribe: slow }), { media: {}, local: { [AUTO_SEND]: '1' } });
  await dictate(env);
  env.els.back.dispatch('click');
  env.els.agents.children[0].children[0].dispatch('click');
  await tick(5);
  release();
  await tick(20);
  assert.equal(sends(env).length, 0);
  assert.equal(env.els.text.value, 'for boss only');
  // Turned off while transcribing: nothing is sent.
  env = await loadChat(hubWith({ agents: bothAgents(), transcribe: slow }), { media: {}, local: { [AUTO_SEND]: '1' } });
  await dictate(env);
  env.els.autosend.checked = false;
  env.els.autosend.dispatch('change');
  release();
  await tick(20);
  assert.equal(sends(env).length, 0);
  assert.equal(env.els.text.value, 'for boss only');
});

// replyArrives: the hub's newest page now holds more messages, and a poll
// reads it.
async function replyArrives(env, hub, list) {
  hub.parts.history = () => ({ status: 200, body: { messages: list } });
  await env.poll();
}

test('auto read-aloud: only new replies, after the first tap, one after another', async () => {
  const hub = hubWith({ agents: bothAgents(), history: () => ({ status: 200, body: { messages: [msg(1)] } }) });
  const env = await loadChat(hub, { speech: { voices: [LOCAL] }, local: { [AUTO_READ]: '1' } });
  assert.equal(env.speech.spoken.length, 0, 'the history loaded on open is not read');
  await replyArrives(env, hub, [msg(2), msg(1)]);
  assert.equal(env.speech.spoken.length, 0, 'no tap yet: nothing may play');
  assert.equal(env.els.readhint.hidden, false, 'the page says a tap is needed');
  env.document.dispatch('click');
  await tick(5);
  assert.equal(env.els.readhint.hidden, true);
  assert.deepEqual(env.speech.spoken.map((u) => u.text), ['', 'msg 2'], 'the tap wakes the device voice, then the waiting reply plays');
  await replyArrives(env, hub, [msg(5), msg(4, { senderId: 'u1' }), msg(3), msg(2), msg(1)]);
  assert.deepEqual(env.speech.spoken.map((u) => u.text), ['', 'msg 2'], 'queued behind the one playing; the login\'s own message is never read');
  env.endSpeech();
  await tick(5);
  assert.deepEqual(env.speech.spoken.map((u) => u.text).slice(2), ['msg 3']);
  env.endSpeech();
  await tick(5);
  assert.deepEqual(env.speech.spoken.map((u) => u.text).slice(2), ['msg 3', 'msg 5']);
  env.endSpeech();
  await tick(5);
  assert.equal(env.speech.spoken.length, 4);
  // The hint shows once a visit.
  await replyArrives(env, hub, [msg(6), msg(5), msg(4, { senderId: 'u1' }), msg(3), msg(2), msg(1)]);
  assert.equal(env.els.readhint.hidden, true);
});

test('auto read-aloud: never history paged back, a restart, another chat, or with the option off', async () => {
  const hub = hubWith({ agents: bothAgents(), history: (path) => (path.includes('cursor=C1')
    ? { status: 200, body: { messages: [msg(2), msg(1)] } } : { status: 200, body: { messages: [msg(10)], nextCursor: 'C1' } }) });
  let env = await loadChat(hub, { speech: { voices: [LOCAL] }, local: { [AUTO_READ]: '1' } });
  env.document.dispatch('click');
  env.els.older.dispatch('click');
  await tick(10);
  assert.equal(env.rows().length, 3);
  assert.deepEqual(env.speech.spoken.map((u) => u.text), [''], 'older messages are not read');
  // A full page with nothing held is a restart: none of it is read.
  await replyArrives(env, hub, page(200));
  assert.deepEqual(env.speech.spoken.map((u) => u.text), ['']);
  // The option off: a new reply waits for a tap on its speaker.
  env = await loadChat(hub, { speech: { voices: [LOCAL] } });
  env.document.dispatch('click');
  await replyArrives(env, hub, [msg(11), msg(10)]);
  assert.equal(env.speech.spoken.length, 0);
  // Another chat: w1's reply is not boss's, and changing chat drops the queue.
  const h2 = hubWith({ agents: bothAgents(), history: () => ({ status: 200, body: { messages: [msg(1)] } }) });
  env = await loadChat(h2, { speech: { voices: [LOCAL] }, local: { [AUTO_READ]: '1' } });
  await replyArrives(env, h2, [msg(3), msg(2), msg(1)]);
  env.els.agents.children[1].children[0].dispatch('click');
  await tick(5);
  env.document.dispatch('click');
  await tick(5);
  assert.deepEqual(env.speech.spoken.map((u) => u.text), [''], 'the replies queued for boss went with its chat');
});

test('auto read-aloud: a mic tap or a speaker tap stops it and empties the queue', async () => {
  const hub = hubWith({ agents: bothAgents(), history: () => ({ status: 200, body: { messages: [msg(1)] } }) });
  let env = await loadChat(hub, { media: {}, speech: { voices: [LOCAL] }, local: { [AUTO_READ]: '1' } });
  env.document.dispatch('click');
  await replyArrives(env, hub, [msg(3), msg(2), msg(1)]);
  assert.deepEqual(env.speech.spoken.map((u) => u.text), ['', 'msg 2']);
  env.els.mic.dispatch('click');
  await tick(5);
  assert.equal(env.speech.cancels, 1, 'reading stops as recording starts');
  env.endSpeech();
  await tick(5);
  assert.equal(env.speech.spoken.length, 2, 'msg 3 is not read');
  // A reply that arrives while recording is not read either.
  await replyArrives(env, hub, [msg(4), msg(3), msg(2), msg(1)]);
  assert.equal(env.speech.spoken.length, 2);
  env.els.mic.dispatch('click');
  await tick(20);
  // A speaker tap: that message plays, the rest of the queue goes.
  hub.parts.history = () => ({ status: 200, body: { messages: [msg(1)] } });
  env = await loadChat(hub, { speech: { voices: [LOCAL] }, local: { [AUTO_READ]: '1' } });
  env.document.dispatch('click');
  await replyArrives(env, hub, [msg(3), msg(2), msg(1)]);
  speaker(env, 0).dispatch('click');
  assert.deepEqual(env.speech.spoken.map((u) => u.text), ['', 'msg 2', 'msg 1']);
  env.endSpeech();
  await tick(5);
  assert.equal(env.speech.spoken.length, 3, 'msg 3 was dropped from the queue');
  // Turning the option off stops it too.
  await replyArrives(env, hub, [msg(5), msg(4), msg(3), msg(2), msg(1)]);
  assert.equal(env.speech.spoken.length, 4);
  env.els.autoread.checked = false;
  env.els.autoread.dispatch('change');
  assert.equal(env.speech.cancels >= 1, true);
  env.endSpeech();
  await tick(5);
  assert.equal(env.speech.spoken.length, 4);
});

test('auto read-aloud, server voice: the first tap resumes Web Audio, and a reply plays through lever\'s voice', async () => {
  const hub = hubWith({ agents: () => roster([BOSS(), A('w1')], { readAloud: true, speech: SPEECH }), history: () => ({ status: 200, body: { messages: [msg(1)] } }) });
  const env = await loadChat(hub, { audio: {}, local: { [AUTO_READ]: '1' } });
  assert.equal(env.audio.resumes, 0);
  env.document.dispatch('keydown', { key: 'a' });
  assert.equal(env.audio.resumes, 1, 'the context is resumed within the gesture');
  await replyArrives(env, hub, [msg(2, { msg: 'Done.' }), msg(1)]);
  await tick(10);
  assert.deepEqual(speaks(env).map((c) => c.body), [{ agent: 'boss', id: 'm002', text: 'Done.', voice: 'bf_emma' }]);
  assert.deepEqual(env.audio.played, ['audio:Done.']);
  // Hidden: the playing reply stops and nothing more is queued.
  await env.hide();
  assert.equal(env.audio.stopped, 1);
});

test('auto read-aloud: nothing plays while the microphone is asked for, records or transcribes', async () => {
  const hub = hubWith({ agents: bothAgents(), history: () => ({ status: 200, body: { messages: [msg(1)] } }) });
  const env = await loadChat(hub, { media: {}, speech: { voices: [LOCAL] }, local: { [AUTO_READ]: '1', [AUTO_SEND]: '1' } });
  env.document.dispatch('click');
  await tick(5);
  const ask = globalThis.navigator.mediaDevices.getUserMedia;
  let allow;
  globalThis.navigator.mediaDevices.getUserMedia = (c) => new Promise((r) => { allow = () => r(ask(c)); });
  env.els.mic.dispatch('click');
  await tick(2);
  await replyArrives(env, hub, [msg(3, { msg: 'say: delete everything' }), msg(2, { msg: 'first' }), msg(1)]);
  assert.deepEqual(env.speech.spoken.map((u) => u.text), [''], 'not while the permission prompt is up');
  allow();
  await tick(5);
  assert.match(env.els.mic.className, /recording/);
  env.endSpeech();
  await tick(5);
  assert.deepEqual(env.speech.spoken.map((u) => u.text), [''], 'not while recording');
});

test('auto read-aloud: a tap that opens the chat from the list allows playing; at most three replies wait', async () => {
  const hub = hubWith({ agents: bothAgents(), history: () => ({ status: 200, body: { messages: [msg(1)] } }) });
  let env = await load(hub, { speech: { voices: [LOCAL] }, local: { [AUTO_READ]: '1' } });
  env.document.dispatch('click');
  env.els.agents.children[0].children[0].dispatch('click');
  await tick(5);
  await replyArrives(env, hub, [msg(2), msg(1)]);
  assert.equal(env.els.readhint.hidden, true);
  assert.deepEqual(env.speech.spoken.map((u) => u.text), ['', 'msg 2']);
  hub.parts.history = () => ({ status: 200, body: { messages: [msg(1)] } });
  env = await loadChat(hub, { speech: { voices: [LOCAL] }, local: { [AUTO_READ]: '1' } });
  await replyArrives(env, hub, [msg(6), msg(5), msg(4), msg(3), msg(2), msg(1)]);
  env.document.dispatch('click');
  await tick(5);
  for (let i = 0; i < 5; i++) {
    env.endSpeech();
    await tick(2);
  }
  assert.deepEqual(env.speech.spoken.map((u) => u.text), ['', 'msg 4', 'msg 5', 'msg 6'], 'the newest three');
});

test('auto-send: after a send with no clear answer, or with the chat unable to take a message, the dictation waits', async () => {
  let n = 0;
  const hub = hubWith({ agents: bothAgents(), post: (body) => (++n === 1 ? { down: true } : { status: 201, body: { id: `s${n}`, content: body.content, senderId: 'u1', createdAt: new Date().toISOString() } }) });
  let env = await loadChat(hub, { media: {}, local: { [AUTO_SEND]: '1' } });
  await type(env, 'transfer the money');
  env.els.composer.dispatch('submit');
  await tick(20);
  await dictate(env);
  assert.equal(sends(env).length, 1, 'only the first, unclear attempt');
  assert.equal(env.els.text.value, 'transfer the money hello from the mic');
  assert.match(env.els.error.textContent, /may not have arrived/);
  // The list reads the agent as not taking messages by the time the clip is back.
  let release;
  const h2 = hubWith({ agents: bothAgents(), transcribe: () => new Promise((r) => { release = () => r({ status: 200, body: { text: 'hi' } }); }) });
  env = await loadChat(h2, { media: {}, local: { [AUTO_SEND]: '1' } });
  await dictate(env);
  h2.parts.agents = bothAgents([BOSS({ state: 'error' }), A('w1')]);
  await env.poll();
  release();
  await tick(20);
  assert.equal(sends(env).length, 0);
  assert.equal(env.els.text.value, 'hi');
});

const activityCalls = (env) => env.calls.filter((c) => c.path.endsWith('/activity'));

test('activity: an operator gets the button for a message agent with a record, nothing else does', async () => {
  let env = await loadChat(hubWith());
  assert.equal(env.els.activity.hidden, false);
  assert.equal(env.els.activitypanel.hidden, true, 'closed until asked');
  assert.equal(activityCalls(env).length, 0, 'nothing is read until asked');
  env = await loadChat(hubWith({ agents: () => roster([BOSS()], { tier: 'contact' }) }));
  assert.equal(env.els.activity.hidden, true, 'a contact gets no button');
  env = await loadChat(hubWith({ agents: () => roster([BOSS({ access: 'see' })]) }));
  assert.equal(env.els.activity.hidden, true, 'a see-only agent');
  env = await loadChat(hubWith({ agents: () => roster([BOSS({ id: '', conversation: '' })]) }));
  assert.equal(env.els.activity.hidden, true, 'no hub record');
  env = await load(hubWith());
  assert.equal(env.els.activity.hidden, true, 'no chat open');
});

test('activity: the button reads the terminal with a POST and writes it as text', async () => {
  const evil = '<img src=x onerror=alert(1)>\u001b[31mred\u0007\u202eflip';
  const env = await loadChat(hubWith({ activity: () => ({ status: 200, body: { text: evil } }) }));
  env.els.activity.dispatch('click');
  await tick(5);
  const calls = activityCalls(env);
  assert.deepEqual(calls.map((c) => `${c.method} ${c.path}`), ['POST /lever/api/agents/boss/activity']);
  assert.equal(env.els.activitypanel.hidden, false);
  assert.equal(env.els.activity.attrs['aria-expanded'], 'true');
  assert.equal(env.els.activitytext.hidden, false);
  assert.equal(env.els.activitytext.children.length, 0, 'text, never nodes');
  assert.equal(env.els.activitytext.textContent, '<img src=x onerror=alert(1)>[31mredflip');
  assert.equal(env.els.activitynote.hidden, true);
  assert.notEqual(env.els.activitywhen.textContent, '');
  env.els.activityreload.dispatch('click');
  await tick(5);
  assert.equal(activityCalls(env).length, 2, 'read again on demand');
  env.els.activity.dispatch('click');
  assert.equal(env.els.activitypanel.hidden, true);
  assert.equal(env.els.activity.attrs['aria-expanded'], 'false');
  assert.equal(env.els.activitytext.textContent, '', 'closing clears the text');
});

test('activity: refusals are fixed words, never the answer\'s text', async () => {
  for (const [res, want] of [
    [{ status: 409, body: { error: 'not-running', state: 'suspended' } }, 'The terminal cannot be read: boss is not running (asleep).'],
    [{ status: 409, body: { error: 'not-running', state: '<b>x</b>' } }, 'The terminal cannot be read: boss is not running (unknown).'],
    [{ status: 429, body: { error: 'rate-limited' } }, 'The terminal cannot be read: it was read a moment ago; try again in a few seconds.'],
    [{ status: 502, body: { error: 'too-large' } }, 'The terminal cannot be read: the terminal holds too much to show; open the Terminal.'],
    [{ status: 200, body: { text: 7 } }, 'The terminal cannot be read: request failed (HTTP 200).'],
    [{ down: true }, 'The terminal cannot be read: cannot reach the server.'],
  ]) {
    const env = await loadChat(hubWith({ activity: () => res }));
    env.els.activity.dispatch('click');
    await tick(5);
    assert.equal(env.els.activitynote.textContent, want);
    assert.equal(env.els.activitytext.hidden, true);
  }
  const env = await loadChat(hubWith({ activity: () => ({ status: 200, body: { text: '' } }) }));
  env.els.activity.dispatch('click');
  await tick(5);
  assert.equal(env.els.activitynote.textContent, 'The terminal is empty.');
});

test('activity: an answer for another chat is dropped, and the panel closes with the chat', async () => {
  let release;
  const gate = new Promise((r) => (release = r));
  const env = await loadChat(hubWith({ agents: () => roster([BOSS(), A('w1')]), activity: async () => {
    await gate;
    return { status: 200, body: { text: 'boss output' } };
  } }));
  env.els.activity.dispatch('click');
  await tick(1);
  env.els.back.dispatch('click');
  assert.equal(env.els.activity.hidden, true, 'no chat, no button');
  assert.equal(env.els.activitypanel.hidden, true);
  release();
  await tick(5);
  assert.equal(env.els.activitytext.textContent, '');
  assert.equal(env.els.activitypanel.hidden, true);
});

// The thinking dots. BOSS reports working by default; idle() is the same
// manager, idle.
const idle = () => roster([BOSS({ activity: 'idle' }), A('w1')]);

test('thinking: a stored send shows the dots at once, before any list read says working', async () => {
  const hub = hubWith({ agents: idle });
  const env = await loadChat(hub);
  assert.deepEqual(dots(env), []);
  await type(env, 'hello');
  env.els.composer.dispatch('submit');
  await tick(10);
  assert.deepEqual(dots(env), ['msg agent thinking: boss / working on a reply'], 'the dots, after the sent message');
  assert.match(env.rows().at(-2), /^msg mine: .*hello$/);
  const row = env.els.list.children.at(-1);
  assert.deepEqual(row.children[1].children.slice(1).map((d) => `${d.className} ${d.attrs['aria-hidden']}`), ['dot true', 'dot true', 'dot true']);
  // The agent's reply ends them.
  hub.parts.history = () => ({ status: 200, body: { messages: [msg(2, { createdAt: new Date(1.9e12).toISOString() }), { id: 'sent1', msg: 'hello', senderId: 'u1', createdAt: new Date(1.8e12).toISOString() }] } });
  env.streams[0].emit('update', { data: JSON.stringify({ subject: 'user.u1.chat.x' }) });
  await env.poll();
  assert.deepEqual(dots(env), []);
  assert.match(env.rows().at(-1), /^msg agent: boss .* \/ msg 2$/);
});

test('thinking: the dots time out when the agent never reports working', async () => {
  const env = await loadChat(hubWith({ agents: idle }));
  await type(env, 'hello');
  env.els.composer.dispatch('submit');
  await tick(10);
  await env.poll();
  assert.equal(dots(env).length, 1, 'a list read before the time is up keeps them');
  await env.runTimers(Infinity);
  assert.deepEqual(dots(env), []);
});

test('thinking: working keeps the dots past the time', async () => {
  const hub = hubWith({ agents: idle });
  const env = await loadChat(hub);
  await type(env, 'hello');
  env.els.composer.dispatch('submit');
  await tick(10);
  hub.parts.agents = defaultAgents;
  await env.poll();
  await env.runTimers(Infinity);
  assert.equal(dots(env).length, 1, 'working: the dots stay while the agent says so');
  hub.parts.agents = idle;
  await env.poll();
  assert.deepEqual(dots(env), []);
});

test('thinking: idle after working ends the dots before the time is up', async () => {
  const hub = hubWith({ agents: idle });
  const env = await loadChat(hub);
  await type(env, 'hello');
  env.els.composer.dispatch('submit');
  await tick(10);
  hub.parts.agents = defaultAgents;
  await env.poll();
  hub.parts.agents = idle;
  await env.poll();
  assert.deepEqual(dots(env), [], 'it stopped working without a reply');
  assert.ok(env.timers.some((t) => t.ms === 30000), 'the time is not up yet');
});

test('thinking: an unknown state keeps a pending send\'s dots', async () => {
  const hub = hubWith({ agents: idle });
  const env = await loadChat(hub);
  await type(env, 'hello');
  env.els.composer.dispatch('submit');
  await tick(10);
  hub.parts.agents = defaultAgents;
  await env.poll();
  hub.parts.agents = () => ({ status: 502, body: '' });
  await env.poll();
  assert.equal(dots(env).length, 1);
});

test('thinking: the dots come and go without rebuilding the messages', async () => {
  const hub = hubWith({ agents: idle, history: () => ({ status: 200, body: { messages: [msg(2, { senderId: 'u1' }), msg(1)] } }) });
  const env = await loadChat(hub);
  assert.deepEqual(dots(env), []);
  const first = env.els.list.children[0];
  hub.parts.agents = defaultAgents;
  await env.poll();
  assert.equal(dots(env).length, 1);
  assert.equal(env.els.list.children[0], first, 'the same row: the log was not rebuilt');
  hub.parts.agents = idle;
  await env.poll();
  assert.deepEqual(dots(env), []);
  assert.equal(env.els.list.children[0], first);
});

test('thinking: an agent working on the last, unanswered message shows the dots after a reload', async () => {
  const mine = (over = {}) => ({ status: 200, body: { messages: [msg(2, { senderId: 'u1', ...over }), msg(1)] } });
  let env = await loadChat(hubWith({ history: () => mine() }));
  assert.equal(dots(env).length, 1);
  env = await loadChat(hubWith({ agents: idle, history: () => mine() }));
  assert.deepEqual(dots(env), [], 'idle: nothing is on its way');
  env = await loadChat(hubWith({ history: () => mine({ dispatchState: 'failed' }) }));
  assert.deepEqual(dots(env), [], 'a failed send waits for nobody');
  env = await loadChat(hubWith({ history: () => ({ status: 200, body: { messages: [msg(2), msg(1, { senderId: 'u1' })] } }) }));
  assert.deepEqual(dots(env), [], 'answered already');
});

test('thinking: the dots belong to the chat they were sent in', async () => {
  const hub = hubWith({ agents: idle });
  const env = await loadChat(hub);
  hub.parts.history = () => ({ status: 200, body: { messages: [{ id: 'sent1', msg: 'hello', senderId: 'u1', createdAt: new Date(1.8e12).toISOString() }] } });
  await type(env, 'hello');
  env.els.composer.dispatch('submit');
  await tick(10);
  assert.equal(dots(env).length, 1);
  env.els.back.dispatch('click');
  assert.deepEqual(dots(env), []);
  // Back within the time: the send is still on its way.
  openRow(env, 'boss');
  await tick(10);
  assert.equal(dots(env).length, 1);
});

test('thinking: activity counts while another chat is open', async () => {
  const hub = hubWith({ agents: idle });
  const env = await loadChat(hub);
  await type(env, 'hello');
  env.els.composer.dispatch('submit');
  await tick(10);
  hub.parts.history = () => ({ status: 200, body: { messages: [{ id: 'sent1', msg: 'hello', senderId: 'u1', createdAt: new Date(1.8e12).toISOString() }] } });
  env.els.back.dispatch('click');
  hub.parts.agents = defaultAgents;
  await env.poll();
  hub.parts.agents = idle;
  await env.poll();
  openRow(env, 'boss');
  await tick(10);
  assert.deepEqual(dots(env), [], 'it worked and stopped while the chat was closed');
});
