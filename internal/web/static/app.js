/* ── Lantern frontend ────────────────────────────────────────────────────── */
'use strict';

// Shared state for edit-mode card selection (accessible to keydown handler)
let _selectedCard = null;
let _selectedCardName = null;

document.addEventListener('DOMContentLoaded', () => {
  // ── Modal ─────────────────────────────────────────────────────────────────
  const modalEl = document.getElementById('app-modal');
  if (modalEl) {
    const modal = bootstrap.Modal.getOrCreateInstance(modalEl);
    document.body.addEventListener('openmodal', () => modal.show());
    document.body.addEventListener('closemodal', () => modal.hide());
  }

  // ── Toast ─────────────────────────────────────────────────────────────────
  const toastEl = document.getElementById('app-toast');
  const toastMsg = document.getElementById('toast-msg');
  if (toastEl) {
    document.body.addEventListener('showtoast', e => {
      toastMsg.textContent = e.detail.msg;
      toastEl.className = 'toast ' + (e.detail.type === 'error' ? 'text-bg-danger' : 'text-bg-success');
      bootstrap.Toast.getOrCreateInstance(toastEl, {delay: 3500}).show();
    });
  }

  // ── Optimistic reorder: swap card-wrappers immediately, sync server async ──
  document.body.addEventListener('click', e => {
    const btn = e.target.closest('.reorder-btn');
    if (!btn) return;
    let dir;
    try { dir = JSON.parse(btn.getAttribute('hx-vals') || '{}').direction; } catch(_) {}
    if (!dir) return;

    const wrapper = btn.closest('.card-wrapper');
    if (!wrapper) return;
    const grid = wrapper.parentElement;
    const siblings = [...grid.children].filter(el => el.classList.contains('card-wrapper'));
    const idx = siblings.indexOf(wrapper);
    const targetIdx = dir === 'left' ? idx - 1 : idx + 1;
    if (targetIdx < 0 || targetIdx >= siblings.length) return;

    const other = siblings[targetIdx];
    if (dir === 'left') grid.insertBefore(wrapper, other);
    else grid.insertBefore(other, wrapper);

    const url = btn.getAttribute('hx-post');
    if (url) {
      const body = new URLSearchParams();
      try { Object.entries(JSON.parse(btn.getAttribute('hx-vals') || '{}')).forEach(([k,v]) => body.set(k, v)); } catch(_) {}
      fetch(url, { method: 'POST', body }).catch(() => {});
    }
    e.stopPropagation(); // prevent HTMX full re-render
  }, true); // capture phase — fires before HTMX bubble-phase listeners

  // ── Edit layout toggle ────────────────────────────────────────────────────
  document.getElementById('edit-layout-toggle')?.addEventListener('change', function() {
    document.body.classList.toggle('edit-mode', this.checked);
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
    const details = e.target.querySelectorAll('details[data-storage-key]');
    if (!details.length) return;
    details.forEach(el => {
      if (localStorage.getItem(el.dataset.storageKey) === '0') el.removeAttribute('open');
    });
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
    if (_selectedCard) {
      _selectedCard.classList.remove('selected');
      _selectedCard = null;
      _selectedCardName = null;
      return;
    }
    const s = document.getElementById('search-input');
    if (s && s.value) { s.value = ''; s.dispatchEvent(new Event('input', {bubbles: true})); s.blur(); }
  } else if (e.key >= '1' && e.key <= '9') {
    if (document.getElementById('edit-layout-toggle')?.checked) return;
    const n = parseInt(e.key, 10);
    const cards = [...document.querySelectorAll('#services-grid .service-card')];
    if (cards[n - 1]) cards[n - 1].click();
  }
});
