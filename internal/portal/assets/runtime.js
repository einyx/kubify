// ── Refresh cadence ────────────────────────────────────────────────────
let refreshMs = 10000;
let refreshTimer;
function scheduleRefresh() {
  clearInterval(refreshTimer);
  if (refreshMs > 0) refreshTimer = setInterval(tick, sseLive ? refreshMs : Math.max(refreshMs, 30000));
}
function tick() {
  if (document.hidden) return;
  refresh();
  if (currentDetail && document.getElementById('detail').style.display !== 'none') {
    fetchDetail(currentDetail.ns, currentDetail.name);
  }
}

// ── Real-time (SSE change signal from the portal) ──────────────────────
// The server watches Stack resources and pushes a "refresh" event on every
// change; browsers immediately re-fetch their data. The fallback interval
// stays as a safety net (10s normally, 30s while SSE is down); SSE makes
// updates effectively real-time.
const evtSource = new EventSource('/api/events');
let sseLive = false;
evtSource.addEventListener('refresh', () => { if (refreshMs > 0) tick(); });
evtSource.onopen = () => {
  sseLive = true;
  scheduleRefresh();
};
evtSource.onerror = () => {
  sseLive = false;
  scheduleRefresh();
};

// ── Bootstrap ──────────────────────────────────────────────────────────
loadTemplates();
refresh();
scheduleRefresh();

// ── Hash routing (#/ns/name[/tab] · #/agents[/tab]) ────────────────────
function routeFromHash() {
  const sm = location.hash.match(/^#\/agents\/session\/([\w.-]+)$/);
  if (sm) { afwSessionDetail(decodeURIComponent(sm[1])); return; }
  const am = location.hash.match(/^#\/agents(\/(dashboard|sessions|requests|usage))?$/);
  if (am) { showAgents(am[1] ? am[1].slice(1) : 'dashboard'); return; }
  if (document.getElementById('agent-view').style.display === 'block') hideAgents(true);
  const m = location.hash.match(/^#\/([^/]+)\/([^/]+)(?:\/([a-z]+))?$/);
  if (m) {
    showDetail(decodeURIComponent(m[1]), decodeURIComponent(m[2]), m[3]);
  } else if (currentDetail) {
    hideDetail();
  }
}
window.addEventListener('popstate', routeFromHash);
routeFromHash();

// ── Keyboard ───────────────────────────────────────────────────────────
let kbdRow = -1;
function kbdRows() { return [...document.querySelectorAll('#rows tr.stack-row')]; }
function kbdHighlight() {
  kbdRows().forEach((r, i) => r.classList.toggle('kbd-focus', i === kbdRow));
  const el = kbdRows()[kbdRow];
  if (el) el.scrollIntoView({ block: 'nearest' });
}
document.addEventListener('keydown', (e) => {
  if (e.metaKey || e.ctrlKey || e.altKey) return;
  if (document.querySelector('dialog[open]')) return;
  if (e.target.tagName === 'INPUT' || e.target.tagName === 'SELECT' || e.target.tagName === 'TEXTAREA') {
    if (e.key === 'Escape') e.target.blur();
    return;
  }
  if (e.key === '/') { e.preventDefault(); document.getElementById('search').focus(); }
  else if (e.key === 'r') { tick(); }
  else if (e.key === 'Escape') { hideAgents(); hideDetail(); }
  else if (e.key === 'n') { document.getElementById('create').showModal(); }
  else if (e.key === '?') { document.getElementById('keys').showModal(); }
  else if (e.key === 'j' || e.key === 'k') {
    const rows = kbdRows();
    if (!rows.length) return;
    e.preventDefault();
    kbdRow = e.key === 'j' ? Math.min(rows.length - 1, kbdRow + 1) : Math.max(0, kbdRow - 1);
    kbdHighlight();
  } else if (e.key === 'Enter' && kbdRow >= 0) {
    const el = kbdRows()[kbdRow];
    if (el) el.click();
  }
});
