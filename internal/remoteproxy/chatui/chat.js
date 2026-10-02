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
  messageText,
  sortedMessages,
  stateLine,
} from './chatcore.js';

const PAGE = 50; // messages per history request
const POLL_MS = 20000; // state + history safety poll while the page shows
const REFRESH_GAP_MS = 1500; // at most one history read per gap on a burst
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
let boot = null;
let olderCursor = '';
let sending = false;
let stream = null;
let streamFailed = false;

// api does one same-origin request and reads the answer as JSON when it is
// JSON, else as text. It never throws: a network fault is status 0.
async function api(path, init) {
  try {
    const res = await fetch(path, { credentials: 'same-origin', headers: { Accept: 'application/json' }, ...init });
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

// localPath admits only a path on this origin, so a link target can never
// leave it whatever the bootstrap says.
function localPath(p) {
  return typeof p === 'string' && p.startsWith('/') && !p.startsWith('//') && !p.startsWith('/\\') ? p : '';
}

function setLink(a, path) {
  const p = localPath(path);
  if (!p) return;
  a.setAttribute('href', p);
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

function render(keepBottom) {
  const stick = keepBottom || nearBottom();
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
    setText(body, messageText(m));
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
  if (boot.conversation) showNotice(shown ? '' : 'No messages yet.');
  if (stick) el.scroll.scrollTop = el.scroll.scrollHeight;
}

function historyPath(cursor) {
  const q = new URLSearchParams({ limit: String(PAGE) });
  if (cursor) q.set('cursor', cursor);
  return `/api/v1/chat/conversations/${encodeURIComponent(boot.conversation)}/messages?${q}`;
}

// refresh reads the newest page again and merges it. Every live signal ends
// here: the page never trusts an event's payload, only the hub's history.
async function refresh(first) {
  if (!boot || !boot.conversation) return;
  const res = await api(historyPath(''));
  if (!res.ok) {
    if (first) showNotice(`Cannot read the conversation: ${errorText(res.status, res.body)}`);
    return;
  }
  const items = historyItems(res.body);
  if (first) {
    olderCursor = typeof res.body.nextCursor === 'string' ? res.body.nextCursor : '';
    el.older.hidden = !olderCursor || items.length < PAGE;
  }
  if (mergeMessages(messages, items) || first) render(first);
}

const refreshSoon = makeCoalescer(() => void refresh(false), REFRESH_GAP_MS);

async function loadOlder() {
  if (!olderCursor) return;
  el.older.disabled = true;
  const res = await api(historyPath(olderCursor));
  el.older.disabled = false;
  if (!res.ok) {
    showError(`Cannot load earlier messages: ${errorText(res.status, res.body)}`);
    return;
  }
  const items = historyItems(res.body);
  olderCursor = typeof res.body.nextCursor === 'string' ? res.body.nextCursor : '';
  el.older.hidden = !olderCursor || items.length < PAGE;
  const before = el.scroll.scrollHeight;
  if (mergeMessages(messages, items)) {
    render(false);
    el.scroll.scrollTop += el.scroll.scrollHeight - before;
  }
}

async function readState() {
  if (!boot || !boot.agent.id) return;
  const res = await api(`/api/v1/agents/${encodeURIComponent(boot.agent.id)}`);
  if (!res.ok) {
    setText(el.state, res.status === 0 ? 'offline' : `state unknown (HTTP ${res.status})`);
    el.state.className = 'state bad';
    return;
  }
  const line = stateLine(res.body && res.body.agent ? res.body.agent : res.body);
  setText(el.state, line.text);
  el.state.className = `state ${line.ok ? 'ok' : 'bad'}`;
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

async function send() {
  const text = el.text.value.trim();
  if (sending || !text || !boot || !boot.conversation) return;
  if (text.length > MAX_MESSAGE) {
    showError(`The message is ${text.length} characters; the limit is ${MAX_MESSAGE}.`);
    return;
  }
  sending = true;
  el.send.disabled = true;
  showError('');
  // One key for both attempts, so the hub stores the message once even if
  // the first attempt reached it.
  const key = newKey();
  let res = await post(text, key);
  if (res.status === 401) {
    // The hub forgot the session. A read makes the proxy sign in again;
    // then the message goes once more.
    await api(historyPath(''));
    res = await post(text, key);
  }
  sending = false;
  if (res.ok) {
    el.text.value = '';
    saveDraft();
    grow();
    if (res.body && typeof res.body === 'object' && mergeMessages(messages, [res.body])) render(true);
    refreshSoon();
  } else {
    showError(`Not sent: ${errorText(res.status, res.body)}`);
    void readState();
  }
  el.send.disabled = false;
  el.text.focus();
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
  const res = await api('/lever/api/chat');
  if (!res.ok || !res.body || typeof res.body !== 'object' || !res.body.agent) {
    setText(el.state, 'unavailable');
    el.state.className = 'state bad';
    showNotice(`The chat page cannot start: ${errorText(res.status, res.body)}`);
    return;
  }
  boot = {
    userId: typeof res.body.userId === 'string' ? res.body.userId : '',
    conversation: typeof res.body.conversation === 'string' ? res.body.conversation : '',
    agent: {
      name: typeof res.body.agent.name === 'string' ? res.body.agent.name : 'agent',
      id: typeof res.body.agent.id === 'string' ? res.body.agent.id : '',
    },
  };
  setText(el.agent, boot.agent.name);
  document.title = boot.agent.name;
  setLink(el.terminal, res.body.terminal);
  setLink(el.console, res.body.console);
  if (!boot.conversation) {
    setText(el.state, 'no hub record');
    el.state.className = 'state bad';
    showNotice(`${boot.agent.name} has no record on the hub yet. Start it on the host with lever up, then reload.`);
    return;
  }
  loadDraft();
  el.text.disabled = false;
  el.send.disabled = false;
  grow();
  await Promise.all([refresh(true), readState()]);
  openStream();
  setInterval(poll, POLL_MS);
  document.addEventListener('visibilitychange', poll);
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
  // is a new line, and the button sends.
  if (ev.key !== 'Enter' || ev.shiftKey || ev.isComposing) return;
  if (!window.matchMedia('(pointer: fine)').matches) return;
  ev.preventDefault();
  void send();
});
el.older.addEventListener('click', () => void loadOlder());

void start();
