/* ── Lantern frontend ────────────────────────────────────────────────────── */
'use strict';

// ── Toast ─────────────────────────────────────────────────────────────────────

function toast(msg, type = 'success') {
  window.dispatchEvent(new CustomEvent('showToast', { detail: { msg, type } }));
}

function closeModal() {
  document.body.dispatchEvent(new CustomEvent('closeModal'));
}

// ── Search / filter ───────────────────────────────────────────────────────────

document.addEventListener('alpine:init', () => {
  Alpine.data('search', () => ({
    query: '',
    filter() {
      const q = this.query.toLowerCase().trim();
      const root = document.getElementById('services-grid');
      if (!root) return;
      root.querySelectorAll('.service-card').forEach(card => {
        const name = (card.dataset.name || '').toLowerCase();
        const sub  = (card.dataset.sub  || '').toLowerCase();
        card.style.display = (!q || name.includes(q) || sub.includes(q)) ? '' : 'none';
      });
      root.querySelectorAll('.category-group').forEach(group => {
        const grid = group.querySelector('.grid');
        if (!grid) return;
        const anyVisible = [...grid.querySelectorAll('.service-card')].some(c => c.style.display !== 'none');
        group.style.display = anyVisible ? '' : 'none';
      });
    }
  }));
});

// ── Keyboard shortcuts ────────────────────────────────────────────────────────

document.addEventListener('keydown', e => {
  const tag = document.activeElement.tagName;
  const inInput = tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT';

  if (e.key === 'Escape' && !inInput) {
    const search = document.getElementById('search-input');
    if (search && search.value) {
      search.value = '';
      search.dispatchEvent(new Event('input', { bubbles: true }));
      search.blur();
    }
    return;
  }

  if (inInput) return;

  if (e.key === '/') {
    e.preventDefault();
    document.getElementById('search-input')?.focus();
    return;
  }

  if (e.key >= '1' && e.key <= '9') {
    const n = parseInt(e.key, 10);
    const cards = [...document.querySelectorAll('#services-grid .service-card')]
      .filter(c => c.style.display !== 'none');
    if (cards[n - 1]) cards[n - 1].click();
  }
});
