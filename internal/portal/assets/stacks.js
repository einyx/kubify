const api = async (path, opts) => {
  const r = await fetch(path, opts);
  if (!r.ok) throw new Error((await r.json().catch(() => ({}))).error || r.statusText);
  return r.json();
};

let currentDetail = null; // {ns, name}
let currentData = null;   // last StackDetail for prefilling the edit dialog

let stacksETag = null;
let lastStacks = [];
let phaseFilter = ''; // set via the phase pills in the stats strip
let aiCostByProduct = {};

async function refresh() {
  let stacks;
  try {
    const headers = stacksETag ? { 'If-None-Match': stacksETag } : {};
    const r = await fetch('/api/stacks', { headers });
    if (r.status === 304) {
      stacks = lastStacks; // unchanged - skip re-render
    } else if (!r.ok) {
      throw new Error((await r.json().catch(() => ({}))).error || r.statusText);
    } else {
      stacksETag = r.headers.get('ETag');
      stacks = await r.json();
      lastStacks = stacks;
    }
    // AgentFW records actual priced token usage. Keep it independent from the
    // stack API so an unavailable agent archive never blocks the stack list.
    try {
      const ar = await fetch('/api/agentfw/products');
      if (ar.ok) {
        const ad = await ar.json();
        aiCostByProduct = Object.fromEntries((ad.products || []).map(p => [p.product, p.cost_micro || 0]));
      }
    } catch (_) {}
    const tb = document.getElementById('rows');
    const t = new Date();
    document.getElementById('list-error').textContent = '';
    document.getElementById('api-banner').style.display = 'none';
    renderStats(stacks);
    renderSortHeaders();
    const q = document.getElementById('search').value.trim().toLowerCase();
    const phase = phaseFilter;
    let visible = stacks.filter(s =>
      (!phase || s.phase === phase) &&
      (!q || s.namespace.includes(q) || s.name.includes(q)));
    visible = sortStacks(visible);
    if (!visible.length) {
      tb.innerHTML = `<tr class="empty-row"><td colspan="7">${stacks.length ? 'No stacks match the filter.' : 'No stacks yet - create one from a template.'}</td></tr>`;
      return;
    }
    tb.innerHTML = visible.map(s => {
      const ratio = s.total > 0 ? s.ready / s.total : 0;
      const barClass = ratio === 1 ? '' : ratio > 0.5 ? 'warn' : 'err';
      return `
      <tr class="stack-row" onclick="showDetail('${esc(s.namespace)}','${esc(s.name)}')">
        <td class="cell-ns"><button class="stack-open" onclick="event.stopPropagation();showDetail('${esc(s.namespace)}','${esc(s.name)}')" aria-label="Open ${esc(s.namespace)} / ${esc(s.name)}">${esc(s.namespace)}</button></td>
        <td class="cell-name">${esc(s.name)}</td>
        <td class="cell-mode">${esc(s.mode)}</td>
        <td>
          <span class="phase ${esc(s.phase)}">${esc(s.phase)}</span>
          ${(s.failing || []).map(f => `<span class="fail-chip" title="${esc(s.failureMsg || '')} - click for events" onclick="event.stopPropagation();showDetail('${esc(s.namespace)}','${esc(s.name)}','events')">${esc(f)}</span>`).join('')}
          ${s.failureMsg ? `<div class="failmsg">${esc(s.failureMsg)}</div>` : ''}
        </td>
        <td>
          <div class="ready-bar">
            <div class="bar-track"><div class="bar-fill ${barClass}" style="width:${Math.round(ratio*100)}%"></div></div>
            <span class="bar-label">${s.ready}/${s.total}</span>
          </div>
        </td>
        <td class="cost-cell" title="Ballpark compute allocation from Kubernetes requests at UAE North Standard_D8as_v6 PAYG rates. AI is recorded AgentFW usage, not an Azure invoice.">
          <strong>~$${Number(s.computeMonthlyUsd || 0).toFixed(0)}<span class="cost-period">/mo</span></strong>
          <span>compute · ${formatAI(aiCostByProduct[s.namespace] || aiCostByProduct[s.name] || 0)} AI</span>
        </td>
        <td class="cell-mode">${esc(s.age)}</td>
      </tr>`;
    }).join('');
    if (typeof loadDemos === 'function') loadDemos();
  } catch (e) {
    document.getElementById('list-error').textContent = e.message;
    const banner = document.getElementById('api-banner');
    if (banner) {
      document.getElementById('api-banner-msg').textContent =
        'API unreachable - ' + e.message + '. Retrying automatically.';
      banner.style.display = 'flex';
    }
  }
}

function formatAI(micro) {
  const usd = Number(micro || 0) / 1e6;
  return usd < 0.01 ? '<$0.01' : '$' + usd.toFixed(2);
}

const PHASE_COLORS = {
  Ready: 'var(--ok)',
  Progressing: 'var(--warn)',
  Failed: 'var(--err)',
  Pending: 'var(--pend)',
  Degraded: 'var(--warn)',
  Paused: 'var(--muted)',
  Terminating: 'var(--muted)',
};

function renderStats(stacks) {
  const counts = {};
  for (const s of stacks) counts[s.phase] = (counts[s.phase] || 0) + 1;
  const order = ['Ready', 'Progressing', 'Failed', 'Pending', 'Degraded', 'Paused', 'Terminating'];
  const ready = stacks.reduce((n, s) => n + (s.phase === 'Ready' ? 1 : 0), 0);
  const ratio = stacks.length ? Math.round(ready / stacks.length * 100) : 0;

  // Readiness ring in the page header.
  const ring = document.getElementById('ready-ring');
  if (ring) {
    ring.style.setProperty('--ring', ratio + '%');
    ring.dataset.tone = ratio === 100 ? 'ok' : ratio >= 50 ? 'warn' : 'err';
    document.getElementById('ready-ring-pct').textContent = ratio + '%';
    document.getElementById('ready-ring-sub').textContent =
      `${ready} of ${stacks.length} ready`;
  }

  // Segmented distribution bar, proportional to phase counts.
  const segs = order.filter(p => counts[p]).map(p =>
    `<div class="phase-seg s-${p.toLowerCase()}" style="flex:${counts[p]}" title="${counts[p]} ${p}"></div>`).join('');

  const active = phaseFilter;
  document.getElementById('stats').innerHTML = `
    <div class="stats-strip">
      <div class="stats-total">
        <div class="stats-total-num">${stacks.length}</div>
        <div class="stat-label">stacks total</div>
      </div>
      <div class="stats-body">
        <div class="phase-dist">${segs || '<div class="phase-seg" style="flex:1"></div>'}</div>
        <div class="phase-legend">
          ${order.map(p => {
            const n = counts[p] || 0;
            return `<button class="phase-pill${n ? '' : ' zero'}${p === active ? ' active' : ''}"
              onclick="filterPhase('${p}')" title="Filter by ${p}">
              <span class="phase-dot" style="background:${PHASE_COLORS[p]}"></span>${p}
              <span class="phase-pill-num">${n}</span>
            </button>`; }).join('')}
        </div>
      </div>
    </div>`;
}

function filterPhase(p) {
  phaseFilter = phaseFilter === p ? '' : p; // click again to clear
  refresh();
}

async function showDetail(ns, name, tab) {
  currentDetail = { ns, name };
  if (location.hash !== `#/${ns}/${name}`) history.pushState(null, '', `#/${ns}/${name}`);
  showTab(tab || 'components');
  await fetchDetail(ns, name);
}

// fetchDetail loads (or re-loads) the detail view. The list auto-refreshes
// every 10s; the detail must follow suit or it freezes on stale failures.
async function fetchDetail(ns, name) {
  try {
    const [d, yamlText] = await Promise.all([
      api(`/api/stacks/${ns}/${name}`),
      fetch(`/api/stacks/${ns}/${name}/yaml`).then(r => r.ok ? r.text() : '(unavailable)'),
    ]);
    if (!currentDetail || currentDetail.ns !== ns || currentDetail.name !== name) return;
    currentData = d;
    document.getElementById('list-view').style.display = 'none';
    document.getElementById('mcp-view').style.display = 'none';
    document.getElementById('agent-view').style.display = 'none';
    clearInterval(afw.timer);
    document.getElementById('detail').style.display = 'block';
    document.getElementById('detail-title').textContent = `${ns} / ${name}`;
    document.getElementById('detail-sub').textContent =
      `${d.phase} · ${d.mode} · ${d.ready}/${d.total} components ready · age ${d.age}`;
    window.__compMsgSeq = (window.__compMsgSeq || 0) + 1;
    const seq = window.__compMsgSeq;    document.getElementById('comp-rows').innerHTML = d.components.map((c, i) => {
      const msg = c.message || '';
      const deps = [].concat(c.dependsOn || []).map(x => `<span class="dep-badge" title="waits for deploy">⟵ ${esc(x)}</span>`).join('')
        + [].concat(c.dependsOnReady || []).map(x => `<span class="dep-badge" title="waits for Ready">⏳ ${esc(x)}</span>`).join('');
      const imgs = (c.images || []).map(im =>
        `<span class="img-chip" title="click to copy" onclick="copyText('${esc(im.repository)}${im.tag ? ':' + esc(im.tag) : ''}')">${esc(im.repository.split('/').pop())}${im.tag ? ':' + esc(im.tag) : ''}</span>`
      ).join('') || '<span class="muted">-</span>';
      const mid = `msg-${seq}-${i}`;
      const open = expandedComps.has(c.name);
      return `
      <tr class="comp-row ${open ? 'comp-open' : ''}" onclick="toggleComponent('${esc(c.name)}')" title="click to expand deployment">
        <td class="cell-name"><span class="comp-chevron">${open ? '▾' : '▸'}</span> ${esc(c.name)}${deps}</td>
        <td><span class="comp-phase ${esc(c.phase)}">${esc(c.phase)}</span></td>
        <td class="cell-mode">${esc(String(c.revision || ''))}</td>
        <td>${imgs}</td>
        <td class="cell-ns"><div class="msg-wrap">
          <div class="msg-text" id="${mid}">${esc(msg) || '-'}</div>
        </div></td>
      </tr>
      <tr class="comp-expand" id="comp-expand-${esc(c.name)}" style="${open ? '' : 'display:none'}">
        <td colspan="5"><div class="comp-detail" id="comp-detail-${esc(c.name)}">${open ? 'loading pods…' : ''}</div></td>
      </tr>`;}).join('')
      || '<tr class="empty-row"><td colspan="5">No component status yet.</td></tr>';
    for (const name of expandedComps) renderComponentDetail(name);
    renderSpec(d);
    setBadge('badge-components', d.components.length, false);
    setBadge('badge-conditions', (d.conditions || []).length, false);
    loadEvents(ns);
    syncPaused(d);
    renderValues(d);
    renderProduct(d);
    loadBackups(ns);
    loadVault(ns);
    document.getElementById('cond-rows').innerHTML = (d.conditions || []).map(c => `
      <tr>
        <td class="cell-name">${esc(c.type)}</td>
        <td>${esc(c.status)}</td>
        <td class="cell-mode">${esc(c.reason || '')}</td>
        <td class="cell-mode">${esc(c.lastTransition || '')}</td>
        <td class="cell-ns">${esc(c.message || '')}</td>
      </tr>`).join('')
      || '<tr class="empty-row"><td colspan="5">No conditions.</td></tr>';
    document.getElementById('stack-yaml').textContent = yamlText;
  } catch (e) { toast(e.message, false); }
}

// ── Expandable component deployments ───────────────────────────────────
const expandedComps = new Set();

function toggleComponent(name) {
  if (expandedComps.has(name)) {
    expandedComps.delete(name);
    const row = document.getElementById('comp-expand-' + CSS.escape(name));
    if (row) row.style.display = 'none';
    const main = document.querySelector(`#comp-rows .comp-row[onclick*="'${esc(name)}'"]`);
    if (main) { main.classList.remove('comp-open'); main.querySelector('.comp-chevron').textContent = '▸'; }
    return;
  }
  expandedComps.add(name);
  const row = document.getElementById('comp-expand-' + CSS.escape(name));
  if (row) row.style.display = '';
  const main = document.querySelector(`#comp-rows .comp-row[onclick*="'${esc(name)}'"]`);
  if (main) { main.classList.add('comp-open'); main.querySelector('.comp-chevron').textContent = '▾'; }
  renderComponentDetail(name);
}

async function renderComponentDetail(name) {
  const box = document.getElementById('comp-detail-' + CSS.escape(name));
  if (!box || !currentDetail || !currentData) return;
  const comp = (currentData.components || []).find(c => c.name === name);
  if (!comp) { box.innerHTML = '<span class="muted">component no longer present</span>'; return; }
  const { ns } = currentDetail;

  const deps = [].concat(comp.dependsOn || []).map(x => `<span class="dep-badge" title="waits for deploy">⟵ ${esc(x)}</span>`).join('')
    + [].concat(comp.dependsOnReady || []).map(x => `<span class="dep-badge" title="waits for Ready">⏳ ${esc(x)}</span>`).join('');
  const msg = comp.message || '';
  const msgHtml = msg
    ? `<div class="msg-wrap"><div class="msg-text open">${esc(msg)}</div>
       <button class="copy-btn" onclick="copyText(this.parentElement.querySelector('.msg-text').textContent)">copy</button></div>`
    : '<span class="muted">no status message</span>';
  const imgs = (comp.images || []).map(im =>
    `<span class="img-chip" title="click to copy" onclick="copyText('${esc(im.repository)}${im.tag ? ':' + esc(im.tag) : ''}')">${esc(im.repository)}${im.tag ? ':' + esc(im.tag) : ''}</span>`
  ).join('') || '<span class="muted">no images recorded</span>';

  box.innerHTML = `
    <div class="comp-detail-grid">
      <div><div class="comp-detail-label">Status message</div>${msgHtml}</div>
      <div><div class="comp-detail-label">Images</div><div>${imgs}</div></div>
      <div><div class="comp-detail-label">Dependencies</div><div>${deps || '<span class="muted">none</span>'}</div></div>
      <div><div class="comp-detail-label">Pods <span class="muted">(release: ${esc(name)})</span></div>
        <div id="comp-pods-${CSS.escape(name)}" class="muted">loading…</div></div>
    </div>`;

  try {
    const pods = await api(`/api/stacks/${ns}/${currentDetail.name}/components/${encodeURIComponent(name)}/pods`);
    const box2 = document.getElementById('comp-pods-' + CSS.escape(name));
    if (!box2) return;
    box2.className = '';
    box2.innerHTML = pods.length
      ? `<table class="pods-table"><thead><tr><th>Pod</th><th>Phase</th><th>Ready</th><th>Restarts</th><th>Age</th></tr></thead><tbody>`
        + pods.map(p => `<tr>
            <td class="cell-name">${esc(p.name)}</td>
            <td><span class="comp-phase ${esc(p.phase)}">${esc(p.phase)}</span></td>
            <td class="cell-mode">${esc(p.ready)}</td>
            <td class="cell-mode">${p.restarts}</td>
            <td class="cell-mode">${esc(p.age)}</td>
          </tr>`).join('') + '</tbody></table>'
      : '<span class="muted">no pods matched (label app.kubernetes.io/instance=' + esc(name) + ')</span>';
  } catch (e) {
    const box2 = document.getElementById('comp-pods-' + CSS.escape(name));
    if (box2) { box2.className = ''; box2.innerHTML = `<span style="color:var(--err)">${esc(e.message)}</span>`; }
  }
}

function renderSpec(d) {
  const chips = [];
  if (d.bundle) chips.push(['Bundle', d.bundle]);
  if (d.operators) for (const [k, v] of Object.entries(d.operators)) {
    if (v) chips.push(['Operator', k]);
  }
  if (d.exclude && d.exclude.length) chips.push(['Excluded', d.exclude.join(', ')]);
  const html = chips.map(([k, v]) =>
    `<span class="spec-chip"><span class="spec-key">${esc(k)}</span> ${esc(v)}</span>`).join('');
  document.getElementById('spec-chips').innerHTML =
    html || '<span class="muted" style="font-size:12px">No spec highlights.</span>';
}

function askDelete() {
  if (!currentDetail) return;
  window.__purgeConfirmed = false;
  document.getElementById('del-ns').textContent = currentDetail.ns;
  document.getElementById('del-confirm').value = '';
  document.getElementById('del-error').textContent = '';
  document.getElementById('purge').checked = false;
  document.getElementById('del').showModal();
}

async function doDelete() {
  if (!currentDetail) return;
  const { ns, name } = currentDetail;
  const confirmVal = document.getElementById('del-confirm').value.trim();
  const purge = document.getElementById('purge').checked;
  const err = document.getElementById('del-error');
  err.textContent = '';
  if (confirmVal !== ns) { err.textContent = `Type "${ns}" to confirm.`; return; }
  if (purge && !window.__purgeConfirmed) {
    // Namespace purge is destructive beyond the stack - require an explicit
    // second confirmation each time the dialog reopens.
    if (!confirm(`Purging namespace "${ns}" deletes EVERYTHING in it, not just this stack.\nContinue?`)) return;
    window.__purgeConfirmed = true;
  }
  const btn = document.getElementById('del-btn');
  btn.disabled = true;
  try {
    await fetch(`/api/stacks/${ns}/${name}?confirm=${encodeURIComponent(confirmVal)}&purge=${purge}`, { method: 'DELETE' })
      .then(async r => { if (!r.ok) throw new Error((await r.json().catch(() => ({}))).error || r.statusText); });
    toast(`Stack ${ns}/${name} deleted${purge ? ' (namespace purging)' : ''}`, true);
    document.getElementById('del').close();
    hideDetail();
    refresh();
  if (typeof loadDemos === 'function') loadDemos();
  } catch (e) { err.textContent = e.message; }
  btn.disabled = false;
}

function hideDetail() {
  currentDetail = null;
  currentData = null;
  document.getElementById('detail').style.display = 'none';
  document.getElementById('list-view').style.display = 'block';
  if (location.hash && location.hash !== '#/') history.pushState(null, '', '#/');
  refresh();
}

// ── Detail tabs ────────────────────────────────────────────────────────
let activeTab = 'components';
function showTab(name) {
  activeTab = name;
  document.querySelectorAll('#tabbar button').forEach(b =>
    b.classList.toggle('active', b.dataset.tab === name));
  document.querySelectorAll('#detail section[data-tab]').forEach(s =>
    s.classList.toggle('tab-open', s.dataset.tab === name));
}
document.querySelectorAll('#tabbar button').forEach(b =>
  b.addEventListener('click', () => {
    showTab(b.dataset.tab);
    if (currentDetail) history.replaceState(null, '', `#/${currentDetail.ns}/${currentDetail.name}${b.dataset.tab === 'components' ? '' : '/' + b.dataset.tab}`);
  }));

function setBadge(id, n, warnIfOver) {
  const el = document.getElementById(id);
  if (!el) return;
  el.textContent = n || '';
  el.classList.toggle('warn', !!warnIfOver && n > 0);
}

async function loadEvents(ns) {
  try {
    const events = await api(`/api/stacks/${ns}/events`);
    document.getElementById('event-rows').innerHTML = events.map(e => `
      <tr>
        <td><span class="comp-phase ${e.type === 'Warning' ? 'Failed' : 'Ready'}">${esc(e.type || 'Normal')}</span></td>
        <td class="cell-name">${esc(e.reason)}</td>
        <td class="cell-mode">${esc(e.object)}</td>
        <td class="cell-mode">${e.count}</td>
        <td class="cell-mode">${esc(e.lastSeen)}</td>
        <td class="cell-ns">${esc(e.message)}</td>
      </tr>`).join('')
      || '<tr class="empty-row"><td colspan="6">No recent events.</td></tr>';
    const warnCount = events.filter(e => e.type === 'Warning').length;
    setBadge('badge-events', events.length, false);
    const evBadge = document.getElementById('badge-events');
    if (warnCount > 0) { evBadge.textContent = warnCount + '⚠'; evBadge.classList.add('warn'); }
  } catch (e) { /* events are best-effort */ }
}

function syncPaused(d) {
  document.getElementById('paused-note').style.display = d.paused ? 'block' : 'none';
  const btn = document.getElementById('pause-btn');
  btn.textContent = d.paused ? 'Resume' : 'Pause';
}

async function togglePause() {
  if (!currentDetail || !currentData) return;
  const { ns, name } = currentDetail;
  const paused = !currentData.paused;
  const btn = document.getElementById('pause-btn');
  // Optimistic flip - reconciliation takes seconds; the label shouldn't.
  currentData.paused = paused;
  btn.textContent = paused ? 'Resume' : 'Pause';
  btn.textContent += '…';
  btn.disabled = true;
  try {
    await fetch(`/api/stacks/${ns}/${name}/pause`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ paused }),
    }).then(async r => { if (!r.ok) throw new Error((await r.json().catch(() => ({}))).error || r.statusText); });
    toast(paused ? `Stack ${ns}/${name} paused` : `Stack ${ns}/${name} resumed`, true);
    fetchDetail(ns, name);
  } catch (e) {
    currentData.paused = !paused; // revert on failure
    syncPaused(currentData);
    toast(e.message, false);
  }
  btn.disabled = false;
}

function renderValues(d) {
  const components = d.valueOverrides || [];
  document.getElementById('values-count').textContent = components.length ? `(${components.length})` : '';
  const rows = components.map(comp => `
    <button class="override-item" onclick="openValues('${esc(comp)}')" aria-label="Edit ${esc(comp)} overrides">
      <span class="override-heading"><span class="override-name">${esc(comp)}</span><span class="override-action" aria-hidden="true">Edit ↗</span></span>
      <span class="override-caption">Current values</span>
      <span class="override-preview">${esc(JSON.stringify(d.componentValues?.[comp] ?? {}, null, 2))}</span>
    </button>`).join('');
  document.getElementById('values-rows').innerHTML =
    rows || '<p class="overrides-empty">No overrides yet. Add one to customize a component.</p>';
}

async function openValues(comp = '') {
  document.getElementById('values-title').textContent = comp
    ? `Values override - ${comp}` : 'Component values override';
  document.getElementById('values-component').value = comp;
  document.getElementById('values-component').disabled = !!comp;
  // Start from the saved override so editing one field preserves the others.
  const saved = currentData?.componentValues?.[comp];
  document.getElementById('values-json').value = saved == null ? '' : JSON.stringify(saved, null, 2);
  document.getElementById('values-error').textContent = '';
  document.getElementById('values').showModal();
}

function renderProduct(d) {
  const urlEl = document.getElementById('product-url');
  if (d.url) {
    urlEl.className = 'product-url';
    urlEl.innerHTML = `
      <a href="${esc(d.url)}" target="_blank" rel="noopener">${esc(d.url)}</a>
      <span class="product-url-actions">
        <button class="copy-btn" onclick="copyText('${esc(d.url)}')">copy</button>
        <a class="btn secondary product-open" href="${esc(d.url)}" target="_blank" rel="noopener">Open ↗</a>
      </span>`;
  } else {
    urlEl.className = 'muted';
    urlEl.innerHTML = 'No VirtualService host configured — this stack is not exposed outside the cluster.';
  }
  const flags = d.featureFlags || {};
  const keys = Object.keys(flags).sort();
  setBadge('badge-flags', keys.length, false);
  const box = document.getElementById('flags-rows');
  if (!keys.length) {
    box.innerHTML = '<span class="muted">No feature flags configured.</span>';
  }
  box.innerHTML = keys.map(k => {
    const on = flags[k] === 'true';
    return `<label class="flag-row">
      <code class="flag-name">${esc(k)}</code>
      <span class="flag-switch">
        <input type="checkbox" ${on ? 'checked' : ''} onchange="toggleFlag('${esc(k)}', this.checked)" aria-label="Toggle ${esc(k)}">
        <span class="flag-track"><span class="flag-knob"></span></span>
      </span>
      <span class="flag-state ${on ? 'on' : 'off'}">${on ? 'on' : 'off'}</span>
    </label>`;
  }).join('');

  const tags = d.imageTags || {};
  const tkeys = Object.keys(tags).sort();
  setBadge('badge-tags', tkeys.length, false);
  const tbox = document.getElementById('tags-rows');
  tbox.innerHTML = tkeys.length ? tkeys.map(k => tagRow(k, tags[k])).join('')
    : '<span class="muted">No image tag overrides configured.</span>';
}

function tagRow(name, tag) {
  return `<label class="flag-row">
    <code class="flag-name">${esc(name)}</code>
    <input class="tag-input" value="${esc(tag || '')}" placeholder="tag or digest"
      onchange="saveTag('${esc(name)}', this.value)" aria-label="Image tag for ${esc(name)}">
    <button class="btn secondary" onclick="removeTag('${esc(name)}')">remove</button>
  </label>`;
}

function addTagRow() {
  const name = prompt('Component name (e.g. backend, ai, frontend):');
  if (!name) return;
  const tags = Object.assign({}, currentData && currentData.imageTags || {});
  if (!(name in tags)) {
    tags[name] = '';
    renderProduct(Object.assign({}, currentData, { imageTags: tags }));
    const box = document.getElementById('tags-rows');
    if (box) box.querySelector(`input[aria-label="Image tag for ${CSS.escape(name)}"]`)?.focus();
  }
}

async function saveTag(name, tag) {
  if (!currentDetail) return;
  const { ns, name: sname } = currentDetail;
  const tags = Object.assign({}, currentData.imageTags || {});
  tags[name] = tag;
  await patchTags(ns, sname, tags, `Tag for ${name} saved`);
}

async function removeTag(name) {
  if (!currentDetail) return;
  const { ns, name: sname } = currentDetail;
  const tags = Object.assign({}, currentData.imageTags || {});
  delete tags[name];
  await patchTags(ns, sname, tags, `Tag override for ${name} removed`);
}

async function patchTags(ns, sname, tags, msg) {
  try {
    await fetch(`/api/stacks/${ns}/${sname}`, {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ imageTags: tags }),
    }).then(async r => { if (!r.ok) throw new Error((await r.json().catch(() => ({}))).error || r.statusText); });
    toast(msg, true);
    fetchDetail(ns, sname);
  } catch (e) { toast(e.message, false); }
}

async function toggleFlag(key, on) {
  if (!currentDetail || !currentData) return;
  const { ns, name } = currentDetail;
  const flags = Object.assign({}, currentData.featureFlags || {});
  flags[key] = on ? 'true' : 'false';
  try {
    await fetch(`/api/stacks/${ns}/${name}`, {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ featureFlags: flags }),
    }).then(async r => { if (!r.ok) throw new Error((await r.json().catch(() => ({}))).error || r.statusText); });
    toast(`Flag ${key} ${on ? 'enabled' : 'disabled'}`, true);
    fetchDetail(ns, name);
  } catch (e) { toast(e.message, false); }
}

async function saveValues() {
  if (!currentDetail) return;
  const { ns, name } = currentDetail;
  const err = document.getElementById('values-error');
  err.textContent = '';
  const comp = document.getElementById('values-component').value.trim();
  if (!comp) { err.textContent = 'Component name is required.'; return; }
  const text = document.getElementById('values-json').value.trim();
  let raw = null;
  if (text) {
    try {
      raw = JSON.parse(text);
    } catch (e) { err.textContent = 'Invalid JSON: ' + e.message; return; }
  }
  const body = { componentValues: { [comp]: raw } };
  const btn = document.getElementById('values-btn');
  btn.disabled = true;
  try {
    await fetch(`/api/stacks/${ns}/${name}`, {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    }).then(async r => { if (!r.ok) throw new Error((await r.json().catch(() => ({}))).error || r.statusText); });
    toast(raw === null ? `Override for ${comp} removed` : `Values for ${comp} applied`, true);
    document.getElementById('values').close();
    fetchDetail(ns, name);
  } catch (e) { err.textContent = e.message; }
  btn.disabled = false;
}
