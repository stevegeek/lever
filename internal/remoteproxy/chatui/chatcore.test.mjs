import test from 'node:test';
import assert from 'node:assert/strict';
import {
  MAX_MESSAGE,
  classify,
  errorText,
  historyItems,
  isChatSubject,
  makeCoalescer,
  nextCursor,
  mergeMessages,
  messageLength,
  oneLine,
  messageText,
  sortedMessages,
  agentList,
  rowTitle,
  chipText,
  badgeText,
  inputView,
  askDraft,
  wakeText,
  WAKE_POLL_MS,
  WAKE_POLLS,
  LIST_MS,
  CONTACTS_MS,
  NOT_SHOWN,
  contactList,
  transcriptItems,
  transcriptPath,
  transcriptWho,
  mergeRows,
  viewErrorText,
} from './chatcore.js';

test('historyItems reads either key and drops junk', () => {
  assert.deepEqual(historyItems({ messages: [{ id: 'a' }, null, 'x', 7] }), [{ id: 'a' }]);
  assert.deepEqual(historyItems({ items: [{ id: 'b' }] }), [{ id: 'b' }]);
  for (const junk of [null, undefined, 'text', 4, [], {}, { messages: 'no' }]) {
    assert.deepEqual(historyItems(junk), []);
  }
});

test('nextCursor reads only a string from an object', () => {
  assert.equal(nextCursor({ nextCursor: 'c1' }), 'c1');
  for (const junk of [null, undefined, 'x', 3, {}, { nextCursor: 7 }, { nextCursor: null }]) assert.equal(nextCursor(junk), '');
});

test('messageText reads msg or content, and only strings', () => {
  assert.equal(messageText({ msg: 'hi' }), 'hi');
  assert.equal(messageText({ content: 'sent' }), 'sent');
  assert.equal(messageText({ msg: { html: '<b>' } }), '');
  assert.equal(messageText(null), '');
});

test('mergeMessages dedupes by id and reports change', () => {
  const map = new Map();
  assert.equal(mergeMessages(map, [{ id: '1', msg: 'a' }, { id: '1', msg: 'a' }, { msg: 'no id' }, null]), true);
  assert.equal(map.size, 1);
  assert.equal(mergeMessages(map, [{ id: '1', msg: 'a' }]), false);
  assert.equal(mergeMessages(map, [{ id: '1', msg: 'edited' }]), true);
  assert.equal(messageText(map.get('1')), 'edited');
  assert.equal(mergeMessages(map, [{ id: 7, msg: 'numeric id' }]), false);
});

test('mergeMessages takes a changed delivery state', () => {
  const map = new Map();
  mergeMessages(map, [{ id: 'm', msg: 'x', dispatchState: 'pending' }]);
  assert.equal(mergeMessages(map, [{ id: 'm', msg: 'x', dispatchState: 'failed' }]), true);
  assert.equal(map.get('m').dispatchState, 'failed');
});

test('messageLength counts characters like the hub, not UTF-16 units', () => {
  assert.equal(messageLength('abc'), 3);
  assert.equal(messageLength('a😀b'), 3);
  assert.equal('a😀b'.length, 4);
  assert.equal(messageLength(''), 0);
});

test('a send answer and its history row are the same message', () => {
  const map = new Map();
  mergeMessages(map, [{ id: 'm', content: 'hello' }]);
  assert.equal(mergeMessages(map, [{ id: 'm', msg: 'hello' }]), false);
});

test('sortedMessages orders by time, then id, whatever the arrival order', () => {
  const map = new Map();
  mergeMessages(map, [
    { id: 'c', msg: '3', createdAt: '2026-10-02T10:00:02Z' },
    { id: 'b', msg: '2', createdAt: '2026-10-02T10:00:01Z' },
    { id: 'a', msg: '1', createdAt: '2026-10-02T10:00:01Z' },
    { id: 'z', msg: '0', createdAt: 'not a date' },
  ]);
  assert.deepEqual(sortedMessages(map).map((m) => m.id), ['z', 'a', 'b', 'c']);
});

test('classify', () => {
  const uid = 'u1';
  assert.equal(classify({ id: '1', msg: 'x', senderId: 'u1', type: 'instruction' }, uid), 'mine');
  assert.equal(classify({ id: '2', msg: 'x', senderId: 'agent-id', type: 'assistant-reply' }, uid), 'agent');
  assert.equal(classify({ id: '3', msg: 'x', senderId: 'agent-id', type: 'something-new' }, uid), 'agent');
  assert.equal(classify({ id: '4', msg: 'working', type: 'state-change' }, uid), 'system');
  assert.equal(classify({ id: '4a', msg: 'note', type: 'system', senderId: 'agent-id' }, uid), 'system');
  assert.equal(classify({ id: '5', msg: 'x', type: 'mention', senderId: 'u1' }, uid), 'hidden');
  // The agent picks its own type: it cannot hide a message with one.
  assert.equal(classify({ id: '5a', msg: 'x', type: 'mention', senderId: 'agent-id' }, uid), 'agent');
  assert.equal(classify({ id: '5b', msg: 'x', type: 'state-change', senderId: 'u1' }, uid), 'mine');
  assert.equal(classify({ id: '6', msg: '' }, uid), 'hidden');
  assert.equal(classify({ id: '7', msg: 'x', senderId: '' }, ''), 'agent');
});

test('isChatSubject admits only this user', () => {
  assert.equal(isChatSubject('user.u1.chat.dm', 'u1'), true);
  assert.equal(isChatSubject('user.u1.chat.message.edited', 'u1'), true);
  assert.equal(isChatSubject('user.u10.chat.dm', 'u1'), false);
  assert.equal(isChatSubject('user.u1.notification', 'u1'), false);
  assert.equal(isChatSubject('user..chat.dm', ''), false);
  assert.equal(isChatSubject(undefined, 'u1'), false);
});

test('errorText prefers the hub message and bounds it', () => {
  // The hub's two real error shapes: an API error, and the session 401.
  assert.equal(errorText(400, { error: { code: 'validation', message: 'too long' } }), 'too long (HTTP 400)');
  assert.equal(errorText(401, { error: 'authentication required' }), 'authentication required (HTTP 401)');
  assert.equal(errorText(403, { message: 'refused' }), 'refused (HTTP 403)');
  assert.equal(errorText(502, 'bad gateway\n'), 'bad gateway (HTTP 502)');
  assert.equal(errorText(500, null), 'request failed (HTTP 500)');
  assert.equal(errorText(500, { error: { message: 7 } }), 'request failed (HTTP 500)');
  assert.equal(errorText(0, 'cannot reach the server'), 'cannot reach the server');
  assert.equal(errorText(0, null), 'no answer');
  // A body cannot break out of the sentence it is quoted in.
  assert.equal(errorText(400, 'x)\n\nSent.\r\n\tAll good'), 'x) Sent. All good (HTTP 400)');
  assert.equal(errorText(400, { error: { message: 'a' + '\n'.repeat(298) + 'b' } }), 'a b (HTTP 400)');
  assert.ok(errorText(500, 'x'.repeat(5000)).length < 320);
});

test('oneLine: quoted text cannot redraw or displace the sentence around it', () => {
  // Direction overrides and isolates, zero-width and other format
  // characters, controls, and the glyphs that draw as blank space.
  for (const bad of ['\u202E', '\u202D', '\u2066', '\u2067', '\u2068', '\u2069', '\u200B', '\u200E', '\u200F', '\u061C', '\u0085', '\u0000', '\u001B',
    '\u009B', '\uFEFF', '\u2028', '\u2029', '\u{E0041}', '\u2800', '\u3164', '\u115F', '\u1160', '\uFFA0', '\n', '\r', '\t', '\v', '\f', '\u00A0']) {
    const out = oneLine(`a${bad.repeat(50)}b`, 300);
    assert.equal(out, 'a b', `U+${bad.codePointAt(0).toString(16)}`);
  }
  assert.equal(oneLine('  plain words, with ünïcödé and 日本語 and 😀  ', 300), 'plain words, with ünïcödé and 日本語 and 😀');
  // A cut is by character: no half of a pair is left behind.
  assert.equal(oneLine('😀'.repeat(400), 300), '😀'.repeat(300));
  assert.equal(oneLine(7, 300), '');
});

test('errorText quotes a reason through oneLine', () => {
  assert.equal(errorText(400, { error: { message: 'no\u202E)revres eht deliaf( .tneS' } }), 'no )revres eht deliaf( .tneS (HTTP 400)');
  assert.ok(!/[\u202A-\u202E\u2066-\u2069]/u.test(errorText(502, '\u2067x\u2069')));
});

test('makeCoalescer runs a burst once now and once at the end of the gap', () => {
  let clock = 1000;
  let runs = 0;
  const timers = [];
  const call = makeCoalescer(() => runs++, 100, () => clock, (f, ms) => timers.push({ f, at: clock + ms }));
  call();
  assert.equal(runs, 1);
  for (let i = 0; i < 50; i++) call();
  assert.equal(runs, 1);
  assert.equal(timers.length, 1);
  assert.equal(timers[0].at, 1100);
  clock = 1100;
  timers.shift().f();
  assert.equal(runs, 2);
  clock = 1150;
  call();
  assert.equal(runs, 2);
  assert.equal(timers.length, 1);
  clock = 5000;
  timers.shift().f();
  assert.equal(runs, 3);
  clock = 6000;
  call();
  assert.equal(runs, 4);
});

test('MAX_MESSAGE is the hub cap', () => {
  assert.equal(MAX_MESSAGE, 16000);
});

test('a contact cannot wake a stopped worker: the operator stopped it on purpose', () => {
  const w = (state) => ({ name: 'deal-3', role: 'worker', access: 'message', state });
  assert.deepEqual(inputView(w('stopped'), true, 'contact'), { input: false, note: '', ask: true, wake: false, viewOnly: false });
  assert.equal(inputView(w('stopped'), false, 'contact').ask, false);
  assert.deepEqual(inputView(w('suspended'), true, 'contact'), { input: true, note: 'asleep – your message wakes it', ask: false, wake: true, viewOnly: false });
  assert.deepEqual(inputView(w('stopped'), true, 'operator'), { input: true, note: 'asleep – your message wakes it', ask: false, wake: true, viewOnly: false });
  // No tier given reads as a contact: the narrower view.
  assert.equal(inputView(w('stopped'), true).wake, false);
});

test('inputView follows the spec table', () => {
  const w = (state, extra = {}) => ({ name: 'deal-3', role: 'worker', access: 'message', state, ...extra });
  assert.deepEqual(inputView(w('running'), true), { input: true, note: '', ask: false, wake: false, viewOnly: false });
  assert.deepEqual(inputView(w('starting'), true), { input: true, note: 'starting – your message waits until it runs', ask: false, wake: false, viewOnly: false });
  for (const s of ['suspended', 'stopped']) {
    assert.deepEqual(inputView(w(s), true, 'operator'), { input: true, note: 'asleep – your message wakes it', ask: false, wake: true, viewOnly: false });
  }
  for (const s of ['error', 'no-record', 'not-fresh']) {
    assert.deepEqual(inputView(w(s), true), { input: false, note: '', ask: true, wake: false, viewOnly: false });
    // A contact who may not message the manager gets no button.
    assert.equal(inputView(w(s), false).ask, false);
  }
  for (const s of ['suspended', 'stopped', 'error', 'no-record', 'not-fresh']) {
    assert.deepEqual(inputView({ name: 'boss', role: 'manager', access: 'message', state: s }, true),
      { input: false, note: 'the assistant is offline', ask: false, wake: false, viewOnly: false });
  }
  assert.deepEqual(inputView({ name: 'boss', role: 'manager', access: 'message', state: 'running' }, true).input, true);
  for (const s of ['unknown', 'weird', undefined]) {
    assert.deepEqual(inputView(w(s), true), { input: false, note: 'state unknown – retrying', ask: false, wake: false, viewOnly: false });
  }
  assert.deepEqual(inputView({ ...w('running'), access: 'see' }, true), { input: false, note: '', ask: false, wake: false, viewOnly: true });
  assert.equal(inputView(null, true).viewOnly, true);
});

test('agentList keeps only what the page may show', () => {
  const l = agentList({ login: 'c', tier: 'contact', userId: 'u', agents: [
    { name: 'w1', role: 'worker', access: 'message', state: 'running', activity: 'working', id: 'a1', conversation: 'dm:agent:a1:user:u', unread: 3, label: 'x‮y' },
    { name: 'w2', role: 'worker', access: 'see', state: 'weird', id: 'leak', conversation: 'leak', unread: 4, activity: 'working', terminal: '/agents/leak/terminal' },
    { name: 7, access: 'message', state: 'running' },
    { name: '', access: 'message', state: 'running' },
    { name: 'w4', access: 'admin', state: 'running' },
    null,
    'w5',
    { name: 'w1', access: 'message', state: 'error' },
  ] });
  assert.equal(l.agents.length, 2, 'junk rows and a repeated name go');
  assert.equal(l.agents[0].label, 'x y');
  assert.equal(l.agents[0].activity, 'working');
  assert.equal(l.agents[0].unread, 3);
  assert.deepEqual(l.agents[1], { name: 'w2', role: 'worker', access: 'see', state: 'unknown', label: '' });
  assert.deepEqual([l.login, l.tier, l.userId, l.console], ['c', 'contact', 'u', '']);
  for (const junk of ['nope', null, 7, {}, { agents: 'x' }]) assert.equal(agentList(junk), null);
});

test('agentList reads every field as data of unknown shape', () => {
  const row = (over) => agentList({ tier: 'operator', agents: [{ name: 'w', access: 'message', state: 'running', ...over }] }).agents[0];
  assert.equal(row({ role: 'manager' }).role, 'manager');
  assert.equal(row({ role: 'admin' }).role, 'worker');
  assert.equal(row({ activity: 'thinking' }).activity, '', 'only the page\'s three activity words');
  assert.equal(row({ state: 'stopped', activity: 'working' }).activity, '', 'activity belongs to a running agent');
  for (const u of [-1, 100, 2.5, '3', null]) assert.equal(row({ unread: u }).unread, undefined, String(u));
  assert.equal(row({ unread: 99 }).unread, 99);
  assert.equal(row({ unread: 0 }).unread, 0);
  assert.deepEqual([row({ id: 7 }).id, row({ conversation: {} }).conversation, row({ terminal: 1 }).terminal], ['', '', '']);
  assert.equal(row({ label: 'a'.repeat(80) }).label.length, 60);
  assert.equal(row({ label: { x: 1 } }).label, '');
  assert.equal(agentList({ tier: 'root', agents: [] }).tier, 'contact', 'only "operator" earns the operator view');
  for (const s of ['running', 'starting', 'suspended', 'stopped', 'error', 'no-record', 'not-fresh', 'unknown']) assert.equal(row({ state: s }).state, s);
});

test('badges, titles, chips, drafts', () => {
  assert.equal(badgeText(0), '');
  assert.equal(badgeText(1), '1');
  assert.equal(badgeText(98), '98');
  assert.equal(badgeText(99), '99+');
  assert.equal(badgeText(undefined), '');
  assert.equal(badgeText('5'), '');
  assert.equal(rowTitle({ name: 'deal-2', label: 'Via Roma 12' }), 'deal-2 · Via Roma 12');
  assert.equal(rowTitle({ name: 'deal-2', label: '' }), 'deal-2');
  assert.equal(chipText({ state: 'running', activity: 'waiting' }), 'waiting');
  assert.equal(chipText({ state: 'running' }), 'running');
  assert.equal(chipText({ state: 'running', activity: '<b>' }), 'running');
  assert.equal(chipText({ state: 'suspended' }), 'asleep');
  assert.equal(chipText({ state: 'stopped' }), 'stopped');
  assert.equal(chipText({ state: 'no-record' }), 'no record');
  assert.equal(chipText({ state: 'not-fresh' }), 'not fresh');
  assert.equal(chipText({ state: '<script>' }), 'unknown');
  assert.equal(askDraft('deal-3'), 'Please start deal-3 for me.');
});

test('wakeText is fixed text per reason, never the server text', () => {
  assert.match(wakeText(429, { error: 'rate-limited' }, 'w1'), /a minute/);
  assert.match(wakeText(403, { error: 'not-allowed' }, 'w1'), /not allowed/);
  assert.match(wakeText(503, { error: 'unavailable' }, 'w1'), /not available/);
  for (const word of ['not-asleep', 'refused', 'failed', 'origin']) assert.ok(wakeText(409, { error: word }, 'w1'), word);
  assert.equal(wakeText(500, '<script>x</script>', 'w1').includes('<'), false);
  assert.equal(wakeText(500, { error: '<b>own words</b>' }, 'w1'), 'w1 could not be woken (HTTP 500).');
  assert.equal(wakeText(0, null, 'w1'), 'w1 could not be woken (HTTP no answer).');
  assert.equal(wakeText(500, { error: 'constructor' }, 'w1'), 'w1 could not be woken (HTTP 500).', 'no word from the prototype');
});

test('the page timings', () => {
  assert.equal(WAKE_POLL_MS, 3000);
  assert.equal(WAKE_POLLS * WAKE_POLL_MS, 90000);
  assert.equal(LIST_MS, 15000);
});

test('contactList keeps well-formed contacts and agents only', () => {
  assert.equal(contactList(null), null);
  assert.equal(contactList({ contacts: 'x' }), null);
  assert.deepEqual(contactList({ contacts: [
    { login: 'c@x', signedIn: true, agents: [{ name: 'w1', label: 'Via\nRoma', state: 'running', id: 'leak' }, { name: 'w1' }, { label: 'no name' }, { name: 'w2', state: 'bogus' }] },
    { login: 'c@x' }, { login: '' }, null, 7,
    { login: 'd@x', signedIn: 'yes', agents: 'none' },
  ] }), [
    { login: 'c@x', signedIn: true, agents: [{ name: 'w1', label: 'Via Roma', state: 'running', access: 'see' }, { name: 'w2', label: '', state: 'unknown', access: 'see' }] },
    { login: 'd@x', signedIn: false, agents: [] },
  ]);
});

test('transcriptItems: only rows with an id; shownToContact only when exactly true', () => {
  assert.deepEqual(transcriptItems({ messages: [
    { id: 'a', from: 'agent', text: 't', createdAt: 'c', shownToContact: true },
    { id: 'b', from: 'contact', text: 7, shownToContact: 'true' },
    { id: 'c', from: '<b>', text: 'x' },
    { from: 'agent', text: 'no id' }, null,
  ] }), [
    { id: 'a', from: 'agent', text: 't', createdAt: 'c', shownToContact: true },
    { id: 'b', from: 'contact', text: '', createdAt: '', shownToContact: false },
    { id: 'c', from: 'agent', text: 'x', createdAt: '', shownToContact: false },
  ]);
  assert.deepEqual(transcriptItems('nope'), []);
});

test('transcriptPath encodes the login and the agent', () => {
  assert.equal(transcriptPath('a/b+c@x', 'w1', ''), '/lever/api/contacts/a%2Fb%2Bc%40x/agents/w1/messages?limit=50');
  assert.equal(transcriptPath('c@x', 'w1', 'C 1'), '/lever/api/contacts/c%40x/agents/w1/messages?limit=50&cursor=C+1');
});

test('transcriptWho names the writer', () => {
  assert.equal(transcriptWho({ from: 'contact' }, 'c@x', 'w1'), 'c@x');
  assert.equal(transcriptWho({ from: 'agent' }, 'c@x', 'w1'), 'w1');
  assert.equal(transcriptWho({ from: 'system' }, 'c@x', 'w1'), 'hub');
});

test('mergeRows reports a new row, new text and a new mark', () => {
  const m = new Map();
  assert.equal(mergeRows(m, [{ id: 'a', text: 't', shownToContact: false }]), true);
  assert.equal(mergeRows(m, [{ id: 'a', text: 't', shownToContact: false }]), false);
  assert.equal(mergeRows(m, [{ id: 'a', text: 't', shownToContact: true }]), true);
  assert.equal(mergeRows(m, [{ id: 'a', text: 'u', shownToContact: true }]), true);
});

test('viewErrorText reads the fixed words', () => {
  assert.equal(viewErrorText(409, { error: 'not-signed-in' }), 'has not signed in yet');
  assert.equal(viewErrorText(409, { error: 'not-signed-in', hint: 'run lever apply' }), 'the contact has a new hub user: run lever apply');
  assert.equal(viewErrorText(409, { error: 'not-signed-in', hint: '<b>other</b>' }), 'has not signed in yet');
  assert.equal(viewErrorText(409, { error: 'no-record' }), 'the agent has no record on the hub yet');
  assert.equal(viewErrorText(502, 'bad gateway\n'), errorText(502, 'bad gateway\n'));
});

test('the operator view constants', () => {
  assert.equal(CONTACTS_MS, 30000);
  assert.equal(NOT_SHOWN, 'not shown to the contact');
});
