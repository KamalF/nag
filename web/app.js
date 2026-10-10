'use strict';

// Nag's page (§9). No client-side persistence anywhere (§12): the server
// state is the state.

const $ = (id) => document.getElementById(id);

// §9.3's three cadences and the flash. 45 s is for the tab in front of you;
// 5 minutes is what keeps a pinned tab's favicon honest without polling on
// behalf of a machine nobody is using — suspending a hidden tab entirely
// would break Goal 4 outright, since a tab you are reading the favicon of is
// hidden by definition.
const VISIBLE_POLL_MS = 45000;
const HIDDEN_POLL_MS = 300000;
const FLASH_MS = 2400;

// §9.2's toasts: the undo offer lasts five seconds; an error stays a little
// longer, since someone has to read it.
const UNDO_MS = 5000;
const ERROR_TOAST_MS = 7000;

// A failed poll is not a toast: the line appears after this many in a row.
const OFFLINE_AFTER_FAILURES = 2;

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
const BADGE_INK = '#212A2E';
const BASE_TITLE = 'Nag';
const PLAIN_FAVICON = '/icons/32.png';

let config = null;

// channels is /api/channels — [{name, enabled}] ORDER BY name, which is the
// chip order in both places it is drawn (§9.2). The URL is never in it.
let channels = [];

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

// A flash is a request, not a style: the row a click names may not have
// been drawn yet at all, since an aborted refresh resolves having rendered
// nothing. pendingFlash is the request waiting for its row; litRow is the
// row currently painted, so clearFlash can find it. The keyed render below
// reuses the node, so the paint survives every pass on its own.
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
    const chRes = await fetch('/api/channels');
    if (!chRes.ok) throw new Error('channels ' + chRes.status);
    channels = await chRes.json();
  } catch {
    // a boot that fails must retry, not brick: there is no list yet to keep
    setTimeout(init, VISIBLE_POLL_MS);
    return;
  }
  buildCapture();
  wirePicker();
  document.addEventListener('keydown', onKeydown);
  // §9.2: autofocus on desktop only — on a phone it opens the keyboard over
  // the list on every load, which fights Goal 4 where glanceability matters
  if (matchMedia('(min-width: 701px) and (hover: hover)').matches) capture.input.focus();
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
  const result = await request('/login', 'POST', { token: $('token').value }, { reloadOn401: false });
  if (result.ok) {
    // the whole of the post-login path: a reload re-runs init under the
    // new cookie (§8.1)
    location.reload();
    return;
  }
  $('login-error').textContent = result.error ?? 'wrong token';
}

// --- the bar: capture and row editor (§9.2) ---

// createBar is the one control behind both the capture bar and the row
// editor — same component, same keys, same behaviour (§9.2): a single-line
// input and the preset chips in file order (§5.4). A chip carries its own
// answer and saves on the click (Goal 1); Enter means whatever the surface
// saves (§9.4), so each caller says what that is.
function createBar({ value = '', placeholder = '', label, onTime, onEnter, pickFrom, attached }) {
  const el = document.createElement('div');
  el.className = 'bar';
  const input = document.createElement('input');
  input.type = 'text';
  input.value = value;
  input.placeholder = placeholder;
  input.setAttribute('aria-label', label);
  input.enterKeyHint = 'done';
  input.autocomplete = 'off';
  // a typing hint, not the rule: maxlength counts UTF-16 units and the
  // server counts bytes, so the server stays the authority (§9.2)
  input.maxLength = 1000;
  const chips = document.createElement('div');
  chips.className = 'chips';
  el.append(input, chips);
  // attached is the row's extra_channels in an editor, undefined in the
  // capture bar; the selector decides on its own whether to render at all
  const selector = channelSelector(attached);
  if (selector) el.append(selector.el);

  const bar = {
    el,
    input,
    text: () => input.value.trim(),
    // the full list of what the channel chips show selected, orphans
    // included — an editor always sends it, never omitting the key (§9.2)
    selectedChannels: () => selector?.selected() ?? [],
    resetChannels: () => selector?.reset(),
    // every chip is disabled while the input is empty or whitespace-only —
    // a chip that visibly isn't ready beats a toast saying so — and while a
    // save is in flight, so a double-tap cannot create two reminders (§9.2)
    busy: false,
    updateEnabled() {
      const off = bar.busy || bar.text() === '';
      for (const b of chips.children) b.disabled = off;
    },
    async submit(save) {
      if (bar.busy) return false;
      bar.busy = true;
      bar.updateEnabled();
      try {
        return await save();
      } finally {
        bar.busy = false;
        bar.updateEnabled();
      }
    },
    redrawChips() {
      chips.replaceChildren();
      for (const p of config.presets) {
        const b = document.createElement('button');
        b.type = 'button';
        b.className = 'chip';
        b.textContent = p.label;
        b.addEventListener('click', () => bar.submit(() => onTime({ preset: p.key })));
        chips.append(b);
      }
      // "Pick a time" is not a preset (§5.4): the client renders it after
      // the configured ones, and the picker's Set reminder is its save
      const pick = document.createElement('button');
      pick.type = 'button';
      pick.className = 'chip';
      pick.textContent = 'Pick a time';
      pick.addEventListener('click', () =>
        openPicker({ opener: pick, from: pickFrom?.(), onSet: (dueAt) => bar.submit(() => onTime({ due_at: dueAt })) }));
      chips.append(pick);
      bar.updateEnabled();
    },
  };
  input.addEventListener('input', bar.updateEnabled);
  input.addEventListener('keydown', (e) => {
    // isComposing: Enter that commits an IME composition is not a save
    if (e.key !== 'Enter' || e.isComposing) return;
    e.preventDefault();
    if (bar.text() !== '') bar.submit(onEnter);
  });
  bar.redrawChips();
  return bar;
}

// channelSelector is §9.2's "Also send to": a toggle that expands a row
// of channel chips. The capture bar renders it only when the catalogue is
// non-empty — fan-out is opt-in and a fresh install has no channels. An
// editor renders it whenever the catalogue is non-empty *or* the row
// carries names: with `nag channel rm` on the last channel, an unrendered
// affordance would send [] on every save and silently wipe the orphans the
// server exists to preserve.
//
// "Greyed and unselectable" is a rule about adding, never removing:
// - a disabled channel already on the row renders selected and greyed, and
//   can be deselected — a mute must not make a name impossible to remove;
// - a name the catalogue does not list (an orphan) gets a greyed chip of its
//   own, selected; deselected, it cannot be re-selected — you may carry an
//   orphan forward, never introduce one (§8.3).
function channelSelector(attached) {
  const names = new Set(attached ?? []);
  const known = new Set(channels.map((c) => c.name));
  const orphans = [...names].filter((n) => !known.has(n));
  if (channels.length === 0 && orphans.length === 0) return null;

  const el = document.createElement('div');
  el.className = 'channels';
  const toggle = document.createElement('button');
  toggle.type = 'button';
  toggle.className = 'channels-toggle';
  toggle.textContent = 'Also send to';
  const row = document.createElement('div');
  row.className = 'chips channel-chips';
  el.append(toggle, row);

  const chips = [];
  const add = (name, selectable) => {
    const b = document.createElement('button');
    b.type = 'button';
    b.className = 'chip channel' + (selectable ? '' : ' muted');
    b.textContent = name;
    const chip = { name, selectable, button: b, selected: names.has(name) };
    const paint = () => {
      b.setAttribute('aria-pressed', String(chip.selected));
      // deselecting always works; selecting needs an enabled, known channel
      b.disabled = !chip.selected && !chip.selectable;
    };
    b.addEventListener('click', () => {
      if (!chip.selected && !chip.selectable) return;
      chip.selected = !chip.selected;
      paint();
    });
    chip.paint = paint;
    paint();
    chips.push(chip);
    row.append(b);
  };
  for (const c of channels) add(c.name, c.enabled);
  for (const name of orphans) add(name, false);

  const setOpen = (open) => {
    toggle.setAttribute('aria-expanded', String(open));
    row.hidden = !open;
  };
  // open when the row already carries names, so they are visible
  setOpen(names.size > 0);
  toggle.addEventListener('click', () => setOpen(row.hidden));

  return {
    el,
    selected: () => chips.filter((c) => c.selected).map((c) => c.name),
    // after each save the selection resets: nothing is remembered (§12)
    reset() {
      for (const c of chips) {
        c.selected = false;
        c.paint();
      }
      setOpen(false);
    },
  };
}

let capture = null;

function buildCapture() {
  capture = createBar({
    placeholder: 'What should come back to you?',
    label: 'New reminder',
    onTime: createReminder,
    // the client resolves default_preset — the server has no fallback (§8.3)
    onEnter: () => createReminder({ preset: config.default_preset }),
  });
  $('capture').append(capture.el);
  $('capture').hidden = false;
}

// createReminder posts the capture bar's text with one timing field. On
// success the input is cleared, so the bar is ready for the next capture.
async function createReminder(timing) {
  const text = capture.text();
  if (text === '') return false;
  const result = await userAction('/api/reminders', 'POST',
    { text, extra_channels: capture.selectedChannels(), ...timing });
  // a save that fails leaves what you typed exactly where it is, next to
  // its toast (§9.2)
  if (!result.ok) return false;
  // only the input is left editable while a save is in flight, so whatever
  // was typed meanwhile is the next reminder and stays
  if (capture.text() === text) capture.input.value = '';
  capture.updateEnabled();
  capture.resetChannels();
  return true;
}

// --- the row editor (§9.2) ---

// Editing a row is the capture bar again, prefilled with the raw text —
// markdown links included. One editor at a time: opening another closes
// the first, unless it holds unsaved text, which is never thrown away. The
// row's time stays on the row beside it, which is where you read the
// current due time while choosing a new one (§9.6).
let editor = null;

function openEditor(row) {
  if (editor) {
    // never eat typing: the open editor's own time, or another row, does
    // not replace an editor holding unsaved text — focus goes back to it
    if (editor.row === row || editor.bar.input.value !== editor.row.reminder.text) {
      editor.bar.input.focus();
      return;
    }
    closeEditor();
  }
  row.editing = true; // the keyed render now leaves this row alone
  const bar = createBar({
    value: row.reminder.text,
    label: 'Edit reminder',
    onTime: (timing) => saveEdit(row, bar, timing),
    // Enter saves the text without touching the time
    onEnter: () => saveEdit(row, bar, {}),
    // the picker opens on the stored moment when it is still ahead (§9.6)
    pickFrom: () => row.reminder.due_at,
    attached: row.reminder.extra_channels,
  });
  editor = { row, bar };
  row.li.classList.add('editing');
  row.txt.hidden = true;
  row.acts.hidden = true;
  if (row.reminder.delivery_error !== null) {
    const failure = document.createElement('p');
    failure.className = 'delivery-error';
    failure.textContent = 'Last delivery failed — ' + row.reminder.delivery_error;
    bar.el.append(failure);
  }
  row.li.prepend(bar.el);
  bar.input.focus();
  bar.input.setSelectionRange(bar.input.value.length, bar.input.value.length);
}

// closeEditor puts the row back and runs a pass, which reconciles whatever
// the polls skipped while it was open — including removing a row that was
// cleared on another device meanwhile.
function closeEditor() {
  const { row, bar } = editor;
  editor = null;
  bar.el.remove();
  row.txt.hidden = false;
  row.acts.hidden = false;
  row.li.classList.remove('editing');
  row.editing = false;
  if (lastState) renderState(lastState);
  const edit = row.li.querySelector('.act-edit');
  // saved from the picker: the chip that opened it has just gone with the
  // editor, so the sheet hands focus here when it closes
  if (picker?.saving && bar.el.contains(picker.opener)) picker.returnTo = edit;
  else edit?.focus({ preventScroll: true });
}

// saveEdit sends every field the editor shows (§9.2): text and
// extra_channels on every save, plus a timing field only when a chip or the
// picker chose one. Absent-means-unchanged (§8.3) makes the unchanged ones
// harmless, so there is one body shape and no branch deciding which keys go.
async function saveEdit(row, bar, timing) {
  const text = bar.text();
  if (text === '') return false;
  const result = await userAction('/api/reminders/' + row.id, 'PATCH',
    { text, extra_channels: bar.selectedChannels(), ...timing });
  if (!result.ok) return false;
  // this save's own editor only: Esc'd and reopened on the same row, the
  // new editor is someone else's typing
  if (editor?.bar !== bar) return true;
  if (bar.text() === text) {
    closeEditor();
  } else {
    // typed into while the save was in flight: the save landed, and what
    // was typed since stays in the open editor, measured against the row
    // as it now stands — whose time, beside the editor, is repainted
    row.reminder = result.data;
    row.when.textContent = whenText(result.data.due_at);
  }
  return true;
}

// --- the picker sheet (§9.6) ---

// The wheels and the calendar only ever move a pending value; nothing is
// written until Set reminder — the one place a two-step commit is right,
// since saving per scroll-snap would PATCH once per notch of the minute
// wheel. picker is null while the sheet is down.
let picker = null;

const ITEM_PX = 44; // --item: one wheel row
const WHEEL_SETTLE_MS = 90;
const PAST_SENTENCE = "in the past — it'll be waiting in Needs you now, and nothing will be sent";
const monthFormat = new Intl.DateTimeFormat(undefined, { month: 'long', year: 'numeric' });
const shortWeekdayFormat = new Intl.DateTimeFormat(undefined, { weekday: 'short' });

// openPicker shows the sheet for one save. opener is the chip that opened
// it, and gets focus back on close; from is the row's due_at when editing.
function openPicker({ opener, from, onSet }) {
  const { date, hour, minute } = openingState(from);
  picker = {
    opener,
    returnTo: null,
    saving: false,
    onSet,
    date, // local midnight of the chosen day
    view: new Date(date.getFullYear(), date.getMonth(), 1),
    hour,
    minute,
  };
  $('main-app').inert = true; // nothing focusable behind the scrim
  $('scrim').classList.add('open');
  $('sheet').classList.add('open');
  buildWeekdays();
  buildCalendar();
  buildWheels();
  renderSummary();
  $('picker-set').disabled = false;
  $('picker-set').focus();
}

// closePicker discards the pending value and returns focus to the chip
// that opened the sheet, so a second Esc lands on what is under it (§9.4).
function closePicker() {
  const { opener, returnTo } = picker;
  picker = null;
  $('main-app').inert = false;
  $('scrim').classList.remove('open');
  $('sheet').classList.remove('open');
  const target = returnTo ?? opener;
  // a capture save empties the input, which disables the chip that opened
  // the sheet; its bar's input is the nearest place to land instead
  if (target.disabled) target.closest('.bar')?.querySelector('input')?.focus({ preventScroll: true });
  else target.focus({ preventScroll: true });
}

// openingState is §9.6's one rule for both modes: the stored moment — hour
// clamped into the range, minute always 00 — when that is still ahead of
// now(), and otherwise the new-reminder default: default_time clamped, on
// today if it is still ahead, else tomorrow. The test is on the value the
// sheet would show, because the minute reset can itself move a future row
// into the past (14:30 today reads back as 14:00), and the sheet never opens
// on the in-the-past warning.
function openingState(from) {
  const { hour_min: lo, hour_max: hi } = config.picker;
  const clamp = (h) => Math.min(Math.max(h, lo), hi);
  if (from !== undefined) {
    const stored = new Date(from * 1000);
    const date = new Date(stored.getFullYear(), stored.getMonth(), stored.getDate());
    const hour = clamp(stored.getHours());
    if (moment(date, hour, 0) > now()) return { date, hour, minute: 0 };
  }
  const [h, m] = config.picker.default_time.split(':').map(Number);
  const hour = clamp(h);
  const today = new Date(now() * 1000);
  const date = new Date(today.getFullYear(), today.getMonth(), today.getDate());
  if (moment(date, hour, m) <= now()) date.setDate(date.getDate() + 1);
  return { date, hour, minute: m };
}

// moment reads the chosen wall-clock time in the browser's timezone and
// returns Unix seconds (§6, §9.3). A time inside a spring-forward gap has no
// local instant; new Date normalises it forward, as Go's time.Date does, and
// the summary shows the moment that will actually be stored.
function moment(date, hour, minute) {
  const d = new Date(date.getFullYear(), date.getMonth(), date.getDate(), hour, minute, 0, 0);
  return Math.floor(d.getTime() / 1000);
}

function pendingMoment() {
  return moment(picker.date, picker.hour, picker.minute);
}

// renderSummary is the confirmation read before pressing Set: the full date
// with the time emphasised, and the relative line — the future rows of
// whenText verbatim, or the fixed past sentence, which never says "late"
// about a time not yet saved (§9.11). Past moments are allowed.
function renderSummary() {
  const dueAt = pendingMoment();
  const { date, time } = fullText(dueAt);
  const b = document.createElement('b');
  b.textContent = time;
  $('summary').replaceChildren(document.createTextNode(date + ' at '), b);
  $('relative').textContent = dueAt - now() <= 0 ? PAST_SENTENCE : whenText(dueAt);
}

// buildWeekdays labels the columns from week_start, in the device locale.
function buildWeekdays() {
  const start = config.picker.week_start === 'sunday' ? 0 : 1;
  const sunday = new Date(2023, 0, 1); // a Sunday
  $('dow').replaceChildren();
  for (let i = 0; i < 7; i++) {
    const span = document.createElement('span');
    span.textContent = shortWeekdayFormat.format(
      new Date(sunday.getFullYear(), sunday.getMonth(), sunday.getDate() + ((start + i) % 7)));
    $('dow').append(span);
  }
}

// buildCalendar draws the viewed month: today with a thin ring, the chosen
// day amber-filled, past days dimmed but still tappable — backdating is
// sometimes intentional. "Today" and "past" come from now(), never from
// the device clock directly. No year stepper and no bound on paging.
function buildCalendar() {
  const { view } = picker;
  const start = config.picker.week_start === 'sunday' ? 0 : 1;
  const nowDate = new Date(now() * 1000);
  const today = new Date(nowDate.getFullYear(), nowDate.getMonth(), nowDate.getDate());
  $('month').textContent = monthFormat.format(view);

  const grid = $('grid');
  grid.replaceChildren();
  const lead = (view.getDay() - start + 7) % 7;
  for (let i = 0; i < lead; i++) {
    const cell = document.createElement('div');
    cell.className = 'cell';
    grid.append(cell);
  }
  const days = new Date(view.getFullYear(), view.getMonth() + 1, 0).getDate();
  for (let d = 1; d <= days; d++) {
    const date = new Date(view.getFullYear(), view.getMonth(), d);
    const cell = document.createElement('div');
    cell.className = 'cell';
    const b = document.createElement('button');
    b.type = 'button';
    b.textContent = d;
    b.setAttribute('aria-label', longDateFormat.format(date));
    if (date < today) b.classList.add('past');
    if (+date === +today) b.classList.add('today');
    b.setAttribute('aria-pressed', String(+date === +picker.date));
    b.addEventListener('click', () => {
      picker.date = date;
      buildCalendar();
      renderSummary();
      grid.querySelector('[aria-pressed="true"]')?.focus();
    });
    cell.append(b);
    grid.append(cell);
  }
}

function pageMonth(delta) {
  picker.view = new Date(picker.view.getFullYear(), picker.view.getMonth() + delta, 1);
  buildCalendar();
}

// buildWheels fills the two wheels: hours hour_min..hour_max — a hard
// range, with no escape hatch (§9.6) — and minutes 0..59 by minute_step.
function buildWheels() {
  const { hour_min: lo, hour_max: hi, minute_step: step } = config.picker;
  const hours = [];
  for (let h = lo; h <= hi; h++) hours.push(h);
  const minutes = [];
  for (let m = 0; m < 60; m += step) minutes.push(m);
  fillWheel($('hours'), hours, picker.hour);
  fillWheel($('minutes'), minutes, picker.minute);
}

// fillWheel: each value is a real button that scrolls itself to the centre
// — tap and keyboard both work — and the selection is read back from
// scrollTop once scrolling settles. Nothing here drags.
function fillWheel(el, values, current) {
  el.replaceChildren();
  values.forEach((v, i) => {
    const b = document.createElement('button');
    b.type = 'button';
    b.textContent = String(v).padStart(2, '0');
    b.addEventListener('click', () => el.scrollTo({ top: i * ITEM_PX, behavior: scrollBehavior() }));
    el.append(b);
  });
  el.values = values;
  const idx = Math.max(0, values.indexOf(current));
  el.scrollTop = idx * ITEM_PX;
  markWheel(el, idx);
}

function markWheel(el, idx) {
  [...el.children].forEach((b, i) => b.classList.toggle('sel', i === idx));
}

function onWheelScroll(el, key) {
  const idx = () => Math.min(el.values.length - 1, Math.max(0, Math.round(el.scrollTop / ITEM_PX)));
  let settle = null;
  el.addEventListener('scroll', () => {
    if (!picker) return;
    markWheel(el, idx());
    clearTimeout(settle);
    settle = setTimeout(() => {
      if (!picker) return;
      picker[key] = el.values[idx()];
      renderSummary();
    }, WHEEL_SETTLE_MS);
  }, { passive: true });
}

function scrollBehavior() {
  return matchMedia('(prefers-reduced-motion: reduce)').matches ? 'auto' : 'smooth';
}

// setPicked is the whole write — POST from the capture bar, PATCH from a
// row editor — and closes the sheet on success only, so a failed save
// leaves the chosen moment on screen.
async function setPicked() {
  const set = $('picker-set');
  if (set.disabled) return;
  const session = picker;
  session.saving = true;
  set.disabled = true;
  const ok = await session.onSet(pendingMoment());
  session.saving = false;
  // Esc'd while saving, and maybe another sheet opened since: this save
  // must not close or refocus a session it does not own
  if (picker !== session) return;
  set.disabled = false;
  if (ok) {
    closePicker();
  } else {
    set.focus(); // disabling it blurred it; keep focus inside the dialog
  }
}

// trapFocus keeps Tab inside the sheet while it is up: the page behind is
// inert, and tabbing into a list you cannot see is a trap the other way.
function trapFocus(e) {
  if (e.key !== 'Tab') return;
  const focusable = [...$('sheet').querySelectorAll('button:not(:disabled)')];
  const first = focusable[0];
  const last = focusable[focusable.length - 1];
  if (e.shiftKey && document.activeElement === first) {
    e.preventDefault();
    last.focus();
  } else if (!e.shiftKey && document.activeElement === last) {
    e.preventDefault();
    first.focus();
  }
}

function wirePicker() {
  $('prev-month').addEventListener('click', () => pageMonth(-1));
  $('next-month').addEventListener('click', () => pageMonth(1));
  $('picker-cancel').addEventListener('click', closePicker);
  $('picker-set').addEventListener('click', setPicked);
  $('scrim').addEventListener('click', closePicker);
  onWheelScroll($('hours'), 'hour');
  onWheelScroll($('minutes'), 'minute');
  $('sheet').addEventListener('keydown', (e) => {
    trapFocus(e);
    // Enter anywhere in the sheet activates Set reminder (§9.4)
    if (e.key === 'Enter' && !e.isComposing) {
      e.preventDefault();
      setPicked();
    }
  });
}

// --- keyboard (§9.4) ---

// onKeydown carries the whole list: / or n focus the capture input, Esc
// closes exactly one thing per press, innermost first. Enter belongs to
// each surface and is handled there.
function onKeydown(e) {
  if (e.key === 'Escape') {
    if (picker) {
      closePicker();
    } else if (editor) {
      closeEditor();
    } else if (capture.input.value !== '') {
      capture.input.value = '';
      capture.updateEnabled();
    } else {
      return; // nothing open and an empty input: Esc does nothing at all
    }
    e.preventDefault();
    return;
  }
  if ((e.key === '/' || e.key === 'n') && !e.ctrlKey && !e.metaKey && !e.altKey) {
    // inert in a text field — otherwise unusable in the one place you are
    // guaranteed to be typing — and under an open editor or the sheet,
    // since focusing the capture bar behind a scrim is nobody's intent
    if (e.target.closest('input, textarea, [contenteditable]') || editor || picker) return;
    e.preventDefault();
    capture.input.focus();
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
    button.type = 'button';
    button.className = 'act pri';
    button.textContent = 'Turn on notifications';
    button.addEventListener('click', turnOnNotifications);
    slot.append(button);
    slot.hidden = false;
    return;
  }
  const line = document.createElement('p');
  line.textContent = SLOT_LINES[want];
  slot.append(line);
  slot.hidden = false;
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
    // aborted by a newer request — which counts for nothing — or offline,
    // which the next trigger retries
    if (controller === stateAbort) pollFailed();
    return;
  }
  if (controller !== stateAbort) return;
  if (res.status === 401) {
    location.reload();
    return;
  }
  if (res.status === 304) {
    pollSucceeded();
    // The ETag excludes server_time so the steady-state poll is cheap (§8.2),
    // which makes this the common case: no body, while every time string on
    // screen has nevertheless moved. Returning early here would freeze them
    // until the data happens to change, which on a quiet afternoon is hours.
    syncClock(res, null);
    if (lastState) renderState(lastState);
    return;
  }
  if (!res.ok) {
    pollFailed();
    return;
  }

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
  pollSucceeded();
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
  if (state.config_version !== config.config_version) reloadWhenQuiet();
  $('overdue-count').textContent = state.overdue_count;
  renderLists(state);
  // a zone heading is hidden entirely when its list is empty, and the empty
  // state replaces both (§9.2)
  const hasOverdue = state.overdue.length > 0;
  const hasLater = state.later.length > 0;
  $('overdue-zone').hidden = !hasOverdue;
  $('later-zone').hidden = !hasLater;
  $('divider').hidden = !(hasOverdue && hasLater);
  $('empty').hidden = hasOverdue || hasLater;
  // driven by the same render pass as the lists, which is what keeps all
  // three outputs correct after a 304 too (§9.3)
  setBadge(state.overdue_count);
  applyPendingFlash();
  armDueTimer(state);
  if (subscriptionSynced) renderNotifySlot(state.push_subscribed);
}

// --- time strings (§9.11) ---

// whenText and fullText are the only two things that format a time, here
// and in sw.js's copy of the same table (the worker cannot import this
// file). The connectives are English; the clock, weekday and month are the
// device's locale, in the browser's timezone (§6).
const clockFormat = new Intl.DateTimeFormat(undefined, { hour: 'numeric', minute: '2-digit' });
const weekdayFormat = new Intl.DateTimeFormat(undefined, { weekday: 'long' });
const dayMonthFormat = new Intl.DateTimeFormat(undefined, { day: 'numeric', month: 'long' });
const dayMonthYearFormat = new Intl.DateTimeFormat(undefined, { day: 'numeric', month: 'long', year: 'numeric' });
const longDateFormat = new Intl.DateTimeFormat(undefined, { weekday: 'long', day: 'numeric', month: 'long' });
const longDateYearFormat = new Intl.DateTimeFormat(undefined,
  { weekday: 'long', day: 'numeric', month: 'long', year: 'numeric' });

// whenText is the short form: a row's time, the picker's relative line, and
// the notification body. First match wins, top to bottom — the conditions
// overlap by design, so the order is the specification. The three rows at
// the top are durations; from "today" down every row is a calendar
// comparison, because mixing the two makes a boundary drift by up to a day
// depending on when you glance at it.
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

// fullText is the long form, only for the picker's summary line: weekday,
// day, month and — only outside the current year — the year, then the time
// the summary emphasises. It returns the two halves so the caller can build
// that emphasis as nodes (§9.7).
function fullText(dueAt) {
  const due = new Date(dueAt * 1000);
  const sameYear = due.getFullYear() === new Date(now() * 1000).getFullYear();
  return {
    date: (sameYear ? longDateFormat : longDateYearFormat).format(due),
    time: clockFormat.format(due),
  };
}

// calendarDaysBetween is the difference between two local midnights, in
// whole days. It goes through Date.UTC so a DST change in between — a
// 23- or 25-hour day — cannot round the count the wrong way.
function calendarDaysBetween(from, to) {
  const utc = (d) => Date.UTC(d.getFullYear(), d.getMonth(), d.getDate());
  return Math.round((utc(to) - utc(from)) / 86400000);
}

// every count is singular at 1: "1 day late", "in 1 minute"
function count(n, unit) {
  return n + ' ' + unit + (n === 1 ? '' : 's');
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
  pendingFlash = { id: String(id), deadline: performance.now() + FLASH_WAIT_MS };
  applyPendingFlash();
}

// applyPendingFlash runs at the end of every render, which is the only place
// it can: scrollIntoView does nothing to an element that is not in the
// document yet, so lighting a row from inside renderLists would paint it and
// never move the page. A request whose row is in neither list simply waits,
// and expires — which is §9.5's "nothing happens" for a reminder that was
// cleared elsewhere, and a second chance for one whose render lost a race.
function applyPendingFlash() {
  if (!pendingFlash) return;
  if (performance.now() > pendingFlash.deadline) {
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
  rowElement(litRow)?.classList.remove('pinged');
  litRow = null;
}

// The flash is a class toggle with its values in app.css (§9.7). Removing
// and re-adding restarts the animation when the same row is asked for again.
function paintFlash(row) {
  row.classList.remove('pinged');
  void row.offsetWidth;
  row.classList.add('pinged');
}

function rowElement(id) {
  return document.querySelector(`li[data-id="${CSS.escape(String(id))}"]`);
}

// --- reminder text: one markdown link form (§9.10) ---

// LINK is the whole grammar, and the same regex as the server's
// flattenLinks (a Go test pins the two): [label](target), label non-empty
// with no brackets, target with no whitespace and no ")".
const LINK = /\[([^\[\]]+)\]\(([^\s)]*)\)/g;

// renderText walks the matches and appends nodes — text nodes between, an
// anchor for each eligible link — so no markup is ever parsed: no innerHTML
// anywhere (§9.7). A match that is not eligible is emitted whole, brackets
// and all.
function renderText(el, text) {
  el.replaceChildren();
  let last = 0;
  for (const m of text.matchAll(LINK)) {
    const [whole, label, target] = m;
    if (!rendersAsLink(target)) continue;
    el.append(document.createTextNode(text.slice(last, m.index)));
    const a = document.createElement('a');
    a.textContent = label;
    a.href = target;
    a.target = '_blank';
    a.rel = 'noopener noreferrer';
    el.append(a);
    last = m.index + whole.length;
  }
  el.append(document.createTextNode(text.slice(last)));
}

// rendersAsLink is the eligibility gate, the same one flattenLinks applies:
// scheme http or https, read off the prefix, and no "(" — a paren-bearing
// URL would have been cut at the first ")", a link that looks right and
// 404s, so it stays visible as raw markdown instead (§9.10).
function rendersAsLink(target) {
  if (target.includes('(')) return false;
  const lower = target.toLowerCase();
  return lower.startsWith('http:') || lower.startsWith('https:');
}

// --- the keyed list (§9.2) ---

// rows maps a reminder id to its row: the li and the parts a pass updates.
// A poll must never do to a row what a reload would do to the page, so a
// pass reuses the node rather than replacing it — which is what keeps focus
// inside a row's actions, the :hover and :focus-within reveal, and the
// browser's scroll anchoring across the 45-second poll and the 304 path.
// row.editing is set by the inline editor: such a row is not touched at all.
const rows = new Map();

// renderLists is the whole pass: every row is updated in place, moved only
// if it is out of order, and removed only once it is in neither list. The
// server-sorted order with its id tiebreak (§8.2) is what makes "only what
// moved, moves" stable.
function renderLists(state) {
  // A pass can take focus away from a row action two ways: moving the row
  // (placeZone moves a row that went up its zone, and moving a node blurs
  // what is inside it), or rebuilding its actions (a row that changed zone).
  // Focus is restored by identity — the same row, the same action — so both
  // cases come back, typically the Edit button a closing editor just took.
  const focused = document.activeElement;
  const focusedAction = focused?.dataset?.action;
  const focusedRow = focused?.closest?.('li.row')?.dataset.id;
  const wanted = new Set();
  for (const r of state.overdue) wanted.add(String(r.id));
  for (const r of state.later) wanted.add(String(r.id));
  placeZone($('overdue'), state.overdue, true);
  placeZone($('later'), state.later, false);
  for (const [id, row] of rows) {
    // a row cleared elsewhere while its editor is open stays on screen until
    // the editor closes, and the next pass reconciles it
    if (wanted.has(id) || row.editing) continue;
    row.li.remove();
    rows.delete(id);
  }
  if (document.activeElement !== focused && focusedAction && rows.has(focusedRow)) {
    const again = [...rows.get(focusedRow).acts.children].find((b) => b.dataset.action === focusedAction);
    again?.focus({ preventScroll: true });
  }
}

// placeZone walks the zone's list against the current children with a
// cursor: a node already in place is passed over untouched, and only an
// out-of-place one is inserted. Two kinds of child are stepped over rather
// than placed: a row with its editor open, which no pass may move — moving
// it would blur the input mid-sentence — and a row that is leaving this
// zone, which the other zone's pass or the removal above picks up. Without
// that, one departure would reinsert every row below it and drop the focus
// inside them (§9.2).
function placeZone(ul, list, late) {
  const here = new Set(list.map((r) => String(r.id)));
  const settled = (node) => {
    while (node && (rows.get(node.dataset.id)?.editing || !here.has(node.dataset.id))) {
      node = node.nextElementSibling;
    }
    return node;
  };
  let cursor = settled(ul.firstElementChild);
  for (const r of list) {
    const row = updateRow(r, late);
    if (row.editing) continue;
    if (row.li === cursor) {
      cursor = settled(cursor.nextElementSibling);
      continue;
    }
    ul.insertBefore(row.li, cursor);
  }
}

// updateRow creates the row on first sight and otherwise touches only what
// changed: for an unchanged row that is the time string and nothing else.
function updateRow(r, late) {
  const id = String(r.id);
  let row = rows.get(id);
  if (!row) {
    row = createRow(id);
    rows.set(id, row);
  }
  if (row.editing) return row;
  row.reminder = r;
  if (row.late !== late) {
    row.late = late;
    row.li.className = 'row ' + (late ? 'late' : 'later');
    row.acts.replaceChildren(...rowActions(row));
  }
  if (row.text !== r.text) {
    row.text = r.text;
    renderText(row.txt, r.text);
  }
  paintMarker(row.marker, r);
  row.when.textContent = whenText(r.due_at);
  return row;
}

// paintMarker is §9.2's channel marker: a small quiet dot on a row with
// channels attached, amber when delivery_error is set — the error is its
// accessible label, and its full text shows in the editor, since a title
// tooltip alone is unreachable on the phone where you notice the amber.
function paintMarker(marker, r) {
  const failed = r.delivery_error !== null;
  marker.hidden = r.extra_channels.length === 0;
  marker.classList.toggle('failed', failed);
  const label = failed
    ? 'Delivery failed: ' + r.delivery_error
    : 'Also sent to ' + r.extra_channels.join(', ');
  marker.setAttribute('aria-label', label);
  marker.title = label;
}

function createRow(id) {
  const li = document.createElement('li');
  li.dataset.id = id; // what §9.5's focus-reminder finds the row by
  const txt = document.createElement('span');
  txt.className = 'txt';
  const marker = document.createElement('span');
  marker.className = 'marker';
  marker.setAttribute('role', 'img');
  const when = document.createElement('span');
  when.className = 'when';
  const acts = document.createElement('span');
  acts.className = 'acts';
  li.append(txt, marker, when, acts);
  const row = { id, li, txt, marker, when, acts, late: null, text: null, reminder: null, editing: false };
  // clicking the text or the time opens the editor; a link inside the text
  // is followed instead (§9.2)
  txt.addEventListener('click', (e) => {
    if (!e.target.closest('a')) openEditor(row);
  });
  when.addEventListener('click', () => openEditor(row));
  return row;
}

// rowActions: Clear for every row, plus the quick re-snooze chips on overdue
// rows only — an offset preset resolves from now (§4.1), so "30 min" on a
// row waiting for next Monday would quietly pull it forward by six days.
function rowActions(row) {
  const path = '/api/reminders/' + row.id;
  const actions = [actionButton('Clear', row.late, () => clearRow(row))];
  // the row text cannot be a <button> — it contains anchors (§9.10) — so
  // pointer users click the text and the keyboard gets a real button
  const edit = actionButton('Edit', false, () => openEditor(row));
  edit.classList.add('act-edit');
  actions.push(edit);
  if (row.late) {
    for (const p of config.presets) {
      if (!p.quick) continue;
      const chip = actionButton(p.label, false, () => userAction(path, 'PATCH', { preset: p.key }));
      chip.dataset.action = 'preset:' + p.key;
      actions.push(chip);
    }
  }
  return actions;
}

// actionButton disables itself while its request is in flight, so a
// double-tap cannot send the action twice (§9.2).
function actionButton(label, primary, onClick) {
  const b = document.createElement('button');
  b.type = 'button';
  b.className = primary ? 'act pri' : 'act';
  b.textContent = label;
  b.dataset.action = label; // what a pass restores focus to (renderLists)
  b.addEventListener('click', async () => {
    if (b.disabled) return;
    b.disabled = true;
    try {
      await onClick();
    } finally {
      b.disabled = false;
    }
  });
  return b;
}

// --- requests and failures (§9.2) ---

// request is every call a person waits for: it never throws, and it says
// what went wrong in the server's own words (§8.3), or in ours when there
// was no answer at all. On /api/* a 401 means the cookie expired under a
// pinned tab: the reload lands on the token field. /login is the one caller
// whose 401 is an ordinary answer, a wrong token.
async function request(path, method, body, { reloadOn401 = true } = {}) {
  const init = { method };
  if (body !== undefined) {
    init.headers = { 'Content-Type': 'application/json' };
    init.body = JSON.stringify(body);
  }
  let res;
  try {
    res = await fetch(path, init);
  } catch {
    return { ok: false, status: 0, error: 'could not reach the server — check the connection and try again' };
  }
  if (res.status === 401 && reloadOn401) {
    location.reload();
    return { ok: false, status: 401, error: null };
  }
  let data = null;
  try {
    data = await res.json();
  } catch {
    // a 204, or a proxy's HTML error page
  }
  if (res.ok) return { ok: true, status: res.status, data };
  return { ok: false, status: res.status, error: data?.error ?? `request failed (${res.status})` };
}

// userAction is request plus §9.2's failure rules: a failed action is a
// toast at once, and every success is followed by a /api/state fetch — the
// response makes the row right, the refetch makes the badge, the title and
// the zone split right (§8.2).
async function userAction(path, method, body) {
  const result = await request(path, method, body);
  if (result.ok) {
    refreshState();
  } else if (isUnknownPreset(result)) {
    await repairPresets();
  } else if (result.error) {
    toast(result.error);
  }
  return result;
}

// A 400 naming an unknown preset means this client holds a config the
// server has since reloaded (§5.5). It is the one error the user cannot act
// on and the app can fix itself: refetch /api/config and redraw the chip
// rows in place — the capture bar's and an open editor's — keeping typed
// text, the open editor and the picker's pending value. The full reload
// stays governed by the quiet-moment rule, which these very states hold.
function isUnknownPreset(result) {
  return result.status === 400 && result.error?.startsWith('unknown preset ');
}

async function repairPresets() {
  const result = await request('/api/config', 'GET');
  if (!result.ok) {
    if (result.error) toast(result.error);
    return;
  }
  config = result.data;
  capture.redrawChips();
  editor?.bar.redrawChips();
  for (const row of rows.values()) {
    // an editing row too: its actions are hidden, not frozen, and come back
    // when the editor closes
    if (row.late) row.acts.replaceChildren(...rowActions(row));
  }
  toast('The preset list changed on the server — the chips are current now, pick again.');
}

// clearRow is the one optimistic path: Clear shows a 5-second undo toast
// that calls /undone. There are no confirm dialogs anywhere.
async function clearRow(row) {
  const result = await userAction('/api/reminders/' + row.id + '/done', 'POST');
  if (!result.ok) return;
  toast('Cleared.', {
    action: 'Undo',
    ms: UNDO_MS,
    onAction: () => userAction('/api/reminders/' + row.id + '/undone', 'POST'),
  });
}

// toast writes into the one aria-live region, so errors and the undo offer
// are both announced. The Undo is a real button reachable by Tab; there is
// deliberately no undo shortcut (§9.2).
function toast(message, { action, onAction, ms = ERROR_TOAST_MS } = {}) {
  const el = document.createElement('div');
  el.className = 'toast';
  const text = document.createElement('span');
  text.textContent = message;
  el.append(text);
  const dismiss = () => el.remove();
  if (action) {
    const b = document.createElement('button');
    b.type = 'button';
    b.className = 'act pri';
    b.textContent = action;
    b.addEventListener('click', () => {
      dismiss();
      onAction();
    });
    el.append(b);
  }
  // a tap dismisses it: on a short phone a toast over the picker sits on
  // its summary line, which you want to read again before retrying
  el.addEventListener('click', (e) => {
    if (e.target === el || e.target === text) dismiss();
  });
  $('toasts').append(el);
  setTimeout(dismiss, ms);
}

// The poll's failures are counted, never toasted: a toast every 45 seconds
// for the length of a network blip, on a tab nobody is looking at, is
// noise. After two in a row one quiet line appears under the capture bar,
// naming when the list was last good; the list and the badge are kept
// as they were — stale but labelled beats empty.
let pollFailures = 0;
let lastPollOk = performance.now();

function pollFailed() {
  pollFailures++;
  if (pollFailures < OFFLINE_AFTER_FAILURES) return;
  const minutes = Math.floor((performance.now() - lastPollOk) / 60000);
  const ago = minutes < 1 ? 'less than a minute ago'
    : minutes < 60 ? minutes + (minutes === 1 ? ' minute ago' : ' minutes ago')
    : Math.floor(minutes / 60) + ' h ago';
  $('offline').textContent = 'Not connected — last updated ' + ago;
  $('offline').hidden = false;
}

function pollSucceeded() {
  pollFailures = 0;
  lastPollOk = performance.now();
  $('offline').hidden = true;
}

// reloadWhenQuiet is §5.5's "any difference means reload", held for a quiet
// moment: reloading mid-sentence would discard what is typed in the capture
// bar, an open editor, or a half-chosen picker. The poll that finds all
// three idle takes it.
function reloadWhenQuiet() {
  if (capture.input.value !== '' || editor || picker) return;
  location.reload();
}

init();
