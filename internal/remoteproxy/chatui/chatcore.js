// The chat page's logic that needs no browser: what a hub answer means, in
// which order messages show, and when to ask the hub again. chat.js does the
// DOM and the network. Tested with `node --test` (chatcore.test.mjs).
//
// Everything that comes from the hub is treated as data of unknown shape: a
// field that is not the expected type reads as absent, never as a fault.

// MAX_MESSAGE is the hub's own cap on one message (scion
// messages.MaxMessageLength), in characters.
export const MAX_MESSAGE = 16000;

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
// 'system' (a state line, shown small), or 'hidden' (a record the hub keeps
// for its own routing, or a message with nothing to show). An unknown type
// from the agent's side shows as an agent message rather than vanishing.
export function classify(m, userId) {
  const type = str(m && m.type);
  if (type === 'mention') return 'hidden';
  if (!messageText(m)) return 'hidden';
  if (type === 'state-change') return 'system';
  if (userId && str(m.senderId) === userId) return 'mine';
  return 'agent';
}

// isChatSubject reports whether an event subject is one of this user's own
// chat subjects. The page only uses it as a hint to read the history again,
// so a subject for anyone else is simply ignored.
export function isChatSubject(subject, userId) {
  return !!userId && str(subject).startsWith(`user.${userId}.chat.`);
}

// stateLine describes the agent for the header: {text, ok}. ok is false when
// a message sent now would not reach a running agent.
export function stateLine(agent) {
  if (!agent || typeof agent !== 'object') return { text: 'state unknown', ok: true };
  const phase = str(agent.phase).toLowerCase();
  const activity = str(agent.activity).toLowerCase();
  if (phase === 'running' || phase === 'resumed') {
    return { text: activity ? activity.replaceAll('_', ' ') : 'running', ok: true };
  }
  if (!phase) return { text: 'state unknown', ok: true };
  return { text: `${phase} (not running: start it with lever up)`, ok: false };
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
