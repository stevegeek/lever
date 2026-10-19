// lever's chat page: the login's agents, and a chat with each one it may
// message. The page holds no credential: the proxy attaches the login's hub
// session to every request.
//
// SECURITY: everything read from the network is agent- or hub-chosen text on
// the login's origin. It is only ever written as text (textContent): no
// markup sink, no code evaluation, no URL built from message text. A test in
// the proxy package enforces this.

import {
  CONTACTS_MS,
  activityErrorText,
  activityOffered,
  activityPath,
  activityText,
  UPLOAD_MS,
  VOICE_MIN_SAMPLES,
  VOICE_RATE,
  clockText,
  encodeWAV,
  insertText,
  localVoices,
  pickServerVoice,
  pickVoice,
  resampledLength,
  speakErrorText,
  speechParts,
  speechText,
  transcriptOf,
  voiceErrorText,
  voiceRequestMs,
  downloadPath,
  fileCheck,
  fileList,
  sizeText,
  uploadErrorText,
  uploadNote,
  viewFilesPath,
  LIST_MS,
  MAX_MESSAGE,
  NEW_CONVERSATION_PATH,
  NEW_CONVERSATION_POLLS,
  NEW_CONVERSATION_STATUS_PATH,
  NOT_SHOWN,
  NOT_YET,
  WAKE_POLLS,
  WAKE_POLL_MS,
  agentList,
  askDraft,
  badgeText,
  chipText,
  classify,
  contactList,
  errorText,
  hashAgent,
  historyItems,
  inputView,
  isChatSubject,
  makeCoalescer,
  mergeMessages,
  mergeRows,
  messageLength,
  messageText,
  newConversationAsk,
  newConversationForceAsk,
  newConversationLostText,
  newConversationOffered,
  newConversationOutcome,
  newConversationText,
  nextCursor,
  oneLine,
  pushKeyBytes,
  pushView,
  rowTitle,
  sortedMessages,
  transcriptItems,
  transcriptPath,
  transcriptWho,
  viewErrorText,
  wakeText,
} from './chatcore.js';

const PAGE = 50; // messages per history request
const REFRESH_GAP_MS = 1500; // at most one read per gap on a burst
const RETRY_MS = 5000; // between attempts to start while the hub is away
// No request may wait for ever: a network change can leave one unsettled.
// Longer than the proxy's 45 s wait for the hub, so its answer comes first.
const REQUEST_MS = 60000;
const HOLD_MS = 2000; // Send stays off this long after the replay note
const SYSTEM_MAX = 500; // characters of a hub line, on one line
const OPEN_KEY = 'lever-chat-open'; // the name of the open chat
// The bare keys are a single-agent layout's; start adopts them once.
const OLD_DRAFT_KEY = 'lever-chat-draft';
const OLD_UNSENT_KEY = 'lever-chat-unsent';
const draftKey = (name) => `${OLD_DRAFT_KEY}:${name}`;
const unsentKey = (name) => `${OLD_UNSENT_KEY}:${name}`;

const $ = (id) => document.getElementById(id);
const el = {
  agents: $('agents'),
  listnote: $('listnote'),
  console: $('console'),
  back: $('back'),
  agent: $('agent'),
  label: $('label'),
  state: $('state'),
  terminal: $('terminal'),
  scroll: $('scroll'),
  older: $('older'),
  notice: $('notice'),
  viewonly: $('viewonly'),
  list: $('list'),
  form: $('composer'),
  error: $('error'),
  note: $('note'),
  ask: $('ask'),
  text: $('text'),
  send: $('send'),
  contacts: $('contacts'),
  contactsTitle: $('contacts-title'),
  refresh: $('refresh'),
  readonly: $('readonly'),
  push: $('push'),
  pushnote: $('pushnote'),
  attach: $('attach'),
  file: $('file'),
  upload: $('upload'),
  files: $('files'),
  filespanel: $('filespanel'),
  filelist: $('filelist'),
  filesnote: $('filesnote'),
  mic: $('mic'),
  voice: $('voice'),
  readvoice: $('readvoice'),
  readvoicerow: $('readvoicerow'),
  settings: $('settings'),
  settingspanel: $('settingspanel'),
  autosend: $('autosend'),
  autosendrow: $('autosendrow'),
  autoread: $('autoread'),
  autoreadrow: $('autoreadrow'),
  readhint: $('readhint'),
  newconv: $('newconv'),
  activity: $('activity'),
  activitypanel: $('activitypanel'),
  activitywhen: $('activitywhen'),
  activityreload: $('activityreload'),
  activitynote: $('activitynote'),
  activitytext: $('activitytext'),
};

let roster = null; // the list as last applied (agentList shape)
let listSeq = 0; // counts list requests
let appliedSeq = 0; // the request whose answer the page shows
let listing = false; // a list read is under way
let listAgain = false; // something asked for one while it was
let current = ''; // the name of the open chat ('' = none)
// The open chat: {name, id, conversation, userId, unsent, view}; null for none
// and for a see-only agent. A request answers only the chat it was made for.
let chat = null;
const lastMarked = new Map(); // conversation → message id marked read

const messages = new Map();
const fromHistory = new Set(); // ids a history read returned (not a send answer)
let generation = 0; // counts restarts of the list
// chat.unsent: {text, key, conversation, tries} for a send under way or
// without a clear "stored", written before the post and kept per agent in
// sessionStorage. The same text to the same conversation reuses key, so the
// hub stores it once while it remembers the key (a few minutes, per user,
// hence conversation). The page never infers from history that a send
// arrived: only the hub's answer to the same key clears the draft.
let loaded = false; // a history read has succeeded
let reading = false; // a history read is under way
let readAgain = false; // something changed while it was
let olderCursor = '';
let sending = false;
let uploading = false; // a file upload is under way
let filesOpen = false; // the Files panel is open
let filesSeq = 0; // counts Files panel reads and closes
let activityOpen = false; // the Recent activity panel is open
let activitySeq = 0; // counts its reads and closes
let waking = ''; // the agent a send is waking ('' = none)
let renewing = ''; // the agent a new conversation is starting ('' = none)
let renewed = null; // {name, text}: how the last one ended, until a send or another chat
let held = false; // Send rests after a note (see hold)
let stream = null;
let streamFailed = false;

// contacts: the operator's read-only view of contact conversations (null for
// a contact login). view is the open transcript ({login, name}); it never
// touches chat, the composer or the read marker.
let contacts = null;
const openContacts = new Set(); // contact logins whose agents show
let view = null;
let viewSeq = 0; // counts transcript opens and closes
let viewReading = false;
let viewOlder = '';
const transcript = new Map();

// api never throws: a network fault is status 0. A redirect is never
// followed: the hub answers an unknown session with a redirect to its login
// page, which followed would read as a 200.
async function api(path, init, ms = REQUEST_MS) {
  const limit = new AbortController();
  const timer = setTimeout(() => limit.abort(), ms);
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

function showNote(text) {
  setText(el.note, text);
  el.note.hidden = !text;
}

function showListNote(text) {
  setText(el.listnote, text);
  el.listnote.hidden = !text;
}

function showState(text, ok) {
  setText(el.state, text);
  el.state.className = text ? `state ${ok ? 'ok' : 'bad'}` : 'state';
}

function store(key, value) {
  try {
    if (value === null) sessionStorage.removeItem(key);
    else sessionStorage.setItem(key, value);
  } catch {
    // storage is off: the value just does not survive a reload
  }
}

function stored(key) {
  try {
    return sessionStorage.getItem(key);
  } catch {
    return null; // storage is off
  }
}

// localLink is p as a link target on this origin, or ''. The browser's URL
// parser decides (it drops tabs, newlines and dot segments a string check
// would miss), and the parsed URL is returned whole so nothing reparses it:
// "/.//evil.test" has the path "//evil.test", another host if read again.
function localLink(p) {
  if (typeof p !== 'string' || !p.startsWith('/')) return '';
  try {
    const u = new URL(p, location.origin);
    return u.origin === location.origin && !u.pathname.startsWith('//') ? u.href : '';
  } catch {
    return '';
  }
}

// A contact's list carries no links, and the page builds none for a contact.
function setLink(a, path) {
  const href = localLink(path);
  a.hidden = !href;
  if (href) a.setAttribute('href', href);
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
  speakButtons.clear();
  let shown = 0;
  for (const m of sortedMessages(messages)) {
    const kind = classify(m, chat.userId, chat.id);
    if (kind === 'hidden') continue;
    shown++;
    const row = document.createElement('div');
    row.className = `msg ${kind}`;
    if (kind !== 'system') {
      const meta = document.createElement('div');
      meta.className = 'meta';
      setText(meta, `${kind === 'mine' ? 'You' : chat.name} ${when(m)}`.trim());
      row.append(meta);
    }
    const body = document.createElement('div');
    body.className = 'body';
    // A system line is the hub's (classify gives the agent's side none),
    // on one line: no text can lay out a notice of its own.
    setText(body, kind === 'system' ? `hub: ${oneLine(messageText(m), SYSTEM_MAX)}` : messageText(m));
    row.append(body);
    if (kind === 'agent' && readAloudOn()) row.append(speakButton(m));
    if (kind === 'mine' && m.dispatchState === 'failed') {
      const fail = document.createElement('div');
      fail.className = 'fail';
      const why = oneLine(m.dispatchFailureReason, 200);
      setText(fail, why ? `Not delivered: ${why}` : 'Not delivered');
      row.append(fail);
    }
    frag.append(row);
  }
  el.list.replaceChildren(frag);
  showNotice(shown ? '' : 'No messages yet.');
  if (stick) el.scroll.scrollTop = el.scroll.scrollHeight;
}

function span(className, text) {
  const s = document.createElement('span');
  s.className = className;
  setText(s, text);
  return s;
}

function renderRows() {
  const frag = document.createDocumentFragment();
  for (const a of roster.agents) {
    const li = document.createElement('li');
    li.className = `${a.access === 'see' ? 'view' : ''} ${a.name === current ? 'current' : ''}`.trim();
    const b = document.createElement('button');
    // a.state is one of agentList's fixed words.
    b.append(span('title', rowTitle(a)), span(`chip ${a.state}`, chipText(a)), span('badge', badgeText(a.unread)));
    if (a.access === 'see') b.append(span('viewtag', 'view only'));
    b.addEventListener('click', () => openChat(a.name));
    li.append(b);
    frag.append(li);
  }
  el.agents.replaceChildren(frag);
}

function renderContacts() {
  const has = !!contacts && contacts.length > 0;
  el.contactsTitle.hidden = !has;
  el.contacts.hidden = !has;
  const frag = document.createDocumentFragment();
  for (const c of has ? contacts : []) {
    const li = document.createElement('li');
    const b = document.createElement('button');
    b.append(span('title', c.login), span('note', c.signedIn ? '' : 'not signed in yet'));
    b.addEventListener('click', () => toggleContact(c.login));
    li.append(b);
    if (openContacts.has(c.login)) {
      const ul = document.createElement('ul');
      ul.className = 'agents nested';
      for (const a of c.agents) {
        const ali = document.createElement('li');
        ali.className = view && view.login === c.login && view.name === a.name ? 'current' : '';
        const ab = document.createElement('button');
        // a.state is one of contactList's fixed words.
        ab.append(span('title', rowTitle(a)), span(`chip ${a.state}`, chipText(a)));
        ab.addEventListener('click', () => openTranscript(c.login, a.name));
        ali.append(ab);
        ul.append(ali);
      }
      li.append(ul);
    }
    frag.append(li);
  }
  el.contacts.replaceChildren(frag);
}

function toggleContact(login) {
  if (openContacts.has(login)) openContacts.delete(login);
  else openContacts.add(login);
  renderContacts();
}

async function reloadContacts() {
  const res = await api('/lever/api/contacts');
  const l = res.ok ? contactList(res.body) : null;
  if (l) contacts = l; // a failed read keeps the last list
  renderContacts();
}

function renderTranscript(toBottom) {
  const stick = toBottom || nearBottom();
  const frag = document.createDocumentFragment();
  for (const m of sortedMessages(transcript)) {
    const row = document.createElement('div');
    // m.from is one of transcriptItems' fixed words.
    row.className = `msg ${m.from}${m.shownToContact ? '' : m.pending ? ' pending' : ' unshown'}`;
    const meta = document.createElement('div');
    meta.className = 'meta';
    setText(meta, `${transcriptWho(m, view.login, view.name)} ${when(m)}`.trim());
    const body = document.createElement('div');
    body.className = 'body';
    setText(body, m.text || '(no text)');
    row.append(meta, body);
    if (!m.shownToContact) {
      const mark = document.createElement('div');
      mark.className = 'mark';
      setText(mark, m.pending ? NOT_YET : NOT_SHOWN);
      row.append(mark);
    }
    frag.append(row);
  }
  el.list.replaceChildren(frag);
  if (stick) el.scroll.scrollTop = el.scroll.scrollHeight;
}

// One read at a time; an answer for a transcript no longer open is dropped.
async function readTranscript(cursor) {
  const v = view;
  if (!v || viewReading) return;
  viewReading = true;
  const seq = viewSeq;
  try {
    const res = await api(transcriptPath(v.login, v.name, cursor));
    if (seq !== viewSeq) return;
    if (!res.ok) {
      showNotice(`Cannot read the conversation: ${viewErrorText(res.status, res.body)}`);
      return;
    }
    const first = transcript.size === 0;
    if (cursor || first) viewOlder = nextCursor(res.body);
    el.older.hidden = !viewOlder;
    const changed = mergeRows(transcript, transcriptItems(res.body));
    if (res.body && res.body.matched === false) showNotice('The broker did not answer: which agent messages the contact sees is not known.');
    else showNotice(transcript.size ? '' : 'No messages yet.');
    if (changed || first) renderTranscript(first);
  } finally {
    viewReading = false;
  }
}

function leaveTranscript() {
  view = null;
  viewSeq++;
  transcript.clear();
  viewOlder = '';
  el.readonly.hidden = true;
  el.refresh.hidden = true;
}

function openTranscript(login, name) {
  const c = contacts && contacts.find((x) => x.login === login);
  const a = c && c.agents.find((x) => x.name === name);
  if (!a) return;
  closeChat();
  view = { login, name, signedIn: !!c.signedIn, noFiles: !!c.noFiles };
  document.body.classList.add('chatting');
  document.title = name;
  setText(el.agent, `${login} · ${name}`);
  setText(el.label, a.label);
  el.label.hidden = !a.label;
  showState('read only', true);
  el.form.hidden = true;
  el.readonly.hidden = false;
  el.refresh.hidden = false;
  showNotice(c.signedIn ? '' : `${login} has not signed in yet.`);
  renderContacts();
  if (c.signedIn) void readTranscript('');
  void loadViewFiles();
}

function pollView() {
  if (document.visibilityState !== 'visible') return;
  void reloadContacts();
  // A contact that has not signed in has no conversation to read (the read
  // answers not-signed-in): the poll leaves its view and note as they are.
  if (view && view.signedIn) {
    void readTranscript('');
    void loadViewFiles();
  }
}

async function loadViewFiles() {
  const v = view;
  if (!v || v.noFiles || !roster || !roster.files) return;
  const seq = ++filesSeq;
  el.filespanel.hidden = false;
  const res = await api(viewFilesPath(v.login, v.name));
  if (seq !== filesSeq || view !== v) return;
  const list = res.ok ? fileList(res.body) : null;
  if (!list) {
    showFilesNote(`The files cannot be read: ${viewErrorText(res.status, res.body)}`);
    return;
  }
  showFilesNote(list.length ? '' : 'No files yet.');
  el.filelist.replaceChildren(...list.map((f) => fileRow(v.name, f, v.login)));
}

function historyPath(c, cursor) {
  const q = new URLSearchParams({ limit: String(PAGE) });
  if (cursor) q.set('cursor', cursor);
  return `/api/v1/chat/conversations/${encodeURIComponent(c.conversation)}/messages?${q}`;
}

// Every live signal ends in refresh: the page never trusts an event's
// payload, only the hub's history. One read at a time, a signal during a read
// asking for one more, so answers apply in order.
async function refresh() {
  const c = chat;
  if (!c || !c.conversation) return;
  if (reading) {
    readAgain = true;
    return;
  }
  reading = true;
  try {
    const res = await api(historyPath(c, ''));
    if (chat === c) {
      applyHistory(res);
      if (res.ok) void markRead(c);
    } else {
      readAgain = true; // the open chat still needs its read
    }
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
  // A full page with nothing already held means more than a page arrived
  // unseen: restart from it rather than show a hole. Only ids from history
  // reads count: a just-sent message is in every newest page.
  const restart = !loaded || (items.length >= PAGE && !items.some((m) => fromHistory.has(m.id)));
  if (restart) {
    messages.clear();
    fromHistory.clear();
    generation++;
    olderCursor = nextCursor(res.body);
    el.older.hidden = !olderCursor;
  }
  const first = !loaded;
  // New agent replies, for auto read-aloud: never the first page, and never
  // a restart, which may be a page of history the chat had not shown.
  const fresh = restart ? [] : sortedMessages(new Map(items.filter((m) => typeof m.id === 'string' && !messages.has(m.id) &&
    classify(m, chat.userId, chat.id) === 'agent').map((m) => [m.id, m])));
  loaded = true;
  for (const m of items) fromHistory.add(m.id);
  if (mergeMessages(messages, items) || restart) render(first);
  queueReplies(chat, fresh);
}

let acted = false;

// Until the user acts the page marks nothing read: a link from another site
// can open it (even in a window it closes) and pick the chat with #agent=.
// A tap on a lever notification, relayed by the worker, counts.
function hasActed() {
  return acted || Boolean(navigator.userActivation && navigator.userActivation.hasBeenActive);
}

function onAct() {
  if (acted) return;
  acted = true;
  if (chat && chat.conversation) void markRead(chat);
}
for (const type of ['pointerdown', 'keydown', 'touchstart']) document.addEventListener(type, onAct, { capture: true, passive: true });

async function markRead(c) {
  if (document.visibilityState !== 'visible' || !hasActed()) return;
  const newest = sortedMessages(messages).filter((m) => fromHistory.has(m.id)).pop();
  if (!newest || typeof newest.id !== 'string' || lastMarked.get(c.conversation) === newest.id) return;
  lastMarked.set(c.conversation, newest.id);
  const res = await api(`/api/v1/chat/conversations/${encodeURIComponent(c.conversation)}/read`, {
    method: 'POST',
    headers: { Accept: 'application/json', 'Content-Type': 'application/json' },
    body: JSON.stringify({ messageId: newest.id }),
  });
  if (res.ok) {
    listSoon();
    return;
  }
  // A fault may pass: a later read tries again. A refusal will not.
  if (unclear(res) && lastMarked.get(c.conversation) === newest.id) lastMarked.delete(c.conversation);
}

// NOTE_REPLAY keeps the draft: a retry and the same words meant twice look
// alike. The first answer may have been lost before the proxy recorded it,
// so the agent may be unable to verify the message.
const NOTE_STORED_AGAIN = 'Stored now. An earlier attempt had no clear answer: if the message shows twice above, both arrived.';
const NOTE_TWICE = 'If it shows twice above, two attempts arrived.';
const NOTE_REPLAY = 'The earlier attempt did arrive: the message is in the conversation, and nothing new was stored. ' +
  'Press Send again only to post the same words a second time. ' +
  'If the agent treats the message as unverified, send it in other words.';

function setUnsent(c, v) {
  c.unsent = v;
  store(unsentKey(c.name), v ? JSON.stringify(v) : null);
}

// A record for another conversation is dropped: its key would get the hub's
// answer for the old one.
function loadUnsent(c) {
  try {
    const v = JSON.parse(stored(unsentKey(c.name)) || 'null');
    if (!v || typeof v.text !== 'string' || typeof v.key !== 'string' || v.conversation !== c.conversation) {
      store(unsentKey(c.name), null);
      return;
    }
    // A record without a count is from a post: one try.
    c.unsent = { text: v.text, key: v.key, conversation: v.conversation, tries: Number.isInteger(v.tries) && v.tries >= 0 ? v.tries : 1 };
  } catch {
    store(unsentKey(c.name), null); // not a record
  }
}

// Moved to the per-agent keys once, so an unclear send keeps its key.
function adoptOldRecords(l) {
  const oldUnsent = stored(OLD_UNSENT_KEY);
  const oldDraft = stored(OLD_DRAFT_KEY);
  if (oldUnsent === null && oldDraft === null) return;
  let owner = l.agents.find((a) => a.role === 'manager' && a.access === 'message');
  try {
    const v = JSON.parse(oldUnsent || 'null');
    const match = v && typeof v.conversation === 'string' && l.agents.find((a) => a.conversation && a.conversation === v.conversation);
    if (match) owner = match;
    if (match && stored(unsentKey(match.name)) === null) store(unsentKey(match.name), oldUnsent);
  } catch {
    // not a record: dropped below
  }
  if (owner && oldDraft && !stored(draftKey(owner.name))) store(draftKey(owner.name), oldDraft);
  store(OLD_UNSENT_KEY, null);
  store(OLD_DRAFT_KEY, null);
}

const refreshSoon = makeCoalescer(() => void refresh(), REFRESH_GAP_MS);
const listSoon = makeCoalescer(() => void reloadList(), REFRESH_GAP_MS);

async function loadOlder() {
  const c = chat;
  if (!c || !olderCursor) return;
  el.older.disabled = true;
  const asked = generation;
  const res = await api(historyPath(c, olderCursor));
  el.older.disabled = false;
  // The list restarted meanwhile, or another chat opened: this page
  // belongs to the old one.
  if (asked !== generation || chat !== c) return;
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

async function readList() {
  const seq = ++listSeq;
  const res = await api('/lever/api/agents');
  const l = res.ok ? agentList(res.body) : null;
  if (!l) return { error: errorText(res.status, res.body), status: res.status, seq };
  l.seq = seq;
  return l;
}

const managerRow = () => roster && roster.agents.find((a) => a.role === 'manager' && a.access === 'message');

// An answer older than the one shown is dropped. An agent with a new hub
// record moves its chat to the new conversation: a message to the old one
// would reach nobody.
function applyRoster(l) {
  if (l.seq < appliedSeq) return;
  appliedSeq = l.seq;
  roster = l;
  renderRows();
  syncAttach();
  setLink(el.console, l.tier === 'operator' ? l.console : '');
  syncActivity();
  if (!current) return;
  const a = l.agents.find((x) => x.name === current);
  if (!a) {
    closeChat();
    return;
  }
  // An unknown state says nothing about the record: the chat stays.
  if (a.access !== 'message' || !chat || (a.state !== 'unknown' && a.conversation !== chat.conversation)) {
    openChat(current);
    return;
  }
  applyView(a);
}

// Every state goes unknown, so inputs stay off until a read succeeds.
function degrade(failed) {
  if (!roster) return;
  showListNote(`The agent list cannot be read: ${failed.error}. Trying again.`);
  applyRoster({ ...roster, seq: failed.seq, agents: roster.agents.map((a) => ({ ...a, state: 'unknown', ...(a.access === 'message' ? { activity: '' } : {}) })) });
}

// One read at a time, like refresh.
async function reloadList() {
  if (listing) {
    listAgain = true;
    return;
  }
  listing = true;
  try {
    const l = await readList();
    if (l.error) {
      degrade(l);
    } else {
      showListNote('');
      applyRoster(l);
    }
  } finally {
    listing = false;
    if (listAgain) {
      listAgain = false;
      listSoon();
    }
  }
}

function applyView(a) {
  if (!chat || a.name !== chat.name) return;
  const v = inputView(a, !!managerRow(), roster && roster.tier);
  chat.view = v;
  showState(chipText(a), v.input);
  el.text.disabled = !v.input;
  syncSend();
  const after = renewed && renewed.name === a.name ? renewed.text : '';
  showNote(waking === a.name ? `waking ${a.name}…` : renewing === a.name ? `starting ${a.name} on a new conversation…` : v.note || after);
  syncNewConversation(a);
  el.ask.hidden = !v.ask;
  setText(el.ask, v.ask ? `Ask the manager to start ${a.name}` : '');
}

function syncSend() {
  el.send.disabled = sending || held || !chat || !chat.view || !chat.view.input;
  syncAttach();
}

function showUpload(text) {
  setText(el.upload, text);
  el.upload.hidden = !text;
}

function showFilesNote(text) {
  setText(el.filesnote, text);
  el.filesnote.hidden = !text;
}

function syncAttach() {
  const on = !!(roster && roster.files && chat);
  el.attach.hidden = !on || !roster.files.uploads;
  el.files.hidden = !on;
  el.attach.disabled = uploading || sending || !chat || !chat.view || !chat.view.input;
  syncMic();
  syncReadVoice();
}

// Dictation (remote.voice): the clip is resampled to 16 kHz mono WAV and
// posted to lever; its text goes into the message box for review, or with
// the device's auto-send option on is sent at once through send().
let recording = null; // {recorder, stream, chunks, started, max, agent, chat, autoSend, timer, discard, done}
let transcribing = false;

const micSupported = () => !!(navigator.mediaDevices && typeof navigator.mediaDevices.getUserMedia === 'function') &&
  typeof window.MediaRecorder === 'function' && typeof window.OfflineAudioContext === 'function';

function showVoice(text) {
  setText(el.voice, text);
  el.voice.hidden = !text;
}

// The mic stays shown while recording: it is the stop control.
function syncMic() {
  el.mic.hidden = !recording && !micOffered();
  el.mic.disabled = transcribing || (!recording && (!chat || !chat.view || !chat.view.input));
  const auto = autoSendOn();
  el.mic.className = `${recording ? 'recording' : ''}${auto ? ' autosend' : ''}`.trim();
  const name = auto ? 'Dictate and send: tap to record, tap again to stop and send' : 'Dictate: tap to record, tap again to stop';
  el.mic.setAttribute('aria-label', name);
  el.mic.title = name;
  setText(el.mic, recording ? '■' : '🎤');
}

function stopTracks(stream) {
  for (const t of stream && typeof stream.getTracks === 'function' ? stream.getTracks() : []) t.stop();
}

let micStarting = false; // a getUserMedia prompt is up

async function toggleMic() {
  if (recording) {
    stopDictation();
    return;
  }
  const c = chat;
  const cfg = roster && roster.voice;
  if (!c || !cfg || transcribing || micStarting || !micSupported()) return;
  // The speaker would be recorded.
  stopReading();
  showError('');
  let stream;
  // One permission prompt at a time.
  micStarting = true;
  try {
    stream = await navigator.mediaDevices.getUserMedia({ audio: { channelCount: 1, echoCancellation: true, noiseSuppression: true } });
  } catch {
    showError('The microphone is not available: allow it for this page in the browser\'s settings.');
    return;
  } finally {
    micStarting = false;
  }
  // A reply that began while the prompt was up would be recorded.
  stopReading();
  // The page went away while the prompt was up: nothing to record for.
  if (chat !== c || recording || document.visibilityState === 'hidden') {
    stopTracks(stream);
    return;
  }
  let recorder;
  try {
    recorder = new window.MediaRecorder(stream);
  } catch {
    stopTracks(stream);
    showError('This browser cannot record here.');
    return;
  }
  const r = { recorder, stream, chunks: [], started: Date.now(), max: cfg.maxSeconds, agent: c.name, chat: c, autoSend: autoSendOn(), timer: 0, discard: false, done: false };
  recorder.addEventListener('dataavailable', (ev) => {
    if (ev.data && ev.data.size) r.chunks.push(ev.data);
  });
  recorder.addEventListener('stop', () => void finishDictation(r));
  try {
    // No timeslice: one chunk at the stop. Safari writes fragmented MP4
    // when asked for chunks, which its own decoder may not take whole.
    recorder.start();
  } catch {
    stopTracks(stream);
    showError('This browser cannot record here.');
    return;
  }
  recording = r;
  r.timer = setInterval(() => tickDictation(r), 500);
  tickDictation(r);
  syncMic();
}

function tickDictation(r) {
  if (recording !== r) return;
  const s = (Date.now() - r.started) / 1000;
  showVoice(`Recording ${clockText(s)} of ${clockText(r.max)}. Tap ■ to stop.`);
  if (s >= r.max) stopDictation();
}

// finishDictation runs on the recorder's stop event and releases the mic.
function stopDictation() {
  const r = recording;
  if (!r) return;
  recording = null;
  clearInterval(r.timer);
  showVoice('');
  try {
    if (r.recorder.state === 'inactive') void finishDictation(r);
    else r.recorder.stop();
  } catch {
    void finishDictation(r);
  }
  syncMic();
}

function cancelDictation() {
  if (recording) recording.discard = true;
  stopDictation();
}

// The microphone must not go on listening while the page is hidden, where
// the tick that stops it at the limit is throttled. The clip is not sent.
function dropHiddenDictation() {
  if (!recording) return;
  cancelDictation();
  showError('Dictation stopped because the page was hidden; nothing was sent.');
}

async function finishDictation(r) {
  if (r.done) return;
  r.done = true;
  stopTracks(r.stream);
  if (r.discard) return;
  transcribing = true;
  showVoice('Transcribing…');
  syncMic();
  try {
    const clip = new Blob(r.chunks, { type: r.recorder.mimeType || '' });
    const wav = await toWAV(await clip.arrayBuffer(), r.max);
    if (!wav) {
      showError('Nothing was recorded.');
      return;
    }
    const res = await api('/lever/api/voice/transcribe', {
      method: 'POST',
      // The proxy refuses a clip without X-Lever-Voice, as an upload without
      // X-Lever-Upload: no other site can send it.
      headers: { Accept: 'application/json', 'Content-Type': 'audio/wav', 'X-Lever-Voice': '1' },
      body: wav,
    }, voiceRequestMs(r.max));
    const text = res.ok ? transcriptOf(res.body) : null;
    if (text === null) {
      showError(`Not transcribed: ${voiceErrorText(res.status, res.body)}`);
      return;
    }
    if (!text) {
      showError('Nothing was heard in the recording.');
      return;
    }
    if (r.autoSend && autoSendOn() && chat === r.chat) {
      sendTranscript(text);
      return;
    }
    placeTranscript(r.agent, text);
  } catch {
    showError('The recording could not be read in this browser.');
  } finally {
    transcribing = false;
    showVoice('');
    syncMic();
  }
}

// null for a clip too short to hold a word.
async function toWAV(buf, max) {
  const decoder = new window.OfflineAudioContext(1, 1, VOICE_RATE);
  const audio = await decoder.decodeAudioData(buf);
  const length = Math.min(resampledLength(audio.length, audio.sampleRate), max * VOICE_RATE);
  if (length < VOICE_MIN_SAMPLES) return null;
  const mix = new window.OfflineAudioContext(1, length, VOICE_RATE);
  const source = mix.createBufferSource();
  source.buffer = audio;
  source.connect(mix.destination);
  source.start();
  const out = await mix.startRendering();
  return encodeWAV(out.getChannelData(0));
}

// Auto-send: the transcript goes after what is in the box, and the whole box
// is sent through send(), as a tap on Send would. finishDictation calls it
// only while the chat the recording started in is the one shown, as the same
// chat object: a chat left and opened again, or moved to a new hub record,
// is a new one. A transcript never goes to another chat.
function sendTranscript(text) {
  const v = el.text.value.trimEnd();
  el.text.value = insertText(v, v.length, v.length, text).value;
  saveDraft();
  grow();
  if (sending || held) {
    showError('The dictation is in the message box: press Send when the message before it has gone.');
    return;
  }
  // An earlier attempt without a clear answer keeps its key only for the
  // same text: sent with the dictation added, it could arrive twice.
  if (chat.unsent) {
    showError('The dictation is in the message box, after a message that may not have arrived: look at the conversation, then press Send.');
    return;
  }
  if (!chat.view || !chat.view.input) {
    showError('The dictation is in the message box; it was not sent, as the chat cannot take a message now.');
    return;
  }
  void send();
}

function placeTranscript(agent, text) {
  if (chat && chat.name === agent) {
    const { value, cursor } = insertText(el.text.value, el.text.selectionStart, el.text.selectionEnd, text);
    el.text.value = value;
    try {
      el.text.setSelectionRange(cursor, cursor);
    } catch {
      // no selection to set: the text is in all the same
    }
    saveDraft();
    grow();
    el.text.focus();
    return;
  }
  const draft = stored(draftKey(agent)) || '';
  store(draftKey(agent), insertText(draft, draft.length, draft.length, text).value);
}

// Read-aloud: server voices (lever-tool-tts on the operator's host) or, as
// fallback, the browser's on-device voices only (localService), so the text
// never goes to a speech service. The voice is kept per device and kind.
const READ_VOICE_KEY = 'lever-read-voice';
const SERVER_VOICE_KEY = 'lever-read-server-voice';
const SPEAK_PATH = '/lever/api/voice/speak';
// SPEAK_MS bounds one part: the tool gives a run 2 minutes, and the proxy
// may queue it behind two others.
const SPEAK_MS = 7 * 60000;
let speaking = null; // {id, button, stop}: the message being read
let audioCtx = null; // one per page, made and resumed on a tap
const speakButtons = new Map(); // message id → its speaker in the list shown
const replyQueue = []; // new agent replies waiting to be read aloud by themselves
let audioUnlocked = false; // a tap has let this page play sound
let hintShown = false; // the tap-to-enable hint has been shown once

const speechOn = () => !!(window.speechSynthesis && typeof window.SpeechSynthesisUtterance === 'function');
const serverSpeech = () => (roster && roster.readAloud && roster.speech) || null;
const readAloudOn = () => !!(roster && roster.readAloud) && (speechOn() || !!serverSpeech());

// A voice choice lasts on the device (localStorage), unlike per-tab drafts.
function kept(key) {
  try {
    return localStorage.getItem(key) || '';
  } catch {
    return ''; // storage is off
  }
}

function keep(key, value) {
  try {
    localStorage.setItem(key, value);
  } catch {
    // storage is off: the choice lasts for this page load
  }
}

const savedReadVoice = () => kept(READ_VOICE_KEY);

function speechLang() {
  return (document.documentElement && document.documentElement.lang) || navigator.language || '';
}

function speakButton(m) {
  const b = document.createElement('button');
  b.type = 'button';
  b.className = 'speak';
  b.title = 'Read aloud';
  const on = !!speaking && speaking.id === m.id;
  if (on) speaking.button = b;
  setText(b, on ? '⏹' : '🔊');
  speakButtons.set(m.id, b);
  b.addEventListener('click', () => {
    // A tap on any speaker ends the replies queued to be read by themselves.
    replyQueue.length = 0;
    speak(m, b);
  });
  return b;
}

function stopReading() {
  replyQueue.length = 0;
  stopSpeaking();
}

function stopSpeaking() {
  const s = speaking;
  speaking = null;
  if (s && s.button) setText(s.button, '🔊');
  if (s && s.stop) s.stop();
  if (s && speechOn()) window.speechSynthesis.cancel();
}

function finished(s) {
  if (speaking !== s) return;
  speaking = null;
  setText(s.button, '🔊');
  // A microtask, so a server voice that falls back to the device voice has
  // taken speaking again before the next reply would start.
  if (replyQueue.length) void Promise.resolve().then(readNextReply);
}

// Device options (localStorage, per login, like the push opt-in): on a
// shared device one login's choice never sends or speaks for another.
const autoSendKey = () => `lever-auto-send:${roster ? roster.login : ''}`;
const autoReadKey = () => `lever-auto-read:${roster ? roster.login : ''}`;
const micOffered = () => !!(roster && roster.voice && chat && micSupported());
// Storage off: the choice lasts for this page load.
const optionMemo = new Map();
function option(key) {
  const v = kept(key);
  return v === '1' || v === '0' ? v === '1' : optionMemo.get(key) === true;
}
function setOption(key, on) {
  optionMemo.set(key, on);
  keep(key, on ? '1' : '0');
}
const autoSendOn = () => micOffered() && option(autoSendKey());
const autoReadOn = () => !!chat && readAloudOn() && option(autoReadKey());

// Only replies the open chat's history read brought after the first one
// (refresh, never loadOlder or a restart) reach here.
// The microphone may be open: anything played now could be recorded and,
// with auto-send, sent as the login's own words.
const micBusy = () => !!recording || micStarting || transcribing;

// At most REPLY_QUEUE replies wait, the newest: an agent that floods the chat
// cannot keep the device (and the login's server-voice allowance) busy for
// longer than that without a tap.
const REPLY_QUEUE = 3;
function queueReplies(c, fresh) {
  if (!fresh.length || chat !== c || !autoReadOn() || micBusy() || document.visibilityState !== 'visible') return;
  replyQueue.push(...fresh);
  replyQueue.splice(0, Math.max(0, replyQueue.length - REPLY_QUEUE));
  if (!audioUnlocked) {
    if (!hintShown) {
      hintShown = true;
      el.readhint.hidden = false;
    }
    return;
  }
  readNextReply();
}

function readNextReply() {
  while (!speaking && replyQueue.length && audioUnlocked && !micBusy()) {
    const m = replyQueue.shift();
    const b = speakButtons.get(m.id);
    if (b && chat && messages.has(m.id)) speak(m, b);
  }
}

// Browsers play sound only after a tap. The first tap on the page, with the
// auto-read option on, wakes both read-aloud paths: Web Audio is resumed, and
// an empty utterance is spoken, which iOS needs before it speaks one later.
// A tap on the agent list counts too: it runs before the chat it opens.
function unlockAudio() {
  if (audioUnlocked || !readAloudOn() || !option(autoReadKey())) return;
  audioUnlocked = true;
  el.readhint.hidden = true;
  if (serverSpeech()) {
    const ctx = audioContext();
    if (ctx && typeof ctx.resume === 'function') void ctx.resume().catch(() => {});
  }
  if (speechOn() && !speaking) {
    try {
      window.speechSynthesis.speak(new window.SpeechSynthesisUtterance(''));
    } catch {
      // the device voice stays locked; the server voice may still play
    }
  }
  readNextReply();
}
for (const type of ['click', 'keydown', 'touchend']) document.addEventListener(type, unlockAudio, { capture: true, passive: true });

function speak(m, button) {
  const again = !!speaking && speaking.id === m.id;
  stopSpeaking();
  if (again) return;
  const text = speechText(messageText(m));
  if (!text) return;
  const sv = serverSpeech();
  if (sv && chat && audioContext()) {
    speakServer(m, button, text, sv, chat.name);
    return;
  }
  speakDevice(m, button, text);
}

function speakDevice(m, button, text) {
  if (!speechOn()) {
    showError('This device has no on-device voice to read with.');
    return;
  }
  const v = pickVoice(window.speechSynthesis.getVoices(), savedReadVoice(), speechLang());
  if (!v) {
    showError('This device has no on-device voice to read with.');
    return;
  }
  const u = new window.SpeechSynthesisUtterance(text);
  u.voice = v;
  u.lang = v.lang;
  const s = { id: m.id, button };
  u.addEventListener('end', () => finished(s));
  u.addEventListener('error', () => finished(s));
  speaking = s;
  setText(button, '⏹');
  window.speechSynthesis.speak(u);
}

function audioContext() {
  if (audioCtx) return audioCtx;
  const AC = window.AudioContext || window.webkitAudioContext;
  if (typeof AC !== 'function') return null;
  try {
    audioCtx = new AC();
  } catch {
    audioCtx = null;
  }
  return audioCtx;
}

// Never throws.
async function speakPart(s, agent, id, text, voice) {
  const limit = new AbortController();
  s.aborts.add(limit);
  const timer = setTimeout(() => limit.abort(), SPEAK_MS);
  try {
    const res = await fetch(SPEAK_PATH, {
      method: 'POST',
      credentials: 'same-origin',
      redirect: 'manual',
      signal: limit.signal,
      // The proxy refuses a request without X-Lever-Voice: a custom header
      // needs a CORS preflight across origins.
      headers: { Accept: 'audio/wav, application/json', 'Content-Type': 'application/json', 'X-Lever-Voice': '1' },
      body: JSON.stringify({ agent, id: String(id), text, voice }),
    });
    if (res.ok && (res.headers.get('Content-Type') || '').startsWith('audio/wav')) {
      return { ok: true, data: await res.arrayBuffer() };
    }
    const raw = await res.text();
    let body = raw;
    try {
      body = JSON.parse(raw);
    } catch {
      // not JSON: keep the text
    }
    return { ok: false, status: res.status, body };
  } catch {
    return { ok: false, status: 0, body: limit.signal.aborted ? 'no answer in time' : 'cannot reach the server' };
  } finally {
    clearTimeout(timer);
    s.aborts.delete(limit);
  }
}

function playBuffer(ctx, buf, s) {
  return new Promise((resolve) => {
    const src = ctx.createBufferSource();
    src.buffer = buf;
    src.connect(ctx.destination);
    src.addEventListener('ended', () => resolve());
    s.source = src;
    src.start();
  });
}

async function decode(ctx, data) {
  try {
    return await ctx.decodeAudioData(data);
  } catch {
    return null;
  }
}

// One request at a time, as the proxy allows: the next part is asked for
// while one plays. A first-part failure falls back to the device voice.
function speakServer(m, button, text, sv, agent) {
  const ctx = audioContext();
  try {
    // iOS: play even with the ring switch on silent, as a media player
    // does (Safari 16.4 and later).
    if (navigator.audioSession) navigator.audioSession.type = 'playback';
  } catch {
    // not settable here: the browser's default applies
  }
  // Resumed within the tap, so the browser lets it play once the audio
  // arrives.
  if (typeof ctx.resume === 'function') void ctx.resume().catch(() => {});
  const parts = speechParts(text, sv.maxChars);
  if (!parts.length) return;
  const voice = pickServerVoice(sv, kept(SERVER_VOICE_KEY));
  const s = { id: m.id, button, aborts: new Set(), source: null };
  s.stop = () => {
    for (const a of s.aborts) a.abort();
    if (s.source) {
      try {
        s.source.stop();
      } catch {
        // not started, or ended already
      }
    }
  };
  speaking = s;
  setText(button, '⏹');
  void (async () => {
    let next = speakPart(s, agent, m.id, parts[0], voice);
    for (let i = 0; i < parts.length; i++) {
      const got = await next;
      if (speaking !== s) return;
      const buf = got.ok ? await decode(ctx, got.data) : null;
      if (speaking !== s) return;
      if (!buf) {
        finished(s);
        if (i === 0 && speechOn()) {
          speakDevice(m, button, text);
          return;
        }
        showError(`Reading aloud stopped: ${got.ok ? 'the audio could not be played' : speakErrorText(got.status, got.body)}`);
        return;
      }
      next = i + 1 < parts.length ? speakPart(s, agent, m.id, parts[i + 1], voice) : null;
      await playBuffer(ctx, buf, s);
      if (speaking !== s) return;
    }
    finished(s);
  })();
}

// The options are rebuilt only when the voices change.
let readVoices = '';
function syncReadVoice() {
  const sv = readAloudOn() ? serverSpeech() : null;
  let options = [];
  let chosen = '';
  if (sv) {
    options = sv.voices.map((v) => ({ value: v.name, label: v.lang ? `${v.name} (${v.lang})` : v.name }));
    chosen = pickServerVoice(sv, kept(SERVER_VOICE_KEY));
  } else if (readAloudOn()) {
    const voices = localVoices(window.speechSynthesis.getVoices());
    options = voices.map((v) => ({ value: v.voiceURI, label: `${oneLine(v.name, 60)} (${oneLine(v.lang, 20)})` }));
    const v = pickVoice(voices, savedReadVoice(), speechLang());
    chosen = v ? v.voiceURI : '';
  }
  el.readvoicerow.hidden = !chat || options.length < 2;
  syncSettings();
  const key = `${sv ? 'server' : 'device'}\n${options.map((o) => o.value).join('\n')}`;
  if (el.readvoicerow.hidden || key === readVoices) return;
  readVoices = key;
  el.readvoice.replaceChildren(...options.map((o) => {
    const opt = document.createElement('option');
    opt.value = o.value;
    setText(opt, o.label);
    return opt;
  }));
  el.readvoice.value = chosen;
}

function saveReadVoice(value) {
  keep(serverSpeech() ? SERVER_VOICE_KEY : READ_VOICE_KEY, value);
}

// An icon button shows its panel's state with aria-expanded, not a label.
function expanded(button, open) {
  button.setAttribute('aria-expanded', open ? 'true' : 'false');
}

function filesShown(open) {
  expanded(el.files, open);
}

// The settings button shows while any of its options applies to this chat.
function syncSettings() {
  el.autosendrow.hidden = !micOffered();
  el.autoreadrow.hidden = !(chat && readAloudOn());
  el.autosend.checked = option(autoSendKey());
  el.autoread.checked = option(autoReadKey());
  el.settings.hidden = el.readvoicerow.hidden && el.autosendrow.hidden && el.autoreadrow.hidden;
  if (!el.settings.hidden) return;
  // Focus inside a panel that goes would fall to the page's body.
  const inside = !!document.activeElement && el.settingspanel.contains(document.activeElement);
  closeSettings(false);
  if (inside) el.back.focus();
}

function openSettings() {
  el.settingspanel.hidden = false;
  expanded(el.settings, true);
  const first = [[el.readvoicerow, el.readvoice], [el.autosendrow, el.autosend], [el.autoreadrow, el.autoread]].find(([row]) => !row.hidden);
  if (first) first[1].focus();
}

function closeSettings(refocus) {
  if (el.settingspanel.hidden) return;
  el.settingspanel.hidden = true;
  expanded(el.settings, false);
  if (refocus) el.settings.focus();
}

// Recent activity: the open agent's terminal, read on demand. The text is
// the agent's own and goes into a <pre> as text.
function showActivityNote(text) {
  setText(el.activitynote, text);
  el.activitynote.hidden = !text;
}

function syncActivity() {
  const a = current && roster ? roster.agents.find((x) => x.name === current) : null;
  const on = !view && activityOffered(roster, a);
  el.activity.hidden = !on;
  if (!on) closeActivity();
}

function closeActivity() {
  activityOpen = false;
  activitySeq++;
  el.activitypanel.hidden = true;
  expanded(el.activity, false);
  setText(el.activitytext, '');
  el.activitytext.hidden = true;
  setText(el.activitywhen, '');
  el.activityreload.disabled = false;
  showActivityNote('');
}

async function loadActivity() {
  const name = current;
  if (!activityOpen || !name) return;
  const seq = ++activitySeq;
  el.activityreload.disabled = true;
  if (el.activitytext.hidden) showActivityNote('Reading the terminal…');
  const res = await api(activityPath(name), { method: 'POST' });
  if (seq !== activitySeq || current !== name || !activityOpen) return;
  el.activityreload.disabled = false;
  const got = res.ok ? activityText(res.body) : null;
  if (!got) {
    showActivityNote(`The terminal cannot be read: ${activityErrorText(res.status, res.body, name)}.`);
    return;
  }
  setText(el.activitytext, got.text);
  el.activitytext.hidden = !got.text;
  setText(el.activitywhen, new Date().toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' }));
  showActivityNote(!got.text ? 'The terminal is empty.' : got.truncated ? 'Only the last part is shown.' : '');
  // The newest lines are at the end.
  el.activitytext.scrollTop = el.activitytext.scrollHeight;
}

function closeFiles() {
  filesOpen = false;
  filesSeq++;
  el.filespanel.hidden = true;
  filesShown(false);
  el.filelist.replaceChildren();
  showFilesNote('');
}

// XMLHttpRequest, not fetch: only it reports upload progress.
function sendFile(name, file, progress) {
  return new Promise((resolve) => {
    const xhr = new XMLHttpRequest();
    const done = (status, text) => {
      let body = text;
      try {
        body = JSON.parse(text);
      } catch {
        // not JSON: keep the text
      }
      resolve({ ok: status >= 200 && status < 300, status, body });
    };
    xhr.open('POST', `/lever/api/files/${encodeURIComponent(name)}`);
    xhr.timeout = UPLOAD_MS;
    xhr.setRequestHeader('Accept', 'application/json');
    // The proxy refuses an upload without it: a custom header needs a CORS
    // preflight across origins, so a resend elsewhere cannot carry it.
    xhr.setRequestHeader('X-Lever-Upload', '1');
    xhr.upload.addEventListener('progress', (ev) => {
      if (ev.lengthComputable && ev.total > 0) progress(Math.floor((ev.loaded * 100) / ev.total));
    });
    xhr.addEventListener('load', () => done(xhr.status, xhr.responseText));
    xhr.addEventListener('error', () => done(0, 'cannot reach the server'));
    xhr.addEventListener('timeout', () => done(0, 'no answer in time'));
    const form = new FormData();
    form.append('file', file, file.name);
    xhr.send(form);
  });
}

// The note goes through deliver, so the agent verifies it as this login's
// message.
async function upload(file) {
  const c = chat;
  const cfg = roster && roster.files;
  if (!file || !c || !cfg || uploading || sending) return;
  const why = fileCheck(file, cfg);
  if (why) {
    showError(why);
    return;
  }
  const shown = oneLine(file.name, 120);
  uploading = true;
  syncAttach();
  showError('');
  showUpload(`Uploading ${shown}… 0%`);
  const res = await sendFile(c.name, file, (pct) => showUpload(`Uploading ${shown}… ${pct}%`));
  uploading = false;
  showUpload('');
  syncAttach();
  const here = () => !!chat && chat.name === c.name;
  if (res.status !== 201 || !res.body || typeof res.body !== 'object' || typeof res.body.name !== 'string') {
    if (here()) showError(`Not uploaded: ${uploadErrorText(res.status, res.body)}`);
    return;
  }
  const name = oneLine(res.body.name, 120);
  if (filesOpen && here()) void loadFiles();
  sending = true;
  syncSend();
  syncAttach();
  const out = await deliver(c, uploadNote(name));
  sending = false;
  syncSend();
  syncAttach();
  if (!out.blocked && out.res && out.res.status === 201) {
    setUnsent(c, null);
    if (chat === c && out.res.body && typeof out.res.body === 'object' && mergeMessages(messages, [out.res.body])) render(true);
    refreshSoon();
  } else if (here()) {
    showError(`Uploaded ${name}, but the chat message was not sent: ${out.blocked || reason(out.res)}. Tell ${c.name} yourself.`);
  }
}

async function loadFiles() {
  const c = chat;
  if (!c || !filesOpen) return;
  const seq = ++filesSeq;
  const res = await api(`/lever/api/files/${encodeURIComponent(c.name)}`);
  if (seq !== filesSeq || chat !== c) return;
  const list = res.ok ? fileList(res.body) : null;
  if (!list) {
    showFilesNote(`The files cannot be read: ${errorText(res.status, res.body)}.`);
    return;
  }
  showFilesNote(list.length ? '' : 'No files yet.');
  el.filelist.replaceChildren(...list.map((f) => fileRow(c.name, f)));
}

// The download link is built here, to lever's own route, never taken from
// the answer. sender '' is this login.
function fileRow(agent, f, sender = '') {
  const li = document.createElement('li');
  li.className = `file ${f.direction}`;
  const a = document.createElement('a');
  // Shares off: a share stays listed, but its download is refused.
  const off = f.direction === 'received' && roster && roster.files && !roster.files.shares;
  const href = off ? '' : localLink(downloadPath(agent, f.id));
  if (href) {
    a.setAttribute('href', href);
    // Bare: the server's Content-Disposition names the file.
    a.setAttribute('download', '');
  }
  setText(a, f.name);
  const meta = `${sizeText(f.size)} · ${when({ createdAt: f.at })}${off ? ' · downloads of shared files are off' : ''}`;
  li.append(span('who', f.direction === 'received' ? `From ${agent}` : sender ? `From ${sender}` : 'You sent'), a, span('meta', meta));
  return li;
}

function resetHistory() {
  stopReading();
  speakButtons.clear();
  el.readhint.hidden = true;
  messages.clear();
  fromHistory.clear();
  generation++;
  olderCursor = '';
  loaded = false;
  el.older.hidden = true;
  el.list.replaceChildren();
}

function openChat(name) {
  leaveTranscript();
  cancelDictation();
  const a = roster && roster.agents.find((x) => x.name === name);
  if (!a) {
    closeChat();
    return;
  }
  current = a.name;
  store(OPEN_KEY, a.name);
  document.body.classList.add('chatting');
  document.title = a.name;
  setText(el.agent, a.name);
  setText(el.label, a.label);
  el.label.hidden = !a.label;
  setLink(el.terminal, roster.tier === 'operator' ? a.terminal : '');
  syncNewConversation(a);
  resetHistory();
  closeFiles();
  closeActivity();
  syncActivity();
  showError('');
  renewed = null;
  renderRows();
  if (a.access !== 'message') {
    chat = null;
    syncAttach();
    showState(chipText(a), false);
    showNote('');
    showNotice('');
    el.viewonly.hidden = false;
    el.form.hidden = true;
    return;
  }
  el.viewonly.hidden = true;
  el.form.hidden = false;
  chat = { name: a.name, id: a.id, conversation: a.conversation, userId: roster.userId, unsent: null, view: null };
  if (chat.conversation) loadUnsent(chat);
  loadDraft();
  applyView(a);
  grow();
  if (!chat.conversation) {
    showNotice(a.state === 'unknown' ? 'The conversation cannot be read while the state is unknown.' : `${a.name} has no record on the hub yet.`);
    return;
  }
  showNotice('');
  void refresh();
}

function closeChat() {
  leaveTranscript();
  cancelDictation();
  current = '';
  chat = null;
  store(OPEN_KEY, null);
  document.body.classList.remove('chatting');
  document.title = 'Chat';
  setText(el.agent, 'Chat');
  setText(el.label, '');
  el.label.hidden = true;
  setLink(el.terminal, '');
  syncNewConversation(null);
  showState('', true);
  resetHistory();
  closeFiles();
  syncActivity();
  showUpload('');
  showError('');
  showNote('');
  el.viewonly.hidden = true;
  el.form.hidden = true;
  syncAttach();
  showNotice('Choose an agent from the list.');
  if (roster) renderRows();
  if (contacts) renderContacts();
}

// An event is only a hint to read again. A stream the browser gave up on is
// replaced by the poll, which also reopens it.
function openStream() {
  if (!roster || !roster.userId) return;
  const uid = roster.userId;
  if (stream) stream.close();
  stream = new EventSource(`/events?sub=${encodeURIComponent(`user.${uid}.chat.>`)}`);
  stream.addEventListener('update', (ev) => {
    try {
      if (!isChatSubject(JSON.parse(ev.data).subject, uid)) return;
    } catch {
      return; // not ours to read
    }
    listSoon();
    refreshSoon();
  });
  stream.addEventListener('open', () => {
    if (streamFailed) {
      streamFailed = false;
      listSoon();
      refreshSoon();
    }
  });
  stream.addEventListener('error', () => {
    streamFailed = true;
  });
}

function poll() {
  if (document.visibilityState !== 'visible') return;
  void reloadList();
  refreshSoon();
  if (filesOpen) void loadFiles();
  if (!stream || stream.readyState === EventSource.CLOSED) openStream();
}

function newKey() {
  if (crypto.randomUUID) return crypto.randomUUID();
  const b = crypto.getRandomValues(new Uint8Array(16));
  return [...b].map((x) => x.toString(16).padStart(2, '0')).join('');
}

function post(c, text, key) {
  return api(`/api/v1/chat/conversations/${encodeURIComponent(c.conversation)}/messages`, {
    method: 'POST',
    headers: { Accept: 'application/json', 'Content-Type': 'application/json' },
    body: JSON.stringify({ content: text, idempotency_key: key }),
  });
}

// unclear: the hub may have stored the message (no answer, or a fault in
// between). A 4xx refusal is not unclear.
function unclear(res) {
  return !res.status || res.status >= 500;
}

// A success that is not the stored message came from something in the hub's
// place (a front, a cache), so whether it was stored is open.
function okButNoMessage(res) {
  return res.status >= 200 && res.status < 300 && res.status !== 201;
}

// The body of a success that is not a message is not shown: it is another
// party's page.
function reason(res) {
  return okButNoMessage(res) ? `unexpected answer (HTTP ${res.status})` : errorText(res.status, res.body);
}

// The hub answers a key whose earlier send is still running (a wake can take
// minutes) with 409 send_in_progress. That attempt may yet be stored; this
// one stored nothing.
function inProgress(res) {
  const err = res.body && typeof res.body === 'object' ? res.body.error : null;
  return res.status === 409 && !!err && typeof err === 'object' && err.code === 'send_in_progress';
}

// The hub answers a key it has seen with 200 and the message stored then.
function replayed(res) {
  return res.status === 200 && !!res.body && typeof res.body === 'object' && typeof res.body.id === 'string';
}

// send keeps chat c to the end: what it shows, it shows only while c's agent
// is still open.
async function send() {
  const c = chat;
  const text = el.text.value.trim();
  if (sending || held || !text || !c || !c.conversation) return;
  const length = messageLength(text);
  if (length > MAX_MESSAGE) {
    showError(`The message is ${length} characters; the limit is ${MAX_MESSAGE}.`);
    return;
  }
  sending = true;
  syncSend();
  showError('');
  renewed = null;
  const out = await deliver(c, text);
  sending = false;
  syncSend();
  // The same agent's chat may have moved to a new record meanwhile; its
  // composer still holds this agent's draft. Its messages are another
  // conversation's, so only c's own view gets the stored message.
  const here = !!chat && chat.name === c.name;
  const say = (t) => {
    if (here) showError(t);
  };
  const { res, again, earlier } = out;
  if (out.blocked) {
    say(`Not sent: ${out.blocked}`);
  } else if (res.status === 201) {
    // Stored now. This is the only answer that clears the draft, and only
    // the text that was sent: text typed while the send was under way stays.
    setUnsent(c, null);
    if (here && el.text.value.trim() === text) {
      el.text.value = '';
      saveDraft();
      grow();
    } else if (!here && (stored(draftKey(c.name)) || '').trim() === text) {
      store(draftKey(c.name), '');
    }
    if (chat === c && res.body && typeof res.body === 'object' && mergeMessages(messages, [res.body])) render(true);
    // The hub keeps a key for a few minutes, in memory. A 201 for a key
    // sent before means it did not know the key: the earlier attempt never
    // arrived, or it did and the hub has forgotten. The page cannot tell.
    if (again) say(NOTE_STORED_AGAIN);
    refreshSoon();
  } else if (again && replayed(res)) {
    // The hub had this key already. The record goes, so another press is a new
    // message; Send rests first so a double tap is not that press. After two or
    // more unclear attempts the hub may have stored each.
    setUnsent(c, null);
    say(earlier > 1 ? `${NOTE_REPLAY} ${NOTE_TWICE}` : NOTE_REPLAY);
    hold();
    refreshSoon();
  } else if (unclear(res) || okButNoMessage(res) || inProgress(res)) {
    // The running attempt is the one already counted, not this one.
    if (inProgress(res)) setUnsent(c, { ...c.unsent, tries: Math.max(earlier, 1) });
    say(`No clear answer (${reason(res)}). The message may have arrived: look at the conversation first. A repeat within a few minutes is stored only once.`);
    refreshSoon();
    listSoon();
  } else if (again) {
    // The hub refused this attempt, which says nothing about the earlier
    // one. The key stays, so a later press still finds it; the refused
    // attempt did not arrive, so it is not counted.
    setUnsent(c, { ...c.unsent, tries: earlier });
    say(`This attempt was refused (${reason(res)}). An earlier attempt had no clear answer and may have arrived: look at the conversation.`);
    listSoon();
  } else {
    // A first attempt the hub refused: nothing arrived, and the key is
    // done with.
    setUnsent(c, null);
    say(`Not sent: ${reason(res)}`);
    listSoon();
  }
  if (here) el.text.focus();
}

function hold() {
  held = true;
  syncSend();
  setTimeout(() => {
    held = false;
    syncSend();
  }, HOLD_MS);
}

// deliver returns {res, again, earlier}, or {blocked} when nothing was posted.
async function deliver(c, text) {
  // Ask lever first: a fresh start gives the agent a new hub record, and a
  // message to the old one would reach nobody.
  const l = await readList();
  if (l.error) return { blocked: l.error };
  applyRoster(l);
  const a = l.agents.find((x) => x.name === c.name);
  if (!a || a.access !== 'message') return { blocked: `${c.name} is no longer in your list.` };
  if (a.conversation !== c.conversation) {
    return { blocked: `${c.name} has a new hub record, and the chat now shows it. Press Send again to send there.` };
  }
  const view = inputView(a, !!managerRow(), roster && roster.tier);
  if (!view.input) return { blocked: view.note || `${c.name} cannot take messages now (${chipText(a)}).` };
  if (view.wake) {
    const woken = await wake(c, text);
    if (woken) return woken;
  }
  const reuse = !!c.unsent && c.unsent.text === text && c.unsent.conversation === c.conversation;
  const key = reuse ? c.unsent.key : newKey();
  const earlier = reuse ? c.unsent.tries : 0;
  const again = earlier > 0;
  // The record is written BEFORE the post: a reload while the post is under
  // way restores the draft, and must restore its key with it.
  setUnsent(c, { text, key, conversation: c.conversation, tries: earlier + 1 });
  let res = await post(c, text, key);
  if (res.status === 401 || res.redirect) {
    // The hub forgot the session. A read makes the proxy sign in again;
    // then the message goes once more under the same key.
    await api(historyPath(c, ''));
    res = await post(c, text, key);
  }
  return { res, again, earlier };
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// wake returns null once the agent runs, or {blocked}. The unsent record is
// written first with no post counted, so a reload keeps the text's key.
async function wake(c, text) {
  const name = c.name;
  if (!(c.unsent && c.unsent.text === text && c.unsent.conversation === c.conversation)) {
    setUnsent(c, { text, key: newKey(), conversation: c.conversation, tries: 0 });
  }
  waking = name;
  showWakeView();
  try {
    const res = await api(`/lever/api/agents/${encodeURIComponent(name)}/wake`, { method: 'POST' });
    const word = res.body && typeof res.body === 'object' ? res.body.error : '';
    // Someone else woke it a moment ago: wait for it the same way.
    if (res.status !== 202 && !(res.status === 409 && word === 'not-asleep')) {
      return { blocked: wakeText(res.status, res.body, name) };
    }
    for (let i = 0; i < WAKE_POLLS; i++) {
      await sleep(WAKE_POLL_MS);
      const l = await readList();
      if (l.error) continue;
      applyRoster(l);
      const a = l.agents.find((x) => x.name === name);
      if (!a || a.access !== 'message') return { blocked: `${name} is no longer in your list.` };
      if (a.state === 'unknown') continue;
      if (a.conversation !== c.conversation) {
        return { blocked: `${name} has a new hub record, and the chat now shows it. Press Send again to send there.` };
      }
      if (a.state === 'running') return null;
      if (!['suspended', 'stopped', 'starting'].includes(a.state)) return { blocked: `${name} did not wake (${chipText(a)}).` };
    }
    return { blocked: `${name} did not wake in time. Your message is kept; send it again later.` };
  } finally {
    waking = '';
    showWakeView();
  }
}

function syncNewConversation(a) {
  el.newconv.hidden = !newConversationOffered(roster, a);
  el.newconv.disabled = !!renewing;
}

// The manager's new conversation: asked twice when it is mid-turn, then
// followed by its own status until lever says how it ended. The draft stays
// in the box throughout.
async function newConversation() {
  const c = chat;
  if (!c || renewing) return;
  const name = c.name;
  if (!window.confirm(newConversationAsk(name))) return;
  showError('');
  renewed = null;
  renewing = name;
  showWakeView();
  try {
    let res = await api(NEW_CONVERSATION_PATH, { method: 'POST' });
    const word = (r) => (r.body && typeof r.body === 'object' ? r.body.error : '');
    if (res.status === 409 && word(res) === 'busy') {
      if (!window.confirm(newConversationForceAsk(name))) return;
      res = await api(NEW_CONVERSATION_PATH, { method: 'POST', body: JSON.stringify({ force: true }) });
    }
    if (res.status !== 202) {
      showError(newConversationText(res.status, res.body, name));
      return;
    }
    const id = res.body && typeof res.body === 'object' && typeof res.body.id === 'string' ? res.body.id : '';
    let out = null;
    for (let i = 0; i < NEW_CONVERSATION_POLLS && !out; i++) {
      await sleep(WAKE_POLL_MS);
      const s = await api(NEW_CONVERSATION_STATUS_PATH, { method: 'POST', body: JSON.stringify({ id }) });
      out = newConversationOutcome(s.status, s.body, name);
    }
    if (!out) out = { note: newConversationLostText(name) };
    if (out.error) {
      if (chat && chat.name === name) showError(out.error);
    } else {
      renewed = { name, text: out.note };
    }
  } finally {
    renewing = '';
    showWakeView();
    listSoon();
  }
}

function showWakeView() {
  const a = chat && roster && roster.agents.find((x) => x.name === chat.name);
  if (a) applyView(a);
}

function grow() {
  el.text.style.height = 'auto';
  el.text.style.height = `${el.text.scrollHeight + 2}px`;
}

function saveDraft() {
  if (chat) store(draftKey(chat.name), el.text.value);
}

function loadDraft() {
  el.text.value = stored(draftKey(chat.name)) || '';
}

// Nothing is sent: the draft is the person's to edit.
function askManager() {
  const mgr = managerRow();
  if (!chat || !mgr) return;
  const draft = askDraft(chat.name);
  openChat(mgr.name);
  if (!chat) return;
  const now = el.text.value.trimEnd();
  if (!now.includes(draft)) el.text.value = now ? `${now}\n${draft}` : draft;
  saveDraft();
  grow();
  el.text.focus();
}

// Notifications (remote.push). The worker is registered only when the login
// turns them on (iOS and Chrome ask permission only on a click). It has no
// fetch handler, so it never stands between the page and its requests.
const PUSH_SCOPE = '/lever/';

// serviceWorker.register is a Trusted Types script-URL sink. The CSP allows
// one policy, lever-sw, whose only output is the literal worker URL;
// anything else throws. A browser without Trusted Types takes the string.
let swPolicy = null;
function swScriptURL() {
  const tt = window.trustedTypes;
  if (!tt || typeof tt.createPolicy !== 'function') return '/lever/sw.js';
  if (!swPolicy) {
    swPolicy = tt.createPolicy('lever-sw', { createScriptURL: (u) => { if (u !== '/lever/sw.js') throw new TypeError('lever-sw: refused'); return u; } });
  }
  return swPolicy.createScriptURL('/lever/sw.js');
}
let push = { available: false, permission: 'default', subscribed: false, busy: false, error: '', key: null };
const pushSupported = () => !!(navigator && navigator.serviceWorker) && typeof window.PushManager !== 'undefined' && typeof Notification !== 'undefined';

function showPush() {
  const v = pushView(push);
  el.push.hidden = v.hidden;
  el.push.disabled = v.disabled;
  setText(el.push, v.text);
  setText(el.pushnote, v.note);
  el.pushnote.hidden = !v.note;
}

function sendSubscription(sub, method) {
  const j = sub.toJSON();
  const body = method === 'DELETE' ? { endpoint: j.endpoint } : { endpoint: j.endpoint, keys: j.keys };
  return api('/lever/api/push/subscriptions', { method, headers: { Accept: 'application/json', 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
}

async function pushRegistration() {
  try {
    return await navigator.serviceWorker.getRegistration(PUSH_SCOPE);
  } catch {
    return undefined;
  }
}

// One push subscription per worker scope whoever is signed in, so on a
// shared device only the login that opted in here re-sends it at load.
const optInKey = () => `lever-push-optin:${roster ? roster.login : ''}`;

function optedIn() {
  try {
    return localStorage.getItem(optInKey()) === '1';
  } catch {
    return false;
  }
}

function setOptIn(on) {
  try {
    if (on) localStorage.setItem(optInKey(), '1');
    else localStorage.removeItem(optInKey());
  } catch {
    // storage is off: the subscription is then not re-sent at load
  }
}

// A notification tap relayed by lever's own worker counts as acting (hasActed).
function onWorkerMessage(ev) {
  const d = ev && ev.data;
  const name = d && typeof d.agent === 'string' ? hashAgent(`#agent=${d.agent}`) : '';
  if (name && roster && roster.agents.some((a) => a.name === name)) openChat(name);
  onAct();
}

async function setupPush() {
  if (!pushSupported()) return;
  navigator.serviceWorker.addEventListener('message', onWorkerMessage);
  const res = await api('/lever/api/push/key');
  const key = res.ok && res.body ? pushKeyBytes(res.body.key) : null;
  const reg = await pushRegistration();
  if (!key) {
    // Push is off on the server: a worker left from when it was on goes.
    if (reg && res.status === 404) await reg.unregister().catch(() => {});
    return;
  }
  push = { ...push, available: true, key, permission: Notification.permission };
  const sub = reg ? await reg.pushManager.getSubscription().catch(() => null) : null;
  if (sub && optedIn()) {
    push.subscribed = true;
    // lever may have dropped it (the push service said gone): send it again.
    void sendSubscription(sub, 'POST');
  }
  showPush();
}

async function turnOn() {
  let perm = 'denied';
  try {
    perm = await Notification.requestPermission();
  } catch {
    // treated as refused
  }
  push.permission = perm;
  if (perm !== 'granted') return;
  let sub;
  try {
    await navigator.serviceWorker.register(swScriptURL(), { scope: '/lever/' });
    const reg = await navigator.serviceWorker.ready;
    sub = await reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: push.key });
  } catch {
    push.error = 'Notifications could not be turned on in this browser.';
    return;
  }
  const res = await sendSubscription(sub, 'POST');
  if (!res.ok) {
    await sub.unsubscribe().catch(() => {});
    push.error = `Notifications could not be turned on: ${errorText(res.status, res.body)}.`;
    return;
  }
  push.subscribed = true;
  setOptIn(true);
}

async function turnOff() {
  const reg = await pushRegistration();
  const sub = reg ? await reg.pushManager.getSubscription().catch(() => null) : null;
  if (sub) {
    await sendSubscription(sub, 'DELETE');
    await sub.unsubscribe().catch(() => {});
  }
  if (reg) await reg.unregister().catch(() => {});
  push.subscribed = false;
  setOptIn(false);
}

async function togglePush() {
  if (push.busy || !push.available) return;
  push.busy = true;
  push.error = '';
  showPush();
  try {
    await (push.subscribed ? turnOff() : turnOn());
  } finally {
    push.busy = false;
    showPush();
  }
}

async function start() {
  const l = await readList();
  if (l.error) {
    // A refusal stays one: no retry loop against the fence.
    if (l.status === 403) {
      showListNote(`This login may not use the chat page: ${l.error}.`);
      return;
    }
    // The hub may be starting: keep trying rather than leave a dead page.
    showListNote(`The agent list cannot be read yet: ${l.error}. Trying again.`);
    setTimeout(() => void start(), RETRY_MS);
    return;
  }
  showListNote('');
  adoptOldRecords(l);
  applyRoster(l);
  if (roster.tier === 'operator') {
    void reloadContacts();
    setInterval(pollView, CONTACTS_MS);
  }
  setInterval(poll, LIST_MS);
  document.addEventListener('visibilitychange', poll);
  // The stream first, so a message stored while a history is read still
  // raises an event.
  openStream();
  const fromHash = hashAgent(location.hash);
  if (fromHash) history.replaceState(null, '', '/lever/chat');
  const want = fromHash || stored(OPEN_KEY);
  if (want && roster.agents.some((a) => a.name === want)) openChat(want);
  else closeChat();
  void setupPush();
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
  // A held key repeats: one press, one send (and no new lines from it).
  if (!ev.repeat) void send();
});
el.older.addEventListener('click', () => void (view ? readTranscript(viewOlder) : loadOlder()));
el.refresh.addEventListener('click', () => {
  void readTranscript('');
  void loadViewFiles();
});
el.back.addEventListener('click', closeChat);
el.newconv.addEventListener('click', () => void newConversation());
el.ask.addEventListener('click', askManager);
el.push.addEventListener('click', () => void togglePush());
el.attach.addEventListener('click', () => {
  el.file.value = '';
  el.file.click();
});
el.file.addEventListener('change', () => void upload(el.file.files && el.file.files[0]));
el.mic.addEventListener('click', () => void toggleMic());
el.readvoice.addEventListener('change', () => saveReadVoice(el.readvoice.value));
el.settings.addEventListener('click', () => {
  if (el.settingspanel.hidden) openSettings();
  else closeSettings(true);
});
document.addEventListener('keydown', (ev) => {
  if (ev.key !== 'Escape' || el.settingspanel.hidden) return;
  ev.preventDefault();
  closeSettings(true);
});
// A tap outside the panel closes it, as a menu does.
document.addEventListener('pointerdown', (ev) => {
  if (el.settingspanel.hidden || !ev.target || el.settingspanel.contains(ev.target) || el.settings.contains(ev.target)) return;
  closeSettings(false);
});
el.autosend.addEventListener('change', () => {
  setOption(autoSendKey(), el.autosend.checked);
  syncMic();
});
el.autoread.addEventListener('change', () => {
  setOption(autoReadKey(), el.autoread.checked);
  if (el.autoread.checked) {
    unlockAudio();
    return;
  }
  stopReading();
  el.readhint.hidden = true;
});
if (speechOn() && typeof window.speechSynthesis.addEventListener === 'function') {
  // Voices load late in some browsers.
  window.speechSynthesis.addEventListener('voiceschanged', syncReadVoice);
}
el.activity.addEventListener('click', () => {
  if (activityOpen) {
    closeActivity();
    return;
  }
  activityOpen = true;
  el.activitypanel.hidden = false;
  expanded(el.activity, true);
  void loadActivity();
});
el.activityreload.addEventListener('click', () => void loadActivity());
el.files.addEventListener('click', () => {
  if (filesOpen) {
    closeFiles();
    return;
  }
  filesOpen = true;
  el.filespanel.hidden = false;
  filesShown(true);
  void loadFiles();
});
window.addEventListener('resize', grow);
document.addEventListener('visibilitychange', () => {
  if (document.visibilityState !== 'hidden') return;
  dropHiddenDictation();
  replyQueue.length = 0;
  // A server voice stops with the page hidden, as dictation does: its
  // requests would go on in the background.
  if (speaking && speaking.aborts) stopSpeaking();
});
// pagehide also stops reading aloud.
window.addEventListener('pagehide', () => {
  dropHiddenDictation();
  stopReading();
});

void start();
