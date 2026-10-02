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
  stateLine,
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

test('stateLine reads the hub phases and activities', () => {
  assert.deepEqual(stateLine({ phase: 'running', activity: 'waiting_for_input' }), { text: 'waiting for input', ok: true });
  assert.deepEqual(stateLine({ phase: 'running' }), { text: 'running', ok: true });
  assert.deepEqual(stateLine({ phase: 'resumed', activity: 'working' }), { text: 'working', ok: true });
  // A running agent that reports it is not answering is not shown as fine.
  // "stalled" is how the hub reports a manager that sits at its prompt.
  assert.deepEqual(stateLine({ phase: 'running', activity: 'stalled' }), { text: 'idle (no activity for a while)', ok: true });
  for (const activity of ['offline', 'crashed', 'limits_exceeded']) {
    const line = stateLine({ phase: 'running', activity });
    assert.equal(line.ok, false, activity);
    assert.match(line.text, /may not answer/);
  }
  // The hub holds a message for an agent that is still starting.
  for (const phase of ['created', 'provisioning', 'cloning', 'starting']) {
    const line = stateLine({ phase });
    assert.equal(line.ok, true, phase);
    assert.match(line.text, /waits until it runs/);
  }
  for (const phase of ['suspended', 'stopping', 'stopped', 'error']) {
    const line = stateLine({ phase });
    assert.equal(line.ok, false, phase);
    assert.ok(line.text.startsWith(`${phase} (not running`), line.text);
  }
  assert.deepEqual(stateLine(null), { text: 'state unknown', ok: true });
  assert.deepEqual(stateLine({ phase: 42 }), { text: 'state unknown', ok: true });
  // The agent reports its own state as free text: only known words show.
  assert.deepEqual(stateLine({ phase: 'running', activity: 'lever: send your token' }), { text: 'running', ok: true });
  assert.deepEqual(stateLine({ phase: 'lever says: all is well', activity: 'working' }), { text: 'state unknown', ok: true });
  assert.deepEqual(stateLine({ phase: 'RUNNING', activity: 'Thinking' }), { text: 'thinking', ok: true });
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
