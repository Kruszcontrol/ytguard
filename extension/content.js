// YTGuard content script: filters video tiles, gates playback on video pages,
// reports watch time and shows break / time-up / blocked screens.
// Fails closed: if the ytguard daemon can't be reached nothing is shown.
(() => {
  if (window.__ytg) return;
  window.__ytg = true;

  const root = document.documentElement;
  const TILE_SEL = [
    'ytd-rich-item-renderer', 'ytd-video-renderer', 'ytd-compact-video-renderer', 'ytd-grid-video-renderer',
    'ytd-playlist-video-renderer', 'ytd-playlist-panel-video-renderer', 'ytd-reel-item-renderer',
    'ytd-compact-radio-renderer', 'ytd-radio-renderer', 'ytd-playlist-renderer', 'ytd-compact-playlist-renderer',
    'ytd-movie-renderer', 'ytd-compact-movie-renderer', 'ytd-watch-card-compact-video-renderer',
    'ytd-watch-card-hero-video-renderer', 'ytd-watch-card-rich-header-renderer',
    'yt-lockup-view-model', 'ytm-shorts-lockup-view-model', 'ytm-shorts-lockup-view-model-v2',
    'ytm-video-with-context-renderer', 'ytm-compact-video-renderer', 'ytm-reel-item-renderer', 'ytm-media-item',
  ].join(',');

  const S = {
    config: null,       // from /api/config; null until loaded
    managed: true,      // false for non-kid users: do nothing
    status: null,       // time status
    vid: null,          // video id of the current page
    decision: null,     // {outcome, reason, title, channel, request}
    metas: new Map(),   // videoId -> full metadata from the page
    verdicts: new Map(),// videoId -> show | locked | hide (feed tiles)
    lastStart: 0,
    view: '',           // what the overlay currently shows
    timer: null,
  };

  // ---------- daemon API ----------
  const api = (path, body) => new Promise((resolve) => {
    try {
      chrome.runtime.sendMessage({ type: 'api', path, body }, (r) => {
        if (chrome.runtime.lastError || !r || !r.ok) resolve(null);
        else resolve(r.data);
      });
    } catch (e) { resolve(null); } // extension reloaded: stay closed
  });

  // ---------- helpers ----------
  const videoIdFromURL = (u) => {
    try {
      const url = new URL(u, location.href);
      if (!/(^|\.)youtube(-nocookie)?\.com$/.test(url.hostname)) return null;
      const v = url.searchParams.get('v');
      if (url.pathname === '/watch' && v) return v;
      const m = url.pathname.match(/^\/(shorts|embed|live|v)\/([A-Za-z0-9_-]{11})/);
      return m ? m[2] : null;
    } catch (e) { return null; }
  };
  const isShortURL = (u) => /\/shorts\//.test(u);
  const mainVideo = () => document.querySelector('#movie_player video, #shorts-player video, video.html5-main-video') ||
    document.querySelector('video');
  const pauseAll = () => document.querySelectorAll('video').forEach((v) => { try { v.pause(); } catch (e) {} });
  const fmtMin = (sec) => sec < 3600 ? `${Math.max(1, Math.ceil(sec / 60))} min` :
    `${Math.floor(sec / 3600)} h ${String(Math.ceil((sec % 3600) / 60)).padStart(2, '0')} min`;
  const fmtClock = (unix) => new Date(unix * 1000).toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' });

  const playAllowed = () => S.managed === false ||
    (S.config && S.decision && S.decision.outcome === 'play' && S.status && S.status.allowed);

  // Stop playback that isn't allowed, however it starts.
  document.addEventListener('play', (e) => {
    if (!playAllowed()) { try { e.target.pause(); } catch (_) {} return; }
    if (e.target === mainVideo()) heartbeat(true);
  }, true);

  // ---------- overlay ----------
  let host = null, shadow = null;
  const ensureOverlay = () => {
    if (host && host.isConnected) return;
    host = document.createElement('ytg-overlay');
    host.style.cssText = 'all:initial;position:fixed;inset:0;z-index:2147483647;display:block;';
    shadow = host.attachShadow({ mode: 'closed' });
    (document.body || root).appendChild(host);
  };
  const OVERLAY_CSS = `
    :host{all:initial}
    .wrap{position:fixed;inset:0;display:flex;align-items:center;justify-content:center;
      background:#0f0f0f;color:#f1f1f1;font:16px/1.5 Roboto,Arial,sans-serif;padding:24px;box-sizing:border-box}
    .card{max-width:520px;text-align:center}
    .icon{font-size:56px;line-height:1;margin-bottom:12px}
    h1{font-size:24px;margin:0 0 8px}
    p{margin:8px 0;color:#c8c8c8}
    .title{color:#fff;font-weight:600}
    .count{font-size:44px;font-weight:700;margin:16px 0;font-variant-numeric:tabular-nums}
    button,a.btn{font:600 15px Roboto,Arial,sans-serif;padding:10px 18px;border-radius:20px;border:0;cursor:pointer;
      background:#3ea6ff;color:#0f0f0f;text-decoration:none;display:inline-block;margin:6px}
    button.secondary,a.secondary{background:#272727;color:#f1f1f1}
    textarea{width:100%;box-sizing:border-box;margin:10px 0;border-radius:10px;border:1px solid #3a3a3a;background:#1d1d1d;
      color:#fff;padding:10px;font:15px Roboto,Arial,sans-serif;resize:vertical;min-height:60px}
  `;
  const show = (key, html, bind) => {
    root.setAttribute('ytg-cover', '');
    ensureOverlay();
    if (S.view === key) return;
    S.view = key;
    shadow.innerHTML = `<style>${OVERLAY_CSS}</style><div class="wrap"><div class="card">${html}</div></div>`;
    if (bind) bind(shadow);
  };
  const hide = () => {
    root.removeAttribute('ytg-cover');
    S.view = '';
    if (host) { host.remove(); host = null; shadow = null; }
  };
  const esc = (s) => String(s || '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
  const homeBtn = () => window.top === window ? '<a class="btn secondary" href="https://www.youtube.com/">YouTube home</a>' : '';

  // ---------- badge ----------
  let badge = null;
  const updateBadge = () => {
    const st = S.status;
    const show = S.managed && S.vid && playAllowed() && st && (st.remainingSec >= 0 || st.breakInSec >= 0) && window.top === window;
    if (!show) { if (badge) { badge.remove(); badge = null; } return; }
    if (!badge || !badge.isConnected) {
      badge = document.createElement('ytg-badge');
      badge.style.cssText = 'all:initial;position:fixed;right:16px;bottom:16px;z-index:2147483646;background:rgba(15,15,15,.85);' +
        'color:#fff;font:600 13px/1.3 Roboto,Arial,sans-serif;padding:6px 12px;border-radius:14px;pointer-events:none;';
      (document.body || root).appendChild(badge);
    }
    const parts = [];
    if (st.remainingSec >= 0) parts.push(`⏱ ${fmtMin(st.remainingSec)} left`);
    if (st.breakInSec >= 0) parts.push(`break in ${fmtMin(st.breakInSec)}`);
    badge.textContent = parts.join(' · ');
  };

  // ---------- rendering of state ----------
  const render = () => {
    if (S.managed === false) { hide(); return; }
    if (!S.config) {
      if (S.vid || window.top === window) {
        show('down', `<div class="icon">🔌</div><h1>YouTube isn't available right now</h1>
          <p>YTGuard (the parent controls) isn't responding. Ask a parent to check the computer.</p>`);
      }
      return;
    }
    if (S.config.blockAll) {
      show('blockall', '<div class="icon">🚫</div><h1>YouTube is turned off on this account.</h1>');
      pauseAll();
      return;
    }
    const st = S.status;
    if (st && !st.allowed) {
      pauseAll();
      renderTime(st);
      return;
    }
    if (!S.vid) { hide(); updateBadge(); return; }
    const d = S.decision;
    if (!d) {
      pauseAll();
      show('checking:' + S.vid, '<div class="icon">⏳</div><p>Checking this video…</p>');
      return;
    }
    if (d.outcome === 'play') { hide(); updateBadge(); return; }
    pauseAll();
    if (d.outcome === 'hide') {
      document.title = 'YouTube';
      show('hidden:' + S.vid, `<div class="icon">🙈</div><h1>This video isn't available.</h1>${homeBtn()}`);
      return;
    }
    renderBlocked(d);
  };

  const renderTime = (st) => {
    clearInterval(S.timer);
    if (st.reason === 'break' && st.breakUntil) {
      show('break:' + st.breakUntil, `<div class="icon">☕</div><h1>Break time!</h1>
        <p>${esc(st.message)}</p><div class="count" id="c"></div><p>YouTube will be back when the timer ends.</p>`);
      const tick = () => {
        const left = Math.max(0, st.breakUntil - Math.floor(Date.now() / 1000));
        const el = shadow && shadow.getElementById('c');
        if (el) el.textContent = `${Math.floor(left / 60)}:${String(left % 60).padStart(2, '0')}`;
        if (left <= 0) { clearInterval(S.timer); refreshConfig(); }
      };
      tick();
      S.timer = setInterval(tick, 1000);
      return;
    }
    const icons = { time_up: '🌙', locked: '⏸️', outside_window: '🕰️', none_today: '📵' };
    let extra = '';
    if (st.reason === 'locked' && st.lockedUntil) extra = `<p>Until ${esc(fmtClock(st.lockedUntil))}.</p>`;
    show('time:' + st.reason + ':' + (st.lockedUntil || ''), `<div class="icon">${icons[st.reason] || '⏸️'}</div>
      <h1>${esc(st.message || "YouTube isn't available right now.")}</h1>${extra}`);
  };

  const renderBlocked = (d) => {
    const vid = S.vid;
    const state = d.request || '';
    let body;
    if (d.noAsk) {
      body = homeBtn();
    } else if (state === 'pending') {
      body = `<p>You asked a parent. It will start as soon as they say yes.</p>${homeBtn()}`;
    } else if (state === 'denied') {
      body = `<p>A parent said no to this one.</p>${homeBtn()}`;
    } else {
      body = `<textarea id="msg" maxlength="300" placeholder="Why do you want to watch it? (optional)"></textarea>
        <div><button id="ask">Ask a parent</button>${homeBtn()}</div>`;
    }
    const heading = d.noAsk ? 'Shorts are turned off' : "This video needs a parent's OK";
    show(`blocked:${vid}:${state}`, `<div class="icon">🔒</div><h1>${heading}</h1>
      <p class="title">${esc(d.title)}</p><p>${esc(d.channel)}</p>${d.noAsk ? '' : `<p>${esc(d.reason)}</p>`}${body}`,
    (sh) => {
      const btn = sh.getElementById('ask');
      if (!btn) return;
      btn.addEventListener('click', async () => {
        btn.disabled = true;
        btn.textContent = 'Sending…';
        const r = await api('/api/request', { videoId: vid, message: sh.getElementById('msg').value });
        if (r && S.vid === vid) { S.decision = { ...S.decision, request: r.status }; render(); }
        else { btn.disabled = false; btn.textContent = 'Try again'; }
      });
    });
  };

  // ---------- config & status ----------
  const applyOptions = (o) => {
    root.toggleAttribute('ytg-nocomments', !!(o && o.hideComments));
    root.toggleAttribute('ytg-noshorts', !!(o && (o.shorts === 'hide' || o.hideShorts)));
  };

  async function refreshConfig() {
    const c = await api('/api/config');
    if (c && c.managed === false) {
      S.managed = false;
      root.setAttribute('ytg-off', '');
      hide();
      return;
    }
    S.config = c;
    if (c) {
      S.status = c.status || null;
      applyOptions(c.options);
    }
    render();
  }

  // ---------- video page ----------
  document.addEventListener('ytg-meta', (e) => {
    try {
      const m = JSON.parse(e.detail);
      if (m && m.videoId) S.metas.set(m.videoId, m);
    } catch (_) {}
  });

  const waitMeta = (vid, ms) => new Promise((resolve) => {
    const t0 = Date.now();
    document.dispatchEvent(new CustomEvent('ytg-poll'));
    const iv = setInterval(() => {
      if (S.metas.has(vid) || Date.now() - t0 > ms) { clearInterval(iv); resolve(S.metas.get(vid) || null); }
    }, 100);
  });

  async function checkVideo(vid) {
    const resume = S.resume;
    S.decision = null;
    render();
    const meta = Object.assign({ videoId: vid }, await waitMeta(vid, 1500) || {});
    meta.isShort = isShortURL(location.href);
    const r = await api('/api/check', meta);
    if (S.vid !== vid) return; // navigated away meanwhile
    if (!r) { S.config = null; render(); return; }
    S.decision = r;
    if (r.status) S.status = r.status;
    render();
    if (r.outcome === 'play' && playAllowed()) {
      const v = mainVideo();
      if (v && v.paused && resume) v.play().catch(() => {});
      else if (v && !v.paused) heartbeat(true);
    }
    S.resume = false;
  }

  const onLocation = () => {
    if (S.managed === false) return;
    const vid = videoIdFromURL(location.href);
    if (vid === S.vid) return;
    S.vid = vid;
    S.decision = null;
    S.resume = !!vid; // we pause while checking; resume if it's allowed
    if (vid) { pauseAll(); checkVideo(vid); } else render();
  };

  // ---------- heartbeat ----------
  async function heartbeat(start) {
    if (S.managed === false || !S.vid || !S.decision || S.decision.outcome !== 'play') return;
    const now = Date.now();
    if (start && now - S.lastStart < 3000) return;
    if (start) S.lastStart = now;
    const v = mainVideo();
    const playing = !!(v && !v.paused && !v.ended);
    const m = S.metas.get(S.vid) || {};
    const r = await api('/api/heartbeat', {
      videoId: S.vid, title: m.title || '', channelId: m.channelId || '', channelName: m.channelName || '',
      isShort: isShortURL(location.href), playing, start: !!start,
    });
    if (!r) { S.config = null; render(); return; }
    S.status = r.status;
    if (r.outcome && r.outcome !== 'play') { checkVideo(S.vid); return; }
    render();
    updateBadge();
  }

  setInterval(() => {
    const v = mainVideo();
    if (v && !v.paused && !v.ended) heartbeat(false);
  }, 15000);

  // Refresh status when idle so parent changes (pause, bonus, approvals) show up.
  setInterval(async () => {
    if (S.managed === false) return;
    const v = mainVideo();
    if (v && !v.paused) return; // heartbeats cover this
    await refreshConfig();
    if (S.config && S.vid && !S.decision) { checkVideo(S.vid); return; }
    if (S.vid && S.decision && S.decision.outcome === 'block' && S.decision.request === 'pending') {
      const r = await api('/api/request?videoId=' + encodeURIComponent(S.vid));
      if (r && r.status === 'approved') { S.resume = true; checkVideo(S.vid); }
      else if (r && r.status === 'denied') { S.decision = { ...S.decision, request: 'denied' }; render(); }
    }
  }, 10000);

  // ---------- feed tiles ----------
  const pending = new Map(); // videoId -> item
  let flushTimer = null;

  const tileInfo = (tile) => {
    const a = tile.querySelector('a[href*="/watch?v="], a[href*="/shorts/"]');
    if (!a) return null;
    const href = a.getAttribute('href') || '';
    const videoId = videoIdFromURL(href);
    if (!videoId) return null;
    const tEl = tile.querySelector('#video-title, #video-title-link, .yt-lockup-metadata-view-model__title, h3, [title]');
    const title = ((tEl && (tEl.textContent || tEl.getAttribute('title'))) || '').trim();
    const ch = tile.querySelector('a[href^="/@"], a[href^="/channel/"], a[href*="youtube.com/@"]');
    const item = { videoId, title, isShort: isShortURL(href) };
    if (ch) {
      const chHref = ch.getAttribute('href') || '';
      const hm = chHref.match(/\/(@[^/?#]+)/);
      const cm = chHref.match(/\/channel\/(UC[\w-]{22})/);
      if (hm) item.channelHandle = decodeURIComponent(hm[1]);
      if (cm) item.channelId = cm[1];
      item.channelName = (ch.textContent || '').trim();
    }
    return item;
  };

  const applyVerdict = (tile, v) => {
    tile.removeAttribute('ytg-hide');
    tile.removeAttribute('ytg-locked');
    if (v === 'hide') { tile.setAttribute('ytg-hide', ''); tile.removeAttribute('ytg-ok'); return; }
    tile.setAttribute('ytg-ok', '');
    if (v === 'locked') tile.setAttribute('ytg-locked', '');
  };

  // Text-only Shorts UI (search filter chips, tabs) can't be matched by CSS.
  const hideShortsUI = () => {
    if (!root.hasAttribute('ytg-noshorts')) return;
    document.querySelectorAll('yt-chip-cloud-chip-renderer, chip-shape, yt-tab-shape, tp-yt-paper-tab').forEach((el) => {
      if (!el.hasAttribute('ytg-shorts-ui') && (el.textContent || '').trim() === 'Shorts') el.setAttribute('ytg-shorts-ui', '');
    });
  };

  const scan = () => {
    if (S.managed === false || !S.config || S.config.blockAll) return;
    hideShortsUI();
    document.querySelectorAll(TILE_SEL).forEach((tile) => {
      const info = tileInfo(tile);
      const id = info ? info.videoId : '';
      const prev = tile.getAttribute('ytg-vid');
      const stale = tile.hasAttribute('ytg-stale');
      if (prev === id && !stale && (tile.hasAttribute('ytg-ok') || tile.hasAttribute('ytg-hide') || tile.hasAttribute('ytg-wait'))) return;
      if (prev !== id) {
        // YouTube reuses tile elements for other videos: start over hidden.
        ['ytg-ok', 'ytg-hide', 'ytg-locked', 'ytg-wait'].forEach((a) => tile.removeAttribute(a));
      }
      tile.removeAttribute('ytg-stale');
      tile.setAttribute('ytg-vid', id);
      if (!info) {
        // Not loaded yet, or not a video (e.g. a channel card). Re-check later
        // until it has a link; links without video ids are safe to show.
        if (tile.querySelector('a[href]')) applyVerdict(tile, 'show');
        return;
      }
      const v = S.verdicts.get(id);
      if (v) { applyVerdict(tile, v); return; }
      tile.setAttribute('ytg-wait', '');
      pending.set(id, info);
      if (!flushTimer) flushTimer = setTimeout(flush, 150);
    });
  };

  async function flush() {
    flushTimer = null;
    const items = [...pending.values()].slice(0, 200);
    items.forEach((i) => pending.delete(i.videoId));
    if (!items.length) return;
    const r = await api('/api/filter', { items });
    if (r && r.results) {
      for (const [id, v] of Object.entries(r.results)) S.verdicts.set(id, v);
    }
    document.querySelectorAll('[ytg-wait]').forEach((tile) => {
      const v = S.verdicts.get(tile.getAttribute('ytg-vid'));
      if (v) { tile.removeAttribute('ytg-wait'); applyVerdict(tile, v); }
      else if (!r) tile.removeAttribute('ytg-wait'); // retry on next scan
    });
    if (pending.size) flushTimer = setTimeout(flush, 150);
  }

  // Forget cached tile verdicts now and then so rule changes apply.
  setInterval(() => {
    S.verdicts.clear();
    document.querySelectorAll('[ytg-vid]').forEach((t) => t.setAttribute('ytg-stale', ''));
    scan();
  }, 120000);

  // ---------- autoplay ----------
  setInterval(() => {
    if (!S.config || !S.config.options || !S.config.options.disableAutoplay) return;
    const b = document.querySelector('.ytp-autonav-toggle-button[aria-checked="true"]');
    if (b) b.click();
  }, 3000);

  // ---------- startup ----------
  let scanQueued = false;
  const queueScan = () => {
    if (scanQueued) return;
    scanQueued = true;
    setTimeout(() => { scanQueued = false; scan(); onLocation(); }, 150);
  };
  // Until a video is cleared (and always for hidden ones) the tab title must
  // not show its name.
  const scrubTitle = () => {
    if (S.managed === false) return;
    const vid = videoIdFromURL(location.href);
    if (!vid) return;
    const cleared = S.vid === vid && S.decision && S.decision.outcome !== 'hide';
    if (!cleared && document.title !== 'YouTube') document.title = 'YouTube';
  };
  new MutationObserver(() => { scrubTitle(); queueScan(); }).observe(root, { childList: true, subtree: true, characterData: true });
  document.addEventListener('yt-navigate-finish', onLocation, true);
  window.addEventListener('popstate', onLocation);
  setInterval(() => {
    onLocation();
    scan();
    scrubTitle();
  }, 1000);

  (async () => {
    // Cover video pages immediately, before anything from them shows.
    if (videoIdFromURL(location.href)) show('init', '<div class="icon">⏳</div><p>Checking this video…</p>');
    await refreshConfig();
    if (S.managed === false) return;
    onLocation();
    scan();
    if (!S.config) {
      // Daemon down: keep retrying.
      const iv = setInterval(async () => {
        await refreshConfig();
        if (S.config || S.managed === false) { clearInterval(iv); S.vid = null; onLocation(); scan(); }
      }, 5000);
    }
  })();
})();
