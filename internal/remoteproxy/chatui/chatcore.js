// The chat page's logic that needs no browser; chat.js does the DOM and the
// network. Everything from the hub is data of unknown shape: a field of the
// wrong type reads as absent, never as a fault.

// MAX_MESSAGE is the hub's cap on one message (scion
// messages.MaxMessageLength), counted in characters, not UTF-16 units.
export const MAX_MESSAGE = 16000;

export function messageLength(text) {
  let n = 0;
  for (const _ of text) n++;
  return n;
}

const str = (v) => (typeof v === 'string' ? v : '');

export function historyItems(body) {
  if (!body || typeof body !== 'object') return [];
  for (const key of ['messages', 'items']) {
    if (Array.isArray(body[key])) return body[key].filter((m) => m && typeof m === 'object');
  }
  return [];
}

export function nextCursor(body) {
  return body && typeof body === 'object' ? str(body.nextCursor) : '';
}

export function messageText(m) {
  return str(m && m.msg) || str(m && m.content);
}

// A message with no id cannot be told from its duplicates, so it is left out.
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

// The id breaks a tie so the order never depends on arrival.
export function sortedMessages(map) {
  return [...map.values()].sort((a, b) => time(a) - time(b) || (str(a.id) < str(b.id) ? -1 : str(a.id) > str(b.id) ? 1 : 0));
}

// classify is 'mine', 'agent', 'system' (a state line) or 'hidden' (the hub's
// copy of an operator message for @mention routing, or nothing to show).
// The agent chooses its message type, so every message from the agent's side
// shows as 'agent': a type never hides it or makes it a state line.
export function classify(m, userId, agentId) {
  const type = str(m && m.type);
  if (!messageText(m)) return 'hidden';
  if (userId && str(m.senderId) === userId) return type === 'mention' ? 'hidden' : 'mine';
  if ((agentId && str(m.senderId) === agentId) || str(m.sender).startsWith('agent:')) return 'agent';
  return type === 'state-change' || type === 'system' ? 'system' : 'agent';
}

// A subject is only a hint to read the history again; anyone else's is
// ignored.
export function isChatSubject(subject, userId) {
  return !!userId && str(subject).startsWith(`user.${userId}.chat.`);
}

// oneLine makes another party's text safe to quote in the page's own
// sentence: one line, at most max characters, without control and format
// characters (a direction override would redraw the page's words) or blank
// glyphs, so it cannot set a part of itself apart or push the page's words
// out of sight.
export function oneLine(text, max) {
  const flat = str(text).replace(/[\p{Cc}\p{Cf}\u2800\u3164\u115F\u1160\uFFA0]/gu, ' ').replace(/\s+/g, ' ').trim();
  // By character, so a cut never splits one.
  return [...flat].slice(0, max).join('');
}

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

// makeCoalescer runs fn at most once per gapMs: the first call at once, later
// ones in the gap as one run at its end.
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

// The agent list (/lever/api/agents) is read as data of unknown shape, and
// shows only fixed words for what an agent reports about itself.

// After a wake the list is read every 3 s for up to 90 s.
export const WAKE_POLL_MS = 3000;
export const WAKE_POLLS = 30;
export const LIST_MS = 15000;

const STATES = new Set(['running', 'starting', 'suspended', 'stopped', 'error', 'no-record', 'not-fresh', 'unknown']);
const ACTIVITIES = new Set(['working', 'waiting', 'idle']);
const LABEL_MAX = 60;
const UNREAD_MAX = 99;

// agentList is null when body is not a list. A row needs a name and a known
// access; a duplicate name is dropped; a see-only row keeps only name, role,
// access, state and label.
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
    // The server offers it to the operator only, and refuses anyone else.
    newConversation: body.tier === 'operator' && body.newConversation === true,
    agents,
    files: filesConfig(body),
    voice: voiceConfig(body),
    readAloud: !!(body && typeof body === 'object' && body.readAloud === true),
    speech: speechConfig(body),
  };
}

// The name always comes first, so a label cannot pass for another agent.
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

export function chipText(a) {
  const state = str(a && a.state);
  if (state === 'running') return ACTIVITIES.has(str(a.activity)) ? a.activity : 'running';
  return Object.hasOwn(CHIP, state) ? CHIP[state] : 'unknown';
}

export function badgeText(unread) {
  if (!Number.isInteger(unread) || unread <= 0) return '';
  return unread >= UNREAD_MAX ? `${UNREAD_MAX}+` : String(unread);
}

const NOTE_STARTING = 'starting – your message waits until it runs';
const NOTE_ASLEEP = 'asleep – your message wakes it';
const NOTE_OFFLINE = 'the assistant is offline';
const NOTE_UNKNOWN = 'state unknown – retrying';

// inputView is what agent a's chat offers: input, note, ask (only when the
// login may message the manager), wake, viewOnly. The manager is never woken
// from the page.
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
      // A contact (anything but 'operator') wakes only a suspended worker: a
      // stopped one was stopped by the operator on purpose. The server refuses too.
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

// Nothing sends the draft but the person.
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

// One fixed sentence per reason word, never the answer's text.
export function wakeText(status, body, name) {
  const word = body && typeof body === 'object' ? str(body.error) : '';
  if (Object.hasOwn(WAKE_TEXT, word)) return WAKE_TEXT[word];
  return `${name} could not be woken (HTTP ${status || 'no answer'}).`;
}

// A new conversation for the manager (POST /lever/api/new-conversation):
// the operator's only.
export const NEW_CONVERSATION_PATH = '/lever/api/new-conversation';
// The accepted change's own state, by the id the POST answered with. The
// manager can stop and start again between two reads of the list, so the
// list cannot tell a change that ended from one that never began.
export const NEW_CONVERSATION_STATUS_PATH = '/lever/api/new-conversation/status';
// The status is read every WAKE_POLL_MS for up to 7 min: lever waits up to
// 3 min for another change to the manager, then suspends, backs up, stops and
// starts it.
export const NEW_CONVERSATION_POLLS = 140;

// newConversationOffered: the open chat is the manager's, and the list offers
// the action.
export function newConversationOffered(roster, a) {
  return !!(roster && roster.newConversation && roster.tier === 'operator' && a && a.role === 'manager' && a.access === 'message');
}

export function newConversationAsk(name) {
  return `Start ${name} on a new conversation? lever backs up the current one (it can be restored on the host) and ${name} starts again with its boot prompt. Its context is cleared.`;
}

export function newConversationForceAsk(name) {
  return `${name} is in the middle of a turn. Interrupt it and start a new conversation anyway?`;
}

const NEW_CONVERSATION_TEXT = {
  busy: 'It is in the middle of a turn; nothing was changed.',
  'in-progress': 'lever is already changing the manager (a new conversation, a restore, or lever up, apply, stop or reload). Try again when it is done.',
  'not-running': 'It is not running; nothing was changed.',
  'rate-limited': 'A new conversation was started a moment ago; try again in a minute.',
  'not-allowed': 'Only the operator can start a new conversation.',
  unavailable: 'A new conversation is not available on this instance.',
  failed: 'lever could not start the new conversation; `lever doctor` on the host shows why.',
  timeout: 'lever did not finish the new conversation in time; `lever conversation list` and .lever-state/logs/conversation-new.log on the host say how it ended.',
  origin: 'The request was refused.',
};

const BACKUP_ID = /^[0-9]{8}T[0-9]{6}Z(-[0-9]{1,3})?$/;

// newConversationOutcome reads a status answer: null while the change runs
// (or the read failed), else {error} for a refusal or failure, or {note}.
export function newConversationOutcome(status, body, name) {
  const b = body && typeof body === 'object' ? body : {};
  if (status === 200 && b.state === 'ok') {
    const backup = str(b.backup);
    if (BACKUP_ID.test(backup)) return { note: `${name} is on a new conversation. The old one is backup ${backup}: \`lever conversation restore ${backup}\` on the host puts it back.` };
    return { note: `${name} is on a new conversation.` };
  }
  if (status === 200 && b.state === 'failed') return { error: newConversationText(status, b, name) };
  if (status === 404) return { note: newConversationLostText(name) };
  return null;
}

// The server no longer holds the change (it restarted), or the page stopped
// waiting for it: neither says it failed.
export function newConversationLostText(name) {
  return `The page lost track of ${name}'s new conversation; \`lever conversation list\` on the host says how it ended.`;
}

// One fixed sentence per reason word, never the answer's text.
export function newConversationText(status, body, name) {
  const word = body && typeof body === 'object' ? str(body.error) : '';
  if (Object.hasOwn(NEW_CONVERSATION_TEXT, word)) return NEW_CONVERSATION_TEXT[word];
  if (!status) return `No answer from the server: ${name}'s new conversation may have started all the same; the list shows when it goes down.`;
  return `${name}'s new conversation could not be started (HTTP ${status}).`;
}

// The operator's read-only view of contact conversations (/lever/api/contacts).

export const CONTACTS_MS = 30000; // the contact list and an open transcript refresh this often
export const NOT_SHOWN = 'not shown to the contact';
export const NOT_YET = 'not yet read by the contact';
const FROM = new Set(['contact', 'agent', 'system']);

// access 'see' keeps this list's agent rows off every input.
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

// An unknown writer reads as the agent; a row is shown to the contact only
// when the answer says exactly true.
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

export function transcriptPath(login, name, cursor) {
  const q = new URLSearchParams({ limit: '50' });
  if (cursor) q.set('cursor', cursor);
  return `/lever/api/contacts/${encodeURIComponent(login)}/agents/${encodeURIComponent(name)}/messages?${q}`;
}

export function viewFilesPath(login, name) {
  return `/lever/api/contacts/${encodeURIComponent(login)}/agents/${encodeURIComponent(name)}/files`;
}

export function transcriptWho(m, login, name) {
  if (m.from === 'contact') return login;
  return m.from === 'agent' ? name : 'hub';
}

// A row's mark changes too: an agent row binds later.
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

export function viewErrorText(status, body) {
  const word = body && typeof body === 'object' ? str(body.error) : '';
  // lever apply binds a login only while no agent container runs, so a new
  // hub user needs a stop and an up.
  if (word === 'not-signed-in' && body.hint === 'rebind') return 'the contact has a new hub user: run lever stop, then lever up';
  if (word === 'not-signed-in') return 'has not signed in yet';
  if (word === 'no-record') return 'the agent has no record on the hub yet';
  return errorText(status, body);
}

// hashAgent is the #agent=<name> a notification opened, if a config name, else ''.
export function hashAgent(hash) {
  const m = typeof hash === 'string' ? /^#agent=([a-z0-9][a-z0-9-]{0,62})$/.exec(hash) : null;
  return m ? m[1] : '';
}

// pushKeyBytes is the 65-byte uncompressed P-256 point, else null.
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

export function pushView({ available, permission, subscribed, busy, error }) {
  if (!available) return { hidden: true, disabled: true, text: '', note: '' };
  if (permission === 'denied') return { hidden: true, disabled: true, text: '', note: 'Notifications are blocked for this page in the browser settings.' };
  return { hidden: false, disabled: !!busy, text: subscribed ? 'Turn off notifications' : 'Turn on notifications', note: error || '' };
}

export const FILE_LIST_MAX = 200;
export const UPLOAD_MS = 3600000; // the server's own body deadline: 10 min plus max_bytes at 64 KiB/s, at most 1 h

const FILE_ID = /^[0-9a-f]{32}$/;
const AGENT_NAME = /^[a-z0-9][a-z0-9-]{0,62}$/;

export function filesConfig(body) {
  const f = body && typeof body === 'object' ? body.files : null;
  if (!f || typeof f !== 'object' || !Number.isInteger(f.maxBytes) || f.maxBytes <= 0 || !Array.isArray(f.extensions)) return null;
  return { maxBytes: f.maxBytes, extensions: f.extensions.filter((e) => typeof e === 'string' && /^[a-z0-9]{1,10}$/.test(e)),
    // A direction is on unless the answer says false (remote.files.uploads/shares).
    uploads: f.uploads !== false, shares: f.shares !== false };
}

// Only rows with a well-formed id; the name is one line of text.
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

export function sizeText(n) {
  if (n < 1024) return `${n} B`;
  if (n < 1048576) return `${Math.round(n / 1024)} KB`;
  return `${(n / 1048576).toFixed(1)} MB`;
}

// fileCheck runs before any request; the server checks the same and more.
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

export function uploadErrorText(status, body) {
  const word = body && typeof body === 'object' && typeof body.error === 'string' ? body.error : '';
  return Object.hasOwn(UPLOAD_WORDS, word) ? UPLOAD_WORDS[word] : errorText(status, body);
}

// The agent reads the note as this login's message and looks the file up.
export function uploadNote(name) {
  return `📎 uploaded ${name}`;
}

// downloadPath is '' unless the agent name and id are ones the page builds a
// link from.
export function downloadPath(agent, id) {
  return AGENT_NAME.test(agent) && FILE_ID.test(id) ? `/lever/api/files/${agent}/${id}` : '';
}

// Dictation and read-aloud (remote.voice). A transcript goes into the message
// box, never straight to the agent.
export const VOICE_RATE = 16000;
export const VOICE_MAX_SECONDS = 600; // the config's ceiling
export const VOICE_MIN_SAMPLES = VOICE_RATE / 10; // the server refuses less

export function voiceConfig(body) {
  const v = body && typeof body === 'object' ? body.voice : null;
  if (!v || typeof v !== 'object' || !Number.isInteger(v.maxSeconds) || v.maxSeconds <= 0 || v.maxSeconds > VOICE_MAX_SECONDS) return null;
  return { maxSeconds: v.maxSeconds };
}

// The server's own body deadline for a clip of maxSeconds, plus transcription.
export function voiceRequestMs(maxSeconds) {
  return (120 + 2 * maxSeconds) * 1000;
}

export function resampledLength(inLength, inRate, outRate = VOICE_RATE) {
  if (!(inLength > 0) || !(inRate > 0)) return 0;
  return Math.max(1, Math.ceil((inLength * outRate) / inRate));
}

// encodeWAV writes the canonical 44-byte header the proxy checks byte for
// byte (voice.CheckWAV), then PCM s16le mono.
export function encodeWAV(samples, rate = VOICE_RATE) {
  const n = samples.length;
  const buf = new ArrayBuffer(44 + 2 * n);
  const v = new DataView(buf);
  const ascii = (at, s) => {
    for (let i = 0; i < s.length; i++) v.setUint8(at + i, s.charCodeAt(i));
  };
  ascii(0, 'RIFF');
  v.setUint32(4, 36 + 2 * n, true);
  ascii(8, 'WAVE');
  ascii(12, 'fmt ');
  v.setUint32(16, 16, true);
  v.setUint16(20, 1, true); // PCM
  v.setUint16(22, 1, true); // mono
  v.setUint32(24, rate, true);
  v.setUint32(28, rate * 2, true);
  v.setUint16(32, 2, true);
  v.setUint16(34, 16, true);
  ascii(36, 'data');
  v.setUint32(40, 2 * n, true);
  for (let i = 0; i < n; i++) {
    const s = Math.max(-1, Math.min(1, Number.isFinite(samples[i]) ? samples[i] : 0));
    v.setInt16(44 + 2 * i, s < 0 ? Math.round(s * 0x8000) : Math.round(s * 0x7fff), true);
  }
  return new Uint8Array(buf);
}

export function clockText(seconds) {
  const s = Math.max(0, Math.floor(seconds));
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, '0')}`;
}

export function insertText(value, start, end, text) {
  const v = typeof value === 'string' ? value : '';
  const t = typeof text === 'string' ? text.trim() : '';
  const a = Number.isInteger(start) && start >= 0 && start <= v.length ? start : v.length;
  const b = Number.isInteger(end) && end >= a && end <= v.length ? end : a;
  if (!t) return { value: v, cursor: b };
  const before = v.slice(0, a);
  const after = v.slice(b);
  const pre = before && !/\s$/.test(before) ? ' ' : '';
  const post = after && !/^\s/.test(after) ? ' ' : '';
  const out = `${before}${pre}${t}${post}`;
  return { value: out + after, cursor: out.length };
}

const VOICE_WORDS = {
  'voice-off': 'dictation is off for this login',
  origin: 'the clip did not come from this page',
  rate: 'too many clips this hour; try again later',
  quota: 'too much dictation today; try again tomorrow',
  busy: 'other clips are being transcribed; try again in a moment',
  'too-long': 'the clip is too long',
  'bad-audio': 'the recording could not be read',
  timeout: 'the clip took too long to send; try again on a faster connection',
  unavailable: 'transcription is unavailable right now',
};

export function voiceErrorText(status, body) {
  const word = body && typeof body === 'object' && typeof body.error === 'string' ? body.error : '';
  return Object.hasOwn(VOICE_WORDS, word) ? VOICE_WORDS[word] : errorText(status, body);
}

export function transcriptOf(body) {
  if (!body || typeof body !== 'object' || typeof body.text !== 'string') return null;
  return [...body.text.trim()].slice(0, MAX_MESSAGE).join('');
}

export const CODE_SKIPPED = 'code block skipped';
const SPEAK_MAX = 20000; // characters read from one message

function hostOf(u) {
  try {
    return new URL(u).hostname.replace(/^www\./, '');
  } catch {
    return '';
  }
}

export function speechText(text) {
  let t = str(text).slice(0, SPEAK_MAX).replace(/\r\n?/g, '\n');
  // Fenced code blocks (``` or ~~~), closed or running to the end.
  t = t.replace(/(^|\n)[ \t]*(`{3,}|~{3,})[^\n]*\n[\s\S]*?(?:\n[ \t]*\2[ \t]*(?=\n|$)|$)/g, `$1${CODE_SKIPPED}.\n`);
  // Indented code: lines indented four spaces or a tab, after a blank line.
  t = t.replace(/(^|\n\n)(?:(?: {4}|\t)[^\n]*(?:\n|$))+/g, `$1${CODE_SKIPPED}.\n`);
  // Images: their alt text. Links: their text.
  t = t.replace(/!\[([^\]]*)\]\([^)]*\)/g, '$1');
  t = t.replace(/\[([^\]]+)\]\([^)]*\)/g, '$1');
  // Bare URLs and <URL>: the host name.
  t = t.replace(/<?\bhttps?:\/\/[^\s<>)\]]+>?/g, (m) => hostOf(m.replace(/^<|>$/g, '')) || 'link');
  // Inline code keeps its words; emphasis, headings, quotes, rules, list
  // markers and table pipes go.
  t = t.replace(/`+([^`\n]*)`+/g, '$1');
  t = t.replace(/(\*\*|__)(.+?)\1/g, '$2').replace(/(^|[\s(])[*_]([^*_\n]+)[*_](?=[\s).,;:!?]|$)/gm, '$1$2').replace(/~~(.+?)~~/g, '$1');
  t = t.replace(/^[ \t]*#{1,6}[ \t]+/gm, '').replace(/^[ \t]*>[ \t]?/gm, '');
  t = t.replace(/^[ \t]*([-*_])(?:[ \t]*\1){2,}[ \t]*$/gm, '');
  t = t.replace(/^[ \t]*\|?(?:[ \t]*:?-+:?[ \t]*\|)+[ \t]*:?-*:?[ \t]*$/gm, '');
  t = t.replace(/^[ \t]*(?:[-*+]|\d+[.)])[ \t]+/gm, '');
  t = t.replace(/^[ \t]*\|/gm, '').replace(/\|[ \t]*$/gm, '').replace(/[ \t]*\|[ \t]*/g, ', ');
  t = t.replace(/<\/?[a-zA-Z][^<>]*>/g, ' ');
  t = t.replace(/[\p{Cc}\p{Cf}]/gu, (c) => (c === '\n' ? '\n' : ' '));
  return t.replace(/[ \t]+/g, ' ').replace(/ *\n+ */g, '\n').trim();
}

// Read-aloud never uses a voice that sends the text to a service.
export function localVoices(voices) {
  return (Array.isArray(voices) ? voices : []).filter((v) => v && v.localService === true && typeof v.voiceURI === 'string');
}

// Server voices: lever-tool-tts on the operator's host.
const SPEECH_VOICES_MAX = 100;
const SPEECH_CHARS_MIN = 100;
const SPEECH_CHARS_MAX = 2000;
// A short first part starts the voice sooner.
export const SPEECH_FIRST = 200;
const speechVoiceRE = /^[A-Za-z0-9_.-]{1,40}$/;

export function speechConfig(body) {
  const v = body && typeof body === 'object' ? body.speech : null;
  if (!v || typeof v !== 'object' || !Array.isArray(v.voices) || !Number.isInteger(v.maxChars) ||
    v.maxChars < SPEECH_CHARS_MIN || v.maxChars > SPEECH_CHARS_MAX) return null;
  const voices = [];
  for (const x of v.voices.slice(0, SPEECH_VOICES_MAX)) {
    if (!x || typeof x !== 'object' || typeof x.name !== 'string' || !speechVoiceRE.test(x.name)) continue;
    voices.push({ name: x.name, lang: oneLine(x.lang, 20) });
  }
  if (!voices.length) return null;
  const voice = voices.some((x) => x.name === v.voice) ? v.voice : voices[0].name;
  return { voice, voices, maxChars: v.maxChars };
}

export function pickServerVoice(cfg, saved) {
  if (!cfg) return '';
  return cfg.voices.some((x) => x.name === saved) ? saved : cfg.voice;
}

// speechParts splits into whole sentences where it can, the first part at
// most SPEECH_FIRST characters, the others maxChars. Characters are code
// points, as the server counts them.
export function speechParts(text, maxChars) {
  const max = Math.max(1, Math.min(maxChars, SPEECH_CHARS_MAX));
  // No lookbehind: older Safari cannot parse it, and the module would not load.
  const sentences = str(text).replace(/([.!?…:;])\s+/g, '$1\n').split(/\n+/).map((x) => x.trim()).filter(Boolean);
  const first = Math.min(SPEECH_FIRST, max);
  const pieces = [];
  for (const sentence of sentences) {
    let rest = [...sentence];
    for (let lim = pieces.length ? max : first; rest.length > lim; lim = max) {
      let cut = rest.lastIndexOf(' ', lim);
      if (cut <= 0) cut = lim;
      pieces.push(rest.slice(0, cut).join('').trim());
      rest = rest.slice(cut);
      while (rest[0] === ' ') rest.shift();
    }
    if (rest.length) pieces.push(rest.join(''));
  }
  const parts = [];
  let cur = '';
  for (const piece of pieces) {
    const limit = parts.length === 0 ? Math.min(SPEECH_FIRST, max) : max;
    const joined = cur ? `${cur} ${piece}` : piece;
    if (cur && [...joined].length > limit) {
      parts.push(cur);
      cur = piece;
    } else {
      cur = joined;
    }
  }
  if (cur) parts.push(cur);
  // A part with nothing to say (only symbols) would be refused.
  return parts.filter((p) => /[\p{L}\p{N}]/u.test(p));
}

const SPEAK_WORDS = {
  'voice-off': 'read-aloud is off for this login',
  origin: 'the request did not come from this page',
  rate: 'too much read aloud this hour; try again later',
  quota: 'too much read aloud today; try again tomorrow',
  busy: 'other messages are being read; try again in a moment',
  'too-long': 'a part of the message is too long',
  'bad-text': 'nothing in the message can be read aloud',
  'bad-voice': 'the voice is not available; choose another',
  agent: 'this agent is not on your list',
  unavailable: 'the server voice is unavailable right now',
};

export function speakErrorText(status, body) {
  const word = body && typeof body === 'object' && typeof body.error === 'string' ? body.error : '';
  return Object.hasOwn(SPEAK_WORDS, word) ? SPEAK_WORDS[word] : errorText(status, body);
}

// pickVoice prefers the saved voice, then the browser's default, then the
// page's language, then the first.
export function pickVoice(voices, savedURI, lang) {
  const local = localVoices(voices);
  if (!local.length) return null;
  const want = str(lang).toLowerCase().split('-')[0];
  return local.find((v) => v.voiceURI === savedURI) ||
    local.find((v) => v.default === true) ||
    (want && local.find((v) => str(v.lang).toLowerCase().split('-')[0] === want)) ||
    local[0];
}

// Recent activity (POST /lever/api/agents/<name>/activity): the last lines of
// an agent's terminal pane, the operator's only. The proxy cleans the text;
// the page cleans it again and writes it as text.
export const ACTIVITY_MAX = 64 * 1024;

export function activityPath(name) {
  return `/lever/api/agents/${encodeURIComponent(str(name))}/activity`;
}

// activityOffered: an operator, an agent it messages, with a hub record.
export function activityOffered(list, a) {
  return !!(list && list.tier === 'operator' && a && a.access === 'message' && a.id);
}

// activityText is {text, truncated}, or null when body is not an answer.
// Control and format characters go (a tab and a line break stay), and the text
// keeps its last ACTIVITY_MAX characters.
export function activityText(body) {
  if (!body || typeof body !== 'object' || typeof body.text !== 'string') return null;
  let text = body.text.replace(/\r/g, '').replace(/[\u0000-\u0008\u000b-\u001f\u007f-\u009f\u061c\u200b\u200e\u200f\u2028-\u202e\u2060-\u2069\ufeff\ufff9-\ufffb\p{Cf}]/gu, '');
  let truncated = body.truncated === true;
  if (text.length > ACTIVITY_MAX) {
    text = text.slice(text.length - ACTIVITY_MAX);
    // Never start on the second half of a surrogate pair.
    if (/^[\udc00-\udfff]/.test(text)) text = text.slice(1);
    truncated = true;
  }
  return { text, truncated };
}

const ACTIVITY_WORDS = {
  'not-allowed': 'this login may not read the terminal',
  'rate-limited': 'it was read a moment ago; try again in a few seconds',
  'too-large': 'the terminal holds too much to show; open the Terminal',
  'hub-refused': 'the hub refused the read',
  unavailable: 'the terminal cannot be read right now',
  origin: 'the request did not come from this page',
};

export function activityErrorText(status, body, name) {
  const word = body && typeof body === 'object' ? str(body.error) : '';
  if (word === 'not-running') return `${name} is not running (${chipText({ state: str(body.state) })})`;
  return Object.hasOwn(ACTIVITY_WORDS, word) ? ACTIVITY_WORDS[word] : errorText(status, body);
}

// The thinking dots: an empty bubble on the agent's side while a reply is
// likely on its way. The last message shown must be the user's own and
// delivered (an agent's reply ends the dots; a failed send waits for nobody).
// Then either the agent reports working, or a send was stored a moment ago
// (pending): the dots come at once, not with the next list read, which can
// be LIST_MS away. The page drops pending after THINKING_MS, or when the
// agent, having reported working, reports anything else.
export const THINKING_MS = 30000;

export function thinkingShown(ordered, userId, agentId, activity, pending) {
  let last = '';
  let failed = false;
  for (const m of ordered) {
    const kind = classify(m, userId, agentId);
    if (kind !== 'mine' && kind !== 'agent') continue;
    last = kind;
    failed = kind === 'mine' && m.dispatchState === 'failed';
  }
  return last === 'mine' && !failed && (activity === 'working' || !!pending);
}
