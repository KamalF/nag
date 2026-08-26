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
// itself fails. §9.8's notification slot reads it in the next commit.
let pushAvailable = pushSupported;
let swRegistration = null;

async function init() {
  // deliberately not awaited: registration must never sit between the load
  // and the first render (§9.5)
  registerWorker();

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
}

// --- service worker (§9.5) ---

// Registration is unconditional, on every load, and needs no gesture and no
// permission — Notification.requestPermission() and pushManager.subscribe()
// are the calls that do (§9.8). It is idempotent, so the repeat costs one
// no-op against the browser's registration store, and it is the only way a
// skipWaiting deploy reaches a tab that has been pinned for months. The
// registration object is kept: never call register a second time to "get" it.
async function registerWorker() {
  if (!pushSupported) return;
  try {
    swRegistration = await navigator.serviceWorker.register('/sw.js');
  } catch (err) {
    // a syntax error in sw.js, a hard-refresh race, a browser with workers
    // disabled by policy: nothing to retry and nothing the user can do. It
    // is caught rather than left to reject because an unhandled rejection on
    // the load path must not stop the poll from starting (§9.5).
    console.error('service worker registration failed', err);
    pushAvailable = false;
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
