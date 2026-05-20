/* ── Lantern frontend ────────────────────────────────────────────────────── */
'use strict';

let _lastWrapper = null; // last dragged/moved card-wrapper (for keyboard reorder)
let _dragSrc = null;     // card-wrapper currently being dragged
let _dragOver = null;    // card-wrapper currently showing drag-before/after highlight

let _reorderTimer = null;
function postReorder(grid) {
  clearTimeout(_reorderTimer);
  _reorderTimer = setTimeout(() => {
    const ids = [...grid.querySelectorAll(':scope > .card-wrapper[data-id]')].map(w => w.dataset.id);
    const url = grid.closest('#services-grid') ? '/api/services/reorder' : '/api/bookmarks/reorder';
    fetch(url, {
      method: 'POST',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({ids}),
    }).catch(() => {});
  }, 300);
}

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

  // ── Edit layout toggle ────────────────────────────────────────────────────
  document.getElementById('edit-layout-toggle')?.addEventListener('change', function() {
    document.body.classList.toggle('edit-mode', this.checked);
    document.querySelectorAll('.card-wrapper').forEach(w => { w.draggable = this.checked; });
    if (!this.checked) _lastWrapper = null;
  });

  // ── Edit mode: block card navigation on click ────────────────────────────
  document.body.addEventListener('click', e => {
    if (!document.getElementById('edit-layout-toggle')?.checked) return;
    const card = e.target.closest('.service-card');
    if (!card) return;
    e.preventDefault();
    _lastWrapper = card.closest('.card-wrapper');
  });

  // ── Drag and drop reorder ─────────────────────────────────────────────────
  document.body.addEventListener('dragstart', e => {
    const wrapper = e.target.closest('.card-wrapper');
    if (!wrapper) return;
    _dragSrc = wrapper;
    _lastWrapper = wrapper;
    wrapper.classList.add('dragging');
    e.dataTransfer.effectAllowed = 'move';
    e.dataTransfer.setData('text/plain', ''); // required for Firefox
  });

  document.body.addEventListener('dragend', () => {
    document.querySelectorAll('.card-wrapper.dragging')
      .forEach(el => el.classList.remove('dragging'));
    if (_dragOver) { _dragOver.classList.remove('drag-before', 'drag-after'); _dragOver = null; }
    _dragSrc = null;
  });

  document.body.addEventListener('dragover', e => {
    if (!_dragSrc) return;
    const wrapper = e.target.closest('.card-wrapper');
    if (!wrapper || wrapper === _dragSrc || wrapper.parentElement !== _dragSrc.parentElement) return;
    e.preventDefault();
    e.dataTransfer.dropEffect = 'move';
    if (_dragOver && _dragOver !== wrapper) _dragOver.classList.remove('drag-before', 'drag-after');
    _dragOver = wrapper;
    const rect = wrapper.getBoundingClientRect();
    wrapper.classList.add(e.clientX < rect.left + rect.width / 2 ? 'drag-before' : 'drag-after');
  });

  document.body.addEventListener('dragleave', e => {
    const wrapper = e.target.closest('.card-wrapper');
    if (wrapper && wrapper === _dragOver) {
      wrapper.classList.remove('drag-before', 'drag-after');
      _dragOver = null;
    }
  });

  document.body.addEventListener('drop', e => {
    if (!_dragSrc) return;
    const wrapper = e.target.closest('.card-wrapper');
    if (!wrapper || wrapper === _dragSrc || wrapper.parentElement !== _dragSrc.parentElement) return;
    e.preventDefault();
    const grid = wrapper.parentElement;
    const rect = wrapper.getBoundingClientRect();
    if (e.clientX < rect.left + rect.width / 2) {
      grid.insertBefore(_dragSrc, wrapper);
    } else {
      grid.insertBefore(_dragSrc, wrapper.nextSibling);
    }
    postReorder(grid);
  });

  // ── Form: subdomain preview ───────────────────────────────────────────────
  document.body.addEventListener('input', e => {
    if (e.target.matches('.subdomain-wrap input[name="subdomain"]')) {
      const wrap = e.target.closest('.subdomain-wrap');
      wrap.querySelector('.subdomain-hint').textContent =
        e.target.value ? e.target.value + '.' + wrap.dataset.domain : '';
    }
  });

  // ── Category collapse persistence ─────────────────────────────────────────
  document.body.addEventListener('htmx:afterSettle', e => {
    const details = e.target.querySelectorAll('details[data-storage-key]');
    if (!details.length) return;
    details.forEach(el => {
      if (localStorage.getItem(el.dataset.storageKey) === '0') el.removeAttribute('open');
    });
  });

  // Persist details open/closed state (toggle doesn't bubble, use capture)
  document.body.addEventListener('toggle', e => {
    const key = e.target.dataset?.storageKey;
    if (key) localStorage.setItem(key, e.target.open ? '1' : '0');
  }, true);
});

// ── Scan log scroll-position helpers (called by hx-on in status.html) ────────
function scanLogBeforeRequest(details) {
  const el = document.querySelector('.scan-log');
  if (el) {
    window._scanLogScroll = el.scrollTop;
    window._scanLogAtBottom = (el.scrollHeight - el.scrollTop - el.clientHeight) < 30;
  }
  window._scanLogOpen = details.open;
}
function scanLogAfterSettle() {
  const d = document.querySelector('.scan-log-details');
  if (d && window._scanLogOpen !== undefined) {
    if (window._scanLogOpen) d.setAttribute('open', ''); else d.removeAttribute('open');
  }
  const el = document.querySelector('.scan-log');
  if (el) {
    if (window._scanLogAtBottom) el.scrollTop = el.scrollHeight;
    else if (window._scanLogScroll !== undefined) el.scrollTop = window._scanLogScroll;
  }
}

// ── Keyboard shortcuts ────────────────────────────────────────────────────────
document.addEventListener('keydown', e => {
  const tag = document.activeElement.tagName;
  if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return;

  // Arrow keys: move last interacted card in edit mode
  if (document.getElementById('edit-layout-toggle')?.checked && _lastWrapper) {
    if (e.key === 'ArrowLeft' || e.key === 'ArrowRight' ||
        e.key === 'ArrowUp'   || e.key === 'ArrowDown') {
      e.preventDefault();
      const grid = _lastWrapper.parentElement;
      const siblings = [...grid.children].filter(el => el.classList.contains('card-wrapper'));
      const idx = siblings.indexOf(_lastWrapper);
      const goLeft = e.key === 'ArrowLeft' || e.key === 'ArrowUp';
      const targetIdx = goLeft ? idx - 1 : idx + 1;
      if (targetIdx < 0 || targetIdx >= siblings.length) return;
      const other = siblings[targetIdx];
      if (goLeft) grid.insertBefore(_lastWrapper, other);
      else grid.insertBefore(other, _lastWrapper);
      postReorder(grid);
      return;
    }
  }

  if (e.key === '/') {
    e.preventDefault();
    document.getElementById('search-input')?.focus();
  } else if (e.key === 'Escape') {
    const s = document.getElementById('search-input');
    if (s && s.value) { s.value = ''; s.dispatchEvent(new Event('input', {bubbles: true})); s.blur(); }
  } else if (e.key >= '1' && e.key <= '9') {
    if (document.getElementById('edit-layout-toggle')?.checked) return;
    const n = parseInt(e.key, 10);
    const cards = [...document.querySelectorAll('#services-grid .service-card')];
    if (cards[n - 1]) cards[n - 1].click();
  }
});
