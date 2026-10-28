(() => {
  const MARK = 'wyrmhoard-companion';
  const allowed = new Set([location.origin]);
  function reply(id, payload) {
    window.postMessage({ channel: MARK, direction: 'to-page', id, ...payload }, location.origin);
  }
  window.addEventListener('message', (event) => {
    if (event.source !== window) return;
    if (event.origin !== location.origin) return;
    const data = event.data;
    if (!data || data.channel !== MARK || data.direction !== 'to-extension') return;
    if (!allowed.has(event.origin)) return;
    try {
      chrome.runtime.sendMessage({ channel: MARK, payload: data.payload }, (resp) => {
        reply(data.id, resp || { ok: false, error: 'no response' });
      });
    } catch (e) {
      reply(data.id, { ok: false, error: 'extension unavailable' });
    }
  });
  // Announce presence without requiring a page request.
  window.postMessage({ channel: MARK, direction: 'to-page', type: 'ready' }, location.origin);
})();
