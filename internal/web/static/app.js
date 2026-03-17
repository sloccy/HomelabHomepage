/* ── Lantern frontend ────────────────────────────────────────────────────── */
'use strict';

// Shared state for edit-mode card selection (accessible to keydown handler)
let _selectedCard = null;
let _selectedCardName = null;

document.addEventListener('DOMContentLoaded', () => {
  // ── Modal ─────────────────────────────────────────────────────────────────
  const dialog = document.getElementById('app-modal');
  if (dialog) {
    document.body.addEventListener('openmodal', () => dialog.showModal());
    document.body.addEventListener('closemodal', () => dialog.close());
  }

  // ── Toast ─────────────────────────────────────────────────────────────────
  const toastEl = document.getElementById('app-toast');
  const toastMsg = document.getElementById('toast-msg');
  if (toastEl) {
    let toastTimer;
    document.body.addEventListener('showtoast', e => {
      toastMsg.textContent = e.detail.msg;
      toastEl.className = 'alert ' + (e.detail.type === 'error' ? 'alert-error' : 'alert-success');
      toastEl.classList.add('showing');
      clearTimeout(toastTimer);
      toastTimer = setTimeout(() => { toastEl.classList.remove('showing'); }, 3500);
    });
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

  // ── Edit layout toggle ────────────────────────────────────────────────────
  document.getElementById('edit-layout-toggle')?.addEventListener('change', function() {
    // Deselect card when exiting edit mode
    if (!this.checked) {
      if (_selectedCard) _selectedCard.classList.remove('selected');
      _selectedCard = null;
      _selectedCardName = null;
    }
  });

  // ── Edit mode: intercept card clicks to select instead of navigate ─────────
  document.body.addEventListener('click', e => {
    if (!document.getElementById('edit-layout-toggle')?.checked) return;
    const card = e.target.closest('.service-card');
    if (!card || e.target.closest('.reorder-btns')) return;
    e.preventDefault();
    const isSame = card === _selectedCard;
    if (_selectedCard) _selectedCard.classList.remove('selected');
    _selectedCard = isSame ? null : card;
    _selectedCardName = isSame ? null : card.dataset.name;
    if (_selectedCard) _selectedCard.classList.add('selected');
  });

  // ── Form: subdomain preview ───────────────────────────────────────────────
  document.body.addEventListener('input', e => {
    if (e.target.matches('.subdomain-wrap input[name="subdomain"]')) {
      const wrap = e.target.closest('.subdomain-wrap');
      wrap.querySelector('.subdomain-hint').textContent =
        e.target.value ? e.target.value + '.' + wrap.dataset.domain : '';
    }
  });

  // ── Category collapse persistence + re-select card after htmx swap ────────
  document.body.addEventListener('htmx:afterSettle', e => {
    e.target.querySelectorAll('details[data-storage-key]').forEach(el => {
      if (localStorage.getItem(el.dataset.storageKey) === '0') el.removeAttribute('open');
    });
    // Re-apply selection after grid re-render
    if (_selectedCardName && document.getElementById('edit-layout-toggle')?.checked) {
      const card = document.querySelector(`.service-card[data-name="${CSS.escape(_selectedCardName)}"]`);
      if (card) {
        _selectedCard = card;
        card.classList.add('selected');
      }
    }
  });

  // Persist details open/closed state (toggle doesn't bubble, use capture)
  document.body.addEventListener('toggle', e => {
    const key = e.target.dataset?.storageKey;
    if (key) localStorage.setItem(key, e.target.open ? '1' : '0');
  }, true);
});

// ── Keyboard shortcuts ────────────────────────────────────────────────────────
document.addEventListener('keydown', e => {
  const tag = document.activeElement.tagName;
  if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return;

  // Arrow keys: move selected card in edit mode
  if (document.getElementById('edit-layout-toggle')?.checked && _selectedCard) {
    if (e.key === 'ArrowLeft' || e.key === 'ArrowRight' ||
        e.key === 'ArrowUp'   || e.key === 'ArrowDown') {
      e.preventDefault();
      const dir = (e.key === 'ArrowLeft' || e.key === 'ArrowUp') ? 'left' : 'right';
      _selectedCard.closest('.card-wrapper')?.querySelector(`.reorder-btn[hx-vals*='"direction":"${dir}"']`)?.click();
      return;
    }
  }

  if (e.key === '/') {
    e.preventDefault();
    document.getElementById('search-input')?.focus();
  } else if (e.key === 'Escape') {
    // Deselect card first if one is selected
    if (_selectedCard) {
      _selectedCard.classList.remove('selected');
      _selectedCard = null;
      _selectedCardName = null;
      return;
    }
    const s = document.getElementById('search-input');
    if (s && s.value) { s.value = ''; s.dispatchEvent(new Event('input', {bubbles: true})); s.blur(); }
  } else if (e.key >= '1' && e.key <= '9') {
    // Disable quick-nav shortcuts while in edit mode
    if (document.getElementById('edit-layout-toggle')?.checked) return;
    const n = parseInt(e.key, 10);
    const cards = [...document.querySelectorAll('#services-grid .service-card')];
    if (cards[n - 1]) cards[n - 1].click();
  }
});
