// Runs in the page's own JavaScript world to read YouTube's player data,
// which the isolated content script can't see. Sends it as a JSON string.
(() => {
  if (window.__ytgMain) return;
  window.__ytgMain = true;
  const sent = new Map(); // videoId -> title sent

  const send = (pr) => {
    try {
      const d = pr && pr.videoDetails;
      if (!d || !d.videoId) return;
      if (sent.get(d.videoId) === d.title) return;
      sent.set(d.videoId, d.title);
      const mf = (pr.microformat && pr.microformat.playerMicroformatRenderer) || {};
      const meta = {
        videoId: d.videoId,
        title: d.title || '',
        description: d.shortDescription || '',
        tags: d.keywords || [],
        channelId: d.channelId || '',
        channelName: d.author || '',
        category: mf.category || '',
        lengthSeconds: parseInt(d.lengthSeconds, 10) || 0,
        isLive: !!(d.isLive || (mf.liveBroadcastDetails && mf.liveBroadcastDetails.isLiveNow)),
        familySafe: typeof mf.isFamilySafe === 'boolean' ? mf.isFamilySafe : null,
        full: true,
      };
      const h = (mf.ownerProfileUrl || '').match(/\/(@[^/?#]+)/);
      if (h) meta.channelHandle = decodeURIComponent(h[1]);
      document.dispatchEvent(new CustomEvent('ytg-meta', { detail: JSON.stringify(meta) }));
    } catch (e) { /* ignore */ }
  };

  const poll = () => {
    try { send(window.ytInitialPlayerResponse); } catch (e) {}
    for (const id of ['movie_player', 'shorts-player']) {
      try {
        const p = document.getElementById(id);
        if (p && typeof p.getPlayerResponse === 'function') send(p.getPlayerResponse());
      } catch (e) {}
    }
  };

  document.addEventListener('yt-navigate-finish', (e) => {
    try { send(e.detail && e.detail.response && e.detail.response.playerResponse); } catch (_) {}
    poll();
  }, true);
  document.addEventListener('yt-player-updated', poll, true);
  document.addEventListener('ytg-poll', () => { sent.clear(); poll(); });
  setInterval(poll, 500);
})();
