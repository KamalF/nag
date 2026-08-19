'use strict';

// M1 minimal UI (§9.2 behaviourally, zero styling). No client-side
// persistence anywhere (§12): the server state is the state.

const $ = (id) => document.getElementById(id);

const VISIBLE_POLL_MS = 45000; // §9.3

let config = null;

async function init() {
  const res = await fetch('/api/state');
  if (res.status === 401) {
    showLogin();
    return;
  }
  config = await (await fetch('/api/config')).json();
  renderChips();
  $('capture').hidden = false;
  $('overdue-zone').hidden = false;
  $('later-zone').hidden = false;
  renderState(await res.json());
  setInterval(() => {
    if (!document.hidden) refreshState();
  }, VISIBLE_POLL_MS);
}

// --- login (§8.1) ---

function showLogin() {
  $('login').hidden = false;
  $('login-btn').addEventListener('click', submitToken);
  $('token').addEventListener('keydown', (e) => {
    if (e.key === 'Enter') submitToken();
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
    // the client resolves default_preset — the server has no fallback (§8.3)
    if (e.key === 'Enter') createReminder(config.default_preset);
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
