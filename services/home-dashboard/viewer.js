import { VideoRTC } from './video-rtc.js';
import { camera, cameras, focusCamera, store } from './profile.js';

class CCTVPlayer extends VideoRTC {
  constructor() {
    super();
    this.mode = 'mse';
    this.media = 'video';
  }

  onopen() {
    super.onopen();
    if (this.onmessage) {
      this.onmessage.dashboard = message => {
        if (message.type === 'error') this.ws?.close();
      };
    }
  }
}

customElements.define('cctv-player', CCTVPlayer);
const byId = id => document.getElementById(id);
const workspace = byId('workspace');
const clockFormat = new Intl.DateTimeFormat([], { month: 'short', day: 'numeric', weekday: 'short', hour: 'numeric', minute: '2-digit', second: '2-digit' });
function updateClock() {
  const clock = byId('clock');
  clock.dateTime = new Date().toISOString();
  clock.textContent = clockFormat.format(new Date());
}

function fullscreen(target) {
  return document.fullscreenElement ? document.exitFullscreen() : target.requestFullscreen();
}

function mountStore() {
  const grid = byId('store-grid');
  const focused = byId('live-view');
  focused.replaceChildren();
  const feeds = cameras.map(({ id, name }) => {
    const tile = document.createElement('section');
    tile.className = 'panel camera';
    tile.dataset.camera = id;
    tile.innerHTML = '<div class="camera-bar"><div class="camera-title"><h2></h2></div><div class="camera-actions"><span class="state"></span><button type="button">Focus</button></div></div><div class="viewer"></div><div class="camera-foot"><span></span></div>';
    tile.querySelector('h2').textContent = name;
    const button = tile.querySelector('button');
    button.setAttribute('aria-label', 'Focus ' + name);
    button.addEventListener('click', () => focusCamera(id));
    grid.append(tile);
    const player = document.createElement('cctv-player');
    return { id, tile, player, lastFrame: 0, mounted: false };
  });

  const upstreamById = new Map();
  let upstreamFetchedAt = 0;

  async function pollStatus() {
    try {
      const response = await fetch('./status.json', { cache: 'no-store', signal: AbortSignal.timeout(1500) });
      if (!response.ok) return;
      const data = await response.json();
      if (!data || !data.cameras) return;
      upstreamFetchedAt = performance.now();
      for (const [id, info] of Object.entries(data.cameras)) upstreamById.set(id, info);
      update();
    } catch {}
  }

  function formatUtc(iso) {
    if (!iso || iso === '-') return '';
    const d = new Date(iso);
    return Number.isNaN(d.getTime()) ? '' : d.toLocaleTimeString();
  }

  function mount(feed, target) {
    const { player } = feed;
    if (player.parentElement === target) return;
    feed.mountedAt = performance.now();
    target.append(player);
    if (feed.mounted) return;
    feed.mounted = true;
    const video = player.video;
    video.muted = true;
    function noteFrame() {
      if (player.isConnected) {
        feed.lastFrame = performance.now();
        feed.lastTime = new Date();
        feed.resolution = `${video.videoWidth}×${video.videoHeight}`;
      }
    }
    if (video.requestVideoFrameCallback) {
      const onFrame = () => { noteFrame(); video.requestVideoFrameCallback(onFrame); };
      video.requestVideoFrameCallback(onFrame);
    } else {
      video.addEventListener('timeupdate', () => { if (!video.paused && video.readyState >= 2) noteFrame(); });
    }
    player.src = '/api/ws?src=' + feed.id;
  }

  function statusFor(feed) {
    const now = performance.now();
    const browserLive = Boolean(feed.lastFrame && now - feed.lastFrame < 3500 && !feed.player.video?.paused);
    const up = upstreamFetchedAt && now - upstreamFetchedAt < 6000 ? upstreamById.get(feed.id) : null;
    const upTime = up ? formatUtc(up.lastFrameUtc) : '';
    const reconText = up && up.reconnects > 0 ? ` · Reconnects ${up.reconnects}` : '';
    const res = feed.resolution || (up?.resolution ? up.resolution.replace('x', '×') : '');
    const resPrefix = res ? `${res} · ` : '';
    const kbpsTxt = up?.bitrateKbps ? `${Math.round(up.bitrateKbps)} kbps · ` : '';

    if (browserLive && (!up || up.state === 'live')) {
      const ts = feed.lastTime ? feed.lastTime.toLocaleTimeString() : upTime;
      return ['live', 'Live', `${resPrefix}${kbpsTxt}Frame ${ts}${reconText}`];
    }
    if (up) {
      if (up.state === 'recovering') {
        const label = up.attempt > 1 ? `Recovering (${up.attempt})` : (up.label || 'Recovering');
        const detail = upTime ? `Last frame ${upTime}${reconText}` : `Reconnecting upstream…${reconText}`;
        return ['recovering', label, detail];
      }
      if (up.state === 'starting') {
        return ['waiting', up.label || 'Starting', up.detail || 'Starting upstream session…'];
      }
      if (up.state === 'stalled') {
        const gapTxt = typeof up.gapS === 'number' ? `Gap ${Math.round(up.gapS)}s · ` : '';
        return ['stalled', 'Stalled', `${gapTxt}${upTime ? 'Last frame ' + upTime : 'Waiting for keyframe'}${reconText}`];
      }
      if (up.state === 'offline') {
        return ['offline', 'Offline', upTime ? `Stopped · Last frame ${upTime}` : 'Upstream not running'];
      }
      if (up.state === 'live' && !browserLive) {
        if (feed.player.isConnected && !feed.player.video?.paused && now - (feed.mountedAt || 0) > 5000 && now - (feed.lastResetAt || 0) > 6500) {
          feed.lastResetAt = now;
          if (feed.player.ws) feed.player.ws.close();
          else feed.player.onconnect?.();
        }
        return ['waiting', 'Syncing video', `${resPrefix}Upstream live (${upTime || 'advancing'})${reconText}`];
      }
    }
    return [browserLive ? 'live' : 'waiting', browserLive ? 'Live' : feed.lastTime ? 'Waiting for video' : 'Waiting for first frame',
      feed.lastTime ? `${feed.resolution} · Last frame ${feed.lastTime.toLocaleTimeString()}` : 'No live frame received'];
  }

  function update() {
    updateClock();
    const view = workspace.dataset.view;
    grid.hidden = view !== 'overview';
    for (const feed of feeds) {
      if (view === 'overview') mount(feed, feed.tile.querySelector('.viewer'));
      else if (view === 'live' && feed.id === camera.id) mount(feed, focused);
      else if (feed.player.isConnected) {
        feed.player.remove();
        feed.lastFrame = 0;
      }
      const [state, label, detail] = statusFor(feed);
      const pill = feed.tile.querySelector('.state');
      pill.dataset.state = state;
      pill.textContent = label;
      feed.tile.querySelector('.camera-foot span').textContent = detail;
      if (view === 'live' && feed.id === camera.id) {
        byId('status').dataset.state = state;
        byId('status').textContent = label;
        byId('details').textContent = detail;
        byId('summary-live').textContent = label;
        const up = upstreamById.get(feed.id);
        byId('summary-resolution').textContent = feed.resolution || (up?.resolution ? up.resolution.replace('x', '×') : '—');
        byId('live-message').hidden = state === 'live' || Boolean(feed.lastTime);
        if (!byId('live-message').hidden) byId('live-message').textContent = `${label} — ${detail}`;
      }
    }
    if (view !== 'live') byId('live-message').hidden = true;
    const active = feeds.find(feed => feed.id === camera.id);
    const activeUp = upstreamById.get(camera.id);
    const activeTime = active.lastTime ? active.lastTime.toLocaleTimeString() : formatUtc(activeUp?.lastFrameUtc);
    byId('integration-live').textContent = activeTime ? 'Last frame ' + activeTime : 'Waiting for upstream frame';
  }
  new MutationObserver(update).observe(workspace, { attributeFilter: ['data-view'] });
  document.addEventListener('cctv-camera', update);
  byId('reconnect').addEventListener('click', () => {
    const feed = feeds.find(item => item.id === camera.id);
    feed.lastFrame = 0;
    feed.lastResetAt = performance.now();
    if (feed.player.ws) feed.player.ws.close();
    else feed.player.onconnect();
    update();
  });
  byId('fullscreen').addEventListener('click', () => fullscreen(byId('viewer')));
  pollStatus();
  update();
  setInterval(pollStatus, 1000);
  setInterval(update, 500);
}

if (store) mountStore();
else mountHome();

function mountHome() {
const player = document.querySelector('cctv-player');
const video = player.video;
const status = byId('status');
const details = byId('details');
const message = byId('live-message');
let startedAt = performance.now();
let lastFrameAt = 0;
let lastFrameTime;
let resolution;

function update() {
  updateClock();
  if (workspace.dataset.view !== 'live' && workspace.dataset.view !== 'overview') {
    byId('integration-live').textContent = lastFrameTime ? `Last viewed ${lastFrameTime.toLocaleTimeString()}` : 'Waiting for a live view';
    return;
  }
  const waiting = performance.now() - (lastFrameAt || startedAt);
  let state = 'waiting';
  let label = 'Connecting';
  if (lastFrameAt && video.paused && !video.ended && video.readyState >= 2) {
    state = 'paused';
    label = 'Paused';
  } else if (lastFrameAt && waiting < 3000) {
    state = 'live';
    label = 'Live';
  } else if (lastFrameAt || waiting >= 20000) {
    label = 'Waiting for video';
  }
  if (status.textContent !== label) status.textContent = label;
  status.dataset.state = state;
  byId('summary-live').textContent = label;
  message.hidden = state !== 'waiting' || Boolean(lastFrameTime);
  if (!message.hidden) message.textContent = waiting >= 20000 ? 'Waiting for the camera to respond…' : 'Connecting to home camera…';
  if (lastFrameTime) details.textContent = `${resolution} · Last frame ${lastFrameTime.toLocaleTimeString()}`;
  else details.textContent = 'Waiting for the first frame';
}

function noteFrame() {
  lastFrameAt = performance.now();
  lastFrameTime = new Date();
  resolution = `${video.videoWidth}×${video.videoHeight}`;
  byId('summary-resolution').textContent = resolution;
}

if (typeof video.requestVideoFrameCallback === 'function') {
  const onFrame = () => {
    noteFrame();
    video.requestVideoFrameCallback(onFrame);
  };
  video.requestVideoFrameCallback(onFrame);
} else {
  video.addEventListener('timeupdate', () => { if (!video.paused && video.readyState >= 2) noteFrame(); });
}
video.muted = true;
player.src = '/api/ws?src=home_camera';
update();
setInterval(update, 500);
byId('reconnect').addEventListener('click', () => {
  startedAt = performance.now();
  lastFrameAt = 0;
  lastFrameTime = undefined;
  if (player.ws) player.ws.close();
  else player.onconnect();
  update();
});
byId('fullscreen').addEventListener('click', () => fullscreen(byId('viewer')));
}
