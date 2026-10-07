// ── Agent traffic (agentfw) ────────────────────────────────────────────
const afw = { tab: 'dashboard', q: '', action: '', session: '', product: '', page: 0, timer: null, products: [], lastSig: '' };
const afwInt = n => (n ?? 0).toLocaleString('en-US');
const afwCost = µ => {
  const d = (µ ?? 0) / 1e6;
  if (d === 0) return '$0';
  if (d < 0.01) return '$' + d.toFixed(6).replace(/0+$/, '').replace(/\.$/, '');
  return '$' + d.toFixed(2);
};
// Friendlier session labels: ip-sessions drop the ephemeral port, long ids
// truncate. The raw id stays available via the title tooltip / drill-down.
const afwSessionLabel = id => {
  if (!id) return '—';
  let s = String(id);
  if (s.startsWith('ip:')) s = s.replace(/:\d+$/, '');
  return s.length > 28 ? s.slice(0, 27) + '…' : s;
};
// The aggregated API returns RFC3339 strings; the raw archive used unix
// seconds — accept both.
const afwDate = t => new Date(typeof t === 'number' ? t * 1000 : t);
const afwTime = t => afwDate(t).toLocaleTimeString();
const afwDay = t => afwDate(t).toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
const afwURL = u => { try { const x = new URL(u); return x.host + x.pathname; } catch { return u; } };
const afwDebounce = (fn, ms) => { let t; return (...a) => { clearTimeout(t); t = setTimeout(() => fn(...a), ms); }; };

async function afwAPI(path) {
  const sep = path.includes('?') ? '&' : '?';
  const r = await fetch('/agentfw/api/v1' + path + (afw.product ? sep + 'product=' + encodeURIComponent(afw.product) : ''));
  if (!r.ok) throw new Error((await r.json().catch(() => ({}))).error || r.statusText);
  return r.json();
}

function showAgents(tab) {
  currentDetail = null;
  document.getElementById('detail').style.display = 'none';
  document.getElementById('mcp-view').style.display = 'none';
  document.getElementById('list-view').style.display = 'none';
  document.getElementById('agent-view').style.display = 'block';
  const nb = document.getElementById('afw-nav-btn');
  const lbl = nb ? (nb.dataset.label || '') : '';
  document.getElementById('agent-label').textContent = lbl ? '· ' + lbl : '';
  afwLoadProducts();
  if (tab) afw.tab = tab;
  document.querySelectorAll('#agent-tabbar button').forEach(b =>
    b.classList.toggle('active', b.dataset.atab === afw.tab));
  const want = '#/agents' + (afw.tab !== 'dashboard' ? '/' + afw.tab : '');
  if (location.hash !== want) history.pushState(null, '', want);
  afwRender();
  clearInterval(afw.timer);
  // Mirror the standalone viewer: keep dashboard/usage fresh while visible.
  afw.timer = setInterval(() => {
    if (afw.tab !== 'dashboard' && afw.tab !== 'usage') return;
    if (document.hidden) return; // no flicker while the tab is in background
    afwAPI(afw.tab === 'dashboard' ? '/stats' : '/usage').then(fresh => {
      const sig = JSON.stringify(fresh);
      if (sig === afw.lastSig) return; // unchanged — skip re-render
      afw.lastSig = sig;
      afwRender();
    }).catch(() => {});
  }, 5000);
}

function hideAgents(fromRoute) {
  clearInterval(afw.timer);
  document.getElementById('agent-view').style.display = 'none';
  if (fromRoute) return;
  document.getElementById('list-view').style.display = 'block';
  if (location.hash.startsWith('#/agents')) history.pushState(null, '', '#/');
  refresh();
}

document.querySelectorAll('#agent-tabbar button').forEach(b =>
  b.addEventListener('click', () => {
    afw.tab = b.dataset.atab; afw.page = 0;
    document.querySelectorAll('#agent-tabbar button').forEach(x => x.classList.toggle('active', x === b));
    history.replaceState(null, '', '#/agents' + (afw.tab !== 'dashboard' ? '/' + afw.tab : ''));
    afwRender();
  }));

async function afwLoadProducts() {
  try {
    const { products } = await fetch('/agentfw/api/v1/products' + (afw.product ? '?product=' + encodeURIComponent(afw.product) : '')).then(r => r.json());
    afw.products = (products || []).map(p => p.product).filter(Boolean);
  } catch { afw.products = []; }
}

async function afwRender() {
  const live = document.getElementById('agent-live');
  try {
    await ({ dashboard: afwDashboard, sessions: afwSessions, requests: afwRequests, usage: afwUsage })[afw.tab]();
    live.textContent = '● live'; live.style.color = 'var(--ok)';
  } catch (e) {
    live.textContent = '○ offline'; live.style.color = 'var(--err)';
    document.getElementById('agent-body').innerHTML =
      `<div class="card-outer"><div class="card-inner"><div class="afw-empty">agentfw unreachable — ${esc(String(e.message || e))}<br>
       <span class="muted" style="font-size:11px">check the agentfw admin port / SetAgentfwURL configuration</span></div></div></div>`;
  }
}

function afwCard(label, value, sub, color) {
  return `<div class="card-outer"><div class="afw-card">
    <div class="stat-num"${color ? ` style="color:var(${color})"` : ''}>${value}</div>
    <div class="stat-label">${label}</div>
    <div class="sub">${sub}</div>
  </div></div>`;
}

async function afwDashboard() {
  const s = await afwAPI('/stats');
  const guarded = (s.blocked || 0) + (s.redacted || 0);
  const guardRate = s.total_requests ? (guarded / s.total_requests * 100).toFixed(1) + '%' : '0%';
  const kinds = Object.entries(s.findings_by_kind || {}).map(([k, n]) =>
    `<span class="afw-badge kind">${esc(k)} ${n}</span>`).join(' ') || '<span class="muted">none</span>';
  const max = Math.max(1, ...(s.top_models || []).map(m => m.cost_micro));
  const models = (s.top_models || []).map(m => `
    <div class="afw-row"><div class="name" title="${esc(m.model)}">${esc(m.model)}</div>
      <div class="bar-track"><div class="bar-fill" style="width:${Math.max(1, m.cost_micro / max * 100)}%"></div></div>
      <div class="num">${afwInt(m.input_tokens)} in / ${afwInt(m.output_tokens)} out · ${afwCost(m.cost_micro)}</div></div>`).join('')
    || '<div class="afw-empty">No priced model calls yet</div>';
  document.getElementById('agent-body').innerHTML = `
    <div class="afw-cards">
      ${afwCard('Requests', afwInt(s.total_requests), afwInt(s.total_sessions) + ' sessions')}
      ${afwCard('Blocked', afwInt(s.blocked), afwInt(s.redacted) + ' redacted', '--err')}
      ${afwCard('Input tokens', afwInt(s.input_tokens), afwInt(s.output_tokens) + ' out')}
      ${afwCard('Spend', afwCost(s.cost_micro), 'estimated', '--ok')}
      ${afwCard('Guardrail rate', guardRate, afwInt(guarded) + ' intervened')}
      ${afwCard('Evidence', 'signed', 'Ed25519 · hash-linked', '--ok')}
    </div>
    <div class="card-outer" style="animation-delay:60ms"><div class="card-inner">
      <div class="section-title">By product</div>
      <div style="padding:12px 16px">${(s.products || []).map(pp => `
        <div class="afw-row"><div class="name" title="${esc(pp.product)}">${esc(pp.product)}${pp.offline ? ' (offline)' : ''}</div>
          <div class="num">${afwInt(pp.total_requests)} req · ${afwCost(pp.cost_micro)}</div></div>`).join('')
        || '<div class="afw-empty">Single product</div>'}</div>
    </div></div>
    <div class="card-outer" style="animation-delay:80ms"><div class="card-inner">
      <div class="section-title">Findings by kind</div>
      <div style="padding:12px 16px">${kinds}</div>
    </div></div>
    <div class="card-outer" style="animation-delay:100ms"><div class="card-inner">
      <div class="section-title">Top models by spend</div>${models}</div></div>
    <div class="card-outer" style="animation-delay:140ms"><div class="card-inner">
      <div class="section-title">Recent blocks</div>
      ${afwTable(s.recent_blocked || [], true) || '<div class="afw-empty">No blocks recorded</div>'}
    </div></div>`;
}

async function afwSessions() {
  const { sessions } = await afwAPI('/sessions?limit=200');
  document.getElementById('agent-body').innerHTML = `
    <div class="card-outer"><div class="card-inner">
      <div class="section-title">Sessions</div>
      ${sessions.length ? `<table><thead><tr><th>Session</th><th>Product</th><th>Requests</th><th>Blocked</th><th>Findings</th><th>Tokens</th><th>Cost</th><th>Models</th><th>Last active</th></tr></thead>
      <tbody>${sessions.map(s => `
        <tr class="stack-row" onclick="afwDrill('${esc(s.session_id)}')">
          <td class="cell-mode" title="${esc(s.session_id)}">${esc(afwSessionLabel(s.session_id))}</td>
          <td class="cell-ns">${esc(s.product || '—')}</td>
          <td class="cell-count">${afwInt(s.requests)}</td>
          <td>${s.blocked ? `<span class="afw-badge block">${s.blocked}</span>` : '<span class="muted">0</span>'}</td>
          <td>${s.findings ? `<span class="afw-badge redact">${s.findings}</span>` : '<span class="muted">0</span>'}</td>
          <td class="cell-mode">${afwInt(s.input_tokens)} / ${afwInt(s.output_tokens)}</td>
          <td class="cell-mode">${afwCost(s.cost_micro)}</td>
          <td class="cell-ns" title="${esc((s.models || []).join(', '))}">${esc((s.models || []).slice(0, 2).join(', '))}</td>
          <td class="cell-mode">${afwDay(s.last_seen)}</td>
        </tr>`).join('')}</tbody></table>` : '<div class="afw-empty">No sessions recorded yet</div>'}
    </div></div>`;
}

function afwDrill(sessionId) {
  afw.session = sessionId || '';
  afw.page = 0;
  afw.tab = 'requests';
  document.querySelectorAll('#agent-tabbar button').forEach(x =>
    x.classList.toggle('active', x.dataset.atab === 'requests'));
  history.replaceState(null, '', '#/agents/requests');
  afwRender();
}

async function afwRequests() {
  const params = new URLSearchParams({ q: afw.q, action: afw.action, session: afw.session, limit: 100, offset: afw.page * 100 });
  const { requests, total } = await afwAPI('/requests?' + params);
  document.getElementById('agent-body').innerHTML = `
    <div class="card-outer"><div class="card-inner">
      <div class="toolbar" style="margin-bottom:0;padding:12px 16px 0">
        <input id="afw-q" class="tool-input" placeholder="Search bodies and URLs…" value="${esc(afw.q)}" autocomplete="off">
        <select id="afw-action" class="tool-select">
          <option value="">all actions</option>
          ${['allow', 'redact', 'block'].map(a => `<option value="${a}"${afw.action === a ? ' selected' : ''}>${a}</option>`).join('')}
        </select>
        <select id="afw-product" class="tool-select">
          <option value="">all products</option>
          ${afw.products.map(p => `<option value="${esc(p)}"${afw.product === p ? ' selected' : ''}>${esc(p)}</option>`).join('')}
        </select>
        ${afw.session ? `<span class="afw-badge kind">session: ${esc(afw.session)} <span style="cursor:pointer" onclick="afwDrill('')">✕</span></span>` : ''}
      </div>
      ${requests.length ? afwTable(requests, false) : '<div class="afw-empty">No matching requests</div>'}
      <div class="afw-pager">
        <span class="muted" style="margin-right:auto;font-size:12px">${afwInt(total)} total</span>
        <button class="btn secondary" ${afw.page === 0 ? 'disabled' : ''} onclick="afwPage(-1)">‹ Prev</button>
        <button class="btn secondary" ${(afw.page + 1) * 100 >= total ? 'disabled' : ''} onclick="afwPage(1)">Next ›</button>
      </div>
    </div></div>`;
  document.getElementById('afw-q').oninput = afwDebounce(e => { afw.q = e.target.value; afw.page = 0; afwRefreshList(); }, 300);
  document.getElementById('afw-action').onchange = e => { afw.action = e.target.value; afw.page = 0; afwRefreshList(); };
  document.getElementById('afw-product').onchange = e => { afw.product = e.target.value; afw.page = 0; afwRefreshList(); };
}

function afwPage(d) { afw.page = Math.max(0, afw.page + d); afwRender(); }

async function afwRefreshList() {
  const params = new URLSearchParams({ q: afw.q, action: afw.action, session: afw.session, limit: 100, offset: afw.page * 100 });
  const { requests, total } = await afwAPI('/requests?' + params);
  const panel = document.querySelector('#agent-body .card-inner');
  if (!panel) return afwRender();
  panel.querySelector('table')?.remove();
  panel.querySelector('.afw-empty')?.remove();
  const pager = panel.querySelector('.afw-pager');
  if (requests.length) pager.insertAdjacentHTML('beforebegin', afwTable(requests, false));
  else pager.insertAdjacentHTML('beforebegin', '<div class="afw-empty">No matching requests</div>');
  pager.querySelector('.muted').textContent = `${afwInt(total)} total`;
  const [prev, next] = panel.querySelectorAll('.afw-pager .btn');
  if (prev) prev.disabled = afw.page === 0;
  if (next) next.disabled = (afw.page + 1) * 100 >= total;
}

function afwTable(recs, compact) {
  return `<table><thead><tr><th>Time</th><th>Product</th><th>Session</th><th>Target</th><th>Model</th><th>Action</th><th>Findings</th><th>Tokens</th><th>Cost</th></tr></thead>
  <tbody>${recs.map(r => `
    <tr class="stack-row" onclick="openAfwDetail(${r.id}, '${esc(r.product || '')}')">
      <td class="cell-mode">${afwTime(r.time)}</td>
      <td class="cell-ns">${esc(r.product || '—')}</td>
      <td class="cell-mode" title="${esc(r.session_id)}">${esc(r.session_id.length > 18 ? r.session_id.slice(0, 18) + '…' : r.session_id)}</td>
      <td class="cell-mode">${esc(afwURL(r.url))}${compact ? '' : ` <span class="muted">${r.status || ''}</span>`}</td>
      <td class="cell-ns">${esc(r.model || '—')}</td>
      <td><span class="afw-badge ${r.action}">${r.action}</span></td>
      <td class="cell-count">${r.findings ? r.findings.length : ''}</td>
      <td class="cell-mode">${r.usage ? afwInt(r.usage.input_tokens) + '/' + afwInt(r.usage.output_tokens) : '—'}</td>
      <td class="cell-mode">${r.usage ? afwCost(r.usage.cost_micro) : '—'}</td>
    </tr>`).join('')}</tbody></table>`;
}

async function openAfwDetail(id, product) {
  // Pin to the product's agentfw instance — the merged list spans all
  // instances, so an unpinned fetch 404s ~2 times in 3.
  const q = product ? '?product=' + encodeURIComponent(product) : '';
  const r = await afwAPI('/requests/' + id + q);
  afw.lastDetail = { id, product };
  const back = afw.session
    ? `onclick="afwDrill('${esc(afw.session)}')"` + ' label="session"'
    : `onclick="afwRender()"`;
  document.getElementById('agent-body').innerHTML = `
    <div class="card-outer"><div class="card-inner">
      <div class="section-title" style="display:flex;gap:10px;align-items:center">
        <button class="back-btn" ${back}>‹ ${afw.session ? 'session' : 'back'}</button>
        <span>request #${r.id} <span class="afw-badge ${r.action}">${r.action}</span></span>
      </div>
      <table><tbody>
        <tr><td class="cell-ns" style="width:120px">Product</td><td class="cell-ns">${esc(r.product || '—')}</td></tr>
        <tr><td class="cell-ns">Session</td><td class="cell-mode"><span class="img-chip" title="open session" onclick="afwDrill('${esc(r.session_id)}')">${esc(r.session_id)}</span></td></tr>
        <tr><td class="cell-ns">Time</td><td class="cell-mode">${afwDate(r.time).toLocaleString()} · ${r.duration_ms}ms</td></tr>
        <tr><td class="cell-ns">Target</td><td class="cell-mode">${esc(r.method)} ${esc(r.url)} → ${r.status || '—'}</td></tr>
        <tr><td class="cell-ns">Model</td><td class="cell-mode">${esc(r.model || '—')}</td></tr>
        ${r.usage ? `<tr><td class="cell-ns">Usage</td><td class="cell-mode">${afwInt(r.usage.input_tokens)} in / ${afwInt(r.usage.output_tokens)} out · <b>${afwCost(r.usage.cost_micro)}</b></td></tr>` : ''}
        ${r.findings?.length ? `<tr><td class="cell-ns">Findings</td><td>${r.findings.map(f => `<span class="afw-badge kind">${esc(f.kind)}:${esc(f.pattern)}</span> <span class="cell-mode">${esc(f.excerpt)}</span>`).join('<br>')}</td></tr>` : ''}
      </tbody></table>
      <div class="afw-pager" style="justify-content:flex-start">
        <button class="btn secondary afw-toggle on" id="afw-b-req">Request body (${afwInt(r.req_bytes)} B)</button>
        <button class="btn secondary afw-toggle on" id="afw-b-resp">Response body (${afwInt(r.resp_bytes)} B)</button>
      </div>
      <div style="padding:0 16px 16px">
        <pre id="afw-p-req">${esc(afwPretty(r.req_body))}</pre>
        <pre id="afw-p-resp">${esc(afwPretty(r.resp_body))}</pre>
      </div>
    </div></div>`;
  const tog = (btn, pre) => document.getElementById(btn).onclick = () => {
    const b = document.getElementById(btn), p = document.getElementById(pre);
    b.classList.toggle('on');
    p.style.display = b.classList.contains('on') ? '' : 'none';
  };
  tog('afw-b-req', 'afw-p-req');
  tog('afw-b-resp', 'afw-p-resp');
}

function afwPretty(s) {
  if (!s) return '(empty)';
  try { return JSON.stringify(JSON.parse(s), null, 2); } catch { return s; }
}

async function afwUsage() {
  const [u, s] = await Promise.all([afwAPI('/usage'), afwAPI('/stats')]);
  const max = Math.max(1, ...(u.models || []).map(m => m.cost_micro));
  const totalTokens = (u.input_tokens || 0) + (u.output_tokens || 0);
  const requests = (u.models || []).reduce((n, m) => n + (m.requests || 0), 0);
  const perRequest = requests ? (u.cost_micro || 0) / requests : 0;
  const perMillion = totalTokens ? (u.cost_micro || 0) / totalTokens : 0;
  const products = (s.products || []).slice().sort((a,b) => (b.cost_micro||0)-(a.cost_micro||0));
  document.getElementById('agent-body').innerHTML = `
    <div class="afw-cards">
      ${afwCard('Total spend', afwCost(u.cost_micro), afwInt(requests) + ' priced requests', '--ok')}
      ${afwCard('Tokens', afwInt(totalTokens), afwInt(u.input_tokens) + ' in · ' + afwInt(u.output_tokens) + ' out')}
      ${afwCard('Avg / request', afwCost(perRequest), 'blended model cost')}
      ${afwCard('Blended / 1M', '$' + perMillion.toFixed(2), 'input + output tokens')}
    </div>
    <div class="card-outer" style="animation-delay:60ms"><div class="card-inner">
      <div class="section-title">Model billing ledger</div>
      ${(u.models || []).length ? `<table><thead><tr><th>Model</th><th>Requests</th><th>Input</th><th>Output</th><th>Share</th><th>Spend</th></tr></thead><tbody>${u.models.map(m => `<tr><td class="cell-ns">${esc(m.model)}</td><td>${afwInt(m.requests)}</td><td>${afwInt(m.input_tokens)}</td><td>${afwInt(m.output_tokens)}</td><td><div class="bar-track"><div class="bar-fill" style="width:${Math.max(1,m.cost_micro/max*100)}%"></div></div></td><td class="cell-mode">${afwCost(m.cost_micro)}</td></tr>`).join('')}</tbody></table>` : '<div class="afw-empty">No usage recorded yet</div>'}
    </div></div>
    <div class="card-outer" style="animation-delay:90ms"><div class="card-inner">
      <div class="section-title">Spend by product</div>
      ${products.length ? `<table><thead><tr><th>Product</th><th>Requests</th><th>Tokens</th><th>Spend</th></tr></thead><tbody>${products.map(p => `<tr><td class="cell-ns">${esc(p.product)}</td><td>${afwInt(p.total_requests)}</td><td>${afwInt((p.input_tokens||0)+(p.output_tokens||0))}</td><td class="cell-mode">${afwCost(p.cost_micro)}</td></tr>`).join('')}</tbody></table>` : '<div class="afw-empty">No product usage yet</div>'}
    </div></div>`;
}
