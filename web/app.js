'use strict';

// M1 minimal UI (§9.2 behaviourally, zero styling). No client-side
// persistence anywhere (§12): the server state is the state.

const $ = (id) => document.getElementById(id);

const VISIBLE_POLL_MS = 45000; // §9.3

let config = null;

// §9.5: one feature-detect boolean, computed once, before any of the push
// machinery runs. A Firefox private window, an older desktop Safari and any
// non-secure origin fail at least one of these, and a naive register() call
// throws a TypeError that would take the rest of the load — the list
// included — with it. Losing push must cost push and nothing else.
const pushSupported =
  'serviceWorker' in navigator && 'PushManager' in window && 'Notification' in window;

// pushAvailable starts at pushSupported and goes false if registration
// itself fails; §9.8's notification slot reads it.
let pushAvailable = pushSupported;

// swRegistration holds the *promise* registerWorker returns, awaited
// wherever the registration is needed. Never call register a second time to
// "get" it (§9.5).
let swRegistration = null;

// §9.8: the slot's condition reads push_subscribed, so drawing it before the
// unconditional re-post has resolved would flash "Turn on notifications" on
// a healthy device that is in the middle of healing itself — an invitation
// to fix something already being fixed, on the one control whose whole
// design is to be absent when things work. The lists never wait; only the
// slot does.
let subscriptionSynced = false;

async function init() {
  // deliberately not awaited: registration must never sit between the load
  // and the first render (§9.5)
  swRegistration = registerWorker();

  let state;
  try {
    const res = await fetch('/api/state');
    if (res.status === 401) {
      showLogin();
      return;
    }
    if (!res.ok) throw new Error('state ' + res.status);
    state = await res.json();
    const cfgRes = await fetch('/api/config');
    if (!cfgRes.ok) throw new Error('config ' + cfgRes.status);
    config = await cfgRes.json();
  } catch {
    // a boot that fails must retry, not brick — full toasts are M3
    setTimeout(init, VISIBLE_POLL_MS);
    return;
  }
  renderChips();
  $('capture').hidden = false;
  $('overdue-zone').hidden = false;
  $('later-zone').hidden = false;
  renderState(state);
  setInterval(() => {
    if (!document.hidden) refreshState();
  }, VISIBLE_POLL_MS);

  await syncSubscription();
  subscriptionSynced = true;
  refreshState(); // the slot is decided by the state that follows (§9.8)
}

// --- service worker (§9.5) ---

// Registration is unconditional, on every load, and needs no gesture and no
// permission — Notification.requestPermission() and pushManager.subscribe()
// are the calls that do (§9.8). It is idempotent, so the repeat costs one
// no-op against the browser's registration store, and it is the only way a
// skipWaiting deploy reaches a tab that has been pinned for months. The
// registration object is kept: never call register a second time to "get" it.
async function registerWorker() {
  if (!pushSupported) return null;
  try {
    return await navigator.serviceWorker.register('/sw.js');
  } catch (err) {
    // a syntax error in sw.js, a hard-refresh race, a browser with workers
    // disabled by policy: nothing to retry and nothing the user can do. It
    // is caught rather than left to reject because an unhandled rejection on
    // the load path must not stop the poll from starting (§9.5).
    console.error('service worker registration failed', err);
    pushAvailable = false;
    return null;
  }
}

// --- login (§8.1) ---

function showLogin() {
  $('login').hidden = false;
  $('login-btn').addEventListener('click', submitToken);
  $('token').addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !e.isComposing) submitToken();
  });
}

async function submitToken() {
  const res = await fetch('/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ token: $('token').value }),
  });
  if (res.status === 204) {
    // the whole of the post-login path: a reload re-runs init under the
    // new cookie (§8.1)
    location.reload();
    return;
  }
  $('login-error').textContent = (await res.json()).error;
}

// --- capture bar ---

function renderChips() {
  const chips = $('chips');
  for (const p of config.presets) { // file order is UI order (§5.4)
    const b = document.createElement('button');
    b.textContent = p.label;
    b.disabled = true;
    b.addEventListener('click', () => createReminder(p.key));
    chips.append(b);
  }
  $('text').addEventListener('input', updateChips);
  $('text').addEventListener('keydown', (e) => {
    // isComposing: Enter that commits an IME composition is not a save
    if (e.key !== 'Enter' || e.isComposing) return;
    // the client resolves default_preset — the server has no fallback (§8.3)
    createReminder(config.default_preset);
  });
}

function updateChips() {
  const empty = $('text').value.trim() === '';
  for (const b of $('chips').children) b.disabled = empty;
}

async function createReminder(preset) {
  const text = $('text').value.trim();
  if (text === '') return;
  const res = await fetch('/api/reminders', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ text, preset }),
  });
  if (res.status === 201) {
    $('text').value = '';
    updateChips();
    refreshState(); // every successful mutation refetches (§8.2)
  }
}

// --- notification permission (§9.8) ---

// syncSubscription is the unconditional re-post: if this browser holds a
// local subscription, POST it on every load. It is an upsert, so it costs
// one request and needs no way to ask the server "do you know *this*
// endpoint" — push_subscribed is instance-wide (§8.2) and could never
// answer that. This is what restores a row the server deleted on a 410 or a
// 403 (§7.1) instead of leaving the app silently deaf.
async function syncSubscription() {
  const registration = await swRegistration;
  if (!registration) return;
  let sub;
  try {
    sub = await registration.pushManager.getSubscription();
    if (!sub) return;

    // Compare keys first, and post the key you compared. Re-posting a
    // subscription still bound to a retired keypair heals nothing — the
    // push service keeps rejecting it — and this is the client half of
    // making a rotation recoverable (§7.1).
    const bound = encodeKey(sub.options.applicationServerKey);
    if (bound !== null && bound !== config.vapid_public) {
      await sub.unsubscribe();
      sub = await subscribeWith(registration, config.vapid_public);
    }
    // A null readback is not a mismatch: every browser new enough to reach
    // this line populates options.applicationServerKey, and treating null
    // as a mismatch would churn unsubscribe/subscribe on a *working*
    // subscription on every page load — the one thing this re-post exists
    // to prevent (§9.8).
    await postSubscription(sub, config.vapid_public);
  } catch (err) {
    // the slot below is the recovery path, and it needs the load to survive
    console.error('subscription re-post failed', err);
  }
}

// renderNotifySlot draws §9.2's slot, whose steady state is absent: no
// permanent settings furniture. The conditions are §9.8's, in the order of
// what each can explain — a browser that cannot do this at all, an iPhone
// that needs installing first, a permission the user has blocked, and only
// then the button.
async function renderNotifySlot(pushSubscribed) {
  const slot = $('notify-slot');
  slot.replaceChildren();
  slot.hidden = true;

  if (iosNeedsHomeScreen()) {
    quietLine(slot, 'On iPhone, notifications need Nag on your Home Screen: Share, then Add to Home Screen.');
    return;
  }
  if (!pushAvailable) {
    quietLine(slot, 'This browser cannot show notifications for Nag. Everything else works.');
    return;
  }
  if (Notification.permission === 'denied') {
    // a denied permission cannot be re-prompted from script, and pretending
    // otherwise is worse than saying so (§9.8)
    quietLine(slot, 'Your browser is blocking notifications for Nag. Re-allow them in site settings to turn them back on.');
    return;
  }
  const registration = await swRegistration;
  const sub = registration ? await registration.pushManager.getSubscription() : null;
  if (Notification.permission === 'granted' && sub && pushSubscribed) return;

  const button = document.createElement('button');
  button.textContent = 'Turn on notifications';
  button.addEventListener('click', turnOnNotifications);
  slot.append(button);
  slot.hidden = false;
}

// turnOnNotifications runs on the click, because that is the gesture the
// permission prompt requires — Firefox rejects a request without one, which
// is why nothing about push is asked for on load (§9.8).
async function turnOnNotifications() {
  const registration = await swRegistration;
  if (!registration) return;
  try {
    if ((await Notification.requestPermission()) === 'granted') {
      const existing = await registration.pushManager.getSubscription();
      const sub = existing ?? (await subscribeWith(registration, config.vapid_public));
      await postSubscription(sub, config.vapid_public);
    }
  } catch (err) {
    console.error('subscribe failed', err);
  }
  // redraw either way: granted removes the slot, denied replaces it with the
  // quiet line
  refreshState();
}

function subscribeWith(registration, key) {
  // applicationServerKey is bytes, not the string (§9.8)
  return registration.pushManager.subscribe({
    userVisibleOnly: true,
    applicationServerKey: decodeKey(key),
  });
}

async function postSubscription(sub, key) {
  const json = sub.toJSON();
  await fetch('/api/push/subscribe', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      endpoint: json.endpoint,
      keys: { p256dh: json.keys.p256dh, auth: json.keys.auth },
      // the key this subscription is bound to, never the server's own value
      // — the column is only worth having if it records that (§7.1)
      application_server_key: key,
    }),
  });
}

// iOS exposes the Push API only to a Home-Screen install, and every iOS
// browser is WebKit, so this is not a Chrome-versus-Safari question (§7.1).
// It is checked before the unsupported line because on iOS the missing
// PushManager has a cause the user can act on.
function iosNeedsHomeScreen() {
  const ua = navigator.userAgent;
  const isIOS = /iPad|iPhone|iPod/.test(ua) || (/Macintosh/.test(ua) && navigator.maxTouchPoints > 1);
  const standalone =
    navigator.standalone === true || matchMedia('(display-mode: standalone)').matches;
  return isIOS && !standalone;
}

function quietLine(slot, text) {
  const line = document.createElement('p');
  line.textContent = text;
  slot.append(line);
  slot.hidden = false;
}

// decodeKey and encodeKey are the only encoding code in app.js, and they
// must be exact inverses over unpadded base64url — the alphabet
// webpush.GenerateVAPIDKeys produced (§5.2). Padding or standard base64 on
// either side turns the key comparison above into a false mismatch on a
// perfectly healthy install.
function decodeKey(key) {
  const b64 = key.replace(/-/g, '+').replace(/_/g, '/');
  const raw = atob(b64.padEnd(Math.ceil(b64.length / 4) * 4, '='));
  const bytes = new Uint8Array(raw.length);
  for (let i = 0; i < raw.length; i++) bytes[i] = raw.charCodeAt(i);
  return bytes;
}

function encodeKey(buffer) {
  if (!buffer) return null;
  let binary = '';
  for (const byte of new Uint8Array(buffer)) binary += String.fromCharCode(byte);
  return btoa(binary).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

// --- the two zones ---

async function refreshState() {
  const res = await fetch('/api/state');
  if (res.status === 401) {
    location.reload();
    return;
  }
  if (res.ok) renderState(await res.json());
}

function renderState(state) {
  $('overdue-count').textContent = state.overdue_count;
  renderZone($('overdue'), state.overdue);
  renderZone($('later'), state.later);
  if (subscriptionSynced) renderNotifySlot(state.push_subscribed);
}

function renderZone(ul, rows) {
  ul.replaceChildren();
  for (const r of rows) {
    const label = document.createElement('span');
    label.textContent = r.text + ' — ' + new Date(r.due_at * 1000).toLocaleString();
    const clear = document.createElement('button');
    clear.textContent = 'Clear';
    clear.addEventListener('click', async () => {
      await fetch('/api/reminders/' + r.id + '/done', { method: 'POST' });
      refreshState();
    });
    const li = document.createElement('li');
    li.append(label, ' ', clear);
    ul.append(li);
  }
}

init();
