// lever's chat page: one conversation, the operator's DM with the manager.
//
// It talks to the hub's own chat routes through the remote proxy, so a
// message sent here is recorded and verified exactly like one sent from the
// hub's web UI. The page holds no credential: the proxy attaches the
// operator's hub session to every request.
//
// SECURITY: everything read from the network is agent- or hub-chosen text on
// the operator's origin. It is only ever written as TEXT (textContent). No
// markup sink, no code evaluation, and no URL is built from message text.
// A test in the proxy package fails the build if one appears in this file.

import {
  MAX_MESSAGE,
  classify,
  errorText,
  historyItems,
  isChatSubject,
  makeCoalescer,
  mergeMessages,
  messageLength,
  messageText,
  nextCursor,
  sortedMessages,
  stateLine,
} from './chatcore.js';

const PAGE = 50; // messages per history request
const POLL_MS = 20000; // state + history safety poll while the page shows
const REFRESH_GAP_MS = 1500; // at most one history read per gap on a burst
const RETRY_MS = 5000; // between attempts to start while the hub is away
// No request may wait for ever: a phone that changes network mid-request can
// leave one that never settles, and the page reads and sends one at a time.
// Longer than the proxy's own wait for the hub (45 s), so the proxy's answer
// comes first when there is one.
const REQUEST_MS = 60000;
const HOLD_MS = 2000; // Send stays off this long after the replay note
const DRAFT_KEY = 'lever-chat-draft';
const UNSENT_KEY = 'lever-chat-unsent';

const $ = (id) => document.getElementById(id);
const el = {
  agent: $('agent'),
  state: $('state'),
  terminal: $('terminal'),
  console: $('console'),
  scroll: $('scroll'),
  older: $('older'),
  notice: $('notice'),
  list: $('list'),
  form: $('composer'),
  error: $('error'),
  text: $('text'),
  send: $('send'),
};

const messages = new Map();
const fromHistory = new Set(); // ids a history read returned (not a send answer)
let generation = 0; // counts restarts of the list
// The send that is under way or got no clear "stored": {text, key,
// conversation}, written before the post and kept in sessionStorage. key is
// its idempotency key. While the same text goes again to the same
// conversation it goes under that key, so the hub stores it once if it still
// knows the key (it keeps one for a few minutes, per user, not per
// conversation, hence the conversation here). One record: an unclear send of
// one text, then another text, then the first again gets a new key.
//
// The page concludes nothing from this record. It never decides from the
// history that a send "must have arrived": only the hub's answer to the
// same key says so. A wrong guess there would clear a draft that was never
// stored.
let unsent = null;
let boot = null;
let loaded = false; // a history read has succeeded
let reading = false; // a history read is under way
let readAgain = false; // something changed while it was
let olderCursor = '';
let sending = false;
let held = false; // Send rests after a note (see hold)
let stream = null;
let streamFailed = false;

// api does one same-origin request and reads the answer as JSON when it is
// JSON, else as text. It never throws: a network fault is status 0.
//
// A redirect is never followed. The hub answers a session it no longer knows
// with a redirect to its login page; followed, that ends in a page and a
// 200, which would read as success. It comes back as {redirect: true}.
async function api(path, init) {
  const limit = new AbortController();
  const timer = setTimeout(() => limit.abort(), REQUEST_MS);
  try {
    const res = await fetch(path, { credentials: 'same-origin', redirect: 'manual', signal: limit.signal, headers: { Accept: 'application/json' }, ...init });
    if (res.type === 'opaqueredirect' || (res.status >= 300 && res.status < 400)) {
      // The proxy signs in to the hub again by itself on a read, so a
      // redirect that reaches the page is the front's own sign-in.
      return { ok: false, status: 0, redirect: true, body: 'sign-in is needed again: reload the page' };
    }
    const raw = await res.text();
    let body = raw;
    try {
      body = JSON.parse(raw);
    } catch {
      // not JSON: keep the text
    }
    return { ok: res.ok, status: res.status, body };
  } catch {
    return { ok: false, status: 0, body: limit.signal.aborted ? 'no answer in time' : 'cannot reach the server' };
  } finally {
    clearTimeout(timer);
  }
}

function setText(node, text) {
  node.textContent = text;
}

function showError(text) {
  setText(el.error, text);
  el.error.hidden = !text;
}

function showNotice(text) {
  setText(el.notice, text);
  el.notice.hidden = !text;
}

function showState(text, ok) {
  setText(el.state, text);
  el.state.className = `state ${ok ? 'ok' : 'bad'}`;
}

// localLink turns a path from the bootstrap into a link target on this
// origin, or '' when it would lead anywhere else. The browser's own URL
// parser decides, since it is what reads the link: it drops tabs and
// newlines, and removes dot segments, which a check on the string would
// miss. The result is the parsed URL itself, whole, so nothing is parsed a
// second time into something else ("/.//evil.test" has the path
// "//evil.test", which read again as a link is another host).
function localLink(p) {
  if (typeof p !== 'string' || !p.startsWith('/')) return '';
  try {
    const u = new URL(p, location.origin);
    return u.origin === location.origin && !u.pathname.startsWith('//') ? u.href : '';
  } catch {
    return '';
  }
}

function setLink(a, path) {
  const href = localLink(path);
  if (!href) return;
  a.setAttribute('href', href);
  a.hidden = false;
}

function when(m) {
  const t = Date.parse(typeof m.createdAt === 'string' ? m.createdAt : '');
  if (Number.isNaN(t)) return '';
  const d = new Date(t);
  const sameDay = d.toDateString() === new Date().toDateString();
  const hm = d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
  return sameDay ? hm : `${d.toLocaleDateString([], { day: 'numeric', month: 'short' })} ${hm}`;
}

function nearBottom() {
  return el.scroll.scrollHeight - el.scroll.scrollTop - el.scroll.clientHeight < 80;
}

function render(toBottom) {
  const stick = toBottom || nearBottom();
  const frag = document.createDocumentFragment();
  let shown = 0;
  for (const m of sortedMessages(messages)) {
    const kind = classify(m, boot.userId);
    if (kind === 'hidden') continue;
    shown++;
    const row = document.createElement('div');
    row.className = `msg ${kind}`;
    if (kind !== 'system') {
      const meta = document.createElement('div');
      meta.className = 'meta';
      setText(meta, `${kind === 'mine' ? 'You' : boot.agent.name} ${when(m)}`.trim());
      row.append(meta);
    }
    const body = document.createElement('div');
    body.className = 'body';
    // A system line says who it is from, so an agent's own text cannot
    // read as a notice from lever or the hub.
    const from = m.senderId === boot.agent.id ? boot.agent.name : 'hub';
    setText(body, kind === 'system' ? `${from}: ${messageText(m)}` : messageText(m));
    row.append(body);
    if (kind === 'mine' && m.dispatchState === 'failed') {
      const fail = document.createElement('div');
      fail.className = 'fail';
      const why = typeof m.dispatchFailureReason === 'string' ? m.dispatchFailureReason.slice(0, 200) : '';
      setText(fail, why ? `Not delivered: ${why}` : 'Not delivered');
      row.append(fail);
    }
    frag.append(row);
  }
  el.list.replaceChildren(frag);
  showNotice(shown ? '' : 'No messages yet.');
  if (stick) el.scroll.scrollTop = el.scroll.scrollHeight;
}

function historyPath(cursor) {
  const q = new URLSearchParams({ limit: String(PAGE) });
  if (cursor) q.set('cursor', cursor);
  return `/api/v1/chat/conversations/${encodeURIComponent(boot.conversation)}/messages?${q}`;
}

// refresh reads the newest page again and merges it. Every live signal ends
// here: the page never trusts an event's payload, only the hub's history.
//
// One read at a time: a signal that arrives during a read asks for one more
// after it. So answers apply in the order they were asked for, and a slow
// hub under a steady stream of events still gets every read applied.
async function refresh() {
  if (!boot || !boot.conversation) return;
  if (reading) {
    readAgain = true;
    return;
  }
  reading = true;
  try {
    applyHistory(await api(historyPath('')));
  } finally {
    reading = false;
    if (readAgain) {
      readAgain = false;
      refreshSoon();
    }
  }
}

function applyHistory(res) {
  if (!res.ok) {
    if (!loaded) showNotice(`Cannot read the conversation: ${errorText(res.status, res.body)}`);
    return;
  }
  const items = historyItems(res.body);
  // A full page with nothing the page already holds means more arrived than
  // one page while it was not looking. Start again from this page instead of
  // showing the two ends with a hole between them; "Load earlier" then
  // reaches the rest.
  //
  // Only ids from earlier history reads count as "already holds": the id of
  // a message just sent is in every newest page, and proves no overlap.
  const restart = !loaded || (items.length >= PAGE && !items.some((m) => fromHistory.has(m.id)));
  if (restart) {
    messages.clear();
    fromHistory.clear();
    generation++;
    olderCursor = nextCursor(res.body);
    el.older.hidden = !olderCursor;
  }
  const first = !loaded;
  loaded = true;
  for (const m of items) fromHistory.add(m.id);
  if (mergeMessages(messages, items) || restart) render(first);
}

// What the page says when the hub answers a repeated key: the earlier
// attempt was stored, and nothing was stored now. The draft stays, since the
// page cannot tell a retry from the same words meant a second time. The
// answer to that first attempt may have been lost before the proxy, which
// then recorded nothing, so the manager may be unable to verify the message.
// And when the hub stores, under a key sent before, a message it should have
// known if the earlier attempt arrived within its memory: both may be there.
const NOTE_STORED_AGAIN = 'Stored now. An earlier attempt had no clear answer: if the message shows twice above, both arrived.';
const NOTE_REPLAY = 'The earlier attempt did arrive: the message is in the conversation, and nothing new was stored. ' +
  'Press Send again only to post the same words a second time. ' +
  'If the manager treats the message as unverified, send it in other words.';

function setUnsent(v) {
  unsent = v;
  try {
    if (!v) {
      sessionStorage.removeItem(UNSENT_KEY);
      return;
    }
    sessionStorage.setItem(UNSENT_KEY, JSON.stringify(v));
  } catch {
    // storage is off: the record just does not survive a reload
  }
}

// loadUnsent restores the record of a send that had no clear answer, so that
// after a reload the same draft still goes under the same key. A record for
// another conversation (the manager has a new hub record) is dropped: its
// key would get the hub's answer for the message in the old one.
function loadUnsent() {
  try {
    const v = JSON.parse(sessionStorage.getItem(UNSENT_KEY) || 'null');
    if (!v || typeof v.text !== 'string' || typeof v.key !== 'string' || v.conversation !== boot.conversation) {
      sessionStorage.removeItem(UNSENT_KEY);
      return;
    }
    unsent = { text: v.text, key: v.key, conversation: v.conversation };
  } catch {
    // storage is off, or holds something else
  }
}

const refreshSoon = makeCoalescer(() => void refresh(), REFRESH_GAP_MS);

async function loadOlder() {
  if (!olderCursor) return;
  el.older.disabled = true;
  const asked = generation;
  const res = await api(historyPath(olderCursor));
  el.older.disabled = false;
  // The list restarted meanwhile: this page belongs to the old one.
  if (asked !== generation) return;
  if (!res.ok) {
    showError(`Cannot load earlier messages: ${errorText(res.status, res.body)}`);
    return;
  }
  olderCursor = nextCursor(res.body);
  el.older.hidden = !olderCursor;
  const before = el.scroll.scrollHeight;
  const items = historyItems(res.body);
  for (const m of items) fromHistory.add(m.id);
  if (mergeMessages(messages, items)) {
    render(false);
    el.scroll.scrollTop += el.scroll.scrollHeight - before;
  }
}

// readBoot asks the proxy who the operator is and which agent and
// conversation the page is for.
async function readBoot() {
  const res = await api('/lever/api/chat');
  const b = res.body;
  if (!res.ok || !b || typeof b !== 'object' || !b.agent || typeof b.agent !== 'object') {
    return { error: errorText(res.status, b) };
  }
  return {
    userId: typeof b.userId === 'string' ? b.userId : '',
    conversation: typeof b.conversation === 'string' ? b.conversation : '',
    agent: {
      name: typeof b.agent.name === 'string' ? b.agent.name : 'agent',
      id: typeof b.agent.id === 'string' ? b.agent.id : '',
    },
    terminal: b.terminal,
    console: b.console,
  };
}

// checkBoot reloads the page when the manager's hub record is not the one
// the page started with: `lever up --fresh` gives the manager a new id, and
// a message to the old conversation would be stored and reach nobody.
async function checkBoot() {
  const now = await readBoot();
  if (!now.error && movedOn(now)) location.reload();
}

const movedOn = (now) => now.agent.id !== boot.agent.id || now.conversation !== boot.conversation;

async function readState() {
  if (!boot || !boot.agent.id) return;
  const res = await api(`/api/v1/agents/${encodeURIComponent(boot.agent.id)}`);
  if (res.status === 404) {
    showState('no hub record', false);
    void checkBoot();
    return;
  }
  if (!res.ok) {
    showState(res.status === 0 ? 'offline' : `state unknown (HTTP ${res.status})`, false);
    return;
  }
  const line = stateLine(res.body && res.body.agent ? res.body.agent : res.body);
  showState(line.text, line.ok);
}

// openStream listens for the hub's chat events for this user. An event is
// only a hint to read the history again. A stream the browser gave up on is
// replaced by the poll, which also reopens it.
function openStream() {
  if (!boot || !boot.conversation) return;
  if (stream) stream.close();
  stream = new EventSource(`/events?sub=${encodeURIComponent(`user.${boot.userId}.chat.>`)}`);
  stream.addEventListener('update', (ev) => {
    try {
      if (isChatSubject(JSON.parse(ev.data).subject, boot.userId)) refreshSoon();
    } catch {
      // not ours to read
    }
  });
  stream.addEventListener('open', () => {
    if (streamFailed) {
      streamFailed = false;
      refreshSoon();
    }
  });
  stream.addEventListener('error', () => {
    streamFailed = true;
  });
}

function poll() {
  if (document.visibilityState !== 'visible') return;
  if (!boot.conversation) {
    // The manager had no hub record at start: look for one.
    void checkBoot();
    return;
  }
  void readState();
  refreshSoon();
  if (!stream || stream.readyState === EventSource.CLOSED) openStream();
}

function newKey() {
  if (crypto.randomUUID) return crypto.randomUUID();
  const b = crypto.getRandomValues(new Uint8Array(16));
  return [...b].map((x) => x.toString(16).padStart(2, '0')).join('');
}

function post(text, key) {
  return api(`/api/v1/chat/conversations/${encodeURIComponent(boot.conversation)}/messages`, {
    method: 'POST',
    headers: { Accept: 'application/json', 'Content-Type': 'application/json' },
    body: JSON.stringify({ content: text, idempotency_key: key }),
  });
}

// unclear reports whether an answer leaves open that the hub stored the
// message: no answer at all, or a fault somewhere between the page and the
// hub. A refusal by the hub (4xx) is not unclear.
function unclear(res) {
  return !res.status || res.status >= 500;
}

// replayed reports whether the hub's answer is the one it gives a key it has
// seen: 200, naming the message it stored then.
function replayed(res) {
  return res.status === 200 && !!res.body && typeof res.body === 'object' && typeof res.body.id === 'string';
}

async function send() {
  const text = el.text.value.trim();
  if (sending || held || !text || !boot || !boot.conversation) return;
  const length = messageLength(text);
  if (length > MAX_MESSAGE) {
    showError(`The message is ${length} characters; the limit is ${MAX_MESSAGE}.`);
    return;
  }
  sending = true;
  el.send.disabled = true;
  showError('');
  const out = await deliver(text);
  sending = false;
  el.send.disabled = false;
  if (!out) return; // the page is reloading onto the manager's new record
  const { res, again } = out;
  if (out.blocked) {
    showError(`Not sent: ${out.blocked}`);
  } else if (res.status === 201) {
    // Stored now. This is the only answer that clears the draft, and only
    // the text that was sent: text typed while the send was under way stays.
    setUnsent(null);
    if (el.text.value.trim() === text) {
      el.text.value = '';
      saveDraft();
      grow();
    }
    if (res.body && typeof res.body === 'object' && mergeMessages(messages, [res.body])) render(true);
    // The hub keeps a key for a few minutes, in memory. A 201 for a key
    // sent before means it did not know the key: the earlier attempt never
    // arrived, or it did and the hub has forgotten. The page cannot tell.
    if (again) showError(NOTE_STORED_AGAIN);
    refreshSoon();
  } else if (again && replayed(res)) {
    // The hub had this key already (see NOTE_REPLAY). The record goes, so
    // one more press is a new message; Send rests a moment first, so a
    // double tap is not that press.
    setUnsent(null);
    showError(NOTE_REPLAY);
    hold();
    refreshSoon();
  } else if (unclear(res)) {
    showError(`No clear answer (${errorText(res.status, res.body)}). The message may have arrived: look at the conversation first. A repeat within a few minutes is stored only once.`);
    refreshSoon();
    void readState();
  } else if (again) {
    // The hub refused this attempt, which says nothing about the earlier
    // one. The key stays, so a later press still finds it.
    showError(`This attempt was refused (${errorText(res.status, res.body)}). An earlier attempt had no clear answer and may have arrived: look at the conversation.`);
    void readState();
  } else {
    // A first attempt the hub refused, or answered with something that is
    // no stored message: nothing arrived, and the key is done with.
    setUnsent(null);
    showError(`Not sent: ${res.status >= 200 && res.status < 300 ? `unexpected answer (HTTP ${res.status})` : errorText(res.status, res.body)}`);
    void readState();
  }
  el.text.focus();
}

// hold keeps Send off for a moment after a note the operator must read
// before pressing again.
function hold() {
  held = true;
  el.send.disabled = true;
  setTimeout(() => {
    held = false;
    if (!sending) el.send.disabled = false;
  }, HOLD_MS);
}

// deliver posts text to the conversation. It returns {res, again}: the hub's
// answer, and whether this text had been posted before under the same key
// with no clear answer. {blocked} means nothing was posted; null means the
// page reloads instead.
async function deliver(text) {
  // The manager may have a new hub record since the page loaded (`lever up
  // --fresh`). The hub would store a message for the old one and deliver it
  // to nobody, so ask first. The draft is kept across the reload.
  const now = await readBoot();
  if (now.error) return { blocked: now.error };
  if (movedOn(now)) {
    saveDraft();
    location.reload();
    return null;
  }
  // The same text sent again after an unclear answer keeps its key, so the
  // hub stores it once if it still knows the key.
  const again = !!unsent && unsent.text === text && unsent.conversation === boot.conversation;
  const key = again ? unsent.key : newKey();
  // The record is written BEFORE the post: a reload while the post is under
  // way restores the draft, and must restore its key with it.
  setUnsent({ text, key, conversation: boot.conversation });
  let res = await post(text, key);
  if (res.status === 401 || res.redirect) {
    // The hub forgot the session. A read makes the proxy sign in again;
    // then the message goes once more under the same key.
    await api(historyPath(''));
    res = await post(text, key);
  }
  return { res, again };
}

function grow() {
  el.text.style.height = 'auto';
  el.text.style.height = `${el.text.scrollHeight + 2}px`;
}

function saveDraft() {
  try {
    sessionStorage.setItem(DRAFT_KEY, el.text.value);
  } catch {
    // storage is off: the draft just does not survive a reload
  }
}

function loadDraft() {
  try {
    el.text.value = sessionStorage.getItem(DRAFT_KEY) || '';
  } catch {
    // storage is off
  }
}

async function start() {
  const b = await readBoot();
  if (b.error) {
    // The hub may be starting: keep trying rather than leave a dead page.
    showState('unavailable', false);
    showNotice(`The chat page cannot start yet: ${b.error}. Trying again.`);
    setTimeout(() => void start(), RETRY_MS);
    return;
  }
  boot = b;
  setText(el.agent, boot.agent.name);
  document.title = boot.agent.name;
  setLink(el.terminal, boot.terminal);
  setLink(el.console, boot.console);
  setInterval(poll, POLL_MS);
  document.addEventListener('visibilitychange', poll);
  if (!boot.conversation) {
    showState('no hub record', false);
    showNotice(`${boot.agent.name} has no record on the hub yet. Start it on the host with lever up.`);
    // Said outright: a browser may restore the fields' state over a reload.
    el.text.disabled = true;
    el.send.disabled = true;
    return;
  }
  showNotice('');
  loadDraft();
  loadUnsent();
  el.text.disabled = false;
  el.send.disabled = false;
  grow();
  // The stream first, so a message stored while the history is read still
  // raises an event.
  openStream();
  await Promise.all([refresh(), readState()]);
}

el.form.addEventListener('submit', (ev) => {
  ev.preventDefault();
  void send();
});
el.text.addEventListener('input', () => {
  grow();
  saveDraft();
});
el.text.addEventListener('keydown', (ev) => {
  // Enter sends where there is a keyboard and a mouse; on a touch screen it
  // is a new line, and the button sends. An Enter that confirms an input
  // method's composition is neither (keyCode 229 is how Safari reports it).
  // A held key repeats: one press, one send.
  if (ev.key !== 'Enter' || ev.shiftKey || ev.isComposing || ev.keyCode === 229 || ev.repeat) return;
  if (!window.matchMedia('(pointer: fine)').matches) return;
  ev.preventDefault();
  void send();
});
el.older.addEventListener('click', () => void loadOlder());
window.addEventListener('resize', grow);

void start();
