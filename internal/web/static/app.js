/* ── Lantern frontend ────────────────────────────────────────────────────── */
'use strict';

document.addEventListener('DOMContentLoaded', () => {
  // ── Modal ─────────────────────────────────────────────────────────────────
  const modalEl = document.getElementById('app-modal');
  if (modalEl) {
    const appModal = new bootstrap.Modal(modalEl);
    document.body.addEventListener('openmodal', () => appModal.show());
    document.body.addEventListener('closemodal', () => appModal.hide());
  }

  // ── Toast ─────────────────────────────────────────────────────────────────
  const toastEl = document.getElementById('app-toast');
  if (toastEl) {
    const appToast = new bootstrap.Toast(toastEl, {autohide: true, delay: 3500});
    const showToast = (msg, type = 'success') => {
      document.getElementById('toast-msg').textContent = msg;
      toastEl.className = 'toast align-items-center border-0 ' +
        (type === 'error' ? 'text-bg-danger' : 'text-bg-success');
      appToast.show();
    };
    document.body.addEventListener('showtoast', e => showToast(e.detail.msg, e.detail.type));
  }

  // ── Clock ─────────────────────────────────────────────────────────────────
  const clockEl = document.getElementById('header-clock');
  if (clockEl) {
    const tick = () => {
      const d = new Date();
      clockEl.textContent = d.toLocaleDateString('en', {weekday: 'short', day: 'numeric', month: 'short'})
        + '\u2002' + d.toLocaleTimeString('en-GB');
    };
    tick(); setInterval(tick, 1000);
  }

  // ── Category collapse persistence ─────────────────────────────────────────
  document.body.addEventListener('htmx:afterSettle', e => {
    e.target.querySelectorAll('.collapse[data-storage-key]').forEach(el => {
      if (localStorage.getItem(el.dataset.storageKey) === '0') {
        el.classList.remove('show');
        el.previousElementSibling?.classList.add('collapsed');
      }
    });
  });
  document.body.addEventListener('shown.bs.collapse', e => {
    if (e.target.dataset.storageKey) localStorage.setItem(e.target.dataset.storageKey, '1');
  });
  document.body.addEventListener('hidden.bs.collapse', e => {
    if (e.target.dataset.storageKey) localStorage.setItem(e.target.dataset.storageKey, '0');
  });
});

// ── Keyboard shortcuts ────────────────────────────────────────────────────────
document.addEventListener('keydown', e => {
  const tag = document.activeElement.tagName;
  if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return;

  if (e.key === '/') {
    e.preventDefault();
    document.getElementById('search-input')?.focus();
  } else if (e.key === 'Escape') {
    const s = document.getElementById('search-input');
    if (s && s.value) { s.value = ''; s.dispatchEvent(new Event('input', {bubbles: true})); s.blur(); }
  } else if (e.key >= '1' && e.key <= '9') {
    const n = parseInt(e.key, 10);
    const cards = [...document.querySelectorAll('#services-grid .service-card')];
    if (cards[n - 1]) cards[n - 1].click();
  }
});
