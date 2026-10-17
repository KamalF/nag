'use strict';

// The Nag service worker (§9.5). It exists for three events: push,
// notificationclick and pushsubscriptionchange. It never intercepts
// navigation: no fetch handler and no cache. Offline support is not a goal.
// A cache here is the fastest way to serve a year-old app.js to a tab pinned
// for months.

// tag is constant, to match the Topic of the server (§7.1). The tray holds at
// most one Nag notification, because the tray is a nudge and the count lives
// on the badge (§9.3). renotify goes with it. In Chrome, a notification that
// replaces another under an existing tag is silent by default. Without
// renotify, the second digest of the morning arrives with no sound and no
// banner, and looks exactly like a bug.
const TAG = 'nag';

// Two assets for two slots (§9.3). Platforms render `icon` at 64–192 device
// pixels, and they upscale a 32-pixel source into a blurry square. `badge` is
// the small status-bar glyph, and 32 is exactly right for it.
const ICON = '/icons/192.png';
const BADGE = '/icons/32.png';

// A new worker takes over on the next load instead of waiting for every tab
// to close (§9.5).
self.addEventListener('install', () => self.skipWaiting());
self.addEventListener('activate', (event) => event.waitUntil(self.clients.claim()));

self.addEventListener('push', (event) => {
  // waitUntil covers the draw and the fan-out together, and it is not
  // optional. showNotification returns a promise. A handler that returns
  // before the promise settles tells the browser that the event is finished.
  // The browser is then free to kill the worker mid-draw. The visible outcome
  // is a push that arrives and shows nothing. Browsers answer such a silent
  // push by revoking the subscription (§7.1).
  event.waitUntil(handlePush(event));
});

async function handlePush(event) {
  const drawn = draw(parsePayload(event));
  const options = { tag: TAG, renotify: true, icon: ICON, badge: BADGE };
  if (drawn.body) options.body = drawn.body;
  // read the field, never infer it from the count: the test push is n == 1
  // and points at no row at all (§7.1, §7.5)
  if (drawn.id !== undefined) options.data = { id: drawn.id };

  // exactly one notification whatever n says. A loop over texts undoes the
  // batching that the server just did (§9.5).
  await self.registration.showNotification(drawn.title, options);

  // an open-but-hidden tab updates its badge at once, not up to five minutes
  // later (§9.3)
  const clients = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
  for (const client of clients) client.postMessage({ type: 'state-changed' });
}

// parsePayload never throws. Wrap the parse, do not guard the call. An early
// return from a push is what gets a subscription revoked. It also hands the
// user the generic "site updated in the background" notice of the browser.
// That notice is strictly worse than a vague one of ours (§9.5).
function parsePayload(event) {
  if (!event.data) return null;
  try {
    return event.data.json();
  } catch {
    return null;
  }
}

// draw is the payload table of §9.5, plus the fallback for a payload that
// carries nothing usable.
function draw(p) {
  if (!p || !Number.isInteger(p.n) || p.n < 1 || !Array.isArray(p.texts) || p.texts.length === 0) {
    return { title: 'Nag reminder' };
  }
  if (p.n === 1) {
    // the title is the reminder itself. The server already link-flattens and
    // length-caps the texts (§7.1), so there is no markdown code and no
    // truncation logic here.
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

// whenText is the short form of §9.11. It is the worker copy of the table in
// app.js, because sw.js cannot import from app.js. A Go test keeps the two
// byte-identical: thresholds and units are exactly what drifts silently
// between copies. The worker has no server offset (there is no page to hold
// one), so its now() is the device clock. The notification is a nudge, and
// the list that it points at is correct either way (§9.11).
const clockFormat = new Intl.DateTimeFormat(undefined, { hour: 'numeric', minute: '2-digit' });
const weekdayFormat = new Intl.DateTimeFormat(undefined, { weekday: 'long' });
const dayMonthFormat = new Intl.DateTimeFormat(undefined, { day: 'numeric', month: 'long' });
const dayMonthYearFormat = new Intl.DateTimeFormat(undefined, { day: 'numeric', month: 'long', year: 'numeric' });

const now = () => Math.floor(Date.now() / 1000);

function whenText(dueAt) {
  const d = dueAt - now();
  if (d <= -86400) return count(Math.floor(-d / 86400), 'day') + ' late';
  if (d <= -3600) return Math.floor(-d / 3600) + ' h late';
  if (d <= -60) return Math.floor(-d / 60) + ' min late';
  if (d < 60) return 'now';
  if (d < 3600) return 'in ' + count(Math.floor(d / 60), 'minute');
  const due = new Date(dueAt * 1000);
  const today = new Date(now() * 1000);
  const days = calendarDaysBetween(today, due);
  const time = clockFormat.format(due);
  if (days === 0) return 'today at ' + time;
  if (days === 1) return 'tomorrow at ' + time;
  if (days <= 6) return weekdayFormat.format(due) + ' at ' + time;
  if (due.getFullYear() === today.getFullYear()) return dayMonthFormat.format(due) + ' at ' + time;
  return dayMonthYearFormat.format(due) + ' at ' + time;
}

function calendarDaysBetween(from, to) {
  const utc = (d) => Date.UTC(d.getFullYear(), d.getMonth(), d.getDate());
  return Math.round((utc(to) - utc(from)) / 86400000);
}

function count(n, unit) {
  return n + ' ' + unit + (n === 1 ? '' : 's');
}

self.addEventListener('notificationclick', (event) => {
  // a dismissal does nothing and has no handler. A notification swiped away
  // leaves the reminder overdue, which is the whole point (§9.5).
  event.notification.close();
  event.waitUntil(focusOrOpen(event.notification.data?.id));
});

// focusOrOpen is the asymmetric pair of §9.5. An existing client is focused
// and told by postMessage. It already listens, and a navigation moves the
// page out from under whatever you were doing. A new window carries the id in
// its URL instead. A postMessage to a window that openWindow just returned is
// a race that the worker always loses: app.js is not parsed yet, so no
// message listener exists. And that is the common case, because a
// notification that you click usually arrived with the tab closed.
async function focusOrOpen(id) {
  // includeUncontrolled: without it, the search does not see a tab loaded
  // before this worker took over. That tab then gets a second window opened
  // beside it.
  const clients = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
  if (clients.length > 0) {
    await clients[0].focus();
    // no id (a digest, or a test push) means that there is no single row to
    // scroll to. To pick the first row flashes one arbitrary row while the
    // others sit unhighlighted below it (§9.5).
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
// of "push worked for a month and then stopped". Browsers rotate
// subscriptions on their own schedule. The key comes from GET /api/config: a
// worker fetch is same-origin and carries the session cookie. The key does
// not come from event.oldSubscription. That field is not reliably populated
// across browsers, and it can hold a key that was rotated away since. On any
// failure, do nothing. The unconditional re-post of §9.8 heals it the next
// time the page opens, and there is nothing useful to do in a worker with no
// credentials. This is the one handler that legitimately talks to the
// server, and it is not on the notification-drawing path.
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
        // the key that this subscription is bound to, which makes a
        // rotation recoverable (§7.1)
        application_server_key: key,
      }),
    });
    // §9.5 says to do nothing on failure and let the page-load re-post heal
    // it. But silence here is indistinguishable from a rotation that worked.
    // The stale row keeps failing until a 404/410 retires it, and this line
    // is the only thing that can ever explain that.
    if (!res.ok) console.error('pushsubscriptionchange: subscribe post rejected', res.status);
  } catch (err) {
    console.error('pushsubscriptionchange: re-subscribe failed', err);
  }
}

// base64UrlToBytes is the exact inverse of the unpadded base64url of the
// server (§5.2). applicationServerKey takes bytes, and atob takes padded
// standard base64.
function base64UrlToBytes(s) {
  const b64 = s.replace(/-/g, '+').replace(/_/g, '/');
  const raw = atob(b64.padEnd(Math.ceil(b64.length / 4) * 4, '='));
  const bytes = new Uint8Array(raw.length);
  for (let i = 0; i < raw.length; i++) bytes[i] = raw.charCodeAt(i);
  return bytes;
}
