/* ── Lantern frontend ────────────────────────────────────────────────────── */
'use strict';

// ── Toast ─────────────────────────────────────────────────────────────────────

function toast(msg, type = 'success') {
  window.dispatchEvent(new CustomEvent('showToast', { detail: { msg, type } }));
}

// ── Keyboard shortcuts ────────────────────────────────────────────────────────

document.addEventListener('keydown', e => {
  const tag = document.activeElement.tagName;
  if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return;

  if (e.key >= '1' && e.key <= '9') {
    const n = parseInt(e.key, 10);
    const cards = [...document.querySelectorAll('#services-grid .service-card')];
    if (cards[n - 1]) cards[n - 1].click();
  }
});
