// The chat page's logic that needs no browser: what a hub answer means, in
// which order messages show, and when to ask the hub again. chat.js does the
// DOM and the network. Tested with `node --test` (chatcore.test.mjs).
//
// Everything that comes from the hub is treated as data of unknown shape: a
// field that is not the expected type reads as absent, never as a fault.

// MAX_MESSAGE is the hub's own cap on one message (scion
// messages.MaxMessageLength). The hub counts characters, not UTF-16 units:
// see messageLength.
export const MAX_MESSAGE = 16000;

// messageLength counts text the way the hub does, so an emoji is one.
export function messageLength(text) {
  let n = 0;
  for (const _ of text) n++;
  return n;
}

const str = (v) => (typeof v === 'string' ? v : '');

// historyItems is the message list of a history answer, whichever key the
// hub used.
export function historyItems(body) {
  if (!body || typeof body !== 'object') return [];
  for (const key of ['messages', 'items']) {
    if (Array.isArray(body[key])) return body[key].filter((m) => m && typeof m === 'object');
  }
  return [];
}

// nextCursor is the history answer's cursor for the page before it, or ''.
export function nextCursor(body) {
  return body && typeof body === 'object' ? str(body.nextCursor) : '';
}

// messageText is a message's text: history rows carry it as `msg`, the
// answer to a send as `content`.
export function messageText(m) {
  return str(m && m.msg) || str(m && m.content);
}

// mergeMessages adds items to the map by id and reports whether anything
// changed. A message with no id cannot be told apart from its duplicates, so
// it is left out.
export function mergeMessages(map, items) {
  let changed = false;
  for (const m of items) {
    const id = str(m && m.id);
    if (!id) continue;
    const old = map.get(id);
    if (!old || messageText(old) !== messageText(m) || str(old.dispatchState) !== str(m.dispatchState)) {
      map.set(id, m);
      changed = true;
    }
  }
  return changed;
}

const time = (m) => {
  const t = Date.parse(str(m.createdAt));
  return Number.isNaN(t) ? 0 : t;
};

// sortedMessages is the map's messages, oldest first; the id breaks a tie so
// the order never depends on arrival.
export function sortedMessages(map) {
  return [...map.values()].sort((a, b) => time(a) - time(b) || (str(a.id) < str(b.id) ? -1 : str(a.id) > str(b.id) ? 1 : 0));
}

// classify says how a message shows: 'mine' (the operator wrote it), 'agent',
// 'system' (a state line, shown small), or 'hidden' (the hub's own copy of an
// operator message for @mention routing, or a message with nothing to show).
//
// The agent chooses the type of what it sends, so a type never hides an
// agent's message or makes it a state line: every type from the agent's
// side (the DM agent's id, agentId, or an agent sender) shows as an agent
// message.
export function classify(m, userId, agentId) {
  const type = str(m && m.type);
  if (!messageText(m)) return 'hidden';
  if (userId && str(m.senderId) === userId) return type === 'mention' ? 'hidden' : 'mine';
  if ((agentId && str(m.senderId) === agentId) || str(m.sender).startsWith('agent:')) return 'agent';
  return type === 'state-change' || type === 'system' ? 'system' : 'agent';
}

// isChatSubject reports whether an event subject is one of this user's own
// chat subjects. The page only uses it as a hint to read the history again,
// so a subject for anyone else is simply ignored.
export function isChatSubject(subject, userId) {
  return !!userId && str(subject).startsWith(`user.${userId}.chat.`);
}

// oneLine makes another party's text safe to quote inside a sentence of the
// page's own: one line, at most max characters. Control and format
// characters go (a direction override would redraw the rest of the
// sentence, the page's words included, right to left), as do the glyphs
// that draw as blank space, so the text cannot set a part of itself apart
// or push the page's words out of sight.
export function oneLine(text, max) {
  const flat = str(text).replace(/[\p{Cc}\p{Cf}\u2800\u3164\u115F\u1160\uFFA0]/gu, ' ').replace(/\s+/g, ' ').trim();
  // By character, so a cut never splits one.
  return [...flat].slice(0, max).join('');
}

// errorText is what to show for a failed request: the hub's own message when
// its answer has one, else the status.
export function errorText(status, body) {
  let msg = '';
  if (body && typeof body === 'object') {
    msg = str(body.message) || str(body.error) || str(body.error && body.error.message);
  } else {
    msg = str(body);
  }
  msg = oneLine(msg, 300);
  // Status 0 is the page's own word for "no answer": nothing to name.
  if (!status) return msg || 'no answer';
  return msg ? `${msg} (HTTP ${status})` : `request failed (HTTP ${status})`;
}

// makeCoalescer wraps fn so that a burst of calls runs it at most once per
// gapMs: the first call runs at once, later ones inside the gap collapse
// into ONE run at the end of it. now and setTimer are injected for the test.
export function makeCoalescer(fn, gapMs, now = () => Date.now(), setTimer = (f, ms) => setTimeout(f, ms)) {
  let last = -Infinity;
  let pending = false;
  const run = () => {
    pending = false;
    last = now();
    fn();
  };
  return () => {
    if (pending) return;
    const wait = last + gapMs - now();
    if (wait <= 0) {
      run();
      return;
    }
    pending = true;
    setTimer(run, wait);
  };
}

// The agent list (/lever/api/agents). The proxy builds it per login; the
// page still reads it as data of unknown shape, and shows only fixed words
// for what an agent reports about itself.

// WAKE_POLL_MS and WAKE_POLLS: after a wake, the list is read every 3 s for
// up to 90 s. LIST_MS: the list is read again this often while the page shows.
export const WAKE_POLL_MS = 3000;
export const WAKE_POLLS = 30;
export const LIST_MS = 15000;

const STATES = new Set(['running', 'starting', 'suspended', 'stopped', 'error', 'no-record', 'not-fresh', 'unknown']);
const ACTIVITIES = new Set(['working', 'waiting', 'idle']);
const LABEL_MAX = 60;
const UNREAD_MAX = 99;

// agentList reads a list answer: {login, tier, userId, console, agents}, or
// null when it is not one. A row needs a name and an access the page knows;
// a later row with the same name is dropped. A see-only row keeps only its
// name, role, access, state and label, whatever else the answer holds.
export function agentList(body) {
  if (!body || typeof body !== 'object' || !Array.isArray(body.agents)) return null;
  const seen = new Set();
  const agents = [];
  for (const r of body.agents) {
    if (!r || typeof r !== 'object') continue;
    const name = str(r.name);
    const access = str(r.access);
    if (!name || seen.has(name) || (access !== 'message' && access !== 'see')) continue;
    seen.add(name);
    const state = STATES.has(str(r.state)) ? r.state : 'unknown';
    const a = { name, role: r.role === 'manager' ? 'manager' : 'worker', access, state, label: oneLine(r.label, LABEL_MAX) };
    if (access === 'message') {
      a.activity = state === 'running' && ACTIVITIES.has(str(r.activity)) ? r.activity : '';
      a.id = str(r.id);
      a.conversation = str(r.conversation);
      a.terminal = str(r.terminal);
      if (Number.isInteger(r.unread) && r.unread >= 0 && r.unread <= UNREAD_MAX) a.unread = r.unread;
    }
    agents.push(a);
  }
  return {
    login: str(body.login),
    // Only the exact word earns the operator's links.
    tier: body.tier === 'operator' ? 'operator' : 'contact',
    userId: str(body.userId),
    console: str(body.console),
    agents,
    files: filesConfig(body),
  };
}

// rowTitle is the name, then the label: the name always comes first, so a
// label cannot pass for another agent.
export function rowTitle(a) {
  return a.label ? `${a.name} · ${a.label}` : a.name;
}

const CHIP = {
  starting: 'starting',
  suspended: 'asleep',
  stopped: 'stopped',
  error: 'error',
  'no-record': 'no record',
  'not-fresh': 'not fresh',
};

// chipText is the state as one of the page's fixed words.
export function chipText(a) {
  const state = str(a && a.state);
  if (state === 'running') return ACTIVITIES.has(str(a.activity)) ? a.activity : 'running';
  return Object.hasOwn(CHIP, state) ? CHIP[state] : 'unknown';
}

// badgeText is the unread count to show, or '' for none or not known.
export function badgeText(unread) {
  if (!Number.isInteger(unread) || unread <= 0) return '';
  return unread >= UNREAD_MAX ? `${UNREAD_MAX}+` : String(unread);
}

const NOTE_STARTING = 'starting – your message waits until it runs';
const NOTE_ASLEEP = 'asleep – your message wakes it';
const NOTE_OFFLINE = 'the assistant is offline';
const NOTE_UNKNOWN = 'state unknown – retrying';

// inputView says what the chat of agent a offers (the spec's state table):
// input (the composer is on), note (a fixed line under it), ask (the "Ask
// the manager to start" button; only when the login may message the
// manager), wake (a send wakes the agent first), viewOnly (no history, no
// input). The manager is never woken from the page. tier is the login's:
// a contact (anything but 'operator') wakes only a suspended worker.
export function inputView(a, canAskManager, tier) {
  const off = { input: false, note: '', ask: false, wake: false, viewOnly: false };
  if (!a || a.access !== 'message') return { ...off, viewOnly: true };
  const manager = a.role === 'manager';
  switch (a.state) {
    case 'running':
      return { ...off, input: true };
    case 'starting':
      return { ...off, input: true, note: NOTE_STARTING };
    case 'suspended':
    case 'stopped':
      if (manager) return { ...off, note: NOTE_OFFLINE };
      // A contact wakes only a suspended worker: a stopped one was stopped
      // by the operator on purpose, so the manager is asked instead (the
      // server refuses the wake too). Anything but 'operator' is a contact.
      if (a.state === 'stopped' && tier !== 'operator') return { ...off, ask: !!canAskManager };
      return { ...off, input: true, note: NOTE_ASLEEP, wake: true };
    case 'error':
    case 'no-record':
    case 'not-fresh':
      return manager ? { ...off, note: NOTE_OFFLINE } : { ...off, ask: !!canAskManager };
    default:
      return { ...off, note: NOTE_UNKNOWN };
  }
}

// askDraft is the editable draft the "Ask the manager" button leaves in the
// manager's chat. Nothing sends it but the person.
export function askDraft(name) {
  return `Please start ${name} for me.`;
}

const WAKE_TEXT = {
  'not-allowed': 'You are not allowed to wake this agent.',
  'not-asleep': 'It is not asleep any more.',
  'rate-limited': 'It was woken a moment ago; try again in a minute.',
  unavailable: 'Waking is not available on this instance.',
  refused: 'lever refused to wake it.',
  failed: 'Waking it failed.',
  origin: 'The request was refused.',
};

// wakeText is the page's own sentence for a refused wake: one per reason
// word the proxy answers with, never the answer's text.
export function wakeText(status, body, name) {
  const word = body && typeof body === 'object' ? str(body.error) : '';
  if (Object.hasOwn(WAKE_TEXT, word)) return WAKE_TEXT[word];
  return `${name} could not be woken (HTTP ${status || 'no answer'}).`;
}

// The operator's read-only view of contact conversations
// (/lever/api/contacts). Like every answer, these are data of unknown shape.

export const CONTACTS_MS = 30000; // the contact list and an open transcript refresh this often
export const NOT_SHOWN = 'not shown to the contact';
export const NOT_YET = 'not yet read by the contact';
const FROM = new Set(['contact', 'agent', 'system']);

// contactList reads the contact list: [{login, signedIn, agents: [{name,
// label, state, access: 'see'}]}], or null when it is not one. access 'see'
// keeps agent rows of this list off every input.
export function contactList(body) {
  if (!body || typeof body !== 'object' || !Array.isArray(body.contacts)) return null;
  const seen = new Set();
  const out = [];
  for (const c of body.contacts) {
    if (!c || typeof c !== 'object') continue;
    const login = str(c.login);
    if (!login || seen.has(login)) continue;
    seen.add(login);
    const names = new Set();
    const agents = [];
    for (const a of Array.isArray(c.agents) ? c.agents : []) {
      const name = str(a && a.name);
      if (!name || names.has(name)) continue;
      names.add(name);
      agents.push({ name, label: oneLine(a.label, LABEL_MAX), state: STATES.has(str(a.state)) ? a.state : 'unknown', access: 'see' });
    }
    // noFiles: files are on, but not for this contact (files: false).
    out.push({ login, signedIn: c.signedIn === true, agents, ...(c.noFiles === true ? { noFiles: true } : {}) });
  }
  return out;
}

// transcriptItems reads a transcript page's rows. A row without an id is
// left out; an unknown writer reads as the agent; a row counts as shown to
// the contact only when the answer says exactly true.
export function transcriptItems(body) {
  if (!body || typeof body !== 'object' || !Array.isArray(body.messages)) return [];
  return body.messages
    .filter((m) => m && typeof m === 'object' && str(m.id))
    .map((m) => {
      const shownToContact = m.shownToContact === true;
      // pending: a record would show it, but the contact has not read it yet.
      return { id: str(m.id), from: FROM.has(str(m.from)) ? m.from : 'agent', text: str(m.text), createdAt: str(m.createdAt), shownToContact, pending: !shownToContact && m.pending === true };
    });
}

// transcriptPath is the route of one transcript page.
export function transcriptPath(login, name, cursor) {
  const q = new URLSearchParams({ limit: '50' });
  if (cursor) q.set('cursor', cursor);
  return `/lever/api/contacts/${encodeURIComponent(login)}/agents/${encodeURIComponent(name)}/messages?${q}`;
}

// viewFilesPath is the route of a contact's files with one agent
// (remote.files, operator only).
export function viewFilesPath(login, name) {
  return `/lever/api/contacts/${encodeURIComponent(login)}/agents/${encodeURIComponent(name)}/files`;
}

// transcriptWho names a row's writer.
export function transcriptWho(m, login, name) {
  if (m.from === 'contact') return login;
  return m.from === 'agent' ? name : 'hub';
}

// mergeRows adds transcript rows by id and reports whether anything
// changed: a row, its text, or its mark (an agent row binds later).
export function mergeRows(map, items) {
  let changed = false;
  for (const m of items) {
    const old = map.get(m.id);
    if (!old || old.text !== m.text || old.shownToContact !== m.shownToContact || old.pending !== m.pending) {
      map.set(m.id, m);
      changed = true;
    }
  }
  return changed;
}

// viewErrorText says why a transcript cannot be read.
export function viewErrorText(status, body) {
  const word = body && typeof body === 'object' ? str(body.error) : '';
  // The hint is lever's fixed word: the contact's hub user is not the one
  // lever apply bound. apply binds a login only while no agent container
  // runs, so the fix is a stop and an up.
  if (word === 'not-signed-in' && body.hint === 'rebind') return 'the contact has a new hub user: run lever stop, then lever up';
  if (word === 'not-signed-in') return 'has not signed in yet';
  if (word === 'no-record') return 'the agent has no record on the hub yet';
  return errorText(status, body);
}

// hashAgent reads the agent a notification opened the page for
// (#agent=<name>): a config name, else ''.
export function hashAgent(hash) {
  const m = typeof hash === 'string' ? /^#agent=([a-z0-9][a-z0-9-]{0,62})$/.exec(hash) : null;
  return m ? m[1] : '';
}

// pushKeyBytes turns lever's VAPID key into what pushManager.subscribe
// takes: the 65 bytes of an uncompressed P-256 point, else null.
export function pushKeyBytes(key) {
  if (typeof key !== 'string' || !/^[A-Za-z0-9_-]{87}$/.test(key)) return null;
  let bin;
  try {
    bin = atob(`${key.replace(/-/g, '+').replace(/_/g, '/')}=`);
  } catch {
    return null;
  }
  if (bin.length !== 65 || bin.charCodeAt(0) !== 4) return null;
  return Uint8Array.from(bin, (c) => c.charCodeAt(0));
}

// pushView is the notifications button and its note. Hidden where the
// browser has no push or lever has it off; a note when the browser blocks it.
export function pushView({ available, permission, subscribed, busy, error }) {
  if (!available) return { hidden: true, disabled: true, text: '', note: '' };
  if (permission === 'denied') return { hidden: true, disabled: true, text: '', note: 'Notifications are blocked for this page in the browser settings.' };
  return { hidden: false, disabled: !!busy, text: subscribed ? 'Turn off notifications' : 'Turn on notifications', note: error || '' };
}

// Files in the chat (remote.files).
export const FILE_LIST_MAX = 200;
export const UPLOAD_MS = 3600000; // the server's own body deadline: 10 min plus max_bytes at 64 KiB/s, at most 1 h

const FILE_ID = /^[0-9a-f]{32}$/;
const AGENT_NAME = /^[a-z0-9][a-z0-9-]{0,62}$/;

// filesConfig is the list's files object, or null when files are off (or
// the object is not one the page understands).
export function filesConfig(body) {
  const f = body && typeof body === 'object' ? body.files : null;
  if (!f || typeof f !== 'object' || !Number.isInteger(f.maxBytes) || f.maxBytes <= 0 || !Array.isArray(f.extensions)) return null;
  return { maxBytes: f.maxBytes, extensions: f.extensions.filter((e) => typeof e === 'string' && /^[a-z0-9]{1,10}$/.test(e)),
    // A direction is on unless the answer says false (remote.files.uploads/shares).
    uploads: f.uploads !== false, shares: f.shares !== false };
}

// fileList reads /lever/api/files/<agent>: rows with a well-formed id only;
// the name is one line of text; direction is "sent" or "received".
export function fileList(body) {
  if (!body || typeof body !== 'object' || !Array.isArray(body.files)) return null;
  const out = [];
  for (const f of body.files.slice(0, FILE_LIST_MAX)) {
    if (!f || typeof f !== 'object' || typeof f.id !== 'string' || !FILE_ID.test(f.id)) continue;
    out.push({ id: f.id, name: oneLine(f.name, 120) || 'file', size: Number.isInteger(f.size) && f.size >= 0 ? f.size : 0,
      at: typeof f.at === 'string' ? f.at : '', direction: f.direction === 'received' ? 'received' : 'sent' });
  }
  return out;
}

// sizeText is a byte count for people.
export function sizeText(n) {
  if (n < 1024) return `${n} B`;
  if (n < 1048576) return `${Math.round(n / 1024)} KB`;
  return `${(n / 1048576).toFixed(1)} MB`;
}

// fileCheck is why a picked file cannot go ('' when it can), before any
// request: the server checks the same and more.
export function fileCheck(file, cfg) {
  const name = typeof file.name === 'string' ? file.name : '';
  const dot = name.lastIndexOf('.');
  const ext = dot > 0 ? name.slice(dot + 1).toLowerCase() : '';
  if (!ext || !cfg.extensions.includes(ext)) {
    return `Files of type ${ext ? `.${ext}` : 'with no extension'} are not accepted (${cfg.extensions.map((e) => `.${e}`).join(', ')}).`;
  }
  if (file.size > cfg.maxBytes) return `The file is ${sizeText(file.size)}; the limit is ${sizeText(cfg.maxBytes)}.`;
  return '';
}

const UPLOAD_WORDS = {
  'too-large': 'the file is too large',
  extension: 'that file type is not accepted',
  rate: 'too many uploads this hour; try again later',
  quota: 'too much uploaded today; try again tomorrow',
  busy: 'other uploads are under way; try again in a moment',
  'not-fresh': 'the agent must restart before it takes files; ask the manager',
  'try-again': "the agent's state cannot be checked now; try again in a moment",
  'not-allowed': 'you may not send files to this agent',
  'one-file': 'send one file at a time',
  'bad-form': 'the upload was not understood',
  timeout: 'the upload took too long; try again on a faster connection',
  workspace: "the agent's file folder is not usable; tell the operator",
  unavailable: 'files are unavailable right now',
  origin: 'the upload did not come from this page',
};

// uploadErrorText is what to show for a refused upload: the proxy's fixed
// word in the page's own words, else errorText.
export function uploadErrorText(status, body) {
  const word = body && typeof body === 'object' && typeof body.error === 'string' ? body.error : '';
  return Object.hasOwn(UPLOAD_WORDS, word) ? UPLOAD_WORDS[word] : errorText(status, body);
}

// uploadNote is the chat message the page sends after an upload: the agent
// reads it as this login's message and looks the file up (contact_files).
export function uploadNote(name) {
  return `📎 uploaded ${name}`;
}

// downloadPath is lever's download route for a row, or '' when the agent
// name or the id is not one the page builds a link from.
export function downloadPath(agent, id) {
  return AGENT_NAME.test(agent) && FILE_ID.test(id) ? `/lever/api/files/${agent}/${id}` : '';
}
