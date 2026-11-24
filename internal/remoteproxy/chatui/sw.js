// lever's push-only service worker (remote.push). It has NO fetch handler
// and keeps no cache, so the page's requests never pass through it and it
// cannot serve a stale page. A push carries only {"v":1,"agent":"<name>"},
// and "kind":"waiting" when the agent waits at its prompt; the name is shown
// only if this login's agent list lets it message that agent.

const NAME = /^[a-z0-9][a-z0-9-]{0,62}$/;
const LIST_MS = 5000;

function parsePush(data) {
  let v = null;
  try {
    v = data ? data.json() : null;
  } catch {
    v = null;
  }
  const agent = v && v.v === 1 && typeof v.agent === 'string' && NAME.test(v.agent) ? v.agent : '';
  return { agent, waiting: !!v && v.kind === 'waiting' };
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
    const p = parsePush(event.data);
    let agent = p.agent;
    if (agent && !(await listed(agent))) agent = '';
    let title = agent ? `New message from ${agent}` : 'New message';
    if (p.waiting) title = agent ? `${agent} is waiting for you` : 'An agent is waiting for you';
    await self.registration.showNotification(title, {
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
