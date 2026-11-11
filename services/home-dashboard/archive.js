import { camera, playbackOrigin, store } from './profile.js';

const byId = id => document.getElementById(id);
const workspace = byId('workspace');
const historyView = byId('history-view');
const dayInput = byId('archive-date');
const timeInput = byId('archive-time');
const timeline = byId('timeline');
const list = byId('recording-list');
const video = byId('archive-video');
const status = byId('playback-status');
const download = byId('download-clip');
const zoom = byId('timeline-zoom');
const maxClipSeconds = 120;
const formatTime = stamp => new Date(stamp).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' });
const views = { overview: 'Overview', live: 'Live View', playback: 'Playback', storage: 'Storage', integrations: 'Integrations', export: 'Export' };

let dayRanges = [];
let clip;
let selection;
let cursor;
let visibleBounds;
let currentView;
let historyRequest = 0;
let historyLoading = false;

function localDate(date) {
  const { year, month, day } = Object.fromEntries(new Intl.DateTimeFormat('en-CA', {
    year: 'numeric', month: '2-digit', day: '2-digit'
  }).formatToParts(date).map(part => [part.type, part.value]));
  return `${year}-${month}-${day}`;
}

function dayBounds() {
  const start = new Date(`${dayInput.value || localDate(new Date())}T00:00:00`);
  const end = new Date(start);
  end.setDate(end.getDate() + 1);
  return [+start, +end];
}

function playbackURL(endpoint, params, path = camera.id) {
  const url = new URL(endpoint, playbackOrigin);
  url.search = new URLSearchParams({ path, ...params });
  return url;
}

async function recordings(start, end, path = camera.id) {
  const url = playbackURL('/list', { start: new Date(start).toISOString(), end: new Date(end).toISOString() }, path);
  const response = await fetch(url, { cache: 'no-store', signal: AbortSignal.timeout(10000) });
  if (response.status === 404) return [];
  if (!response.ok) throw new Error(`Archive returned HTTP ${response.status}`);
  const ranges = (await response.json()).map(row => {
    const begin = Date.parse(row.start);
    return { begin, end: begin + row.duration * 1000, sourceStart: row.start };
  }).filter(range => Number.isFinite(range.begin) && range.end > range.begin)
    .sort((a, b) => a.begin - b.begin);
  if (!store || !ranges.length) return ranges;
  // MediaMTX can list a single segment even when its frame timestamps have a hole.
  const coverage = await fetch('./coverage.json', { cache: 'no-store', signal: AbortSignal.timeout(5000) });
  if (!coverage.ok) throw new Error('Saved video index unavailable');
  const indexed = (await coverage.json())[path] ?? [];
  return ranges.flatMap(range => indexed.flatMap(([first, last]) => {
    const begin = Math.max(first, range.begin);
    const finish = Math.min(last, range.end);
    return finish > begin ? [{ begin, end: finish, sourceStart: begin === range.begin ? range.sourceStart : new Date(begin).toISOString() }] : [];
  }));
}

function stopPlayback(message = '') {
  clip = undefined;
  video.pause();
  video.removeAttribute('src');
  video.load();
  video.hidden = true;
  byId('playback-empty').hidden = false;
  status.textContent = message;
  updateTransport();
}

function playAt(stamp) {
  if (!Number.isFinite(stamp)) return;
  navigate('playback');
  cursor = stamp;
  if (!visibleBounds || stamp < visibleBounds[0] || stamp > visibleBounds[1]) renderTimeline();
  const range = dayRanges.find(item => stamp >= item.begin && stamp < item.end);
  if (!range) {
    stopPlayback(`No recording at ${formatTime(stamp)} · Choose a blue interval`);
    return;
  }
  const end = Math.min(stamp + maxClipSeconds * 1000, range.end, dayBounds()[1]);
  const start = stamp === range.begin ? range.sourceStart : new Date(stamp).toISOString();
  const params = { start, duration: ((end - stamp) / 1000).toFixed(3) };
  clip = { start: stamp, end, limit: Math.min(range.end, dayBounds()[1]), params, camera };
  selection = clip;
  video.src = playbackURL('/get', params).href;
  video.hidden = false;
  byId('playback-empty').hidden = true;
  status.textContent = `Loading video from ${formatTime(stamp)}…`;
  updateTransport();
  // Native controls remain available when the browser requires a play gesture.
  video.play().catch(() => {});
}

function timelineBounds() {
  const [start, end] = dayBounds();
  const length = Math.min(end - start, Number(zoom.value) * 3600000);
  const anchor = cursor ?? dayRanges.at(-1)?.end ?? Math.min(end, Date.now());
  const left = Math.max(start, Math.min(end - length, anchor - length * .75));
  return [left, left + length];
}

function renderTimeline() {
  visibleBounds = timelineBounds();
  const [start, end] = visibleBounds;
  timeline.replaceChildren();
  byId('timeline-ticks').replaceChildren(...[0, .25, .5, .75, 1].map(part => {
    const tick = document.createElement('span');
    tick.textContent = new Date(start + part * (end - start)).toLocaleTimeString([], { hour: 'numeric', minute: '2-digit', ...(store && end - start <= 120000 ? { second: '2-digit' } : {}) });
    return tick;
  }));
  for (const range of dayRanges) {
    const begin = Math.max(start, range.begin);
    const finish = Math.min(end, range.end);
    if (finish <= begin) continue;
    const bar = document.createElement('button');
    bar.type = 'button';
    bar.title = `Play ${formatTime(begin)} – ${formatTime(finish)}`;
    bar.setAttribute('aria-label', bar.title);
    bar.style.left = `${100 * (begin - start) / (end - start)}%`;
    bar.style.width = `${100 * (finish - begin) / (end - start)}%`;
    bar.addEventListener('click', event => {
      const relative = event.detail ? Math.max(0, Math.min(1, event.offsetX / bar.clientWidth)) : 0;
      playAt(Math.min(finish - 1, Math.round(begin + (finish - begin) * relative)));
    });
    timeline.append(bar);
  }
  const marker = document.createElement('div');
  marker.className = 'playhead';
  marker.setAttribute('aria-hidden', 'true');
  timeline.append(marker);
  updateTransport();
}

function playbackPosition() {
  return clip ? Math.min(clip.end, clip.start + video.currentTime * 1000) : cursor;
}

function updateTransport() {
  const position = playbackPosition();
  const marker = timeline.querySelector('.playhead');
  const [start, end] = visibleBounds ?? dayBounds();
  if (marker) {
    marker.hidden = position == null || position < start || position > end;
    if (!marker.hidden) marker.style.left = `${100 * (position - start) / (end - start)}%`;
  }
  byId('playback-clock').textContent = position == null ? '—' : formatTime(position);
  byId('toggle-play').textContent = clip && !video.paused ? 'Pause' : 'Play';
  byId('backward').disabled = position == null;
  byId('forward').disabled = position == null;
  byId('previous-interval').disabled = !dayRanges.some(range => range.end <= (position ?? Infinity));
  byId('next-interval').disabled = !dayRanges.some(range => range.begin > (position ?? -Infinity));
}

async function refreshHistory() {
  const request = ++historyRequest;
  historyLoading = true;
  const [start, end] = dayBounds();
  const availableUntil = Math.min(end, Date.now());
  try {
    const ranges = availableUntil > start ? await recordings(start, availableUntil) : [];
    if (request !== historyRequest) return false;
    dayRanges = ranges;
    renderTimeline();
    list.replaceChildren();
    for (const range of dayRanges) {
      const begin = Math.max(start, range.begin);
      const finish = Math.min(end, range.end);
      if (finish <= begin) continue;
      const label = `${formatTime(begin)} – ${formatTime(finish)}`;
      const button = document.createElement('button');
      button.type = 'button';
      button.textContent = `Play ${label}`;
      button.addEventListener('click', () => playAt(begin));
      list.append(button);
    }
    byId('archive-summary').textContent = dayRanges.length
      ? `${dayRanges.length} recorded ${dayRanges.length === 1 ? 'interval' : 'intervals'}`
      : 'No recordings for this date.';
    return true;
  } catch {
    if (request !== historyRequest) return false;
    dayRanges = [];
    renderTimeline();
    list.replaceChildren();
    byId('archive-summary').textContent = 'Could not read recordings. Check that the local recorder is running and try Refresh.';
    return false;
  } finally {
    if (request === historyRequest) historyLoading = false;
  }
}

async function refreshRecorderStatus() {
  const now = Date.now();
  const label = byId('recorder-status');
  const path = camera.id;
  try {
    const ranges = await recordings(now - 5 * 60 * 1000, now, path);
    if (camera.id !== path) return;
    const saved = ranges.length ? formatTime(Math.max(...ranges.map(range => range.end))) : '';
    label.textContent = saved ? `Saved through ${saved}` : 'No video saved in the last five minutes';
    byId('summary-saved').textContent = saved || 'No recent footage';
    byId('integration-archive').textContent = saved ? `Available · Saved through ${saved}` : 'Available · No recent footage';
  } catch {
    if (camera.id !== path) return;
    label.textContent = 'Recorder unavailable';
    byId('summary-saved').textContent = 'Unavailable';
    byId('integration-archive').textContent = 'Unavailable';
  }
}

async function refreshStorage() {
  const note = byId('storage-note');
  note.textContent = 'Loading archive coverage…';
  const path = camera.id;
  try {
    const now = Date.now();
    const ranges = await recordings(now - 7 * 86400000, now, path);
    if (camera.id !== path) return;
    const minutes = Math.floor(ranges.reduce((total, range) => total + range.end - range.begin, 0) / 60000);
    byId('storage-duration').textContent = minutes >= 60 ? `${Math.floor(minutes / 60)} h ${minutes % 60} min` : minutes ? `${minutes} min` : ranges.length ? 'Under 1 min' : '0 min';
    byId('storage-count').textContent = ranges.length;
    note.textContent = ranges.length ? `Oldest available · ${new Date(ranges[0].begin).toLocaleString()}` : 'There is no available footage in the retention window.';
  } catch {
    if (camera.id !== path) return;
    byId('storage-duration').textContent = '—';
    byId('storage-count').textContent = '—';
    note.textContent = 'Could not read archive coverage.';
  }
}

function showView(view) {
  if (!Object.hasOwn(views, view)) view = 'overview';
  if (view === currentView) return;
  const monitor = ['overview', 'live', 'playback'].includes(view);
  currentView = view;
  workspace.dataset.view = view;
  byId('view-title').textContent = views[view];
  for (const link of document.querySelectorAll('.main-nav a')) {
    if (link.hash === `#${view}`) link.setAttribute('aria-current', 'page');
    else link.removeAttribute('aria-current');
  }
  historyView.hidden = !monitor;
  byId('monitor').hidden = !monitor || (store && view === 'overview');
  byId('live-view').hidden = view === 'playback';
  byId('playback-view').hidden = view !== 'playback';
  byId('reconnect').hidden = view === 'playback';
  if (view === 'playback') {
    byId('status').textContent = 'Playback';
    byId('status').dataset.state = 'playback';
    byId('details').textContent = 'Saved video';
    byId('live-message').hidden = true;
  } else {
    stopPlayback();
  }
  for (const panel of ['storage', 'integrations', 'export']) byId(`${panel}-view`).hidden = view !== panel;
  if (view === 'storage') refreshStorage();
  if (view === 'export') {
    byId('export-status').textContent = '';
    byId('export-selection').textContent = selection
      ? `${selection.camera.name} · ${new Date(selection.start).toLocaleString()}`
      : 'Choose a recording in Playback to export a clip.';
    download.disabled = !selection;
  }
}

function navigate(view) {
  if (location.hash !== `#${view}`) location.hash = view;
  showView(view);
}

const today = new Date();
dayInput.value = dayInput.max = localDate(today);
const oldest = new Date(today);
oldest.setDate(oldest.getDate() - 7);
dayInput.min = localDate(oldest);
timeInput.value = '00:00:00';
window.addEventListener('hashchange', () => showView(location.hash.slice(1)));
showView(location.hash.slice(1));
renderTimeline();
refreshHistory();
refreshRecorderStatus();
setInterval(() => {
  dayInput.max = localDate(new Date());
  refreshRecorderStatus();
  if (!historyView.hidden && !historyLoading && !list.contains(document.activeElement) && !timeline.contains(document.activeElement)) refreshHistory();
}, 15000);

byId('refresh-history').addEventListener('click', refreshHistory);
byId('refresh-storage').addEventListener('click', refreshStorage);
zoom.addEventListener('change', renderTimeline);
document.addEventListener('cctv-camera', () => {
  stopPlayback();
  cursor = undefined;
  dayRanges = [];
  renderTimeline();
  list.replaceChildren();
  byId('archive-summary').textContent = 'Loading recordings…';
  refreshHistory();
  refreshRecorderStatus();
  if (currentView === 'storage') refreshStorage();
});
dayInput.addEventListener('change', () => {
  if (!dayInput.reportValidity()) return;
  stopPlayback();
  cursor = undefined;
  dayRanges = [];
  renderTimeline();
  list.replaceChildren();
  byId('archive-summary').textContent = 'Loading recordings…';
  refreshHistory();
});
byId('seek-form').addEventListener('submit', async event => {
  event.preventDefault();
  const stamp = Date.parse(`${dayInput.value}T${timeInput.value}`);
  if (await refreshHistory()) playAt(stamp);
});
async function playLatest() {
  if (!await refreshHistory()) return;
  const last = dayRanges.at(-1);
  if (!last) return;
  playAt(Math.floor(Math.max(dayBounds()[0], last.begin, last.end - 30000) + 1));
}
byId('play-latest').addEventListener('click', playLatest);
byId('overview-latest').addEventListener('click', playLatest);
timeline.addEventListener('click', event => {
  if (event.target !== timeline) return;
  const [start, end] = visibleBounds;
  playAt(Math.floor(start + (end - start) * event.offsetX / timeline.clientWidth));
});
byId('toggle-play').addEventListener('click', () => {
  if (!clip) playLatest();
  else if (video.paused) video.play().catch(() => {});
  else video.pause();
});
byId('backward').addEventListener('click', () => playAt(Math.max(dayBounds()[0], playbackPosition() - 10000)));
byId('forward').addEventListener('click', () => playAt(Math.min(dayBounds()[1] - 1, playbackPosition() + 10000)));
byId('previous-interval').addEventListener('click', () => {
  const previous = dayRanges.findLast(range => range.end <= (playbackPosition() ?? Infinity));
  if (previous) playAt(Math.max(dayBounds()[0], previous.begin));
});
byId('next-interval').addEventListener('click', () => {
  const next = dayRanges.find(range => range.begin > (playbackPosition() ?? -Infinity));
  if (next) playAt(Math.max(dayBounds()[0], next.begin));
});
for (const event of ['timeupdate', 'play', 'pause']) video.addEventListener(event, updateTransport);
video.addEventListener('playing', () => {
  if (clip) status.textContent = `Playing saved video from ${formatTime(clip.start)}`;
});
video.addEventListener('error', () => {
  if (clip) status.textContent = 'Playback could not load. Refresh the archive and choose a recorded interval.';
});
video.addEventListener('ended', () => {
  if (!clip) return;
  if (clip.end + 1000 < clip.limit) playAt(clip.end);
  else status.textContent = `End of available video at ${formatTime(clip.end)}`;
});
download.addEventListener('click', async () => {
  const selected = selection;
  const length = byId('export-duration');
  const feedback = byId('export-status');
  if (!selected || !length.reportValidity()) return;
  const duration = Math.min(Number(length.value), (selected.limit - selected.start) / 1000);
  download.disabled = true;
  feedback.textContent = 'Preparing the MP4…';
  try {
    const response = await fetch(playbackURL('/get', { start: selected.params.start, duration: duration.toFixed(3), format: 'mp4' }, selected.camera.id), { signal: AbortSignal.timeout(60000) });
    if (!response.ok) throw new Error(`Download returned HTTP ${response.status}`);
    const blob = await response.blob();
    if (!blob.size || !blob.type.startsWith('video/mp4')) throw new Error('No MP4 was returned');
    const url = URL.createObjectURL(blob);
    const link = document.createElement('a');
    link.href = url;
    link.download = `${selected.camera.id.replaceAll('_', '-')}-${new Date(selected.start).toISOString().replace(/[:.]/g, '-')}.mp4`;
    link.click();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
    feedback.textContent = 'MP4 sent to your browser’s downloads.';
  } catch {
    feedback.textContent = 'Could not download this clip. Try again from the saved interval.';
  } finally {
    download.disabled = !selection;
  }
});
