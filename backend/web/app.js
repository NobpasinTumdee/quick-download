/* Quick Download dashboard.
 *
 * Transport: WebSocket to ws://127.0.0.1:<port>/ws, with automatic reconnect
 * and a polling fallback (GET /api/downloads) whenever the socket is down, so
 * the page keeps working even if the WebSocket upgrade is blocked.
 */
(() => {
  'use strict';

  const API = location.origin;
  const WS_URL = (location.protocol === 'https:' ? 'wss://' : 'ws://') + location.host + '/ws';

  const el = {
    list: document.getElementById('list'),
    empty: document.getElementById('empty'),
    tpl: document.getElementById('row-tpl'),
    dot: document.getElementById('conn-dot'),
    label: document.getElementById('conn-label'),
    form: document.getElementById('add-form'),
    url: document.getElementById('url-input'),
    clear: document.getElementById('clear-btn'),
    dir: document.getElementById('dl-dir'),
    banner: document.getElementById('tools-banner'),
    toolsPath: document.getElementById('tools-path'),
    active: document.getElementById('stat-active'),
    done: document.getElementById('stat-done'),
    speed: document.getElementById('stat-speed'),
    conns: document.getElementById('stat-conns'),
  };

  /** @type {Map<string, HTMLElement>} rendered rows, keyed by job id */
  const rows = new Map();
  let socket = null;
  let retryDelay = 500;
  let pollTimer = null;

  // ----------------------------------------------------------------- helpers

  const ACTIVE_STATES = new Set(['queued', 'probing', 'downloading', 'merging']);

  function bytes(n) {
    if (!Number.isFinite(n) || n <= 0) return '0 B';
    const units = ['B', 'KB', 'MB', 'GB', 'TB'];
    const i = Math.min(units.length - 1, Math.floor(Math.log(n) / Math.log(1024)));
    const v = n / Math.pow(1024, i);
    return `${v >= 100 || i === 0 ? Math.round(v) : v.toFixed(1)} ${units[i]}`;
  }

  function duration(seconds) {
    if (!Number.isFinite(seconds) || seconds < 0) return '--';
    const s = Math.round(seconds);
    if (s < 60) return `${s}s`;
    if (s < 3600) return `${Math.floor(s / 60)}m ${s % 60}s`;
    return `${Math.floor(s / 3600)}h ${Math.floor((s % 3600) / 60)}m`;
  }

  function iconFor(job) {
    const name = (job.filename || job.url || '').toLowerCase();
    const mime = job.mime || '';
    if (job.engine === 'yt-dlp') return job.kind === 'site' ? '🌐' : '📡';
    if (mime.startsWith('video/') || /\.(mp4|webm|mkv|mov|avi|m4v|flv)(\?|$)/.test(name)) return '🎬';
    if (mime.startsWith('audio/') || /\.(mp3|m4a|aac|ogg|wav|flac)(\?|$)/.test(name)) return '🎵';
    if (mime.startsWith('image/') || /\.(jpe?g|png|gif|webp|avif|svg|bmp)(\?|$)/.test(name)) return '🖼️';
    if (/\.(zip|rar|7z|tar|gz)(\?|$)/.test(name)) return '🗜️';
    if (/\.pdf(\?|$)/.test(name)) return '📄';
    return '📦';
  }

  async function post(path, body) {
    try {
      const res = await fetch(API + path, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body || {}),
      });
      return await res.json();
    } catch (err) {
      console.error('[quick-download] request failed', path, err);
      return { ok: false, error: String(err) };
    }
  }

  // ---------------------------------------------------------------- rendering

  function render(jobs) {
    const seen = new Set();
    let active = 0;
    let done = 0;
    let speed = 0;
    let conns = 0;

    for (const job of jobs) {
      seen.add(job.id);
      paint(job);
      if (ACTIVE_STATES.has(job.state)) {
        active++;
        speed += job.speed || 0;
        conns += job.state === 'downloading' ? job.connections || 0 : 0;
      }
      if (job.state === 'completed') done++;
    }

    for (const [id, node] of rows) {
      if (!seen.has(id)) {
        node.remove();
        rows.delete(id);
      }
    }

    el.active.textContent = active;
    el.done.textContent = done;
    el.speed.textContent = bytes(speed) + '/s';
    el.conns.textContent = conns;
    el.empty.hidden = jobs.length > 0;
  }

  function paint(job) {
    let node = rows.get(job.id);
    if (!node) {
      node = el.tpl.content.firstElementChild.cloneNode(true);
      node.dataset.id = job.id;
      node.addEventListener('click', onCardClick);
      // Newest first: the server already sorts, we just prepend.
      el.list.prepend(node);
      rows.set(job.id, node);
    }

    const q = (sel) => node.querySelector(sel);
    node.dataset.state = job.state;
    node.dataset.engine = job.engine || 'http';

    q('[data-kind]').textContent = iconFor(job);

    // Streaming jobs are handled by yt-dlp; say so rather than pretending the
    // byte-range machinery is involved.
    const engineTag = q('[data-engine]');
    if (job.engine === 'yt-dlp') {
      engineTag.textContent = job.kind && job.kind !== 'site' ? job.kind : 'yt-dlp';
      engineTag.hidden = false;
      engineTag.title = `handled by yt-dlp (${job.kind || 'stream'})`;
    } else {
      engineTag.hidden = true;
    }

    q('[data-phase]').textContent = job.phase && job.state === 'downloading' ? job.phase : '';
    q('[data-filename]').textContent = job.filename || 'resolving…';
    q('[data-url]').textContent = job.url;
    q('[data-url]').title = job.url;

    const pct = Math.max(0, Math.min(100, job.progress || 0));
    q('[data-fill]').style.width = pct.toFixed(1) + '%';

    q('[data-state]').textContent = job.state;
    q('[data-progress]').textContent = pct.toFixed(1) + '%';
    q('[data-size]').textContent =
      job.size > 0
        ? `${bytes(job.downloaded)} / ${bytes(job.size)}`
        : job.downloaded > 0
          ? bytes(job.downloaded)
          : '';
    q('[data-speed]').textContent =
      job.state === 'downloading' ? bytes(job.speed) + '/s' : '';
    q('[data-eta]').textContent =
      job.state === 'downloading' && job.eta >= 0 ? 'ETA ' + duration(job.eta) : '';

    const err = q('[data-error]');
    err.hidden = !job.error;
    err.textContent = job.error || '';

    paintChunks(q('[data-chunks]'), job);

    const finished = !ACTIVE_STATES.has(job.state);
    q('[data-action="cancel"]').hidden = finished;
    q('[data-action="retry"]').hidden = !(job.state === 'failed' || job.state === 'canceled');
    q('[data-action="reveal"]').hidden = job.state !== 'completed';
  }

  // One segment per HTTP connection: makes the parallel download visible.
  function paintChunks(container, job) {
    const chunks = job.chunks || [];
    if (chunks.length <= 1 || job.state === 'completed') {
      container.innerHTML = '';
      return;
    }
    if (container.children.length !== chunks.length) {
      container.innerHTML = '';
      for (let i = 0; i < chunks.length; i++) {
        const seg = document.createElement('span');
        seg.className = 'chunk';
        seg.appendChild(document.createElement('i'));
        container.appendChild(seg);
      }
    }
    chunks.forEach((c, i) => {
      const seg = container.children[i];
      seg.classList.toggle('done', !!c.done);
      seg.firstElementChild.style.width = Math.min(100, c.progress || 0).toFixed(1) + '%';
      seg.title = `connection ${c.index + 1}: ${bytes(c.downloaded)} / ${bytes(c.total)}`;
    });
  }

  // ------------------------------------------------------------------ actions

  function onCardClick(event) {
    const btn = event.target.closest('[data-action]');
    if (!btn) return;
    const id = event.currentTarget.dataset.id;
    const action = btn.dataset.action;
    if (action === 'cancel') post('/api/cancel', { id });
    if (action === 'retry') post('/api/retry', { id });
    if (action === 'reveal') post('/api/reveal', { id });
  }

  el.form.addEventListener('submit', (e) => {
    e.preventDefault();
    const url = el.url.value.trim();
    if (!url) return;
    post('/api/enqueue', { url, userAgent: navigator.userAgent });
    el.url.value = '';
  });

  el.clear.addEventListener('click', () => post('/api/clear', {}));

  // ---------------------------------------------------------------- transport

  function setConnected(on, text) {
    el.dot.className = 'dot ' + (on ? 'online' : 'offline');
    el.label.textContent = text;
  }

  function connect() {
    try {
      socket = new WebSocket(WS_URL);
    } catch (err) {
      scheduleReconnect();
      return;
    }

    socket.addEventListener('open', () => {
      retryDelay = 500;
      setConnected(true, 'engine connected');
      stopPolling();
    });

    socket.addEventListener('message', (event) => {
      try {
        const msg = JSON.parse(event.data);
        if (msg.type === 'snapshot') render(msg.jobs || []);
      } catch (err) {
        console.warn('[quick-download] bad frame', err);
      }
    });

    socket.addEventListener('close', () => {
      setConnected(false, 'engine offline — retrying');
      startPolling();
      scheduleReconnect();
    });

    socket.addEventListener('error', () => socket && socket.close());
  }

  function scheduleReconnect() {
    setTimeout(connect, retryDelay);
    retryDelay = Math.min(retryDelay * 2, 10000); // capped exponential backoff
  }

  // Polling fallback keeps the UI alive when the socket cannot be established.
  function startPolling() {
    if (pollTimer) return;
    pollTimer = setInterval(async () => {
      try {
        const res = await fetch(API + '/api/downloads');
        const data = await res.json();
        render(data.jobs || []);
        setConnected(true, 'engine connected (polling)');
      } catch (err) {
        setConnected(false, 'engine offline — retrying');
      }
    }, 1000);
  }

  function stopPolling() {
    if (pollTimer) {
      clearInterval(pollTimer);
      pollTimer = null;
    }
  }

  async function loadHealth() {
    try {
      const res = await fetch(API + '/api/health');
      const info = await res.json();
      el.dir.textContent = info.downloadDir || '?';
    } catch (err) {
      el.dir.textContent = 'engine offline';
    }
  }

  // Tell the user up front when streaming downloads cannot work, instead of
  // letting the first m3u8 job fail with a wall of text.
  async function loadTools() {
    try {
      const res = await fetch(API + '/api/tools');
      const info = await res.json();
      el.banner.hidden = !!info.ready;
      if (!info.ready && Array.isArray(info.searchedIn) && info.searchedIn.length) {
        const missing = [
          info.ytdlp?.found ? null : 'yt-dlp',
          info.ffmpeg?.found ? null : 'ffmpeg',
        ].filter(Boolean);
        el.toolsPath.textContent = `missing: ${missing.join(' + ')} — searched ${info.searchedIn[0]}`;
      }
    } catch (err) {
      el.banner.hidden = true;
    }
  }

  // Pre-fill from ?url= so the extension can deep-link into the dashboard.
  const preset = new URLSearchParams(location.search).get('url');
  if (preset) el.url.value = preset;

  loadHealth();
  loadTools();
  connect();
})();
