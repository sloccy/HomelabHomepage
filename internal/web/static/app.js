/* ── Lantern frontend ────────────────────────────────────────────────────── */
'use strict';

let currentView = 'home';
let statusData = {};

// ── View routing ─────────────────────────────────────────────────────────────

function showView(view) {
  currentView = view;
  document.getElementById('view-home').style.display   = view === 'home'   ? '' : 'none';
  document.getElementById('view-manage').style.display = view === 'manage' ? '' : 'none';
  window.location.hash = view === 'home' ? '' : view;
  if (view === 'home')   { loadHome(); startSysinfo(); startClock(); _initSearch(); }
  if (view === 'manage') { stopSysinfo(); stopClock(); loadManage(); }
}

window.addEventListener('DOMContentLoaded', () => {
  loadSettings();
  const hash = window.location.hash.replace('#', '');
  showView(hash === 'manage' ? 'manage' : 'home');
});

// ── API helpers ───────────────────────────────────────────────────────────────

async function api(method, path, body) {
  const opts = { method, headers: { 'Content-Type': 'application/json' } };
  if (body !== undefined) opts.body = JSON.stringify(body);
  const res = await fetch(path, opts);
  if (res.status === 204) return null;
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || `HTTP ${res.status}`);
  return data;
}

// ── Toast ─────────────────────────────────────────────────────────────────────

let toastTimer;
function toast(msg, type = 'success') {
  const el = document.getElementById('toast');
  el.textContent = msg;
  el.className = `toast ${type}`;
  el.style.display = 'block';
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { el.style.display = 'none'; }, 3500);
}

// ── Home view ─────────────────────────────────────────────────────────────────

async function loadHome() {
  const root  = document.getElementById('services-grid');
  const empty = document.getElementById('home-empty');
  try {
    const [services, status, health] = await Promise.all([
      api('GET', '/api/services'),
      api('GET', '/api/status'),
      api('GET', '/api/health').catch(() => ({})),
    ]);
    statusData = status;

    if (!services || services.length === 0) {
      root.innerHTML  = '';
      empty.style.display = '';
      return;
    }
    empty.style.display = 'none';
    const sorted = services.sort((a, b) => (a.order ?? 0) - (b.order ?? 0) || a.name.localeCompare(b.name));
    renderGroupedServices(root, sorted, status.domain, health);
    loadBookmarksHome();
  } catch (e) {
    root.innerHTML = `<p style="color:var(--danger);padding:2rem">${e.message}</p>`;
  }
}

function renderGroupedServices(root, sorted, domain, health) {
  // Group by category; uncategorized services use empty string key.
  const groups = new Map();
  for (const svc of sorted) {
    const key = svc.category || '';
    if (!groups.has(key)) groups.set(key, []);
    groups.get(key).push(svc);
  }

  // Render: uncategorized first (no header), then named groups alphabetically.
  const namedKeys = [...groups.keys()].filter(k => k !== '').sort();
  const orderedKeys = groups.has('') ? ['', ...namedKeys] : namedKeys;

  root.innerHTML = '';
  for (const key of orderedKeys) {
    const svcs = groups.get(key);
    const section = document.createElement('div');
    section.className = 'category-group';
    section.dataset.category = key;

    if (key !== '') {
      const collapsed = _isCategoryCollapsed(key);
      section.innerHTML = `
        <div class="category-header" onclick="_toggleCategory(this)" data-category="${esc(key)}">
          <span class="category-arrow">${collapsed ? '▶' : '▼'}</span>
          <span class="category-name">${esc(key)}</span>
          <span class="category-count">${svcs.length}</span>
        </div>`;
      const grid = document.createElement('div');
      grid.className = 'grid';
      if (collapsed) grid.style.display = 'none';
      grid.innerHTML = svcs.map(svc => renderCard(svc, domain, health)).join('');
      section.appendChild(grid);
      _initDrag(grid, svcs);
    } else {
      const grid = document.createElement('div');
      grid.className = 'grid';
      grid.innerHTML = svcs.map(svc => renderCard(svc, domain, health)).join('');
      section.appendChild(grid);
      _initDrag(grid, svcs);
    }

    root.appendChild(section);
  }
}

function _isCategoryCollapsed(name) {
  try { return localStorage.getItem('cat-collapsed:' + name) === '1'; } catch { return false; }
}

function _toggleCategory(header) {
  const name = header.dataset.category;
  const grid = header.nextElementSibling;
  const arrow = header.querySelector('.category-arrow');
  const collapsed = grid.style.display === 'none';
  grid.style.display = collapsed ? '' : 'none';
  arrow.textContent = collapsed ? '▼' : '▶';
  try { localStorage.setItem('cat-collapsed:' + name, collapsed ? '' : '1'); } catch {}
}

function renderCard(svc, domain, health) {
  const url  = `https://${svc.subdomain}.${domain}`;
  let icon;
  if (svc.icon) {
    icon = `<img class="card-icon" src="${esc(svc.icon)}" alt="" loading="lazy" onerror="this.style.display='none'">`;
  } else {
    const faviconSrc = `/api/favicon?url=${encodeURIComponent(svc.target)}`;
    icon = `<img class="card-icon" src="${faviconSrc}" alt="" loading="lazy" onerror="this.style.display='none';this.nextElementSibling.style.display='flex'"><div class="card-icon-placeholder" style="display:none">${svcEmoji(svc)}</div>`;
  }
  const dot = health?.[svc.id] || '';
  return `
    <a class="service-card" href="${esc(url)}" target="_blank" rel="noopener" draggable="true" data-id="${esc(svc.id)}" data-name="${esc(svc.name)}" data-sub="${esc(svc.subdomain)}">
      <span class="health-dot ${dot}" title="${dot}"></span>
      ${icon}
      <div class="card-name">${esc(svc.name)}</div>
      <div class="card-url">${esc(svc.subdomain)}.${esc(domain)}</div>
    </a>`;
}

function svcEmoji(svc) {
  const n = (svc.name || '').toLowerCase();
  if (n.includes('plex'))    return '🎬';
  if (n.includes('sonarr'))  return '📺';
  if (n.includes('radarr'))  return '🎥';
  if (n.includes('portainer')) return '🐳';
  if (n.includes('home') || n.includes('ha')) return '🏠';
  if (n.includes('grafana') || n.includes('prometheus')) return '📊';
  if (n.includes('git'))     return '📦';
  if (n.includes('vpn') || n.includes('wire')) return '🔒';
  return '🖥️';
}

// ── Search / filter ───────────────────────────────────────────────────────────

function _initSearch() {
  const input = document.getElementById('search-input');
  if (!input) return;
  input.addEventListener('input', _filterCards);
}

function _filterCards() {
  const input = document.getElementById('search-input');
  const q = (input ? input.value : '').toLowerCase().trim();
  const root = document.getElementById('services-grid');
  if (!root) return;

  root.querySelectorAll('.service-card').forEach(card => {
    const name = (card.dataset.name || '').toLowerCase();
    const sub  = (card.dataset.sub  || '').toLowerCase();
    card.style.display = (!q || name.includes(q) || sub.includes(q)) ? '' : 'none';
  });

  // Hide category headers whose entire grid is hidden.
  root.querySelectorAll('.category-group').forEach(group => {
    const header = group.querySelector('.category-header');
    const grid   = group.querySelector('.grid');
    if (!header || !grid) return;
    const anyVisible = [...grid.querySelectorAll('.service-card')].some(c => c.style.display !== 'none');
    group.style.display = anyVisible ? '' : 'none';
  });
}

// ── Clock ─────────────────────────────────────────────────────────────────────

let _clockTimer = null;

function startClock() {
  _updateClock();
  _clockTimer = setInterval(_updateClock, 1000);
}

function stopClock() {
  clearInterval(_clockTimer);
  _clockTimer = null;
}

function _updateClock() {
  const el = document.getElementById('header-clock');
  if (!el) return;
  const now = new Date();
  const days = ['Sun','Mon','Tue','Wed','Thu','Fri','Sat'];
  const months = ['Jan','Feb','Mar','Apr','May','Jun','Jul','Aug','Sep','Oct','Nov','Dec'];
  const pad = n => String(n).padStart(2, '0');
  el.textContent = `${days[now.getDay()]} ${pad(now.getDate())} ${months[now.getMonth()]}  ${pad(now.getHours())}:${pad(now.getMinutes())}:${pad(now.getSeconds())}`;
}

// ── System stats bar ─────────────────────────────────────────────────────────

let _sysinfoTimer = null;

async function startSysinfo() {
  await _fetchSysinfo();
  _sysinfoTimer = setInterval(_fetchSysinfo, 5000);
}

function stopSysinfo() {
  clearInterval(_sysinfoTimer);
  _sysinfoTimer = null;
}

async function _fetchSysinfo() {
  try {
    const s = await api('GET', '/api/sysinfo');
    const bar = document.getElementById('sysinfo-bar');
    if (!bar) return;
    bar.style.display = '';
    bar.innerHTML = `
      <span class="sysinfo-item"><span class="sysinfo-label">CPU</span>${s.cpu_percent.toFixed(1)}%</span>
      <span class="sysinfo-sep">·</span>
      <span class="sysinfo-item"><span class="sysinfo-label">RAM</span>${_fmtGB(s.mem_used_mb)} / ${_fmtGB(s.mem_total_mb)}</span>
      <span class="sysinfo-sep">·</span>
      <span class="sysinfo-item"><span class="sysinfo-label">Disk</span>${s.disk_used_gb} / ${s.disk_total_gb} GB</span>`;
  } catch {
    const bar = document.getElementById('sysinfo-bar');
    if (bar) bar.style.display = 'none';
  }
}

function _fmtGB(mb) {
  const gb = mb / 1024;
  return gb >= 1 ? gb.toFixed(1) + ' GB' : mb + ' MB';
}

// ── Drag-to-reorder ──────────────────────────────────────────────────────────

let _dragSrc = null;

function _initDrag(grid, sorted) {
  grid.querySelectorAll('.service-card').forEach(card => {
    card.addEventListener('dragstart', e => {
      _dragSrc = card;
      card.classList.add('dragging');
      e.dataTransfer.effectAllowed = 'move';
    });
    card.addEventListener('dragend', () => {
      _dragSrc = null;
      grid.querySelectorAll('.service-card').forEach(c => c.classList.remove('dragging', 'drag-over'));
    });
    card.addEventListener('dragover', e => {
      e.preventDefault();
      e.dataTransfer.dropEffect = 'move';
      if (card !== _dragSrc) {
        grid.querySelectorAll('.service-card').forEach(c => c.classList.remove('drag-over'));
        card.classList.add('drag-over');
      }
    });
    card.addEventListener('drop', e => {
      e.preventDefault();
      if (!_dragSrc || _dragSrc === card) return;
      const cards = [...grid.querySelectorAll('.service-card')];
      const srcIdx = cards.indexOf(_dragSrc);
      const dstIdx = cards.indexOf(card);
      if (srcIdx < dstIdx) {
        card.after(_dragSrc);
      } else {
        card.before(_dragSrc);
      }
      card.classList.remove('drag-over');
      _saveOrder(grid);
    });
  });
}

async function _saveOrder(grid) {
  const ids = [...grid.querySelectorAll('.service-card')].map(c => c.dataset.id);
  try {
    await api('POST', '/api/services/reorder', { ids });
  } catch (e) {
    toast('Failed to save order', 'error');
  }
}

// ── Manage view ───────────────────────────────────────────────────────────────

async function loadManage() {
  await Promise.all([loadStatus(), loadScanSubnets(), loadServices(), loadBookmarks(), loadDiscovered(), loadDDNS(), loadIgnored(), loadSettings(), _refreshCategoryCache()]);
}

// ── Status ────────────────────────────────────────────────────────────────────

async function loadStatus() {
  try {
    const s = await api('GET', '/api/status');
    statusData = s;
    renderStatus(s);
  } catch (e) {
    document.getElementById('status-grid').innerHTML =
      `<div class="status-cell"><span style="color:var(--danger)">${e.message}</span></div>`;
  }
}

function renderStatus(s) {
  const lastScan = s.last_scan && !s.last_scan.startsWith('0001')
    ? relativeTime(new Date(s.last_scan)) : 'Never';
  const nextScan = s.next_scan && !s.next_scan.startsWith('0001')
    ? relativeTime(new Date(s.next_scan)) : '—';
  const scanning = s.scanning
    ? '<span class="status-value scanning">⟳ Scanning…</span>'
    : '<span class="status-value ok">Idle</span>';

  document.getElementById('status-grid').innerHTML = `
    <div class="status-cell">
      <div class="status-label">Scanner</div>
      ${scanning}
    </div>
    <div class="status-cell">
      <div class="status-label">Last Scan</div>
      <div class="status-value">${lastScan}</div>
    </div>
    <div class="status-cell">
      <div class="status-label">Next Scan</div>
      <div class="status-value">${nextScan}</div>
    </div>
    <div class="status-cell">
      <div class="status-label">Interval</div>
      <div class="status-value">${s.scan_interval}</div>
    </div>
    <div class="status-cell">
      <div class="status-label">Public IP</div>
      <div class="status-value">${s.public_ip || '—'}</div>
    </div>
    <div class="status-cell">
      <div class="status-label">Server IP</div>
      <div class="status-value">${s.server_ip || '—'}</div>
    </div>`;

  const logEl = document.getElementById('scan-log');
  if (s.scan_log && s.scan_log.length > 0) {
    clearTimeout(_logHideTimer);
    logEl.style.display = '';
    logEl.innerHTML = s.scan_log.map(line => `<div class="${_scanLogClass(line)}">${esc(line)}</div>`).join('');
    logEl.scrollTop = logEl.scrollHeight;
    if (!s.scanning) {
      _logHideTimer = setTimeout(() => { logEl.style.display = 'none'; }, 15000);
    }
  } else if (!s.scanning) {
    logEl.style.display = 'none';
  }
}

function _scanLogClass(line) {
  if (line.includes('[OPEN]'))  return 'scan-log-line open';
  if (line.includes('[ERR]'))   return 'scan-log-line err';
  if (line.includes('[TCP]'))   return 'scan-log-line tcp';
  if (line.includes('[HTTP]'))  return 'scan-log-line http';
  if (line.includes('[ARP]'))   return 'scan-log-line arp';
  if (line.includes('[SCAN]'))  return 'scan-log-line scan';
  return 'scan-log-line';
}

let _logHideTimer = null;
let _scanPollTimer = null;

async function triggerScan() {
  const btn = document.getElementById('scan-btn');
  btn.disabled = true;
  btn.textContent = '⟳ Scanning…';
  try {
    await api('POST', '/api/scan');
    toast('Network scan started');
    _pollScanStatus();
  } catch (e) {
    toast(e.message, 'error');
    btn.disabled = false;
    btn.textContent = '⟳ Scan Now';
  }
}

async function _pollScanStatus() {
  clearTimeout(_scanPollTimer);
  try {
    const s = await api('GET', '/api/status');
    statusData = s;
    renderStatus(s);
    if (s.scanning) {
      _scanPollTimer = setTimeout(_pollScanStatus, 1500);
    } else {
      document.getElementById('scan-btn').disabled = false;
      document.getElementById('scan-btn').textContent = '⟳ Scan Now';
      loadDiscovered();
    }
  } catch (e) {
    document.getElementById('scan-btn').disabled = false;
    document.getElementById('scan-btn').textContent = '⟳ Scan Now';
  }
}

// ── Scan Subnets ──────────────────────────────────────────────────────────────

async function loadScanSubnets() {
  const el = document.getElementById('subnets-list');
  try {
    const subnets = await api('GET', '/api/scan/subnets');
    if (!subnets || subnets.length === 0) {
      el.innerHTML = '<div class="empty-small">No subnets configured — local /24 will be auto-detected.</div>';
      return;
    }
    el.innerHTML = subnets.map(cidr => `
      <div class="ddns-item">
        <div class="ddns-domain">${esc(cidr)}</div>
        <button class="btn btn-ghost btn-sm" onclick="removeSubnet('${esc(cidr)}')">✕</button>
      </div>`).join('');
  } catch (e) {
    el.innerHTML = `<p style="color:var(--danger);padding:1rem">${e.message}</p>`;
  }
}

async function removeSubnet(cidr) {
  if (!confirm(`Remove subnet "${cidr}" from scan list?`)) return;
  try {
    await api('DELETE', '/api/scan/subnets', { cidr });
    toast(`Removed ${cidr}`);
    loadScanSubnets();
  } catch (e) {
    toast(e.message, 'error');
  }
}

function showAddSubnetModal() {
  openModal(`
    <h3>Add Scan Subnet</h3>
    <p style="color:var(--muted);font-size:.875rem;margin-bottom:1.25rem">
      Enter a subnet in CIDR notation. It will be included in every network scan.
    </p>
    <div class="form-group">
      <label>Subnet (CIDR)</label>
      <input id="m-cidr" type="text" placeholder="10.0.1.0/24">
    </div>
    <div class="form-actions">
      <button class="btn btn-ghost" onclick="closeModal()">Cancel</button>
      <button class="btn btn-primary" onclick="submitAddSubnet()">Add →</button>
    </div>`);
}

async function submitAddSubnet() {
  const cidr = document.getElementById('m-cidr').value.trim();
  if (!cidr) { toast('CIDR is required', 'error'); return; }
  try {
    await api('POST', '/api/scan/subnets', { cidr });
    closeModal();
    toast(`Added ${cidr}`);
    loadScanSubnets();
  } catch (e) {
    toast(e.message, 'error');
  }
}

// ── Services ──────────────────────────────────────────────────────────────────

async function loadServices() {
  const el = document.getElementById('services-list');
  try {
    const services = await api('GET', '/api/services');
    if (!services || services.length === 0) {
      el.innerHTML = '<div class="empty-small">No services assigned yet.</div>';
      return;
    }
    const domain = statusData.domain || '';
    const rows = services
      .sort((a, b) => (a.order ?? 0) - (b.order ?? 0) || a.name.localeCompare(b.name))
      .map(svc => serviceRow(svc, domain))
      .join('');
    el.innerHTML = `
      <div class="table-wrap">
        <table>
          <thead><tr>
            <th class="td-icon"></th>
            <th>Name</th><th>Subdomain</th><th>Target</th><th>Source</th><th>Actions</th>
          </tr></thead>
          <tbody>${rows}</tbody>
        </table>
      </div>`;
  } catch (e) {
    el.innerHTML = `<p style="color:var(--danger);padding:1rem">${e.message}</p>`;
  }
}

function serviceRow(svc, domain) {
  const url  = `https://${svc.subdomain}.${domain}`;
  let icon;
  if (svc.icon) {
    icon = `<img class="svc-icon" src="${esc(svc.icon)}" alt="" loading="lazy" onerror="this.style.display='none'">`;
  } else {
    const faviconSrc = `/api/favicon?url=${encodeURIComponent(svc.target)}`;
    icon = `<img class="svc-icon" src="${faviconSrc}" alt="" loading="lazy" onerror="this.style.display='none';this.nextElementSibling.style.display='inline-flex'"><span class="svc-icon-placeholder" style="display:none">${svcEmoji(svc)}</span>`;
  }
  const tag = `<span class="tag tag-${svc.source}">${svc.source}</span>`;
  return `
    <tr>
      <td class="td-icon">${icon}</td>
      <td><strong>${esc(svc.name)}</strong></td>
      <td class="link-cell"><a href="${esc(url)}" target="_blank" rel="noopener">${esc(svc.subdomain)}.${esc(domain)}</a></td>
      <td><code style="font-size:.8rem;color:var(--muted)">${esc(svc.target)}</code></td>
      <td>${tag}</td>
      <td>
        <div class="actions">
          <button class="btn btn-ghost btn-sm" onclick="showEditModal('${esc(svc.id)}')">✏ Edit</button>
          <button class="btn btn-danger btn-sm" onclick="deleteService('${esc(svc.id)}','${esc(svc.name)}')">✕</button>
        </div>
      </td>
    </tr>`;
}

async function deleteService(id, name) {
  if (!confirm(`Remove service "${name}" and its DNS record?`)) return;
  try {
    await api('DELETE', `/api/services/${id}`);
    toast(`Removed ${name}`);
    loadServices();
    loadHome();
  } catch (e) {
    toast(e.message, 'error');
  }
}

// ── Discovered ────────────────────────────────────────────────────────────────

async function loadDiscovered() {
  const el    = document.getElementById('discovered-list');
  const badge = document.getElementById('discovered-count');
  try {
    const items = await api('GET', '/api/discovered');
    badge.textContent = items ? items.length : 0;
    if (!items || items.length === 0) {
      el.innerHTML = '<div class="empty-small">No unassigned services. Run a scan to discover services.</div>';
      return;
    }
    el.innerHTML = items.map(d => discItem(d)).join('');
  } catch (e) {
    el.innerHTML = `<p style="color:var(--danger);padding:1rem">${e.message}</p>`;
  }
}

function discItem(d) {
  const scheme = [443, 5001, 8443, 8920, 9443].includes(d.port) ? 'https' : 'http';
  const url    = `${scheme}://${d.ip}:${d.port}`;
  // Emoji icons from fingerprint vs base64 favicons need different rendering.
  const icon   = d.icon && d.icon.startsWith('data:')
    ? `<img class="svc-icon" src="${esc(d.icon)}" alt="" loading="lazy" onerror="this.style.display='none'">`
    : `<span class="svc-icon-placeholder">${esc(d.icon || '🖥️')}</span>`;
  const src    = `<span class="tag tag-${d.source}">${d.source}</span>`;
  // Prefer fingerprinted service name over raw page title.
  const label  = d.service_name || d.title || d.container_name || `${d.ip}:${d.port}`;
  const conf   = d.service_name && d.confidence
    ? ` <span style="font-size:.7rem;color:var(--muted);background:rgba(255,255,255,.06);padding:.1em .4em;border-radius:.25em;margin-left:.35em">${Math.round(d.confidence * 100)}%</span>`
    : '';
  return `
    <div class="disc-item">
      ${icon}
      <div class="disc-info">
        <div class="disc-title">${esc(label)}${conf}</div>
        <div class="disc-meta">${src} <a href="${esc(url)}" target="_blank" rel="noopener">${esc(url)}</a></div>
      </div>
      <div class="disc-actions">
        <button class="btn btn-primary btn-sm" onclick="showAssignModal('${esc(d.id)}','${esc(label)}')">Assign</button>
        <button class="btn btn-ghost btn-sm" onclick="ignoreDiscovered('${esc(d.id)}')">Ignore</button>
        <button class="btn btn-ghost btn-sm" onclick="dismissDiscovered('${esc(d.id)}')">✕</button>
      </div>
    </div>`;
}

async function dismissDiscovered(id) {
  try {
    await api('DELETE', `/api/discovered/${id}`);
    loadDiscovered();
  } catch (e) {
    toast(e.message, 'error');
  }
}

async function ignoreDiscovered(id) {
  try {
    await api('POST', `/api/discovered/${id}/ignore`);
    toast('Service ignored — will not reappear in scans');
    loadDiscovered();
    loadIgnored();
  } catch (e) {
    toast(e.message, 'error');
  }
}

// ── Ignored ───────────────────────────────────────────────────────────────────

let _ignoredVisible = false;

function toggleIgnored() {
  _ignoredVisible = !_ignoredVisible;
  document.getElementById('ignored-section').style.display = _ignoredVisible ? '' : 'none';
  if (_ignoredVisible) loadIgnored();
}

async function loadIgnored() {
  const el = document.getElementById('ignored-list');
  if (!el) return;
  try {
    const items = await api('GET', '/api/ignored');
    if (!items || items.length === 0) {
      el.innerHTML = '<div class="empty-small">No ignored services.</div>';
      return;
    }
    el.innerHTML = items.map(ig => `
      <div class="disc-item">
        <span class="svc-icon-placeholder">🚫</span>
        <div class="disc-info">
          <div class="disc-title">${esc(ig.ip)}:${ig.port}${ig.title ? ' — ' + esc(ig.title) : ''}</div>
          <div class="disc-meta" style="font-size:.8rem;color:var(--muted)">Ignored ${relativeTime(new Date(ig.ignored_at))}</div>
        </div>
        <div class="disc-actions">
          <button class="btn btn-ghost btn-sm" onclick="unignoreService('${esc(ig.id)}')">Un-ignore</button>
        </div>
      </div>`).join('');
  } catch (e) {
    el.innerHTML = `<p style="color:var(--danger);padding:1rem">${e.message}</p>`;
  }
}

async function unignoreService(id) {
  try {
    await api('DELETE', `/api/ignored/${id}`);
    toast('Service un-ignored — will reappear in future scans');
    loadIgnored();
  } catch (e) {
    toast(e.message, 'error');
  }
}

// ── DDNS ──────────────────────────────────────────────────────────────────────

async function loadDDNS() {
  const el = document.getElementById('ddns-list');
  try {
    const data = await api('GET', '/api/ddns');
    const domains = data ? data.domains : [];
    const publicIP = data ? data.public_ip : '—';

    if (!domains || domains.length === 0) {
      el.innerHTML = '<div class="empty-small">No DDNS domains configured.</div>';
      return;
    }
    el.innerHTML = domains.map(d => `
      <div class="ddns-item">
        <div class="ddns-domain">${esc(d)}</div>
        <div style="font-size:.8rem;color:var(--muted)">→ ${esc(publicIP)}</div>
        <button class="btn btn-ghost btn-sm" onclick="removeDDNS('${esc(d)}')">✕</button>
      </div>`).join('');
  } catch (e) {
    el.innerHTML = `<p style="color:var(--danger);padding:1rem">${e.message}</p>`;
  }
}

async function removeDDNS(domain) {
  if (!confirm(`Remove DDNS domain "${domain}"?`)) return;
  try {
    await api('DELETE', `/api/ddns/${encodeURIComponent(domain)}`);
    toast(`Removed ${domain}`);
    loadDDNS();
  } catch (e) {
    toast(e.message, 'error');
  }
}

// ── Modals ────────────────────────────────────────────────────────────────────

function openModal(html) {
  document.getElementById('modal-content').innerHTML = html;
  document.getElementById('modal-backdrop').style.display = 'flex';
}

function closeModal() {
  document.getElementById('modal-backdrop').style.display = 'none';
  document.getElementById('modal-content').innerHTML = '';
}

// Assign a discovered service
function showAssignModal(id, title) {
  const suggested = sanitiseSubdomain(title);
  openModal(`
    <h3>Assign Subdomain</h3>
    <p style="color:var(--muted);font-size:.875rem;margin-bottom:1.25rem">
      Assign a subdomain for <strong>${esc(title)}</strong>.
      A DNS record will be created automatically.
    </p>
    <div class="form-group">
      <label>Service Name</label>
      <input id="m-name" type="text" value="${esc(title)}" placeholder="My Service">
    </div>
    <div class="form-group">
      <label>Subdomain</label>
      <input id="m-sub" type="text" value="${esc(suggested)}" placeholder="myservice">
      <div class="input-hint">Will be available at <span id="sub-preview">${esc(suggested)}.${esc(statusData.domain || '')}</span></div>
    </div>
    <div class="form-group">
      <label>Category <span style="color:var(--muted);font-weight:400">(optional)</span></label>
      <input id="m-category" type="text" list="category-list" placeholder="e.g. Media">
      ${_categoryDatalist()}
    </div>
    <div class="form-actions">
      <button class="btn btn-ghost" onclick="closeModal()">Cancel</button>
      <button class="btn btn-primary" onclick="submitAssign('${esc(id)}')">Assign →</button>
    </div>`);

  document.getElementById('m-sub').addEventListener('input', (e) => {
    const val = sanitiseSubdomain(e.target.value) || e.target.value;
    document.getElementById('sub-preview').textContent = `${val}.${statusData.domain || ''}`;
  });
}

async function submitAssign(discoveredID) {
  const name      = document.getElementById('m-name').value.trim();
  const subdomain = document.getElementById('m-sub').value.trim();
  const category  = document.getElementById('m-category').value.trim();
  if (!subdomain) { toast('Subdomain is required', 'error'); return; }
  try {
    await api('POST', '/api/services', { discovered_id: discoveredID, name, subdomain, category });
    closeModal();
    toast(`Assigned ${subdomain}.${statusData.domain || ''}`);
    loadServices();
    loadDiscovered();
  } catch (e) {
    toast(e.message, 'error');
  }
}

// Add a service manually
function showAddManualModal() {
  openModal(`
    <h3>Add Service</h3>
    <div class="form-group">
      <label>Service Name</label>
      <input id="m-name" type="text" placeholder="Plex">
    </div>
    <div class="form-group">
      <label>Subdomain</label>
      <input id="m-sub" type="text" placeholder="plex">
      <div class="input-hint">Will be available at <span id="sub-preview">.${esc(statusData.domain || '')}</span></div>
    </div>
    <div class="form-group">
      <label>Target URL</label>
      <input id="m-target" type="text" placeholder="http://10.0.0.5:32400">
    </div>
    <div class="form-group">
      <label>Category <span style="color:var(--muted);font-weight:400">(optional)</span></label>
      <input id="m-category" type="text" list="category-list" placeholder="e.g. Media">
      ${_categoryDatalist()}
    </div>
    <div class="form-actions">
      <button class="btn btn-ghost" onclick="closeModal()">Cancel</button>
      <button class="btn btn-primary" onclick="submitAddManual()">Add →</button>
    </div>`);

  document.getElementById('m-sub').addEventListener('input', (e) => {
    const val = sanitiseSubdomain(e.target.value) || e.target.value;
    document.getElementById('sub-preview').textContent = `${val}.${statusData.domain || ''}`;
  });
}

async function submitAddManual() {
  const name      = document.getElementById('m-name').value.trim();
  const subdomain = document.getElementById('m-sub').value.trim();
  const target    = document.getElementById('m-target').value.trim();
  const category  = document.getElementById('m-category').value.trim();
  if (!subdomain) { toast('Subdomain is required', 'error'); return; }
  if (!target)    { toast('Target URL is required', 'error'); return; }
  try {
    await api('POST', '/api/services', { name, subdomain, target, category });
    closeModal();
    toast(`Added ${subdomain}.${statusData.domain || ''}`);
    loadServices();
  } catch (e) {
    toast(e.message, 'error');
  }
}

// Edit existing service
async function showEditModal(id) {
  let svc;
  try {
    const all = await api('GET', '/api/services');
    svc = all.find(s => s.id === id);
    if (!svc) { toast('Service not found', 'error'); return; }
  } catch (e) {
    toast(e.message, 'error');
    return;
  }

  const iconPreview = svc.icon
    ? `<img src="${esc(svc.icon)}" alt="" style="width:40px;height:40px;border-radius:6px;object-fit:contain">`
    : `<span class="svc-icon-placeholder" style="width:40px;height:40px;font-size:1.25rem">${svcEmoji(svc)}</span>`;
  const removeBtn = svc.icon
    ? `<button class="btn btn-ghost btn-sm" onclick="_clearEditIcon('${esc(id)}')">Remove</button>`
    : '';

  openModal(`
    <h3>Edit Service</h3>
    <div class="form-group">
      <label>Service Name</label>
      <input id="m-name" type="text" value="${esc(svc.name)}">
    </div>
    <div class="form-group">
      <label>Subdomain</label>
      <input id="m-sub" type="text" value="${esc(svc.subdomain)}">
      <div class="input-hint">Will be available at <span id="sub-preview">${esc(svc.subdomain)}.${esc(statusData.domain || '')}</span></div>
    </div>
    <div class="form-group">
      <label>Target URL</label>
      <input id="m-target" type="text" value="${esc(svc.target)}">
    </div>
    <div class="form-group">
      <label>Category <span style="color:var(--muted);font-weight:400">(optional)</span></label>
      <input id="m-category" type="text" list="category-list" value="${esc(svc.category || '')}" placeholder="e.g. Media">
      ${_categoryDatalist()}
    </div>
    <div class="form-group">
      <label>Icon</label>
      <div class="icon-edit-row">
        <div id="m-icon-preview">${iconPreview}</div>
        <label class="btn btn-ghost btn-sm" for="m-icon-file" style="cursor:pointer">Upload</label>
        <input id="m-icon-file" type="file" accept="image/*" style="display:none" onchange="_previewIconFile(this)">
        <button class="btn btn-ghost btn-sm" id="m-pull-favicon-btn" onclick="_pullFavicon('${esc(id)}')">Pull Favicon</button>
        ${removeBtn}
      </div>
      <div class="input-hint">Upload a custom icon, pull from the service, or leave empty to use the live favicon proxy.</div>
    </div>
    <div class="form-actions">
      <button class="btn btn-ghost" onclick="closeModal()">Cancel</button>
      <button class="btn btn-primary" onclick="submitEdit('${esc(id)}')">Save →</button>
    </div>`);

  document.getElementById('m-sub').addEventListener('input', (e) => {
    const val = sanitiseSubdomain(e.target.value) || e.target.value;
    document.getElementById('sub-preview').textContent = `${val}.${statusData.domain || ''}`;
  });
}

async function _pullFavicon(id) {
  const btn = document.getElementById('m-pull-favicon-btn');
  if (btn) { btn.disabled = true; btn.textContent = '⟳ Fetching…'; }
  try {
    const svc = await api('POST', `/api/services/${id}/favicon`);
    document.getElementById('m-icon-preview').innerHTML =
      `<img src="${esc(svc.icon)}" alt="" style="width:40px;height:40px;border-radius:6px;object-fit:contain">`;
    const fileInput = document.getElementById('m-icon-file');
    if (fileInput) { fileInput.value = ''; fileInput.dataset.pendingClear = ''; }
    toast('Favicon pulled');
  } catch (e) {
    toast(e.message || 'No favicon found', 'error');
  } finally {
    if (btn) { btn.disabled = false; btn.textContent = 'Pull Favicon'; }
  }
}

function _previewIconFile(input) {
  const file = input.files[0];
  if (!file) return;
  const reader = new FileReader();
  reader.onload = e => {
    document.getElementById('m-icon-preview').innerHTML =
      `<img src="${e.target.result}" alt="" style="width:40px;height:40px;border-radius:6px;object-fit:contain">`;
  };
  reader.readAsDataURL(file);
  input.dataset.pendingClear = '';
}

function _clearEditIcon(id) {
  document.getElementById('m-icon-preview').innerHTML =
    `<span class="svc-icon-placeholder" style="width:40px;height:40px;font-size:1.25rem">🖥️</span>`;
  const f = document.getElementById('m-icon-file');
  f.value = '';
  f.dataset.pendingClear = 'yes';
}

async function submitEdit(id) {
  const name      = document.getElementById('m-name').value.trim();
  const subdomain = document.getElementById('m-sub').value.trim();
  const target    = document.getElementById('m-target').value.trim();
  const category  = document.getElementById('m-category').value.trim();
  if (!subdomain) { toast('Subdomain is required', 'error'); return; }
  try {
    const fileInput = document.getElementById('m-icon-file');
    if (fileInput && fileInput.files[0]) {
      const formData = new FormData();
      formData.append('icon', fileInput.files[0]);
      const res = await fetch(`/api/services/${id}/icon`, { method: 'POST', body: formData });
      if (!res.ok) {
        const d = await res.json().catch(() => ({}));
        throw new Error(d.error || `HTTP ${res.status}`);
      }
      await api('PUT', `/api/services/${id}`, { name, subdomain, target, category });
    } else if (fileInput && fileInput.dataset.pendingClear === 'yes') {
      await api('DELETE', `/api/services/${id}/icon`);
      await api('PUT', `/api/services/${id}`, { name, subdomain, target, category });
    } else {
      await api('PUT', `/api/services/${id}`, { name, subdomain, target, category });
    }
    closeModal();
    toast('Service updated');
    loadServices();
    loadHome();
  } catch (e) {
    toast(e.message, 'error');
  }
}

// DDNS
function showAddDDNSModal() {
  openModal(`
    <h3>Add DDNS Domain</h3>
    <p style="color:var(--muted);font-size:.875rem;margin-bottom:1.25rem">
      This domain's A record will be kept pointing to your current public IP.
    </p>
    <div class="form-group">
      <label>Domain</label>
      <input id="m-domain" type="text" placeholder="home.example.com">
    </div>
    <div class="form-actions">
      <button class="btn btn-ghost" onclick="closeModal()">Cancel</button>
      <button class="btn btn-primary" onclick="submitAddDDNS()">Add →</button>
    </div>`);
}

async function submitAddDDNS() {
  const domain = document.getElementById('m-domain').value.trim();
  if (!domain) { toast('Domain is required', 'error'); return; }
  try {
    await api('POST', '/api/ddns', { domain });
    closeModal();
    toast(`Added ${domain}`);
    loadDDNS();
  } catch (e) {
    toast(e.message, 'error');
  }
}

// ── Settings / background ─────────────────────────────────────────────────────

async function loadSettings() {
  try {
    const s = await api('GET', '/api/settings');
    _applyBackground(s.background || '');
    const input = document.getElementById('bg-input');
    if (input) input.value = s.background || '';
  } catch {}
}

function _applyBackground(value) {
  document.body.style.background = value || '';
}

function _setBgPreset(value) {
  const input = document.getElementById('bg-input');
  if (input) input.value = value;
}

async function saveBackground(override) {
  const value = override !== undefined ? override : (document.getElementById('bg-input')?.value.trim() || '');
  try {
    await api('PUT', '/api/settings', { background: value });
    _applyBackground(value);
    if (override === '') {
      const input = document.getElementById('bg-input');
      if (input) input.value = '';
    }
    toast('Background saved');
  } catch (e) {
    toast(e.message, 'error');
  }
}

// ── Bookmarks ─────────────────────────────────────────────────────────────────

async function loadBookmarksHome() {
  const container = document.getElementById('bookmarks-home');
  const grid      = document.getElementById('bookmarks-grid');
  if (!container || !grid) return;
  try {
    const bms = await api('GET', '/api/bookmarks');
    if (!bms || bms.length === 0) {
      container.style.display = 'none';
      return;
    }
    container.style.display = '';

    // Group by category like services.
    const groups = new Map();
    for (const bm of bms) {
      const key = bm.category || '';
      if (!groups.has(key)) groups.set(key, []);
      groups.get(key).push(bm);
    }
    const namedKeys = [...groups.keys()].filter(k => k !== '').sort();
    const orderedKeys = groups.has('') ? ['', ...namedKeys] : namedKeys;

    grid.innerHTML = '';
    for (const key of orderedKeys) {
      const section = document.createElement('div');
      section.className = 'category-group';
      if (key !== '') {
        const collapsed = _isCategoryCollapsed('bm:' + key);
        section.innerHTML = `
          <div class="category-header" onclick="_toggleCategory(this)" data-category="bm:${esc(key)}">
            <span class="category-arrow">${collapsed ? '▶' : '▼'}</span>
            <span class="category-name">${esc(key)}</span>
            <span class="category-count">${groups.get(key).length}</span>
          </div>`;
        const inner = document.createElement('div');
        inner.className = 'grid';
        if (collapsed) inner.style.display = 'none';
        inner.innerHTML = groups.get(key).map(bm => renderBookmarkCard(bm)).join('');
        section.appendChild(inner);
      } else {
        const inner = document.createElement('div');
        inner.className = 'grid';
        inner.innerHTML = groups.get(key).map(bm => renderBookmarkCard(bm)).join('');
        section.appendChild(inner);
      }
      grid.appendChild(section);
    }
  } catch {
    container.style.display = 'none';
  }
}

function renderBookmarkCard(bm) {
  const icon = bm.icon
    ? `<img class="card-icon" src="${esc(bm.icon)}" alt="" loading="lazy" onerror="this.style.display='none';this.nextElementSibling.style.display='flex'"><div class="card-icon-placeholder" style="display:none">🔗</div>`
    : `<div class="card-icon-placeholder">🔗</div>`;
  return `
    <a class="service-card" href="${esc(bm.url)}" target="_blank" rel="noopener" data-name="${esc(bm.name)}" data-sub="">
      ${icon}
      <div class="card-name">${esc(bm.name)}</div>
      <div class="card-url">${esc(bm.url)}</div>
    </a>`;
}

async function loadBookmarks() {
  const el = document.getElementById('bookmarks-list');
  if (!el) return;
  try {
    const bms = await api('GET', '/api/bookmarks');
    if (!bms || bms.length === 0) {
      el.innerHTML = '<div class="empty-small">No bookmarks yet.</div>';
      return;
    }
    el.innerHTML = `
      <div class="table-wrap">
        <table>
          <thead><tr><th>Name</th><th>URL</th><th>Category</th><th>Actions</th></tr></thead>
          <tbody>${bms.map(bm => bookmarkRow(bm)).join('')}</tbody>
        </table>
      </div>`;
  } catch (e) {
    el.innerHTML = `<p style="color:var(--danger);padding:1rem">${e.message}</p>`;
  }
}

function bookmarkRow(bm) {
  return `
    <tr>
      <td><strong>${esc(bm.name)}</strong></td>
      <td><a href="${esc(bm.url)}" target="_blank" rel="noopener" style="font-size:.85rem">${esc(bm.url)}</a></td>
      <td>${bm.category ? `<span class="tag tag-manual">${esc(bm.category)}</span>` : '<span style="color:var(--muted)">—</span>'}</td>
      <td>
        <div class="actions">
          <button class="btn btn-ghost btn-sm" onclick="showEditBookmarkModal('${esc(bm.id)}')">✏ Edit</button>
          <button class="btn btn-danger btn-sm" onclick="deleteBookmark('${esc(bm.id)}','${esc(bm.name)}')">✕</button>
        </div>
      </td>
    </tr>`;
}

function showAddBookmarkModal() {
  openModal(`
    <h3>Add Bookmark</h3>
    <div class="form-group">
      <label>Name</label>
      <input id="m-name" type="text" placeholder="GitHub">
    </div>
    <div class="form-group">
      <label>URL</label>
      <input id="m-bm-url" type="url" placeholder="https://github.com">
    </div>
    <div class="form-group">
      <label>Category <span style="color:var(--muted);font-weight:400">(optional)</span></label>
      <input id="m-category" type="text" list="category-list" placeholder="e.g. Tools">
      ${_categoryDatalist()}
    </div>
    <div class="form-actions">
      <button class="btn btn-ghost" onclick="closeModal()">Cancel</button>
      <button class="btn btn-primary" onclick="submitAddBookmark()">Add →</button>
    </div>`);
}

async function submitAddBookmark() {
  const name     = document.getElementById('m-name').value.trim();
  const url      = document.getElementById('m-bm-url').value.trim();
  const category = document.getElementById('m-category').value.trim();
  if (!url) { toast('URL is required', 'error'); return; }
  try {
    await api('POST', '/api/bookmarks', { name, url, category });
    closeModal();
    toast('Bookmark added');
    loadBookmarks();
    loadBookmarksHome();
  } catch (e) {
    toast(e.message, 'error');
  }
}

async function showEditBookmarkModal(id) {
  const bms = await api('GET', '/api/bookmarks').catch(() => []);
  const bm  = (bms || []).find(b => b.id === id);
  if (!bm) { toast('Bookmark not found', 'error'); return; }
  openModal(`
    <h3>Edit Bookmark</h3>
    <div class="form-group">
      <label>Name</label>
      <input id="m-name" type="text" value="${esc(bm.name)}">
    </div>
    <div class="form-group">
      <label>URL</label>
      <input id="m-bm-url" type="url" value="${esc(bm.url)}">
    </div>
    <div class="form-group">
      <label>Category <span style="color:var(--muted);font-weight:400">(optional)</span></label>
      <input id="m-category" type="text" list="category-list" value="${esc(bm.category || '')}" placeholder="e.g. Tools">
      ${_categoryDatalist()}
    </div>
    <div class="form-actions">
      <button class="btn btn-ghost" onclick="closeModal()">Cancel</button>
      <button class="btn btn-primary" onclick="submitEditBookmark('${esc(id)}')">Save →</button>
    </div>`);
}

async function submitEditBookmark(id) {
  const name     = document.getElementById('m-name').value.trim();
  const url      = document.getElementById('m-bm-url').value.trim();
  const category = document.getElementById('m-category').value.trim();
  if (!url) { toast('URL is required', 'error'); return; }
  try {
    await api('PUT', `/api/bookmarks/${id}`, { name, url, category });
    closeModal();
    toast('Bookmark updated');
    loadBookmarks();
    loadBookmarksHome();
  } catch (e) {
    toast(e.message, 'error');
  }
}

async function deleteBookmark(id, name) {
  if (!confirm(`Remove bookmark "${name}"?`)) return;
  try {
    await api('DELETE', `/api/bookmarks/${id}`);
    toast(`Removed ${name}`);
    loadBookmarks();
    loadBookmarksHome();
  } catch (e) {
    toast(e.message, 'error');
  }
}

// ── Category helpers ──────────────────────────────────────────────────────────

let _cachedCategories = [];

function _categoryDatalist() {
  return `<datalist id="category-list">${_cachedCategories.map(c => `<option value="${esc(c)}">`).join('')}</datalist>`;
}

async function _refreshCategoryCache() {
  try {
    const services = await api('GET', '/api/services');
    const cats = new Set((services || []).map(s => s.category).filter(Boolean));
    _cachedCategories = [...cats].sort();
  } catch {}
}

// ── Utilities ─────────────────────────────────────────────────────────────────

function esc(str) {
  if (str == null) return '';
  return String(str)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}

function sanitiseSubdomain(s) {
  return String(s || '')
    .toLowerCase()
    .replace(/[^a-z0-9-]/g, '-')
    .replace(/-+/g, '-')
    .replace(/^-+|-+$/g, '');
}

function relativeTime(date) {
  const diff = Date.now() - date.getTime();
  const abs  = Math.abs(diff);
  const future = diff < 0;
  const secs   = Math.floor(abs / 1000);
  const mins   = Math.floor(secs / 60);
  const hours  = Math.floor(mins / 60);
  const days   = Math.floor(hours / 24);
  let label;
  if (secs < 60)        label = 'just now';
  else if (mins < 60)   label = `${mins}m`;
  else if (hours < 24)  label = `${hours}h`;
  else                  label = `${days}d`;
  if (label === 'just now') return label;
  return future ? `in ${label}` : `${label} ago`;
}

// ── Keyboard shortcuts ────────────────────────────────────────────────────────

document.addEventListener('keydown', e => {
  // Ignore when typing in an input/textarea.
  const tag = document.activeElement.tagName;
  const inInput = tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT';

  if (e.key === 'Escape') {
    const modal = document.getElementById('modal-backdrop');
    if (modal && modal.style.display !== 'none') {
      closeModal();
      return;
    }
    const search = document.getElementById('search-input');
    if (search && search.value) {
      search.value = '';
      _filterCards();
      search.blur();
      return;
    }
    return;
  }

  if (inInput) return;

  // '/' focuses search bar.
  if (e.key === '/' && currentView === 'home') {
    e.preventDefault();
    document.getElementById('search-input')?.focus();
    return;
  }

  // 1–9 opens the Nth visible service card.
  if (currentView === 'home' && e.key >= '1' && e.key <= '9') {
    const n = parseInt(e.key, 10);
    const cards = [...document.querySelectorAll('#services-grid .service-card')]
      .filter(c => c.style.display !== 'none');
    if (cards[n - 1]) {
      cards[n - 1].click();
    }
  }
});
