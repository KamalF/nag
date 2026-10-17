'use strict';

// The Nag page (§9). The client persists nothing (§9.2): the server state is
// the only state.

const $ = (id) => document.getElementById(id);

// The three cadences of §9.3, and the flash. 45 s is for the tab in front of
// you. 5 minutes keeps the favicon of a pinned tab correct without polling for
// a machine that nobody uses. If a hidden tab stops polling, Goal 4 breaks:
// a tab whose favicon you read is hidden by definition.
const VISIBLE_POLL_MS = 45000;
const HIDDEN_POLL_MS = 300000;
const FLASH_MS = 2400;

// The toasts of §9.2: the undo offer lasts five seconds. An error stays a
// little longer, because someone has to read it.
const UNDO_MS = 5000;
const ERROR_TOAST_MS = 7000;

// A failed poll is not a toast: the line appears after this many in a row.
const OFFLINE_AFTER_FAILURES = 2;

// How long a flash request waits for the render that shows its row.
// It is not the duration of the highlight itself: the row can take several
// passes to arrive. If the highlight expired before the row arrived, a slow
// connection looks identical to a reminder that no longer exists.
const FLASH_WAIT_MS = 15000;

// setTimeout truncates its delay to a signed 32-bit integer. A delay longer
// than ~24.8 days wraps and fires immediately (§9.3).
const MAX_TIMER_MS = 2000000000;

// §9.3 pins the drawn favicon. The two colors are hardcoded on purpose, and
// not read from a custom property. A tab strip is not a page. The dot must
// stay legible against a light and a dark browser chrome, and no media query
// reports which one Firefox painted. BASE_TITLE is a constant for a similar
// reason: if the code read document.title, the prefix compounds into
// "(3) (2) Nag" on the second call.
const BADGE_FILL = '#E0A33E';
const BADGE_INK = '#212A2E';
const BASE_TITLE = 'Nag';
const PLAIN_FAVICON = '/icons/32.png';

let config = null;

// channels holds /api/channels: [{name, enabled}] ORDER BY name. This is the
// chip order in both places that draw it (§9.2). The URL is never in it.
let channels = [];

// The state of the poll. stateETag and lastState are a pair: the 304 path
// renders again from the list already in memory. If the code stores an ETag
// without the body that it describes, every later poll revalidates against a
// body that is not on screen. Each 304 then renders the wrong list again
// (§9.3).
let stateETag = null;
let lastState = null;

// clockOffsetMs is the server clock minus the clock of this device. now() is
// pinned to seconds, to match every timestamp that the API sends (§8.2).
let clockOffsetMs = 0;

let stateAbort = null;
let dueTimer = null;
let pollTimer = null;

// A flash is a request, not a style. The row that a click names can be
// absent from the page, because an aborted refresh resolves and renders
// nothing. pendingFlash is the request that waits for its row. litRow is the
// row currently painted, so that clearFlash can find it. The keyed render
// below reuses the node, so the highlight stays on the row through every pass.
let pendingFlash = null;
let litRow = null;
let flashTimer = null;

// §9.5: one feature-detect boolean, computed once, before any of the push
// machinery runs. A Firefox private window, an older desktop Safari and any
// non-secure origin fail at least one of these tests. There, a naive
// register() call throws a TypeError, and the TypeError takes down the rest
// of the load, the list included. Losing push must cost push and nothing else.
const pushSupported =
  'serviceWorker' in navigator && 'PushManager' in window && 'Notification' in window;

// pushAvailable starts at pushSupported. It becomes false if registration
// itself fails. The notification slot of §9.8 reads it.
let pushAvailable = pushSupported;

// swRegistration holds the *promise* that registerWorker returns. The code
// awaits it wherever it needs the registration. Never call register a second
// time to "get" it (§9.5).
let swRegistration = null;

// repostFailed remembers the re-post outcome of this load. push_subscribed is
// instance-wide (§8.2). Take a laptop whose row the server dropped and whose
// re-post then failed: the flag still reads true, because the phone is
// subscribed. The slot then reports that everything is fine while this device
// gets no notifications. The re-post of §9.8 exists to prevent that outcome,
// so its own result is the signal.
let repostFailed = false;

// §9.8: the condition of the slot reads push_subscribed. If the slot is drawn
// before the unconditional re-post resolves, it flashes "Turn on
// notifications" on a healthy device while the re-post repairs its row.
// That is an invitation to fix what the re-post already fixes. And it is on
// the one control whose whole design is to be absent when things work. The
// lists never wait. Only the slot does.
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
    // after the 401 check, the two reads are independent: one round trip
    // does both
    const [cfgRes, chRes] = await Promise.all([fetch('/api/config'), fetch('/api/channels')]);
    if (!cfgRes.ok) throw new Error('config ' + cfgRes.status);
    if (!chRes.ok) throw new Error('channels ' + chRes.status);
    config = await cfgRes.json();
    channels = await chRes.json();
  } catch {
    // a failed boot must retry, not leave a dead page: there is no list yet
    // to keep
    setTimeout(init, VISIBLE_POLL_MS);
    return;
  }
  buildCapture();
  wirePicker();
  document.addEventListener('keydown', onKeydown);
  // §9.2: autofocus on desktop only. On a phone, autofocus opens the keyboard
  // over the list on every load. That works against Goal 4, where
  // glanceability matters.
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
  refreshState(); // the state that follows decides the slot (§9.8)
}

// --- service worker (§9.5) ---

// Registration is unconditional, on every load. It needs no gesture and no
// permission. Notification.requestPermission() and pushManager.subscribe()
// are the calls that need them (§9.8). Registration is idempotent, so the
// repeat costs one no-op against the registration store of the browser. It is
// also the only way for a skipWaiting deploy to reach a tab pinned for months.
// The code keeps the registration object: never call register a second time
// to "get" it.
async function registerWorker() {
  if (!pushSupported) return null;
  try {
    const registration = await navigator.serviceWorker.register('/sw.js');
    // init retries itself on a failed boot, and a deploy fails the state
    // fetch and the worker script fetch together. So a later registration
    // that succeeds must set pushAvailable back to true. Otherwise one unlucky
    // first load leaves the slot reading "this browser cannot show
    // notifications" on a browser that can (§9.8).
    pushAvailable = true;
    return registration;
  } catch (err) {
    // a syntax error in sw.js, a hard-refresh race, a browser with workers
    // disabled by policy: nothing to retry and nothing that the user can do.
    // The code catches the error and does not let it reject: an unhandled
    // rejection on the load path must not stop the poll from starting (§9.5).
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
    // this is the whole post-login path: a reload runs init again under the
    // new cookie (§8.1)
    location.reload();
    return;
  }
  $('login-error').textContent = result.error ?? 'wrong token';
}

// --- the bar: capture and row editor (§9.2) ---

// createBar is the one control behind both the capture bar and the row
// editor: same component, same keys, same behavior (§9.2). It is a
// single-line input and the preset chips in file order (§5.4). A chip carries
// its own time and saves on the click (Goal 1). Enter means whatever the
// surface saves (§9.4), so each caller says what that is.
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
  // attached is the extra_channels of the row in an editor, and undefined in
  // the capture bar. The selector decides on its own whether to render at all.
  const selector = channelSelector(attached);
  if (selector) el.append(selector.el);

  const bar = {
    el,
    input,
    text: () => input.value.trim(),
    // the full list that the channel chips show as selected, orphans
    // included. An editor always sends it and never omits the key (§9.2).
    selectedChannels: () => selector?.selected() ?? [],
    resetChannels: () => selector?.reset(),
    // every chip is disabled while the input is empty or whitespace-only. A
    // chip that is visibly not ready is better than a toast that says so.
    // Every chip is also disabled while a save is in flight, so a double-tap
    // cannot create two reminders (§9.2).
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
      const chip = (label, onClick) => {
        const b = document.createElement('button');
        b.type = 'button';
        b.className = 'chip';
        b.textContent = label;
        b.addEventListener('click', () => onClick(b));
        chips.append(b);
      };
      for (const p of config.presets) {
        chip(p.label, () => bar.submit(() => onTime({ preset: p.key })));
      }
      // "Pick a time" is not a preset (§5.4). The client renders it after
      // the configured ones, and the Set reminder button of the picker is its
      // save.
      chip('Pick a time', (pick) =>
        openPicker({ opener: pick, from: pickFrom?.(), onSet: (dueAt) => bar.submit(() => onTime({ due_at: dueAt })) }));
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

// channelSelector is the "Also send to" control of §9.2: a toggle that
// expands a row of channel chips. The capture bar renders it only when the
// catalog is non-empty, because fan-out is opt-in and a fresh install has no
// channels. An editor renders it whenever the catalog is non-empty *or* the
// row carries names. After `nag channel rm` on the last channel, a control
// that is not rendered sends [] on every save. That silently wipes the
// orphans that the server exists to preserve.
//
// "Grayed and unselectable" is a rule about adding, never about removing:
// - a disabled channel already on the row renders selected and grayed, and
//   you can deselect it. If you disable a channel, its name must stay
//   removable from the row.
// - a name that the catalog does not list (an orphan) gets its own grayed
//   chip, selected. After you deselect it, you cannot select it again. You
//   can carry an orphan forward, but never introduce one (§8.3).
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
      // deselection always works. Selection needs an enabled, known channel.
      b.disabled = !chip.selected && !chip.selectable;
    };
    // a chip that cannot be selected is disabled, so a click always toggles
    b.addEventListener('click', () => {
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
    // after each save the selection resets: nothing is remembered (§9.2)
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
    // the client resolves default_preset: the server has no fallback (§8.3)
    onEnter: () => createReminder({ preset: config.default_preset }),
  });
  $('capture').append(capture.el);
  $('capture').hidden = false;
}

// createReminder posts the text of the capture bar with one timing field. On
// success, it clears the input, so that the bar is ready for the next capture.
async function createReminder(timing) {
  const text = capture.text();
  if (text === '') return false;
  const result = await userAction('/api/reminders', 'POST',
    { text, extra_channels: capture.selectedChannels(), ...timing });
  // a save that fails leaves what you typed exactly where it is, next to
  // its toast (§9.2)
  if (!result.ok) return false;
  // only the input stays editable while a save is in flight. Text typed in
  // the meantime is the next reminder, so it stays.
  if (capture.text() === text) capture.input.value = '';
  capture.updateEnabled();
  capture.resetChannels();
  return true;
}

// --- the row editor (§9.2) ---

// The row editor is the capture bar again, prefilled with the raw text,
// markdown links included. One editor at a time: to open another closes the
// first, unless the first holds unsaved text. Unsaved text is never thrown
// away. The time of the row stays on the row beside the editor. There you
// read the current due time while you choose a new one (§9.6).
let editor = null;

function openEditor(row) {
  if (editor) {
    // never discard typed text. A click on the row of the open editor only
    // focuses that editor again. A click on another row does the same while
    // the open editor holds unsaved text.
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

// closeEditor puts the row back and runs a pass. The pass reconciles what
// the polls skipped while the editor was open. This includes the removal of a
// row that another device cleared in the meantime.
function closeEditor() {
  const { row, bar } = editor;
  editor = null;
  bar.el.remove();
  row.txt.hidden = false;
  row.acts.hidden = false;
  row.li.classList.remove('editing');
  row.editing = false;
  if (lastState) renderState(lastState);
  const edit = row.li.querySelector('[data-action="Edit"]');
  // saved from the picker: the chip that opened the sheet was removed with
  // the editor, so the sheet gives focus to Edit when it closes
  if (picker?.saving && bar.el.contains(picker.opener)) picker.returnTo = edit;
  else edit?.focus({ preventScroll: true });
}

// replaceInState swaps the object of one reminder in the last state, in
// place. The content of the row becomes right at once. The zone of the row
// stays the answer of the server, from the refetch that follows every
// mutation.
function replaceInState(reminder) {
  if (!lastState) return;
  for (const list of [lastState.overdue, lastState.later]) {
    const i = list.findIndex((r) => r.id === reminder.id);
    if (i >= 0) list[i] = reminder;
  }
}

// saveEdit sends every field that the editor shows (§9.2): text and
// extra_channels on every save, plus a timing field only when a chip or the
// picker chose one. Absent-means-unchanged (§8.3) makes the unchanged fields
// harmless. So there is one body shape and no branch that decides which keys
// go.
async function saveEdit(row, bar, timing) {
  const text = bar.text();
  if (text === '') return false;
  const result = await userAction('/api/reminders/' + row.id, 'PATCH',
    { text, extra_channels: bar.selectedChannels(), ...timing });
  if (!result.ok) return false;
  // close only the editor of this save. If Esc closed it and the same row
  // opened again, the new editor holds typing that this save does not own.
  if (editor?.bar !== bar) return true;
  // the returned object is what makes the row redraw right (§8.2). Put it in
  // the state that the next pass draws from, so that the row never shows its
  // old text for a round trip. The refetch still owns the zone split.
  replaceInState(result.data);
  if (bar.text() === text) {
    closeEditor();
  } else {
    // typed into while the save was in flight: the save landed, and the
    // text typed since stays in the open editor. The unsaved-text check now
    // compares that text against the saved row. The code repaints the time
    // and the marker of that row, beside the editor.
    row.reminder = result.data;
    paintMarker(row.marker, result.data);
    row.when.textContent = whenText(result.data.due_at);
  }
  return true;
}

// --- the picker sheet (§9.6) ---

// The wheels and the calendar only ever move a pending value. Nothing is
// written until Set reminder. This is the one place where a two-step commit
// is right: if each scroll-snap saved, the minute wheel sends one PATCH per
// notch. picker is null while the sheet is down.
let picker = null;

const ITEM_PX = 44; // --item: one wheel row
const WHEEL_SETTLE_MS = 90;
// The sentence says "nothing", not "no notification": a backdated write skips
// its channels too (§4.1). It names no window and no cooldown, because neither
// applies to a moment picked here.
const PAST_SENTENCE = "in the past — it will wait in Needs you now, and nothing will be sent";
const monthFormat = new Intl.DateTimeFormat(undefined, { month: 'long', year: 'numeric' });
const shortWeekdayFormat = new Intl.DateTimeFormat(undefined, { weekday: 'short' });

// openPicker shows the sheet for one save. opener is the chip that opened
// it, and gets focus back on close. from is the due_at of the row when a row
// editor opens the sheet.
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
  // the sheet. The input of its bar is the nearest place to land instead.
  if (target.disabled) target.closest('.bar')?.querySelector('input')?.focus({ preventScroll: true });
  else target.focus({ preventScroll: true });
}

// openingState is the one rule of §9.6 for both modes. If the stored moment
// is still ahead of now(), it opens there, with the hour clamped into the
// range and the minute always 00. Otherwise it opens on the new-reminder
// default: default_time clamped, on today if it is still ahead, else
// tomorrow. The test is on the value that the sheet shows. The minute reset
// can itself move a future row into the past (14:30 today reads back as
// 14:00), and the sheet never opens on the in-the-past warning.
//
// The minute is 00, never the stored minute and never a snap to the nearest
// minute_step. The wheel often cannot show the stored minute (23 is not on a
// 15-minute step), and a snap is a silent move that looks like the stored
// value. 00 is always on the wheel (§5.5). The hour clamp can move the time
// (a preset can give 07:00 under hour_min = 8). That is accepted: the summary
// shows the moment before the save.
function openingState(from) {
  const { hour_min: lo, hour_max: hi } = config.picker;
  const clamp = (h) => Math.min(Math.max(h, lo), hi);
  if (from !== undefined) {
    const stored = new Date(from * 1000);
    const date = localMidnight(stored);
    const hour = clamp(stored.getHours());
    if (moment(date, hour, 0) > now()) return { date, hour, minute: 0 };
  }
  const [h, m] = config.picker.default_time.split(':').map(Number);
  const hour = clamp(h);
  const date = localMidnight(new Date(now() * 1000));
  if (moment(date, hour, m) <= now()) date.setDate(date.getDate() + 1);
  return { date, hour, minute: m };
}

// moment reads the chosen wall-clock time in the timezone of the browser and
// returns Unix seconds (§6, §9.3). A time inside a spring-forward gap has no
// local instant. new Date normalizes it forward, as time.Date in Go does. The
// summary shows the moment that will actually be stored.
function moment(date, hour, minute) {
  const d = new Date(date.getFullYear(), date.getMonth(), date.getDate(), hour, minute, 0, 0);
  return Math.floor(d.getTime() / 1000);
}

function pendingMoment() {
  return moment(picker.date, picker.hour, picker.minute);
}

// renderSummary is the confirmation that the user reads before pressing Set:
// the full date with the time emphasized, and the relative line. For a future
// moment, the relative line is the output of whenText, unchanged. For a past
// moment, it is the fixed past sentence.
// The past sentence never says "late" about a time not yet saved (§9.11).
// Past moments are allowed.
function renderSummary() {
  const dueAt = pendingMoment();
  const { date, time } = fullText(dueAt);
  const b = document.createElement('b');
  b.textContent = time;
  $('summary').replaceChildren(document.createTextNode(date + ' at '), b);
  $('relative').textContent = dueAt - now() <= 0 ? PAST_SENTENCE : whenText(dueAt);
}

// localMidnight is the start of the day of d, in the timezone of the browser.
function localMidnight(d) {
  return new Date(d.getFullYear(), d.getMonth(), d.getDate());
}

// weekStart is week_start as a JS day number: Sunday 0, Monday 1.
function weekStart() {
  return config.picker.week_start === 'sunday' ? 0 : 1;
}

// buildWeekdays labels the columns from week_start, in the device locale.
function buildWeekdays() {
  const start = weekStart();
  $('dow').replaceChildren();
  for (let i = 0; i < 7; i++) {
    const span = document.createElement('span');
    // 1 January 2023 was a Sunday
    span.textContent = shortWeekdayFormat.format(new Date(2023, 0, 1 + ((start + i) % 7)));
    $('dow').append(span);
  }
}

// buildCalendar draws the viewed month: today with a thin ring, the chosen
// day amber-filled. Past days are dimmed but still tappable, because
// backdating is sometimes intentional. "Today" and "past" come from now(),
// never from the device clock directly. There is no year stepper and no bound
// on paging.
function buildCalendar() {
  const { view } = picker;
  const start = weekStart();
  const today = localMidnight(new Date(now() * 1000));
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

// buildWheels fills the two wheels: hours hour_min..hour_max, and minutes
// 0..59 by minute_step. The hour range is a hard limit, with no way to pick
// an hour outside it (§9.6).
function buildWheels() {
  const { hour_min: lo, hour_max: hi, minute_step: step } = config.picker;
  const hours = [];
  for (let h = lo; h <= hi; h++) hours.push(h);
  const minutes = [];
  for (let m = 0; m < 60; m += step) minutes.push(m);
  fillWheel($('hours'), hours, picker.hour);
  fillWheel($('minutes'), minutes, picker.minute);
}

// fillWheel: each value is a real button that scrolls itself to the center,
// so tap and keyboard both work. The code reads the selection back from
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

// setPicked is the whole write: POST from the capture bar, PATCH from a row
// editor. It closes the sheet on success only, so a failed save leaves the
// chosen moment on screen.
async function setPicked() {
  const set = $('picker-set');
  if (set.disabled) return;
  const session = picker;
  session.saving = true;
  set.disabled = true;
  const ok = await session.onSet(pendingMoment());
  session.saving = false;
  // Esc closed the sheet during the save, and another sheet can be open
  // now: this save must not close or refocus a session that it does not own
  if (picker !== session) return;
  set.disabled = false;
  if (ok) {
    closePicker();
  } else {
    set.focus(); // the disable blurred it. Keep focus inside the dialog.
  }
}

// trapFocus keeps Tab inside the sheet while it is up. The page behind is
// inert, and to tab into a list that you cannot see is a trap the other way.
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
    // Enter anywhere in the sheet activates Set reminder (§9.4, §9.6). A
    // clicked day or wheel value keeps focus, and Enter must still save from
    // there. The one exception is Cancel: there, a save on Enter does the
    // opposite of the button that the user is on.
    if (e.key === 'Enter' && !e.isComposing && e.target !== $('picker-cancel')) {
      e.preventDefault();
      setPicked();
    }
  });
}

// --- keyboard (§9.4) ---

// onKeydown handles every page-wide key: / or n focus the capture input, and
// Esc closes exactly one thing per press, innermost first. Enter belongs to
// each surface, and each surface handles it. There are no per-preset digit
// shortcuts: the capture input is autofocused, so a digit must type ("renew
// the domain in 2 years"). Alt+digit already switches browser tabs.
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
    // inert in a text field: otherwise the key is unusable in the one place
    // where you are guaranteed to type. Also inert under an open editor or
    // the sheet, because nobody intends to focus the capture bar behind a
    // scrim.
    if (e.target.closest('input, textarea, [contenteditable]') || editor || picker) return;
    e.preventDefault();
    capture.input.focus();
  }
}

// --- notification permission (§9.8) ---

// syncSubscription is the unconditional re-post: if this browser holds a
// local subscription, POST it on every load. It is an upsert. So it costs one
// request and needs no way to ask the server "do you know *this* endpoint".
// push_subscribed is instance-wide (§8.2) and can never answer that. This
// re-post restores a row that the server deleted on a 410 or a 403 (§7.1).
// Without it, this device silently gets no notifications.
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

// renderNotifySlot draws the slot of §9.2. Its steady state is absent: no
// permanent settings control. It runs on every render, so it rebuilds only
// when the answer actually changed. Otherwise the button is destroyed and
// created again every 45 seconds. That drops the focus of a keyboard user and
// loses a click that arrives during an await. And it happens on the one
// control designed to be invisible when things work.
let slotGeneration = 0;
let slotShowing = null;

async function renderNotifySlot(pushSubscribed) {
  const generation = ++slotGeneration;
  const want = await desiredSlot(pushSubscribed);
  // a newer render resolved while this one awaited: the newer one owns the
  // slot. Without this test, both clear the slot and both append.
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
// permission that the user blocked, and only then the button. push_subscribed
// is a backup check. The re-post is awaited before the slot is decided, so
// push_subscribed is almost always redundant. It still catches
// two cases: the re-post failed, or this browser holds a subscription that no
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
    // getSubscription rejects, and does not resolve null, where push is
    // disabled by policy or its service is unreachable. If unhandled, it
    // rejects a render pass every 45 s and leaves the slot frozen: no
    // button, no line, nothing to act on. So the slot shows the same quiet
    // line as for an unsupported browser (§9.5).
    return 'unsupported';
  }
}

// turnOnNotifications runs on the click, because the permission prompt
// requires that gesture. Firefox rejects a request without one. That is why
// nothing about push is asked for on load (§9.8).
async function turnOnNotifications() {
  const registration = await swRegistration;
  if (!registration) return;
  try {
    if ((await Notification.requestPermission()) === 'granted') {
      const existing = await registration.pushManager.getSubscription();
      // an existing subscription goes through the same key check as the
      // re-post. A post that skips the check corrupts the row on the server.
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

// boundToCurrentKey is the "compare keys first, and post the key you
// compared" rule of §9.8. Both entry points go through it. If a subscription
// that nobody examined is posted under the configured key, the vapid_public
// of the row equals that key by construction. A 403 then always takes the
// "matching, so it is a bug" branch of §7.1. The row is kept, an ERROR is
// logged on every sweep, the boot mismatch WARNING cannot see the stale key,
// and the slot reports that everything is fine.
async function boundToCurrentKey(registration, sub) {
  const bound = encodeKey(sub.options.applicationServerKey);
  // A null readback is not a mismatch: every browser new enough to reach
  // this line populates options.applicationServerKey. If null counted as a
  // mismatch, every page load churns unsubscribe/subscribe on a *working*
  // subscription. That is the one thing this function exists to prevent
  // (§9.8).
  if (bound === null || bound === config.vapid_public) return sub;
  // re-posting a subscription still bound to a retired keypair heals
  // nothing: the push service keeps rejecting it (§7.1)
  await sub.unsubscribe();
  return subscribeWith(registration, config.vapid_public);
}

async function subscribeWith(registration, key) {
  // register() resolves as soon as the registration exists. But subscribe()
  // rejects with InvalidStateError while the worker is still installing.
  // That is exactly the first-ever visit: the one where the button shows and
  // a press on it is the whole intent. `ready` waits for the active worker
  // and registers nothing (§9.5).
  await navigator.serviceWorker.ready;
  // applicationServerKey is bytes, not the string (§9.8)
  return registration.pushManager.subscribe({
    userVisibleOnly: true,
    applicationServerKey: decodeKey(key),
  });
}

// postSubscription throws on any non-2xx. fetch resolves for a 400 or a
// 500, so an unchecked call cannot tell a stored subscription from a
// rejected one. §8.2 answers 400 on every shape check and 500 on a store
// error. Each of these means that this device is not subscribed.
async function postSubscription(sub, key) {
  const json = sub.toJSON();
  const res = await fetch('/api/push/subscribe', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      endpoint: json.endpoint,
      keys: { p256dh: json.keys.p256dh, auth: json.keys.auth },
      // the key that this subscription is bound to, never the key that the
      // server holds now. The column is only worth having if it records that
      // (§7.1).
      application_server_key: key,
    }),
  });
  if (!res.ok) throw new Error('subscribe ' + res.status);
}

// iOS exposes the Push API only to a Home-Screen install, and every iOS
// browser is WebKit. So this is not a Chrome-versus-Safari question (§7.1).
// desiredSlot checks it before the unsupported line, because on iOS the
// missing PushManager has a cause that the user can act on.
function iosNeedsHomeScreen() {
  const ua = navigator.userAgent;
  const isIOS = /iPad|iPhone|iPod/.test(ua) || (/Macintosh/.test(ua) && navigator.maxTouchPoints > 1);
  const standalone =
    navigator.standalone === true || matchMedia('(display-mode: standalone)').matches;
  return isIOS && !standalone;
}

// A script cannot prompt again for a denied permission. To pretend otherwise
// is worse than to say so (§9.8).
const SLOT_LINES = {
  ios: 'On iPhone, notifications need Nag on your Home Screen: Share, then Add to Home Screen.',
  unsupported: 'This browser cannot show notifications for Nag. Everything else works.',
  denied:
    'Your browser is blocking notifications for Nag. To turn them back on, allow them again in the site settings.',
};

// decodeKey and encodeKey are the only encoding code in app.js. They must be
// exact inverses over unpadded base64url, the alphabet that
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
// message, and every successful mutation. These triggers overlap by
// construction: the refetch of a mutation routinely lands on top of a poll.
// Nothing orders two responses to the same endpoint. So one AbortController
// makes the newest request the only one that can write anything. An aborted
// fetch updates nothing at all: not the list, not the badge, not the clock
// offset, and not the stored ETag (§9.3).
async function refreshState() {
  stateAbort?.abort();
  const controller = new AbortController();
  stateAbort = controller;

  let res;
  try {
    res = await fetch('/api/state', {
      signal: controller.signal,
      // the ETag lives in a variable here. If a stale response overwrites
      // it, the stale ETag stays in use after the wrong render is gone. Every
      // later poll then revalidates against a body that is not on screen
      // (§9.3).
      headers: stateETag ? { 'If-None-Match': stateETag } : {},
    });
  } catch {
    // aborted by a newer request, which counts for nothing, or offline,
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
    // The ETag excludes server_time so that the steady-state poll is cheap
    // (§8.2). That makes this the common case: no body, but every time string
    // on screen moved all the same. An early return here freezes the time
    // strings until the data changes. On a quiet afternoon, that is hours.
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
    // Without this guard, it rejects refreshState itself. Then every caller
    // (both timers, and the awaited one in handleWorkerMessage) becomes an
    // unhandled rejection or silently loses what follows it.
    return;
  }
  if (controller !== stateAbort) return;
  pollSucceeded();
  stateETag = res.headers.get('ETag');
  lastState = body;
  syncClock(res, body);
  renderState(body);
}

// syncClock prefers the server_time of the body. It falls back to the Date
// header, which is all that a 304 carries (§9.3).
function syncClock(res, state) {
  const serverMs = state ? state.server_time * 1000 : Date.parse(res.headers.get('Date'));
  if (!Number.isNaN(serverMs) && serverMs) clockOffsetMs = serverMs - Date.now();
}

// now() is the skew-corrected clock that every time string reads (§9.11). It
// is pinned to seconds so that it compares directly against due_at.
function now() {
  return Math.floor((Date.now() + clockOffsetMs) / 1000);
}

// armPoll switches cadence and never suspends: see the constants above.
function armPoll() {
  clearInterval(pollTimer);
  pollTimer = setInterval(refreshState, document.hidden ? HIDDEN_POLL_MS : VISIBLE_POLL_MS);
}

function onVisibilityChange() {
  armPoll();
  if (!document.hidden) refreshState();
}

// armDueTimer is the one-shot timer of §9.3. Every render arms it again from
// the first row of `later`, because the server sorts the list (§8.2). The
// server computes the overdue/later split, and this code never recomputes it.
// Without the timer, a row that comes due reads "now" for up to 45 seconds
// while the badge disagrees with it. The timer is armed regardless of
// visibility. Browsers throttle background timers to roughly one a minute.
// That degrades the timer into a slightly-late poll, not a missed one.
function armDueTimer(state) {
  clearTimeout(dueTimer);
  const next = state.later[0];
  if (!next) return;
  // Without the clamp, one reminder six months out fires the timer at once.
  // The timer polls, arms again from the same first element and fires again.
  // That is a tight loop against the server for as long as the tab is open
  // (§9.3).
  const delay = Math.min((next.due_at - now()) * 1000, MAX_TIMER_MS);
  dueTimer = setTimeout(refreshState, Math.max(delay, 0));
}

function renderState(state) {
  if (state.config_version !== config.config_version) reloadWhenQuiet();
  $('overdue-count').textContent = state.overdue_count;
  // a zone heading is hidden entirely when its list is empty, and the empty
  // state replaces both (§9.2). The headings are shown before the lists are
  // drawn: a row that moves into an empty zone gets its focus restored there,
  // and focus() does nothing inside a hidden section.
  // A row with its editor open stays in its zone whatever the response says
  // (§9.2). That zone stays shown even when the server emptied it. The zone
  // that the server moved the row to counts it only after the editor closes.
  const editingIn = editor?.row.li.parentElement;
  const editingId = editor?.row.id;
  const listed = (list) => list.some((r) => String(r.id) !== editingId);
  const hasOverdue = listed(state.overdue) || editingIn === $('overdue');
  const hasLater = listed(state.later) || editingIn === $('later');
  $('overdue-zone').hidden = !hasOverdue;
  $('later-zone').hidden = !hasLater;
  $('divider').hidden = !(hasOverdue && hasLater);
  $('empty').hidden = hasOverdue || hasLater;
  renderLists(state);
  // the same render pass as the lists drives the badge. That keeps all three
  // outputs correct after a 304 too (§9.3).
  setBadge(state.overdue_count);
  applyPendingFlash();
  armDueTimer(state);
  if (subscriptionSynced) renderNotifySlot(state.push_subscribed);
}

// --- time strings (§9.11) ---

// whenText and fullText are the only two things that format a time, here
// and in the copy of the same table in sw.js (the worker cannot import this
// file). The connectives are English. The clock, weekday and month use the
// locale of the device, in the timezone of the browser (§6).
const clockFormat = new Intl.DateTimeFormat(undefined, { hour: 'numeric', minute: '2-digit' });
const weekdayFormat = new Intl.DateTimeFormat(undefined, { weekday: 'long' });
const dayMonthFormat = new Intl.DateTimeFormat(undefined, { day: 'numeric', month: 'long' });
const dayMonthYearFormat = new Intl.DateTimeFormat(undefined, { day: 'numeric', month: 'long', year: 'numeric' });
const longDateFormat = new Intl.DateTimeFormat(undefined, { weekday: 'long', day: 'numeric', month: 'long' });
const longDateYearFormat = new Intl.DateTimeFormat(undefined,
  { weekday: 'long', day: 'numeric', month: 'long', year: 'numeric' });

// whenText is the short form: the time of a row, the relative line of the
// picker, and the notification body. The first match wins, top to bottom.
// The conditions overlap by design, so the order is the specification. The
// three rows at the top are durations. From "today" down, every row is a
// calendar comparison. A mix of the two makes a boundary drift by up to a
// day, depending on when you glance at it.
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

// fullText is the long form, only for the summary line of the picker:
// weekday, day, month, the year only outside the current year, then the time
// that the summary emphasizes. It returns the two halves so that the caller
// can build that emphasis as nodes (§9.7).
function fullText(dueAt) {
  const due = new Date(dueAt * 1000);
  const sameYear = due.getFullYear() === new Date(now() * 1000).getFullYear();
  return {
    date: (sameYear ? longDateFormat : longDateYearFormat).format(due),
    time: clockFormat.format(due),
  };
}

// calendarDaysBetween is the difference between two local midnights, in
// whole days. It goes through Date.UTC so that a DST change in between (a
// 23- or 25-hour day) cannot round the count the wrong way.
function calendarDaysBetween(from, to) {
  const utc = (d) => Date.UTC(d.getFullYear(), d.getMonth(), d.getDate());
  return Math.round((utc(to) - utc(from)) / 86400000);
}

// every count is singular at 1: "1 day late", "in 1 minute"
function count(n, unit) {
  return n + ' ' + unit + (n === 1 ? '' : 's');
}

// --- badge (§9.3) ---

// setBadge writes the overdue count, never the total, to all three outputs:
// the Badging API where it exists, a canvas-drawn favicon everywhere, and the
// title prefix. Upcoming reminders must never add to the badge. That is
// exactly what makes an icon badge mean something. setBadge(0) is a full
// reset, not a no-op.
function setBadge(count) {
  if ('setAppBadge' in navigator) {
    // clearAppBadge, not setAppBadge(0). Firefox has no Badging API on any
    // platform, so the other two outputs are the ones that always run.
    const badged = count > 0 ? navigator.setAppBadge(count) : navigator.clearAppBadge();
    badged?.catch(() => {});
  }
  document.title = count > 0 ? `(${count}) ${BASE_TITLE}` : BASE_TITLE;
  drawFavicon(count);
}

// drawFavicon paints 32.png at full size. Above zero, it adds a filled circle
// in the bottom-right quadrant with the count in it. The numeral is the count
// up to 9, and the literal 9+ above it. At that point the dot means
// "several". That is all a 12-pixel circle can ever mean, and the exact
// number is in the title and the list.
let badgeDrawn = null;
let badgeGeneration = 0;

function drawFavicon(count) {
  const link = document.querySelector('link[rel=icon]');
  if (!link) return;
  // this runs from every render pass. Without this test, a quiet instance
  // makes a fresh Image, decode, canvas and toDataURL every 45 seconds forever
  if (count === badgeDrawn) return;
  badgeDrawn = count;
  // the zero path writes synchronously and the badged one writes from a
  // promise. Without a generation, the count that a reset clears can
  // overwrite that reset.
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
    // The dot is drawn on a plate-colored disc a pixel and a half wider, so
    // it reads as one object over whatever part of the mark it lands on.
    // Without that ring, the amber dot and the amber row under it merge into
    // a blob at the size that a tab strip actually shows.
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
    // a favicon that cannot be drawn costs the dot and nothing else. Clear
    // the record so that the next render tries again, and does not skip the
    // count as already drawn.
    badgeDrawn = null;
    link.href = PLAIN_FAVICON;
  });
}

// --- messages from the worker (§9.5) ---

// The page half of the switch of §9.5. state-changed arrives on a tab that
// nobody looks at. It shows a reminder that just fired on the favicon
// immediately, not up to five minutes later. So it refreshes and does
// nothing else: a scroll or a flash moves the page under whatever you return
// to.
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
// the worker had no client to talk to. A postMessage to a window that
// openWindow just returned is a race that the worker always loses (§9.5).
// The id counts as exactly the same message. The URL is cleaned immediately,
// so that a refresh does not flash again a row that you dealt with an hour
// ago. And the address never becomes something that you bookmark.
function consumeFocusParam() {
  const id = new URLSearchParams(location.search).get('focus');
  if (id === null) return;
  history.replaceState({}, '', '/');
  handleWorkerMessage({ type: 'focus-reminder', id });
}

// flashRow scrolls to a row and highlights it for ~2.4 s. If the id is in
// neither list after the refresh, the app is focused and nothing else
// happens: no scroll, no flash, no toast, no error. In that case, either
// another device cleared the row while the notification sat in the tray, or
// this device cleared it and the row was purged (§4.4). Both cases are
// ordinary: a notification is never the record of anything (§7.6). Never fall
// back to flashing the first row.
function flashRow(id) {
  clearFlash();
  pendingFlash = { id: String(id), deadline: performance.now() + FLASH_WAIT_MS };
  applyPendingFlash();
}

// applyPendingFlash runs at the end of every render, the only place where it
// can run. scrollIntoView does nothing to an element that is not in the
// document yet. So a row lit from inside renderLists gets painted, but the
// page never moves. A request whose row is in neither list waits, and then
// expires. That is the "nothing happens" of §9.5 for a reminder cleared
// elsewhere. It is also a second chance for a reminder whose render lost a
// race.
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
  row.scrollIntoView({ block: 'center', behavior: scrollBehavior() });
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

// The flash is a class toggle with its values in app.css (§9.7). To remove
// and add the class again restarts the animation when the same row is asked
// for again.
function paintFlash(row) {
  row.classList.remove('pinged');
  void row.offsetWidth;
  row.classList.add('pinged');
}

function rowElement(id) {
  return document.querySelector(`li[data-id="${CSS.escape(String(id))}"]`);
}

// --- reminder text: one markdown link form (§9.10) ---

// LINK is the whole grammar. It is the same regex as flattenLinks on the
// server (a Go test pins the two): [label](target), label non-empty with no
// brackets, target with no whitespace and no ")".
const LINK = /\[([^\[\]]+)\]\(([^\s)]*)\)/g;

// renderText walks the matches and appends nodes: text nodes between, and an
// anchor for each eligible link. So no markup is ever parsed: no innerHTML
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

// rendersAsLink is the eligibility gate, the same one that flattenLinks
// applies: scheme http or https, read off the prefix, and no "(". LINK cuts a
// URL with a paren at the first ")", which gives a link that looks right and
// 404s. So such a URL stays visible as raw markdown instead (§9.10).
function rendersAsLink(target) {
  if (target.includes('(')) return false;
  const lower = target.toLowerCase();
  return lower.startsWith('http:') || lower.startsWith('https:');
}

// --- the keyed list (§9.2) ---

// rows maps a reminder id to its row: the li and the parts that a pass
// updates. A poll must never do to a row what a reload does to the page. So
// a pass reuses the node and does not replace it. Across the 45-second poll
// and the 304 path, that keeps focus inside the actions of a row, the :hover
// and :focus-within reveal, and the scroll anchoring of the browser. The
// inline editor sets row.editing: a pass does not touch such a row at all.
const rows = new Map();

// renderLists is the whole pass: every row is updated in place, moved only
// if it is out of order, and removed only when it is in neither list. The
// server-sorted order with its id tiebreak (§8.2) makes "only what moved,
// moves" stable.
function renderLists(state) {
  // A pass can take focus away from a row action in two ways. It can move
  // the row: placeZone moves a row that went up its zone, and a node move
  // blurs what is inside the node. Or it can rebuild the actions of the row
  // (a row that changed zone). Focus is restored by identity, the same row
  // and the same action, so both cases come back. Typically that is the Edit
  // button that a closing editor just focused.
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

// placeZone walks the list of the zone against the current children with a
// cursor. A node already in place is passed over untouched, and only an
// out-of-place node is inserted. The cursor steps over two kinds of child and
// does not place them. The first is a row with its editor open, which a pass
// must never move: a move blurs the input mid-sentence. The second is a row
// that leaves this zone, which the pass of the other zone or the removal
// above picks up. Without that, one departure reinserts every row below it
// and drops the focus inside them (§9.2).
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
// changed. For an unchanged row, that is the time string and nothing else.
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

// paintMarker is the channel marker of §9.2: a small quiet dot on a row with
// channels attached. It is amber when delivery_error is set. The error is its
// accessible label, and its full text shows in the editor. A title tooltip
// alone is unreachable on the phone, where you notice the amber.
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
  li.dataset.id = id; // the focus-reminder of §9.5 finds the row by this
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
  // a click on the text or the time opens the editor. A click on a link
  // inside the text follows the link instead (§9.2).
  txt.addEventListener('click', (e) => {
    if (!e.target.closest('a')) openEditor(row);
  });
  when.addEventListener('click', () => openEditor(row));
  return row;
}

// rowActions: Clear for every row, plus the quick re-snooze chips on overdue
// rows only. An offset preset resolves from now (§4.1). So "30 min" on a row
// that waits for next Monday quietly pulls it forward by six days.
function rowActions(row) {
  const path = '/api/reminders/' + row.id;
  const actions = [actionButton('Clear', row.late, () => clearRow(row))];
  // the row text cannot be a <button>, because it contains anchors (§9.10).
  // So pointer users click the text, and the keyboard gets a real button.
  const edit = actionButton('Edit', false, () => openEditor(row));
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

// request is every call that a person waits for. It never throws. It says
// what went wrong in the words of the server itself (§8.3), or in ours when
// there was no answer at all. On /api/*, a 401 means that the cookie expired
// under a pinned tab: the reload lands on the token field. /login is the one
// caller whose 401 is an ordinary answer, a wrong token.
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
    // a 204, or the HTML error page of a proxy
  }
  if (res.ok) return { ok: true, status: res.status, data };
  return { ok: false, status: res.status, error: data?.error ?? `request failed (${res.status})` };
}

// userAction is request plus the failure rules of §9.2. A failed action is a
// toast at once. Every success is followed by a /api/state fetch. The
// response makes the row right, and the refetch makes the badge, the title
// and the zone split right (§8.2).
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

// A 400 that names an unknown preset means that this client holds a config
// that the server reloaded since (§5.5). It is the one error that the user
// cannot act on and the app can fix itself. Refetch /api/config and redraw
// the chip rows in place, in the capture bar and in an open editor. Keep the
// typed text, the open editor and the pending value of the picker. The full
// reload still waits for a quiet moment (reloadWhenQuiet). Typed text, an open
// editor and an open picker are exactly what delays that reload.
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
  toast('The preset list changed on the server. The chips are up to date now: pick again.');
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
// are both announced. The Undo is a real button that Tab reaches. There is
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
  // a tap dismisses it. On a short phone, a toast over the picker sits on
  // its summary line, which you want to read again before you retry.
  el.addEventListener('click', (e) => {
    if (e.target === el || e.target === text) dismiss();
  });
  $('toasts').append(el);
  setTimeout(dismiss, ms);
}

// The failures of the poll are counted, never toasted. A toast every 45
// seconds for the length of a network blip, on a tab that nobody looks at, is
// noise. After two failures in a row, one quiet line appears under the
// capture bar. It names when the list was last good. The list and the badge
// stay as they were: stale but labeled is better than empty.
let pollFailures = 0;
let lastPollOk = performance.now();

function pollFailed() {
  pollFailures++;
  if (pollFailures < OFFLINE_AFTER_FAILURES) return;
  const minutes = Math.floor((performance.now() - lastPollOk) / 60000);
  const ago = minutes < 1 ? 'less than a minute ago'
    : minutes < 60 ? count(minutes, 'minute') + ' ago'
    : Math.floor(minutes / 60) + ' h ago';
  $('offline').textContent = 'Not connected — last updated ' + ago;
  $('offline').hidden = false;
}

function pollSucceeded() {
  pollFailures = 0;
  lastPollOk = performance.now();
  $('offline').hidden = true;
}

// reloadWhenQuiet is the "any difference means reload" rule of §5.5, held
// for a quiet moment. A reload mid-sentence discards what is typed in the
// capture bar, an open editor, or a half-chosen picker. The poll that finds
// all three idle does the reload.
function reloadWhenQuiet() {
  if (capture.input.value !== '' || editor || picker) return;
  location.reload();
}

init();
