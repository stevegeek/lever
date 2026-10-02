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
// agent's message: any type from the agent's side but a state line shows as
// an agent message.
export function classify(m, userId) {
  const type = str(m && m.type);
  if (!messageText(m)) return 'hidden';
  if (userId && str(m.senderId) === userId) return type === 'mention' ? 'hidden' : 'mine';
  return type === 'state-change' || type === 'system' ? 'system' : 'agent';
}

// isChatSubject reports whether an event subject is one of this user's own
// chat subjects. The page only uses it as a hint to read the history again,
// so a subject for anyone else is simply ignored.
export function isChatSubject(subject, userId) {
  return !!userId && str(subject).startsWith(`user.${userId}.chat.`);
}

// The hub's agent states (scion pkg/agent/state): the phases before
// "running", the phases after it, the activities of a running agent, and
// those that mean it is not answering.
const STARTING = new Set(['created', 'provisioning', 'cloning', 'starting']);
const STOPPED = new Set(['suspended', 'stopping', 'stopped', 'error']);
const ACTIVE = new Set(['working', 'thinking', 'executing', 'waiting_for_input', 'blocked', 'completed']);
const NOT_ANSWERING = new Set(['offline', 'crashed', 'stalled', 'limits_exceeded']);

const label = (s) => s.replaceAll('_', ' ');

// stateLine describes the agent for the header: {text, ok}. ok is false when
// a message sent now would likely get no answer.
//
// An agent reports its own phase and activity to the hub, as free text. So
// only the words above are ever shown: a value that is not one of them shows
// as unknown, and an agent cannot write a line of its own into the header.
export function stateLine(agent) {
  const unknown = { text: 'state unknown', ok: true };
  if (!agent || typeof agent !== 'object') return unknown;
  const phase = str(agent.phase).toLowerCase();
  const activity = str(agent.activity).toLowerCase();
  // "resumed" is what the hub reports for a short time after a resume.
  if (phase === 'running' || phase === 'resumed') {
    if (NOT_ANSWERING.has(activity)) return { text: `${label(activity)} (it may not answer)`, ok: false };
    return { text: ACTIVE.has(activity) ? label(activity) : 'running', ok: true };
  }
  if (STARTING.has(phase)) return { text: `${phase} (a message waits until it runs)`, ok: true };
  if (STOPPED.has(phase)) return { text: `${phase} (not running: start it with lever up)`, ok: false };
  return unknown;
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
  msg = msg.trim().slice(0, 300);
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
