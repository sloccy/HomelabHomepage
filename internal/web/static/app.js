/* ── Launchpad frontend ─────────────────────────────────────────────────── */
'use strict';

let currentView = 'home';
let statusData = {};

// ── View routing ─────────────────────────────────────────────────────────────

function showView(view) {
  currentView = view;
  document.getElementById('view-home').style.display   = view === 'home'   ? '' : 'none';
  document.getElementById('view-manage').style.display = view === 'manage' ? '' : 'none';
  window.location.hash = view === 'home' ? '' : view;
  if (view === 'home')   loadHome();
  if (view === 'manage') loadManage();
}

window.addEventListener('DOMContentLoaded', () => {
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
  const grid  = document.getElementById('services-grid');
  const empty = document.getElementById('home-empty');
  try {
    const services = await api('GET', '/api/services');
    const status   = await api('GET', '/api/status');
    statusData = status;

    if (!services || services.length === 0) {
      grid.innerHTML  = '';
      empty.style.display = '';
      return;
    }
    empty.style.display = 'none';
    grid.innerHTML = services
      .sort((a, b) => a.name.localeCompare(b.name))
      .map(svc => renderCard(svc, status.domain))
      .join('');
  } catch (e) {
    grid.innerHTML = `<p style="color:var(--danger);padding:2rem">${e.message}</p>`;
  }
}

function renderCard(svc, domain) {
  const url  = `https://${svc.subdomain}.${domain}`;
  const icon = svc.icon
    ? `<img class="card-icon" src="${esc(svc.icon)}" alt="" loading="lazy" onerror="this.style.display='none'">`
    : `<div class="card-icon-placeholder">${svcEmoji(svc)}</div>`;
  return `
    <a class="service-card" href="${esc(url)}" target="_blank" rel="noopener">
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

// ── Manage view ───────────────────────────────────────────────────────────────

async function loadManage() {
  await Promise.all([loadStatus(), loadScanSubnets(), loadServices(), loadDiscovered(), loadDDNS()]);
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
}

async function triggerScan() {
  const btn = document.getElementById('scan-btn');
  btn.disabled = true;
  btn.textContent = '⟳ Scanning…';
  try {
    await api('POST', '/api/scan');
    toast('Network scan started');
    setTimeout(loadStatus, 1500);
    setTimeout(loadStatus, 8000);
    setTimeout(() => { loadStatus(); loadDiscovered(); }, 20000);
  } catch (e) {
    toast(e.message, 'error');
  } finally {
    btn.disabled = false;
    btn.textContent = '⟳ Scan Now';
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
      .sort((a, b) => a.name.localeCompare(b.name))
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
  const icon = svc.icon
    ? `<img class="svc-icon" src="${esc(svc.icon)}" alt="" loading="lazy" onerror="this.style.display='none'">`
    : `<span class="svc-icon-placeholder">${svcEmoji(svc)}</span>`;
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
  const scheme = [443, 8443, 9443].includes(d.port) ? 'https' : 'http';
  const url    = `${scheme}://${d.ip}:${d.port}`;
  const icon   = d.icon
    ? `<img class="svc-icon" src="${esc(d.icon)}" alt="" loading="lazy" onerror="this.style.display='none'">`
    : `<span class="svc-icon-placeholder">🖥️</span>`;
  const src = `<span class="tag tag-${d.source}">${d.source}</span>`;
  const label = d.title || d.container_name || `${d.ip}:${d.port}`;
  return `
    <div class="disc-item">
      ${icon}
      <div class="disc-info">
        <div class="disc-title">${esc(label)}</div>
        <div class="disc-meta">${src} <a href="${esc(url)}" target="_blank" rel="noopener">${esc(url)}</a></div>
      </div>
      <div class="disc-actions">
        <button class="btn btn-primary btn-sm" onclick="showAssignModal('${esc(d.id)}','${esc(label)}')">Assign</button>
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
  if (!subdomain) { toast('Subdomain is required', 'error'); return; }
  try {
    await api('POST', '/api/services', { discovered_id: discoveredID, name, subdomain });
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
  if (!subdomain) { toast('Subdomain is required', 'error'); return; }
  if (!target)    { toast('Target URL is required', 'error'); return; }
  try {
    await api('POST', '/api/services', { name, subdomain, target });
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
    <div class="form-actions">
      <button class="btn btn-ghost" onclick="closeModal()">Cancel</button>
      <button class="btn btn-primary" onclick="submitEdit('${esc(id)}')">Save →</button>
    </div>`);

  document.getElementById('m-sub').addEventListener('input', (e) => {
    const val = sanitiseSubdomain(e.target.value) || e.target.value;
    document.getElementById('sub-preview').textContent = `${val}.${statusData.domain || ''}`;
  });
}

async function submitEdit(id) {
  const name      = document.getElementById('m-name').value.trim();
  const subdomain = document.getElementById('m-sub').value.trim();
  const target    = document.getElementById('m-target').value.trim();
  if (!subdomain) { toast('Subdomain is required', 'error'); return; }
  try {
    await api('PUT', `/api/services/${id}`, { name, subdomain, target });
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
      <input id="m-domain" type="text" placeholder="home.sloccy.com">
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

// Keyboard: Escape closes modal
document.addEventListener('keydown', e => {
  if (e.key === 'Escape') closeModal();
});
