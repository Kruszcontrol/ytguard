// Relays API calls from content scripts to the local ytguard daemon.
// The daemon identifies the kid from the Linux user running Chrome.
const BASE = 'http://127.0.0.1:7878';

chrome.runtime.onMessage.addListener((msg, _sender, sendResponse) => {
  if (!msg || msg.type !== 'api' || typeof msg.path !== 'string' || !msg.path.startsWith('/api/')) return false;
  const init = { headers: { 'X-YTGuard': '1' }, cache: 'no-store' };
  if (msg.body !== undefined) {
    init.method = 'POST';
    init.headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(msg.body);
  }
  const ctl = new AbortController();
  const timer = setTimeout(() => ctl.abort(), 12000);
  init.signal = ctl.signal;
  fetch(BASE + msg.path, init)
    .then(async r => {
      if (!r.ok) throw new Error('HTTP ' + r.status + ' ' + (await r.text()).slice(0, 200));
      return r.json();
    })
    .then(data => sendResponse({ ok: true, data }))
    .catch(err => sendResponse({ ok: false, error: String(err) }))
    .finally(() => clearTimeout(timer));
  return true; // async response
});
