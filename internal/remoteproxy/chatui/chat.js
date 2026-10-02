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
const DRAFT_KEY = 'lever-chat-draft';

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
let lastFailed = null; // {text, key} of a send that got no clear answer
let boot = null;
let loaded = false; // a history read has succeeded
let reading = false; // a history read is under way
let readAgain = false; // something changed while it was
let olderCursor = '';
let sending = false;
let stream = null;
let streamFailed = false;

// api does one same-origin request and reads the answer as JSON when it is
// JSON, else as text. It never throws: a network fault is status 0.
//
// A redirect is never followed. The hub answers a session it no longer knows
// with a redirect to its login page; followed, that ends in a page and a
// 200, which would read as success. It comes back as {redirect: true}.
async function api(path, init) {
  try {
    const res = await fetch(path, { credentials: 'same-origin', redirect: 'manual', headers: { Accept: 'application/json' }, ...init });
    if (res.type === 'opaqueredirect' || (res.status >= 300 && res.status < 400)) {
      return { ok: false, status: res.status, redirect: true, body: 'the hub asked to sign in again' };
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
    return { ok: false, status: 0, body: 'cannot reach the server' };
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

// sent reports whether the hub's answer says the message is stored: a 201
// with the message, or the 200 the hub gives a repeated key. Any other
// answer, a 2xx included, is not a stored message.
function sent(res) {
  return res.status === 201 || (res.status === 200 && !!res.body && typeof res.body === 'object' && typeof res.body.id === 'string');
}

async function send() {
  const text = el.text.value.trim();
  if (sending || !text || !boot || !boot.conversation) return;
  const length = messageLength(text);
  if (length > MAX_MESSAGE) {
    showError(`The message is ${length} characters; the limit is ${MAX_MESSAGE}.`);
    return;
  }
  sending = true;
  el.send.disabled = true;
  showError('');
  const res = await deliver(text);
  sending = false;
  el.send.disabled = false;
  if (!res) return; // the page is reloading onto the manager's new record
  if (sent(res)) {
    lastFailed = null;
    // Clear only what was sent: text typed while the send was under way stays.
    if (el.text.value.trim() === text) {
      el.text.value = '';
      saveDraft();
      grow();
    }
    // 201 carries the stored message. The answer to a repeated key carries
    // less, so the history read shows that one instead.
    if (res.status === 201 && res.body && typeof res.body === 'object' && mergeMessages(messages, [res.body])) render(true);
    refreshSoon();
  } else {
    showError(`Not sent: ${errorText(res.status, res.body)}`);
    void readState();
  }
  el.text.focus();
}

// deliver posts text to the conversation and returns the hub's answer, or
// null when the page reloads instead.
async function deliver(text) {
  // The manager may have a new hub record since the page loaded (`lever up
  // --fresh`). The hub would store a message for the old one and deliver it
  // to nobody, so ask first. The draft is kept across the reload.
  const now = await readBoot();
  if (now.error) return { ok: false, status: 0, body: now.error };
  if (movedOn(now)) {
    saveDraft();
    location.reload();
    return null;
  }
  // The same text sent again after an unclear answer keeps its key, so the
  // hub stores it once even if the first attempt did reach it.
  const key = lastFailed && lastFailed.text === text ? lastFailed.key : newKey();
  let res = await post(text, key);
  if (res.status === 401 || res.redirect) {
    // The hub forgot the session. A read makes the proxy sign in again;
    // then the message goes once more under the same key.
    await api(historyPath(''));
    res = await post(text, key);
  }
  if (!sent(res)) lastFailed = { text, key };
  return res;
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
  if (ev.key !== 'Enter' || ev.shiftKey || ev.isComposing || ev.keyCode === 229) return;
  if (!window.matchMedia('(pointer: fine)').matches) return;
  ev.preventDefault();
  void send();
});
el.older.addEventListener('click', () => void loadOlder());
window.addEventListener('resize', grow);

void start();
