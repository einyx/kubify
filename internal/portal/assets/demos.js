// Demo requests: website submissions awaiting approval and live demo
// tenants. Loaded with the stack list; actions are approve / reject (pre-
// approval only) and extend TTL (+24h default) for live ones.
async function loadDemos() {
  const card = document.getElementById('demos-card');
  const rows = document.getElementById('demo-rows');
  if (!card || !rows) return;
  let demos = [];
  try { demos = await api('/api/demorequests'); } catch { card.style.display = 'none'; return; }
  card.style.display = demos.length ? '' : 'none';
  const pending = demos.filter(d => !d.approved).length;
  setBadge('badge-demos', pending, true);
  rows.innerHTML = demos.map(d => {
    const actions = !d.approved
      ? `<button class="btn secondary" style="padding:2px 8px;font-size:11px" onclick="approveDemo('${esc(d.name)}')">Approve</button>
         <button class="btn secondary" style="padding:2px 8px;font-size:11px;color:var(--err)" onclick="rejectDemo('${esc(d.name)}')">Reject</button>`
      : (d.phase !== 'Expired'
        ? `<button class="btn secondary" style="padding:2px 8px;font-size:11px" onclick="extendDemo('${esc(d.name)}')" title="Add 24h to the demo lifetime">+24h</button>`
        : '-');
    const expires = d.expiresAt ? new Date(d.expiresAt).toLocaleString() : '-';
    return `<tr>
      <td>${esc(d.email)}</td>
      <td class="muted">${esc(d.company || '-')}</td>
      <td><span class="phase ${d.phase}" title="${esc(d.message || '')}">${esc(d.approved ? d.phase : 'Pending approval')}</span></td>
      <td class="cell-ns">${d.tenant ? esc(d.tenant) : '-'}</td>
      <td class="cell-count">${expires}</td>
      <td class="cell-count" style="text-align:right">${actions}</td>
    </tr>`;
  }).join('');
}

async function approveDemo(name) {
  try {
    await api(`/api/demorequests/${name}/approve`, { method: 'POST' });
    toast('Approved — provisioning starts on the next reconcile');
  } catch (e) { toast(e.message, false); }
  loadDemos();
}

async function rejectDemo(name) {
  try {
    await api(`/api/demorequests/${name}/reject`, { method: 'POST' });
    toast('Rejected');
  } catch (e) { toast(e.message, false); }
  loadDemos();
}

async function extendDemo(name, hours = 24) {
  try {
    await api(`/api/demorequests/${name}/extend`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ hours }),
    });
    toast(`Extended ${name} by ${hours}h`);
  } catch (e) { toast(e.message, false); }
  loadDemos();
}
window.approveDemo = approveDemo;
window.rejectDemo = rejectDemo;
window.extendDemo = extendDemo;
window.loadDemos = loadDemos;
