'use strict';

// Nag's service worker (§9.5). It exists for three events — push,
// notificationclick, pushsubscriptionchange — and it never intercepts
// navigation: no fetch handler and no cache. Offline support is not a goal,
// and a cache here is the fastest way to serve a year-old app.js to a tab
// that has been pinned for months.

// tag is constant, matching the server's Topic (§7.1): the tray holds at
// most one Nag notification, because the tray is a nudge and the count lives
// on the badge (§9.3). renotify goes with it — replacing a notification
// under an existing tag is silent by default in Chrome, so without it the
// second digest of the morning arrives with no sound and no banner, looking
// exactly like a bug.
const TAG = 'nag';

// Two assets for two slots (§9.3): platforms render `icon` at 64–192 device
// pixels and would upscale a 32-pixel source into a blurry square, while
// `badge` is the small status-bar glyph that 32 is exactly right for.
const ICON = '/icons/192.png';
const BADGE = '/icons/32.png';

// A new worker takes over on the next load instead of waiting for every tab
// to close (§9.5).
self.addEventListener('install', () => self.skipWaiting());
self.addEventListener('activate', (event) => event.waitUntil(self.clients.claim()));

self.addEventListener('push', (event) => {
  // waitUntil covers the draw and the fan-out together, and it is not
  // optional: showNotification returns a promise, and a handler that returns
  // before it settles tells the browser the event is finished — which frees
  // it to kill the worker mid-draw. The visible outcome is a push that
  // arrives and shows nothing, which is the silent push browsers answer by
  // revoking the subscription (§7.1).
  event.waitUntil(handlePush(event));
});

async function handlePush(event) {
  const drawn = draw(parsePayload(event));
  const options = { tag: TAG, renotify: true, icon: ICON, badge: BADGE };
  if (drawn.body) options.body = drawn.body;
  // read the field, never infer it from the count: the test push is n == 1
  // and points at no row at all (§7.1, §7.5)
  if (drawn.id !== undefined) options.data = { id: drawn.id };

  // exactly one notification whatever n says — looping over texts would undo
  // the batching the server just did (§9.5)
  await self.registration.showNotification(drawn.title, options);

  // an open-but-hidden tab updates its badge at once rather than up to five
  // minutes later (§9.3)
  const clients = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
  for (const client of clients) client.postMessage({ type: 'state-changed' });
}

// parsePayload never throws. Wrap the parse, don't guard the call: returning
// early from a push is what gets a subscription revoked, and it hands the
// user the browser's own generic "site updated in the background" notice,
// which is strictly worse than a vague one of ours (§9.5).
function parsePayload(event) {
  if (!event.data) return null;
  try {
    return event.data.json();
  } catch {
    return null;
  }
}

// draw is §9.5's payload table, plus the fallback for a payload that carries
// nothing usable.
function draw(p) {
  if (!p || !Number.isInteger(p.n) || p.n < 1 || !Array.isArray(p.texts) || p.texts.length === 0) {
    return { title: 'Nag reminder' };
  }
  if (p.n === 1) {
    // the title is the reminder itself; texts arrive already link-flattened
    // and length-capped by the server (§7.1), so there is no markdown code
    // and no truncation logic here
    return {
      title: p.texts[0],
      body: typeof p.due_at === 'number' ? whenText(p.due_at) : undefined,
      id: p.id,
    };
  }
  let body = p.texts.join(' · ');
  const more = p.n - p.texts.length;
  if (more > 0) body += ' · +' + more + ' more';
  return { title: p.n + ' reminders need you', body };
}

// whenText is a provisional §9.11 short form: the three duration rows, and a
// locale-formatted date for everything below them. M3 replaces it with the
// full table — here and in app.js separately, since sw.js cannot import from
// app.js. The worker has no server offset (there is no page to hold one), so
// its now() is the device clock; it is a nudge, and the list it points at is
// correct either way (§9.11).
function whenText(dueAt) {
  const d = dueAt - Math.floor(Date.now() / 1000);
  if (d <= -86400) return count(Math.floor(-d / 86400), 'day') + ' late';
  if (d <= -3600) return Math.floor(-d / 3600) + ' h late';
  if (d <= -60) return Math.floor(-d / 60) + ' min late';
  if (d < 60) return 'now';
  if (d < 3600) return 'in ' + count(Math.floor(d / 60), 'minute');
  return new Date(dueAt * 1000).toLocaleString();
}

function count(n, unit) {
  return n + ' ' + unit + (n === 1 ? '' : 's');
}

self.addEventListener('notificationclick', (event) => {
  // dismissing does nothing and has no handler — swiping a notification away
  // leaves the reminder overdue, which is the whole point (§9.5)
  event.notification.close();
  event.waitUntil(focusOrOpen(event.notification.data?.id));
});

// focusOrOpen is §9.5's asymmetric pair. An existing client is focused and
// told by postMessage: it is already listening, and navigating it would move
// the page out from under whatever you were doing. A new window carries the
// id in its URL instead, because postMessage to a window openWindow just
// returned is a race the worker always loses — app.js has not parsed yet, so
// no message listener exists — and that is the common case, since a
// notification you click usually arrived with the tab closed.
async function focusOrOpen(id) {
  // includeUncontrolled: a tab loaded before this worker took over is
  // invisible to the search without it, and gets a second window opened
  // beside it
  const clients = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
  if (clients.length > 0) {
    await clients[0].focus();
    // no id — a digest, or a test push — means there is no single row to
    // scroll to, and picking the first would flash one arbitrary row while
    // the others sit unhighlighted below it (§9.5)
    clients[0].postMessage(
      id === undefined ? { type: 'state-changed' } : { type: 'focus-reminder', id }
    );
    return;
  }
  await self.clients.openWindow(id === undefined ? '/' : '/?focus=' + encodeURIComponent(id));
}

self.addEventListener('pushsubscriptionchange', (event) => {
  event.waitUntil(resubscribe());
});

// resubscribe is the handler whose absence is the single most common cause
// of "push worked for a month and then stopped": browsers rotate
// subscriptions on their own schedule. The key comes from GET /api/config —
// a worker fetch is same-origin and carries the session cookie — rather than
// from event.oldSubscription, which is not reliably populated across
// browsers and would re-subscribe under a key that may since have been
// rotated away. On any failure, do nothing: §9.8's unconditional re-post
// heals it the next time the page opens, and there is nothing useful to do
// in a worker with no credentials. This is the one handler that legitimately
// talks to the server, and it is not on the notification-drawing path.
async function resubscribe() {
  try {
    const configRes = await fetch('/api/config');
    if (!configRes.ok) return;
    const key = (await configRes.json()).vapid_public;

    const sub = await self.registration.pushManager.subscribe({
      userVisibleOnly: true,
      applicationServerKey: base64UrlToBytes(key),
    });
    const json = sub.toJSON();
    const res = await fetch('/api/push/subscribe', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        endpoint: json.endpoint,
        keys: { p256dh: json.keys.p256dh, auth: json.keys.auth },
        // the key this subscription is bound to, which is what makes a
        // rotation recoverable (§7.1)
        application_server_key: key,
      }),
    });
    // §9.5 says do nothing on failure and let the page-load re-post heal it,
    // but silence here is indistinguishable from a rotation that worked: the
    // stale row keeps failing until a 404/410 retires it, and this line is
    // the only thing that would ever explain that.
    if (!res.ok) console.error('pushsubscriptionchange: subscribe post rejected', res.status);
  } catch (err) {
    console.error('pushsubscriptionchange: re-subscribe failed', err);
  }
}

// base64UrlToBytes is the exact inverse of the server's unpadded base64url
// (§5.2): applicationServerKey takes bytes, and atob takes padded standard
// base64.
function base64UrlToBytes(s) {
  const b64 = s.replace(/-/g, '+').replace(/_/g, '/');
  const raw = atob(b64.padEnd(Math.ceil(b64.length / 4) * 4, '='));
  const bytes = new Uint8Array(raw.length);
  for (let i = 0; i < raw.length; i++) bytes[i] = raw.charCodeAt(i);
  return bytes;
}
