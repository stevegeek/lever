// lever's push-only service worker (remote.push).
//
// It shows a notification for a push and opens the chat on a tap. It has
// NO fetch handler and keeps no cache, so the page's requests never pass
// through it and it cannot serve a page older than the binary. A push from
// lever carries only {"v":1,"agent":"<name>"}: there is no message text to
// show. The name is shown only if this login's agent list (the one the page
// shows) lists it as an agent it may message.

const NAME = /^[a-z0-9][a-z0-9-]{0,62}$/;
const LIST_MS = 5000;

function agentName(data) {
  let v = null;
  try {
    v = data ? data.json() : null;
  } catch {
    v = null;
  }
  return v && v.v === 1 && typeof v.agent === 'string' && NAME.test(v.agent) ? v.agent : '';
}

async function listed(agent) {
  const limit = new AbortController();
  const timer = setTimeout(() => limit.abort(), LIST_MS);
  try {
    const res = await fetch('/lever/api/agents', { credentials: 'same-origin', redirect: 'error', signal: limit.signal, headers: { Accept: 'application/json' } });
    if (!res.ok) return false;
    const body = await res.json();
    return Array.isArray(body && body.agents) && body.agents.some((a) => a && a.name === agent && a.access === 'message');
  } catch {
    return false;
  } finally {
    clearTimeout(timer);
  }
}

self.addEventListener('push', (event) => {
  event.waitUntil((async () => {
    let agent = agentName(event.data);
    if (agent && !(await listed(agent))) agent = '';
    await self.registration.showNotification(agent ? `New message from ${agent}` : 'New message', {
      tag: agent || 'lever',
      data: { agent },
      icon: '/lever/icon-192.png',
    });
  })());
});

self.addEventListener('notificationclick', (event) => {
  event.notification.close();
  const d = event.notification.data;
  const agent = d && typeof d.agent === 'string' && NAME.test(d.agent) ? d.agent : '';
  event.waitUntil((async () => {
    const wins = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
    const open = wins.find((c) => {
      try {
        return new URL(c.url).pathname === '/lever/chat';
      } catch {
        return false;
      }
    });
    if (open) {
      if (agent) open.postMessage({ agent });
      return open.focus();
    }
    return self.clients.openWindow(agent ? `/lever/chat#agent=${agent}` : '/lever/chat');
  })());
});
