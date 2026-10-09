// lever's chat page: the login's agents, and a chat with each one it may
// message.
//
// The list comes from lever (/lever/api/agents), built per login on the
// server: the page shows what it is given and nothing else. Each chat talks
// to the hub's own chat routes through the remote proxy, so a message sent
// here is recorded and verified exactly like one sent from the hub's web UI.
// The page holds no credential: the proxy attaches the login's hub session
// to every request.
//
// SECURITY: everything read from the network is agent- or hub-chosen text on
// the login's origin. It is only ever written as TEXT (textContent). No
// markup sink, no code evaluation, and no URL is built from message text.
// A test in the proxy package fails the build if one appears in this file.

import {
  CONTACTS_MS,
  UPLOAD_MS,
  VOICE_MIN_SAMPLES,
  VOICE_RATE,
  clockText,
  encodeWAV,
  insertText,
  localVoices,
  pickVoice,
  resampledLength,
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
// No request may wait for ever: a phone that changes network mid-request can
// leave one that never settles, and the page reads and sends one at a time.
// Longer than the proxy's own wait for the hub (45 s), so the proxy's answer
// comes first when there is one.
const REQUEST_MS = 60000;
const HOLD_MS = 2000; // Send stays off this long after the replay note
const SYSTEM_MAX = 500; // characters of a hub line, on one line
const OPEN_KEY = 'lever-chat-open'; // the name of the open chat
// Drafts and unsent records are kept per agent name (draftKey, unsentKey).
// The bare keys are what the one-agent page kept; start adopts them once.
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
};

let roster = null; // the list as last applied (agentList shape)
let listSeq = 0; // counts list requests
let appliedSeq = 0; // the request whose answer the page shows
let listing = false; // a list read is under way
let listAgain = false; // something asked for one while it was
let current = ''; // the name of the open chat ('' = none)
// The open chat with an agent the login may message: {name, id,
// conversation, userId, unsent, view}. null for none, and for a see-only
// agent. A request keeps the chat it was made for and answers only it.
let chat = null;
const lastMarked = new Map(); // conversation → message id marked read

const messages = new Map();
const fromHistory = new Set(); // ids a history read returned (not a send answer)
let generation = 0; // counts restarts of the list
// chat.unsent: the send that is under way or got no clear "stored": {text,
// key, conversation, tries}, written before the post and kept in
// sessionStorage per agent. key is its idempotency key. While the same text
// goes again to the same conversation it goes under that key, so the hub
// stores it once if it still knows the key (it keeps one for a few minutes,
// per user, not per conversation, hence the conversation here). One record
// per agent: an unclear send of one text, then another text, then the first
// again gets a new key. tries counts the posts made under the key.
//
// The page concludes nothing from this record. It never decides from the
// history that a send "must have arrived": only the hub's answer to the
// same key says so. A wrong guess there would clear a draft that was never
// stored.
let loaded = false; // a history read has succeeded
let reading = false; // a history read is under way
let readAgain = false; // something changed while it was
let olderCursor = '';
let sending = false;
let uploading = false; // a file upload is under way
let filesOpen = false; // the Files panel is open
let filesSeq = 0; // counts Files panel reads and closes
let waking = ''; // the agent a send is waking ('' = none)
let held = false; // Send rests after a note (see hold)
let stream = null;
let streamFailed = false;

// The operator's read-only view of contact conversations. contacts is the
// list from /lever/api/contacts (null for a contact login: it never asks).
// view is the open transcript ({login, name}); a transcript has its own
// rows and never touches chat, the composer or the read marker.
let contacts = null;
const openContacts = new Set(); // contact logins whose agents show
let view = null;
let viewSeq = 0; // counts transcript opens and closes
let viewReading = false;
let viewOlder = '';
const transcript = new Map();

// api does one same-origin request and reads the answer as JSON when it is
// JSON, else as text. It never throws: a network fault is status 0.
//
// A redirect is never followed. The hub answers a session it no longer knows
// with a redirect to its login page; followed, that ends in a page and a
// 200, which would read as success. It comes back as {redirect: true}.
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

// localLink turns a path from the list into a link target on this origin,
// or '' when it would lead anywhere else. The browser's own URL parser
// decides, since it is what reads the link: it drops tabs and newlines, and
// removes dot segments, which a check on the string would miss. The result
// is the parsed URL itself, whole, so nothing is parsed a second time into
// something else ("/.//evil.test" has the path "//evil.test", which read
// again as a link is another host).
function localLink(p) {
  if (typeof p !== 'string' || !p.startsWith('/')) return '';
  try {
    const u = new URL(p, location.origin);
    return u.origin === location.origin && !u.pathname.startsWith('//') ? u.href : '';
  } catch {
    return '';
  }
}

// setLink shows a with path as its target, or hides it when path is not a
// link on this origin. The page asks only for the operator's links; a
// contact's list carries none, and the page builds none for a contact.
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

// renderRows draws the list: per agent a button with its title (name, then
// label), its state chip and its unread badge, all text.
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

// renderContacts draws the Contacts section: per contact a button with its
// login (and a note when it never signed in), and, opened, its agents.
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

// renderTranscript draws the open transcript, all text. A row the contact
// is not shown carries a mark: "not yet read" when a record would show it
// on the contact's next read, else "not shown".
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

// readTranscript reads the newest page (cursor '') or the page before
// cursor, one read at a time; an answer for a transcript no longer open
// is dropped.
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

// leaveTranscript closes the open transcript, if any.
function leaveTranscript() {
  view = null;
  viewSeq++;
  transcript.clear();
  viewOlder = '';
  el.readonly.hidden = true;
  el.refresh.hidden = true;
}

// openTranscript shows a contact's conversation with one of its agents,
// read-only: no composer, no read marker, no events.
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

// pollView refreshes the contact list and the open transcript.
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

// loadViewFiles lists the open transcript's files (the contact's uploads to
// the agent and the agent's shares to it), read-only, while files are on.
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

// refresh reads the open chat's newest page again and merges it. Every live
// signal ends here: the page never trusts an event's payload, only the
// hub's history.
//
// One read at a time: a signal that arrives during a read asks for one more
// after it. So answers apply in the order they were asked for, and a slow
// hub under a steady stream of events still gets every read applied. An
// answer for a chat that is no longer open is dropped.
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

// acted is set by the user's first tap or key on this page load (onAct).
let acted = false;

// hasActed reports whether the user has acted on this page load. Until then
// the page marks nothing read: a link from another site can open the page,
// even in a window it closes again, and choose the chat with #agent=, so
// opening and showing a chat is no sign that anyone read it. A tap on one of
// lever's notifications counts too, when the service worker sends it to this
// page (onWorkerMessage). The browser's own sticky activation is used where
// it exists.
function hasActed() {
  return acted || Boolean(navigator.userActivation && navigator.userActivation.hasBeenActive);
}

// onAct records the first act and marks the open chat read then, as a read
// that was held back for it.
function onAct() {
  if (acted) return;
  acted = true;
  if (chat && chat.conversation) void markRead(chat);
}
for (const type of ['pointerdown', 'keydown', 'touchstart']) document.addEventListener(type, onAct, { capture: true, passive: true });

// markRead moves the login's read marker to the newest message a history
// read showed, while the chat is in view and once the user has acted on the
// page (hasActed), so the agent's badge drops. It uses the hub's own read
// route; the list is read again after it.
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

// What the page says when the hub answers a repeated key: the earlier
// attempt was stored, and nothing was stored now. The draft stays, since the
// page cannot tell a retry from the same words meant a second time. The
// answer to that first attempt may have been lost before the proxy, which
// then recorded nothing, so the agent may be unable to verify the message.
// And when the hub stores, under a key sent before, a message it should have
// known if the earlier attempt arrived within its memory: both may be there.
const NOTE_STORED_AGAIN = 'Stored now. An earlier attempt had no clear answer: if the message shows twice above, both arrived.';
const NOTE_TWICE = 'If it shows twice above, two attempts arrived.';
const NOTE_REPLAY = 'The earlier attempt did arrive: the message is in the conversation, and nothing new was stored. ' +
  'Press Send again only to post the same words a second time. ' +
  'If the agent treats the message as unverified, send it in other words.';

function setUnsent(c, v) {
  c.unsent = v;
  store(unsentKey(c.name), v ? JSON.stringify(v) : null);
}

// loadUnsent restores the record of a send that had no clear answer, so that
// after a reload the same draft still goes under the same key. A record for
// another conversation (the agent has a new hub record) is dropped: its key
// would get the hub's answer for the message in the old one.
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

// adoptOldRecords moves what the one-agent page kept (one draft, one unsent
// record, both for the manager) to the per-agent keys, once: an unclear send
// keeps its key across the upgrade.
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

// readList asks lever which agents this login has, in which state. The
// answer carries seq, the order it was asked in.
async function readList() {
  const seq = ++listSeq;
  const res = await api('/lever/api/agents');
  const l = res.ok ? agentList(res.body) : null;
  if (!l) return { error: errorText(res.status, res.body), status: res.status, seq };
  l.seq = seq;
  return l;
}

const managerRow = () => roster && roster.agents.find((a) => a.role === 'manager' && a.access === 'message');

// applyRoster shows a list answer: the rows, the operator's console link,
// and the open chat's state. An answer older than the one shown is
// dropped. The open agent with a new hub record (a fresh start gives it a
// new id) moves its chat to the new conversation: a message to the old one
// would be stored and reach nobody.
function applyRoster(l) {
  if (l.seq < appliedSeq) return;
  appliedSeq = l.seq;
  roster = l;
  renderRows();
  syncAttach();
  setLink(el.console, l.tier === 'operator' ? l.console : '');
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

// degrade keeps the rows of the last list when a new one cannot be read,
// with every state unknown: inputs go off until a read succeeds.
function degrade(failed) {
  if (!roster) return;
  showListNote(`The agent list cannot be read: ${failed.error}. Trying again.`);
  applyRoster({ ...roster, seq: failed.seq, agents: roster.agents.map((a) => ({ ...a, state: 'unknown', ...(a.access === 'message' ? { activity: '' } : {}) })) });
}

// reloadList reads the list again. One read at a time, like refresh.
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

// applyView sets the open chat's header state, composer, note and "Ask the
// manager" button from the agent's row (inputView).
function applyView(a) {
  if (!chat || a.name !== chat.name) return;
  const v = inputView(a, !!managerRow(), roster && roster.tier);
  chat.view = v;
  showState(chipText(a), v.input);
  el.text.disabled = !v.input;
  syncSend();
  showNote(waking === a.name ? `waking ${a.name}…` : v.note);
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

// syncAttach shows the paperclip and the Files button for an open chat
// with an agent the login may message, while files are on.
function syncAttach() {
  const on = !!(roster && roster.files && chat);
  el.attach.hidden = !on || !roster.files.uploads;
  el.files.hidden = !on;
  el.attach.disabled = uploading || sending || !chat || !chat.view || !chat.view.input;
  syncMic();
  syncReadVoice();
}

// Dictation (remote.voice). The mic records a clip in the browser; on stop
// the page decodes it, resamples it to 16 kHz mono (OfflineAudioContext),
// encodes WAV and posts it to lever, which answers its text. The text goes
// into the message box at the cursor, for the login to review: the page
// never sends it by itself.
let recording = null; // {recorder, stream, chunks, started, max, agent, timer, discard, done}
let transcribing = false;

const micSupported = () => !!(navigator.mediaDevices && typeof navigator.mediaDevices.getUserMedia === 'function') &&
  typeof window.MediaRecorder === 'function' && typeof window.OfflineAudioContext === 'function';

function showVoice(text) {
  setText(el.voice, text);
  el.voice.hidden = !text;
}

// syncMic shows the mic for an open chat while the list carries voice for
// this login and the browser can record, and always while it records (it is
// the stop control). It is off while the message box is, and while a clip
// is being transcribed.
function syncMic() {
  el.mic.hidden = !recording && !(roster && roster.voice && chat && micSupported());
  el.mic.disabled = transcribing || (!recording && (!chat || !chat.view || !chat.view.input));
  el.mic.className = recording ? 'recording' : '';
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
  showError('');
  let stream;
  // One permission prompt at a time: a second tap while it is up does
  // nothing.
  micStarting = true;
  try {
    stream = await navigator.mediaDevices.getUserMedia({ audio: { channelCount: 1, echoCancellation: true, noiseSuppression: true } });
  } catch {
    showError('The microphone is not available: allow it for this page in the browser\'s settings.');
    return;
  } finally {
    micStarting = false;
  }
  if (chat !== c || recording) {
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
  const r = { recorder, stream, chunks: [], started: Date.now(), max: cfg.maxSeconds, agent: c.name, timer: 0, discard: false, done: false };
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

// tickDictation shows the time recorded, and stops at the clip limit.
function tickDictation(r) {
  if (recording !== r) return;
  const s = (Date.now() - r.started) / 1000;
  showVoice(`Recording ${clockText(s)} of ${clockText(r.max)}. Tap ■ to stop.`);
  if (s >= r.max) stopDictation();
}

// stopDictation ends the recording; its clip is transcribed (finishDictation
// runs on the recorder's stop event, after the last chunk, and releases the
// microphone).
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

// cancelDictation ends a recording without transcribing it (the chat it was
// for closed).
function cancelDictation() {
  if (recording) recording.discard = true;
  stopDictation();
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
    placeTranscript(r.agent, text);
  } catch {
    showError('The recording could not be read in this browser.');
  } finally {
    transcribing = false;
    showVoice('');
    syncMic();
  }
}

// toWAV decodes a recorded clip and resamples it to 16 kHz mono WAV, at most
// max seconds; null for a clip too short to hold a word.
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

// placeTranscript inserts text at the cursor of agent's message box, or,
// when another chat is open by now, at the end of agent's draft. Nothing is
// sent.
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

// Read-aloud: the browser's speech synthesis, with on-device voices only
// (localService true), so the text never goes to a speech service. The
// voice chosen is kept per device.
const READ_VOICE_KEY = 'lever-read-voice';
let speaking = null; // {id, button}: the message being read

const speechOn = () => !!(window.speechSynthesis && typeof window.SpeechSynthesisUtterance === 'function');
// readAloudOn: the device can speak, and lever offers read-aloud to this
// login (remote.voice on and read_aloud not off; the roster says so).
const readAloudOn = () => speechOn() && !!(roster && roster.readAloud);

function savedReadVoice() {
  try {
    return localStorage.getItem(READ_VOICE_KEY) || '';
  } catch {
    return ''; // storage is off
  }
}

function saveReadVoice(uri) {
  try {
    localStorage.setItem(READ_VOICE_KEY, uri);
  } catch {
    // storage is off: the choice lasts for this page load
  }
}

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
  b.addEventListener('click', () => speak(m, b));
  return b;
}

function stopSpeaking() {
  const s = speaking;
  speaking = null;
  if (s && s.button) setText(s.button, '🔊');
  if (s && speechOn()) window.speechSynthesis.cancel();
}

// speak reads m aloud; a tap on the message being read stops it, a tap on
// another stops it and reads that one.
function speak(m, button) {
  const again = !!speaking && speaking.id === m.id;
  stopSpeaking();
  if (again || !speechOn()) return;
  const v = pickVoice(window.speechSynthesis.getVoices(), savedReadVoice(), speechLang());
  if (!v) {
    showError('This device has no on-device voice to read with.');
    return;
  }
  const text = speechText(messageText(m));
  if (!text) return;
  const u = new window.SpeechSynthesisUtterance(text);
  u.voice = v;
  u.lang = v.lang;
  const s = { id: m.id, button };
  const done = () => {
    if (speaking !== s) return;
    speaking = null;
    setText(s.button, '🔊');
  };
  u.addEventListener('end', done);
  u.addEventListener('error', done);
  speaking = s;
  setText(button, '⏹');
  window.speechSynthesis.speak(u);
}

// syncReadVoice offers the voice choice in an open chat when the device has
// more than one on-device voice. The options are built again only when the
// voices change.
let readVoices = '';
function syncReadVoice() {
  const voices = readAloudOn() ? localVoices(window.speechSynthesis.getVoices()) : [];
  el.readvoice.hidden = !chat || voices.length < 2;
  const key = voices.map((v) => v.voiceURI).join('\n');
  if (el.readvoice.hidden || key === readVoices) return;
  readVoices = key;
  const chosen = pickVoice(voices, savedReadVoice(), speechLang());
  el.readvoice.replaceChildren(...voices.map((v) => {
    const o = document.createElement('option');
    o.value = v.voiceURI;
    setText(o, `${oneLine(v.name, 60)} (${oneLine(v.lang, 20)})`);
    return o;
  }));
  el.readvoice.value = chosen ? chosen.voiceURI : '';
}

function closeFiles() {
  filesOpen = false;
  filesSeq++;
  el.filespanel.hidden = true;
  setText(el.files, 'Files');
  el.filelist.replaceChildren();
  showFilesNote('');
}

// sendFile posts one file to the agent's upload route. XMLHttpRequest, not
// fetch: only it reports upload progress. It never throws.
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

// upload sends the picked file to the open chat's agent, then tells the
// agent in the chat with the normal send path (deliver: the list check, a
// wake, the idempotency key), so the agent verifies the note as this
// login's message.
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

// fileRow is one file as text, with a download link only to lever's own
// route for that agent and id (built here, never taken from the answer).
// sender names who uploaded a "sent" row ('' = this login).
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
  stopSpeaking();
  messages.clear();
  fromHistory.clear();
  generation++;
  olderCursor = '';
  loaded = false;
  el.older.hidden = true;
  el.list.replaceChildren();
}

// openChat shows the chat with name. A see-only agent shows its name,
// label and state, and nothing is read for it.
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
  resetHistory();
  closeFiles();
  showError('');
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

// closeChat goes back to the list.
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
  showState('', true);
  resetHistory();
  closeFiles();
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

// openStream listens for the hub's chat events for this login: one stream
// for the page's life. An event is only a hint to read the list and the
// open history again. A stream the browser gave up on is replaced by the
// poll, which also reopens it.
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

// unclear reports whether an answer leaves open that the hub stored the
// message: no answer at all, or a fault somewhere between the page and the
// hub. A refusal by the hub (4xx) is not unclear.
function unclear(res) {
  return !res.status || res.status >= 500;
}

// okButNoMessage reports whether an answer claims success without being the
// stored message or a replay of it. Something answered in the hub's place
// (a front, a cache), so whether the hub stored the message is open.
function okButNoMessage(res) {
  return res.status >= 200 && res.status < 300 && res.status !== 201;
}

// reason is the text for an answer that is not a stored message. The body
// of a success that is none is not shown: it is some other party's page.
function reason(res) {
  return okButNoMessage(res) ? `unexpected answer (HTTP ${res.status})` : errorText(res.status, res.body);
}

// replayed reports whether the hub's answer is the one it gives a key it has
// seen: 200, naming the message it stored then.
function replayed(res) {
  return res.status === 200 && !!res.body && typeof res.body === 'object' && typeof res.body.id === 'string';
}

// send posts the composer's text to the open chat. It keeps that chat (c)
// to the end: the records it writes are c's, and what it shows, it shows
// only while c's agent is still the open chat.
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
    // The hub had this key already (see NOTE_REPLAY). The record goes, so
    // one more press is a new message; Send rests a moment first, so a
    // double tap is not that press.
    // After two or more attempts with no clear answer, the hub may have
    // forgotten the key between them and stored the message each time.
    setUnsent(c, null);
    say(earlier > 1 ? `${NOTE_REPLAY} ${NOTE_TWICE}` : NOTE_REPLAY);
    hold();
    refreshSoon();
  } else if (unclear(res) || okButNoMessage(res)) {
    say(`No clear answer (${reason(res)}). The message may have arrived: look at the conversation first. A repeat within a few minutes is stored only once.`);
    refreshSoon();
    listSoon();
  } else if (again) {
    // The hub refused this attempt, which says nothing about the earlier
    // one. The key stays, so a later press still finds it.
    // A refused attempt did not arrive, so it does not count as one that may have.
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

// hold keeps Send off for a moment after a note the operator must read
// before pressing again.
function hold() {
  held = true;
  syncSend();
  setTimeout(() => {
    held = false;
    syncSend();
  }, HOLD_MS);
}

// deliver posts text to chat c. It returns {res, again, earlier}: the hub's
// answer, whether this text had been posted before under the same key with
// no clear answer, and how often. {blocked} means nothing was posted.
async function deliver(c, text) {
  // Ask lever first: the agent may have a new hub record since the chat
  // opened (a fresh start), and the hub would store a message for the old
  // one and deliver it to nobody; or it may not take messages now. The
  // draft is kept either way.
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
  // The same text sent again after an unclear answer keeps its key, so the
  // hub stores it once if it still knows the key.
  const reuse = !!c.unsent && c.unsent.text === text && c.unsent.conversation === c.conversation;
  const key = reuse ? c.unsent.key : newKey();
  // How many times this text went out before under this key.
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

// wake asks lever to wake c's asleep agent, then reads the list until it
// runs. It returns null when it runs (deliver then posts) or {blocked} with
// the page's own words; the text is kept either way.
//
// The unsent record is written first, with no post counted: a reload during
// the wake restores the text with its key, and the later post goes under
// that key. An earlier unclear send of the same text keeps its own.
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

// showWakeView applies the open chat's row again, so its note says
// whether a wake is under way.
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

// askManager opens the manager's chat with a draft asking it to start the
// open agent. Nothing is sent: the draft is the person's to edit and send.
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

// Notifications (remote.push). The page asks lever for its key only where
// the browser can push, and registers the push-only worker only when the
// login turns notifications on (a click: iOS and Chrome ask for permission
// only then). The worker has no fetch handler: it never stands between the
// page and its requests.
const PUSH_SCOPE = '/lever/';

// serviceWorker.register is a script-URL sink: with the page's Trusted
// Types policy on (require-trusted-types-for 'script'), Chrome refuses a
// plain string there. With push on, the CSP allows exactly one policy,
// lever-sw, and this is it: its only output is the literal worker URL;
// anything else throws. Created once, on the first Turn on. A browser
// without Trusted Types takes the plain string.
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

// The per-login opt-in marker. A browser has one push subscription per
// worker scope, whoever is signed in, so on a shared device another login
// finds the subscription a first login made. Only the login that turned
// notifications on here re-sends it at load; any other sees "Turn on" and
// gets pushes only after it opts in itself.
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

// onWorkerMessage handles the service worker's one message: a tap on a
// notification while this page is loaded (sw.js). Only lever's own worker can
// send it, so it counts as the user acting on the page (hasActed).
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
el.ask.addEventListener('click', askManager);
el.push.addEventListener('click', () => void togglePush());
el.attach.addEventListener('click', () => {
  el.file.value = '';
  el.file.click();
});
el.file.addEventListener('change', () => void upload(el.file.files && el.file.files[0]));
el.mic.addEventListener('click', () => void toggleMic());
el.readvoice.addEventListener('change', () => saveReadVoice(el.readvoice.value));
if (speechOn() && typeof window.speechSynthesis.addEventListener === 'function') {
  // Voices load late in some browsers.
  window.speechSynthesis.addEventListener('voiceschanged', syncReadVoice);
}
el.files.addEventListener('click', () => {
  if (filesOpen) {
    closeFiles();
    return;
  }
  filesOpen = true;
  el.filespanel.hidden = false;
  setText(el.files, 'Hide files');
  void loadFiles();
});
window.addEventListener('resize', grow);

void start();
