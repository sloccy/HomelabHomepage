/* ── Lantern frontend ────────────────────────────────────────────────────── */
'use strict';

// Shared state for edit-mode card selection (accessible to keydown handler)
let _selectedCard = null;
let _selectedCardName = null;

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

  // ── Edit layout toggle ────────────────────────────────────────────────────
  document.getElementById('edit-layout-btn')?.addEventListener('click', function() {
    document.body.classList.toggle('edit-layout-mode');
    this.classList.toggle('btn-warning');
    this.classList.toggle('btn-outline-secondary');
    // Deselect card when exiting edit mode
    if (!document.body.classList.contains('edit-layout-mode')) {
      if (_selectedCard) _selectedCard.classList.remove('selected');
      _selectedCard = null;
      _selectedCardName = null;
    }
  });

  // ── Edit mode: intercept card clicks to select instead of navigate ─────────
  document.body.addEventListener('click', e => {
    if (!document.body.classList.contains('edit-layout-mode')) return;
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
      wrap.querySelector('.form-text').textContent =
        e.target.value ? e.target.value + '.' + wrap.dataset.domain : '';
    }
  });

  // ── Form: direct-link toggle ──────────────────────────────────────────────
  document.body.addEventListener('change', e => {
    if (e.target.matches('[name="direct_only"]')) {
      e.target.closest('form').querySelector('.subdomain-group').style.display =
        e.target.checked ? 'none' : '';
    }
  });

  // ── Category collapse persistence + re-select card after htmx swap ────────
  document.body.addEventListener('htmx:afterSettle', e => {
    e.target.querySelectorAll('.collapse[data-storage-key]').forEach(el => {
      if (localStorage.getItem(el.dataset.storageKey) === '0') {
        el.classList.remove('show');
        el.previousElementSibling?.classList.add('collapsed');
      }
    });
    // Re-apply selection after grid re-render
    if (_selectedCardName && document.body.classList.contains('edit-layout-mode')) {
      const card = document.querySelector(`.service-card[data-name="${CSS.escape(_selectedCardName)}"]`);
      if (card) {
        _selectedCard = card;
        card.classList.add('selected');
      }
    }
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

  // Arrow keys: move selected card in edit mode
  if (document.body.classList.contains('edit-layout-mode') && _selectedCard) {
    if (e.key === 'ArrowLeft' || e.key === 'ArrowRight' ||
        e.key === 'ArrowUp'   || e.key === 'ArrowDown') {
      e.preventDefault();
      const dir = (e.key === 'ArrowLeft' || e.key === 'ArrowUp') ? 'left' : 'right';
      _selectedCard.querySelector(`.reorder-btn[hx-vals*='"direction":"${dir}"']`)?.click();
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
    if (document.body.classList.contains('edit-layout-mode')) return;
    const n = parseInt(e.key, 10);
    const cards = [...document.querySelectorAll('#services-grid .service-card')];
    if (cards[n - 1]) cards[n - 1].click();
  }
});
