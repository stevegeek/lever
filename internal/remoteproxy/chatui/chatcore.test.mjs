import test from 'node:test';
import assert from 'node:assert/strict';
import {
  MAX_MESSAGE,
  classify,
  errorText,
  historyItems,
  isChatSubject,
  makeCoalescer,
  mergeMessages,
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
  assert.equal(classify({ id: '5', msg: 'x', type: 'mention', senderId: 'u1' }, uid), 'hidden');
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

test('stateLine', () => {
  assert.deepEqual(stateLine({ phase: 'running', activity: 'waiting_for_input' }), { text: 'waiting for input', ok: true });
  assert.deepEqual(stateLine({ phase: 'running' }), { text: 'running', ok: true });
  assert.equal(stateLine({ phase: 'stopped' }).ok, false);
  assert.match(stateLine({ phase: 'stopped' }).text, /^stopped/);
  assert.equal(stateLine({ phase: 'error' }).ok, false);
  assert.deepEqual(stateLine(null), { text: 'state unknown', ok: true });
  assert.deepEqual(stateLine({ phase: 42 }), { text: 'state unknown', ok: true });
});

test('errorText prefers the hub message and bounds it', () => {
  assert.equal(errorText(409, { message: 'agent is not running' }), 'agent is not running (HTTP 409)');
  assert.equal(errorText(400, { error: { message: 'too long' } }), 'too long (HTTP 400)');
  assert.equal(errorText(502, 'bad gateway\n'), 'bad gateway (HTTP 502)');
  assert.equal(errorText(500, null), 'request failed (HTTP 500)');
  assert.ok(errorText(500, 'x'.repeat(5000)).length < 320);
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
