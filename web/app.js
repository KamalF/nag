'use strict';

// M1 minimal UI (§9.2 behaviourally, zero styling). No client-side
// persistence anywhere (§12): the server state is the state.

const $ = (id) => document.getElementById(id);

// §9.3's three cadences and the flash. 45 s is for the tab in front of you;
// 5 minutes is what keeps a pinned tab's favicon honest without polling on
// behalf of a machine nobody is using — suspending a hidden tab entirely
// would break Goal 4 outright, since a tab you are reading the favicon of is
// hidden by definition.
const VISIBLE_POLL_MS = 45000;
const HIDDEN_POLL_MS = 300000;
const FLASH_MS = 2400;

// How long a click's request waits for the render that shows its row. It is
// not the highlight's own duration: the row may take several passes to
// arrive, and a highlight that had already expired by then would make a slow
// connection look identical to a reminder that no longer exists.
const FLASH_WAIT_MS = 15000;

// setTimeout truncates its delay to a signed 32-bit integer, so anything
// past ~24.8 days wraps and fires immediately (§9.3).
const MAX_TIMER_MS = 2000000000;

// §9.3 pins the drawn favicon, and the pair of colours is hardcoded rather
// than read from a custom property on purpose: a tab strip is not a page,
// the dot has to stay legible against a light and a dark browser chrome,
// and no media query reports which one Firefox painted. BASE_TITLE is read
// from a constant for the same class of reason — reading document.title
// would compound the prefix into "(3) (2) Nag" on the second call.
const BADGE_FILL = '#E0A33E';
const FLASH_FILL = '#E0A33E';
const BADGE_INK = '#212A2E';
const BASE_TITLE = 'Nag';
const PLAIN_FAVICON = '/icons/32.png';

let config = null;

// The poll's state. stateETag and lastState are a pair: the 304 path
// re-renders from the list already in memory, so an ETag stored without the
// body it describes would outlive the wrong render (§9.3).
let stateETag = null;
let lastState = null;

// clockOffsetMs is the server's clock minus this device's. now() is pinned
// to seconds, matching every timestamp the API sends (§8.2).
let clockOffsetMs = 0;

let stateAbort = null;
let dueTimer = null;
let pollTimer = null;

// A flash is a request, not a style. renderZone rebuilds every row on every
// render and a 304 re-renders routinely (§9.3), so the request has to outlive
// the element — and the row a click names may not have been drawn yet at all,
// since an aborted refresh resolves having rendered nothing. pendingFlash is
// the request waiting for its row; litRow is what is currently painted, so a
// rebuild can restore it and clearFlash can find it. M3's keyed render
// (commit 33) is what eventually makes the rebuild unnecessary.
let pendingFlash = null;
let litRow = null;
let flashTimer = null;

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

// repostFailed remembers this load's re-post outcome. push_subscribed is
// instance-wide (§8.2), so on a laptop whose row the server dropped and
// whose re-post then failed, the flag still reads true because the phone is
// subscribed — and the slot would report that everything is fine while this
// device is deaf. That is the outcome §9.8's re-post exists to prevent, so
// its own result is the signal.
let repostFailed = false;

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
    stateETag = res.headers.get('ETag');
    lastState = state;
    syncClock(res, state);
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
  armPoll();
  document.addEventListener('visibilitychange', onVisibilityChange);
  if (pushSupported) {
    navigator.serviceWorker.addEventListener('message', (event) => handleWorkerMessage(event.data));
  }
  consumeFocusParam();

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
    const registration = await navigator.serviceWorker.register('/sw.js');
    // init retries itself on a failed boot, and a deploy fails the state
    // fetch and the worker script fetch together — so this has to be able to
    // recover, or one unlucky first load leaves the slot reading "this
    // browser cannot show notifications" on a browser that can (§9.8)
    pushAvailable = true;
    return registration;
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
  try {
    const sub = await registration.pushManager.getSubscription();
    if (!sub) return;
    await postSubscription(await boundToCurrentKey(registration, sub), config.vapid_public);
    repostFailed = false;
  } catch (err) {
    // the slot is the recovery path, and it needs the load to survive
    repostFailed = true;
    console.error('subscription re-post failed', err);
  }
}

// renderNotifySlot draws §9.2's slot, whose steady state is absent: no
// permanent settings furniture. It runs on every render, so it rebuilds only
// when the answer actually changed — otherwise the button is destroyed and
// re-created every 45 seconds, which drops the focus of a keyboard user and
// swallows a click that lands mid-await, on the one control designed to be
// invisible when things work.
let slotGeneration = 0;
let slotShowing = null;

async function renderNotifySlot(pushSubscribed) {
  const generation = ++slotGeneration;
  const want = await desiredSlot(pushSubscribed);
  // a newer render resolved while this one was awaiting: it owns the slot,
  // and without this both would clear and both would append
  if (generation !== slotGeneration || want === slotShowing) return;
  slotShowing = want;

  const slot = $('notify-slot');
  slot.replaceChildren();
  slot.hidden = true;
  if (want === 'absent') return;
  if (want === 'button') {
    const button = document.createElement('button');
    button.textContent = 'Turn on notifications';
    button.addEventListener('click', turnOnNotifications);
    slot.append(button);
    slot.hidden = false;
    return;
  }
  quietLine(slot, SLOT_LINES[want]);
}

// The conditions of §9.8, in the order of what each can explain: a browser
// that cannot do this at all, an iPhone that needs installing first, a
// permission the user has blocked, and only then the button. push_subscribed
// is the belt to the other two braces — with the re-post awaited before the
// slot is decided it is almost always redundant, and what it still catches is
// the re-post having failed, or a subscription this browser holds that no
// longer exists server-side.
async function desiredSlot(pushSubscribed) {
  if (iosNeedsHomeScreen()) return 'ios';
  if (!pushAvailable) return 'unsupported';
  if (Notification.permission === 'denied') return 'denied';
  try {
    const registration = await swRegistration;
    const sub = registration ? await registration.pushManager.getSubscription() : null;
    if (Notification.permission === 'granted' && sub && pushSubscribed && !repostFailed) {
      return 'absent';
    }
    return 'button';
  } catch {
    // getSubscription rejects rather than resolving null where push is
    // disabled by policy or its service is unreachable. Unhandled, it
    // rejects a render pass every 45 s and leaves the slot frozen — no
    // button, no line, nothing to act on — so it lands on the same quiet
    // line as an unsupported browser (§9.5).
    return 'unsupported';
  }
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
      // an existing subscription goes through the same key check as the
      // re-post: posting one nobody compared is what poisons the row
      const sub = existing
        ? await boundToCurrentKey(registration, existing)
        : await subscribeWith(registration, config.vapid_public);
      await postSubscription(sub, config.vapid_public);
      repostFailed = false;
    }
  } catch (err) {
    repostFailed = true;
    console.error('subscribe failed', err);
  }
  // redraw either way: granted removes the slot, denied replaces it with the
  // quiet line
  refreshState();
}

// boundToCurrentKey is §9.8's "compare keys first, and post the key you
// compared", and both entry points go through it. Posting a subscription
// nobody examined under the configured key makes the row's vapid_public
// equal to it by construction, which sends §7.1's 403 rung down its
// "matching, so it is a bug" branch forever: the row is kept, an ERROR is
// logged on every sweep, the boot mismatch WARNING is blinded, and the slot
// reports that everything is fine.
async function boundToCurrentKey(registration, sub) {
  const bound = encodeKey(sub.options.applicationServerKey);
  // A null readback is not a mismatch: every browser new enough to reach
  // this line populates options.applicationServerKey, and treating null as a
  // mismatch would churn unsubscribe/subscribe on a *working* subscription
  // on every page load — the one thing this exists to prevent (§9.8).
  if (bound === null || bound === config.vapid_public) return sub;
  // re-posting a subscription still bound to a retired keypair heals
  // nothing: the push service keeps rejecting it (§7.1)
  await sub.unsubscribe();
  return subscribeWith(registration, config.vapid_public);
}

async function subscribeWith(registration, key) {
  // register() resolves as soon as the registration exists, but subscribe()
  // rejects with InvalidStateError while the worker is still installing —
  // which is exactly the first-ever visit, the one where the button is
  // showing and pressing it is the whole intent. `ready` waits for the
  // active worker and registers nothing (§9.5).
  await navigator.serviceWorker.ready;
  // applicationServerKey is bytes, not the string (§9.8)
  return registration.pushManager.subscribe({
    userVisibleOnly: true,
    applicationServerKey: decodeKey(key),
  });
}

// postSubscription throws on any non-2xx. fetch resolves for a 400 or a
// 500, so an unchecked call cannot tell a stored subscription from a
// rejected one — and §8.2 answers 400 on every shape check and 500 on a
// store error, all of which mean this device is not subscribed.
async function postSubscription(sub, key) {
  const json = sub.toJSON();
  const res = await fetch('/api/push/subscribe', {
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
  if (!res.ok) throw new Error('subscribe ' + res.status);
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

// A denied permission cannot be re-prompted from script, and pretending
// otherwise is worse than saying so (§9.8).
const SLOT_LINES = {
  ios: 'On iPhone, notifications need Nag on your Home Screen: Share, then Add to Home Screen.',
  unsupported: 'This browser cannot show notifications for Nag. Everything else works.',
  denied:
    'Your browser is blocking notifications for Nag. Re-allow them in site settings to turn them back on.',
};

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

// refreshState is the only reader of /api/state, and every trigger goes
// through it: the two cadences, visibilitychange, the due timer, a worker
// message, and every successful mutation. Those overlap by construction — a
// mutation's refetch lands on top of a poll routinely — and nothing orders
// two responses to the same endpoint, so one AbortController makes the
// newest request the only one that can write anything. An aborted fetch
// updates nothing at all: not the list, not the badge, not the clock offset,
// and not the stored ETag (§9.3).
async function refreshState() {
  stateAbort?.abort();
  const controller = new AbortController();
  stateAbort = controller;

  let res;
  try {
    res = await fetch('/api/state', {
      signal: controller.signal,
      // the ETag lives in a variable here, so a stale response overwriting
      // it would outlive the wrong render entirely (§9.3)
      headers: stateETag ? { 'If-None-Match': stateETag } : {},
    });
  } catch {
    return; // aborted by a newer request, or offline: the next trigger tries again
  }
  if (controller !== stateAbort) return;
  if (res.status === 401) {
    location.reload();
    return;
  }
  if (res.status === 304) {
    // The ETag excludes server_time so the steady-state poll is cheap (§8.2),
    // which makes this the common case: no body, while every time string on
    // screen has nevertheless moved. Returning early here would freeze them
    // until the data happens to change, which on a quiet afternoon is hours.
    syncClock(res, null);
    if (lastState) renderState(lastState);
    return;
  }
  if (!res.ok) return;

  let body;
  try {
    body = await res.json();
  } catch {
    // aborted between the headers and the end of the body, or a truncated
    // response: this request writes nothing and the next trigger tries again.
    // Unguarded, it would reject refreshState itself — and every caller
    // (both timers, and the awaited one in handleWorkerMessage) either
    // becomes an unhandled rejection or silently loses what follows it.
    return;
  }
  if (controller !== stateAbort) return;
  stateETag = res.headers.get('ETag');
  lastState = body;
  syncClock(res, body);
  renderState(body);
}

// syncClock prefers the body's server_time and falls back to the Date
// header, which is all a 304 carries (§9.3).
function syncClock(res, state) {
  const serverMs = state ? state.server_time * 1000 : Date.parse(res.headers.get('Date'));
  if (!Number.isNaN(serverMs) && serverMs) clockOffsetMs = serverMs - Date.now();
}

// now() is the skew-corrected clock every time string reads (§9.11), pinned
// to seconds so it compares directly against due_at.
function now() {
  return Math.floor((Date.now() + clockOffsetMs) / 1000);
}

// armPoll switches cadence rather than suspending: see the constants above.
function armPoll() {
  clearInterval(pollTimer);
  pollTimer = setInterval(refreshState, document.hidden ? HIDDEN_POLL_MS : VISIBLE_POLL_MS);
}

function onVisibilityChange() {
  armPoll();
  if (!document.hidden) refreshState();
}

// armDueTimer is the one-shot timer of §9.3, re-armed on every render from
// the first row of `later` — the list is server-sorted (§8.2). The
// overdue/later split is server-computed and never recomputed here, so
// without it a row that comes due sits reading "now" for up to 45 seconds
// while the badge disagrees with it. It is armed regardless of visibility:
// browsers throttle background timers to roughly one a minute, which
// degrades it into a slightly-late poll rather than a missed one.
function armDueTimer(state) {
  clearTimeout(dueTimer);
  const next = state.later[0];
  if (!next) return;
  // Without the clamp, one reminder six months out fires the timer at once,
  // polls, re-arms from the same first element and fires again — a tight
  // loop against the server for as long as the tab is open (§9.3).
  const delay = Math.min((next.due_at - now()) * 1000, MAX_TIMER_MS);
  dueTimer = setTimeout(refreshState, Math.max(delay, 0));
}

function renderState(state) {
  $('overdue-count').textContent = state.overdue_count;
  renderZone($('overdue'), state.overdue);
  renderZone($('later'), state.later);
  // driven by the same render pass as the lists, which is what keeps all
  // three outputs correct after a 304 too (§9.3)
  setBadge(state.overdue_count);
  applyPendingFlash();
  armDueTimer(state);
  if (subscriptionSynced) renderNotifySlot(state.push_subscribed);
}

// --- badge (§9.3) ---

// setBadge writes the overdue count — never the total — to all three
// outputs: the Badging API where it exists, a canvas-drawn favicon
// everywhere, and the title prefix. Upcoming items must never light it up;
// that is exactly what makes an icon badge mean something. setBadge(0) is a
// full reset rather than a no-op.
function setBadge(count) {
  if ('setAppBadge' in navigator) {
    // clearAppBadge, not setAppBadge(0): Firefox has no Badging API on any
    // platform, so the other two outputs are the ones that always run
    const badged = count > 0 ? navigator.setAppBadge(count) : navigator.clearAppBadge();
    badged?.catch(() => {});
  }
  document.title = count > 0 ? `(${count}) ${BASE_TITLE}` : BASE_TITLE;
  drawFavicon(count);
}

// drawFavicon paints 32.png at full size and, above zero, a filled circle in
// the bottom-right quadrant with the count in it. The numeral is the count
// up to 9 and the literal 9+ above it, at which point the dot means
// "several" — which is all a 12-pixel circle can ever mean, and the exact
// number is in the title and the list.
let badgeDrawn = null;
let badgeGeneration = 0;

function drawFavicon(count) {
  const link = document.querySelector('link[rel=icon]');
  if (!link) return;
  // this runs from every render pass, which on a quiet instance is a fresh
  // Image, decode, canvas and toDataURL every 45 seconds forever
  if (count === badgeDrawn) return;
  badgeDrawn = count;
  // the zero path writes synchronously and the badged one writes from a
  // promise, so without a generation a reset can be overwritten by the count
  // it was resetting
  const generation = ++badgeGeneration;
  if (count === 0) {
    link.href = PLAIN_FAVICON;
    return;
  }
  const base = new Image();
  base.src = PLAIN_FAVICON;
  base.decode().then(() => {
    if (generation !== badgeGeneration) return;
    const canvas = document.createElement('canvas');
    canvas.width = 32;
    canvas.height = 32;
    const ctx = canvas.getContext('2d');
    ctx.drawImage(base, 0, 0, 32, 32);
    // The dot is drawn on a plate-coloured disc a pixel and a half wider, so
    // it reads as one object over whatever part of the mark it lands on —
    // without that ring the amber dot and the amber row underneath it merge
    // into a blob at the size a tab strip actually shows.
    ctx.fillStyle = BADGE_INK;
    ctx.beginPath();
    ctx.arc(21, 21, 11, 0, 2 * Math.PI);
    ctx.fill();
    ctx.fillStyle = BADGE_FILL;
    ctx.beginPath();
    ctx.arc(21, 21, 9.5, 0, 2 * Math.PI);
    ctx.fill();
    ctx.fillStyle = BADGE_INK;
    ctx.font = 'bold 12px sans-serif';
    ctx.textAlign = 'center';
    ctx.textBaseline = 'middle';
    ctx.fillText(count > 9 ? '9+' : String(count), 21, 22);
    link.href = canvas.toDataURL('image/png');
  }).catch(() => {
    if (generation !== badgeGeneration) return;
    // a favicon that cannot be drawn costs the dot and nothing else; clear
    // the record so the next render tries again rather than skipping as
    // already-drawn
    badgeDrawn = null;
    link.href = PLAIN_FAVICON;
  });
}

// --- messages from the worker (§9.5) ---

// The page half of §9.5's switch. state-changed arrives on a tab nobody is
// looking at — it is what puts a firing on the favicon immediately rather
// than up to five minutes later — so it refreshes and nothing else:
// scrolling or flashing would move the page under whatever you return to.
async function handleWorkerMessage(message) {
  if (!message) return;
  if (message.type === 'state-changed') {
    refreshState();
    return;
  }
  if (message.type === 'focus-reminder') {
    await refreshState();
    flashRow(message.id);
  }
}

// consumeFocusParam handles the other path: the id travels in the URL when
// the worker had no client to talk to, because postMessage to a window
// openWindow just returned is a race it always loses (§9.5). It is treated
// as exactly the same message, and the URL is cleaned immediately so a
// refresh does not re-flash a row you dealt with an hour ago and the address
// never becomes something you might bookmark.
function consumeFocusParam() {
  const id = new URLSearchParams(location.search).get('focus');
  if (id === null) return;
  history.replaceState({}, '', '/');
  handleWorkerMessage({ type: 'focus-reminder', id });
}

// flashRow scrolls to a row and highlights it for ~2.4 s. If the id is in
// neither list after the refresh, the app is focused and nothing else
// happens — no scroll, no flash, no toast, no error. The row was cleared on
// another device while the notification sat in the tray, or cleared here and
// purged (§4.4), and both are ordinary: a notification is never the record
// of anything (§7.6). Never fall back to flashing the first row.
function flashRow(id) {
  clearFlash();
  pendingFlash = { id: String(id), deadline: Date.now() + FLASH_WAIT_MS };
  applyPendingFlash();
}

// applyPendingFlash runs at the end of every render, which is the only place
// it can: scrollIntoView does nothing to an element that is not in the
// document yet, so lighting a row from inside renderZone would paint it and
// never move the page. A request whose row is in neither list simply waits,
// and expires — which is §9.5's "nothing happens" for a reminder that was
// cleared elsewhere, and a second chance for one whose render lost a race.
function applyPendingFlash() {
  if (!pendingFlash) return;
  if (Date.now() > pendingFlash.deadline) {
    pendingFlash = null;
    return;
  }
  const row = rowElement(pendingFlash.id);
  if (!row) return;

  litRow = pendingFlash.id;
  pendingFlash = null;
  paintFlash(row);
  const still = matchMedia('(prefers-reduced-motion: reduce)').matches;
  row.scrollIntoView({ block: 'center', behavior: still ? 'auto' : 'smooth' });
  // the highlight is measured from the moment the row is on screen, not from
  // the click that asked for it
  flashTimer = setTimeout(clearFlash, FLASH_MS);
}

function clearFlash() {
  clearTimeout(flashTimer);
  pendingFlash = null;
  if (litRow === null) return;
  const row = rowElement(litRow);
  if (row) row.style.backgroundColor = '';
  litRow = null;
}

// CSSOM rather than a style attribute, which style-src 'self' forbids and
// §9.7 makes a rule; M3 replaces the provisional colour with the token
// (commit 38).
function paintFlash(row) {
  row.style.backgroundColor = FLASH_FILL;
}

function rowElement(id) {
  return document.querySelector(`li[data-id="${CSS.escape(String(id))}"]`);
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
    li.dataset.id = r.id; // what §9.5's focus-reminder finds the row by
    li.append(label, ' ', clear);
    ul.append(li);
    // a rebuild inside the 2.4 s must not drop the highlight it is showing
    if (litRow === String(r.id)) paintFlash(li);
  }
}

init();
