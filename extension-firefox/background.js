// Native-messaging bridge. The host is limited to the Starhold companion by the
// native messaging manifest (allowed_origins), not by a network port.
const HOST = 'cloud.mithraic.wyrmhoard_companion';
let port = null;
let seq = 0;
const pending = new Map();

function connect() {
  if (port) return port;
  try {
    port = chrome.runtime.connectNative(HOST);
    port.onMessage.addListener((msg) => {
      const id = msg && msg.id;
      const p = pending.get(id);
      if (p) { pending.delete(id); p(msg); }
    });
    port.onDisconnect.addListener(() => {
      const err = port.error?.message || 'companion disconnected';
      console.error('companion native host disconnected:', err);
      port = null;
      for (const [, resolve] of pending) resolve({ ok: false, error: err });
      pending.clear();
    });
  } catch (e) {
    console.error('companion native host connect failed:', String(e));
    port = null;
  }
  return port;
}

chrome.runtime.onMessage.addListener((msg, sender, sendResponse) => {
  if (!msg || msg.channel !== 'wyrmhoard-companion') return false;
  const p = connect();
  if (!p) { sendResponse({ ok: false, error: 'companion is not installed' }); return false; }
  const id = ++seq;
  pending.set(id, (reply) => sendResponse(reply));
  try { p.postMessage({ ...msg.payload, id }); }
  catch (e) { pending.delete(id); sendResponse({ ok: false, error: String(e) }); }
  return true;
});
