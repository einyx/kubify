let allBackups = [];

async function loadBackups(ns) {
  try {
    allBackups = await api('/api/backups');
    renderBackups(ns);
  } catch (e) { /* best-effort */ }
}

function renderBackups(ns) {
  const mine = allBackups.filter(b => b.source === ns);
  setBadge('badge-backups', mine.length, false);
  document.getElementById('backup-rows').innerHTML = mine.map(b => {
    const done = b.phase === 'Succeeded' || b.phase === 'Failed';
    const acts = [];
    if (done) acts.push(`<button class="btn secondary" style="padding:3px 10px;font-size:11px" onclick="retryBackup('${esc(b.namespace)}','${esc(b.name)}')">Retry</button>`);
    acts.push(`<button class="btn secondary" style="padding:3px 10px;font-size:11px;color:var(--err)" onclick="deleteBackup('${esc(b.namespace)}','${esc(b.name)}')">Delete</button>`);
    return `
    <tr>
      <td class="cell-name" style="font-family:'SF Mono','Fira Code',monospace;font-size:11px">${esc(b.name)}</td>
      <td class="cell-ns">${esc(b.target)}</td>
      <td class="cell-mode">${esc((b.include || ['database', 's3']).join(', '))}</td>
      <td><span class="phase ${b.phase === 'Succeeded' ? 'Ready' : b.phase === 'Failed' ? 'Failed' : 'Progressing'}">${esc(b.phase)}</span>
        ${b.message ? `<div class="failmsg">${esc(b.message)}</div>` : ''}</td>
      <td class="cell-mode">${esc(b.age)}</td>
      <td style="text-align:right;white-space:nowrap">${acts.join(' ')}</td>
    </tr>`;
  }).join('')
    || '<tr class="empty-row"><td colspan="6">No backups yet.</td></tr>';
}

async function retryBackup(ns, name) {
  try {
    const bk = await api(`/api/backups/${ns}/${name}/retry`, { method: 'POST' });
    toast(`Retry created: ${bk.name}`, true);
    loadBackups(ns);
  } catch (e) { toast(e.message, false); }
}

async function deleteBackup(ns, name) {
  if (!confirm(`Delete backup record ${name}? Its copy job is garbage-collected.`)) return;
  try {
    await fetch(`/api/backups/${ns}/${name}`, { method: 'DELETE' })
      .then(async r => { if (!r.ok) throw new Error((await r.json().catch(() => ({}))).error || r.statusText); });
    toast(`Backup ${name} deleted`, true);
    loadBackups(ns);
  } catch (e) { toast(e.message, false); }
}

async function openBackup() {
  if (!currentDetail) return;
  document.getElementById('backup-source').value = currentDetail.ns;
  try {
    const stacks = await api('/api/stacks');
    const targets = [...new Set(stacks.map(s => s.namespace))].filter(n => n !== currentDetail.ns);
    document.getElementById('backup-target').innerHTML = targets.map(n =>
      `<option value="${esc(n)}">${esc(n)}</option>`).join('');
  } catch (e) { /* leave whatever is there */ }
  document.getElementById('backup-error').textContent = '';
  try { // prefill the last target used from this source namespace
    const last = (JSON.parse(localStorage.getItem('kubo.backupTargets') || '{}'))[currentDetail.ns];
    const sel = document.getElementById('backup-target');
    if (last && [...sel.options].some(o => o.value === last)) sel.value = last;
  } catch (e) { /* ignore */ }
  document.getElementById('backup').showModal();
}

async function createBackup() {
  if (!currentDetail) return;
  const err = document.getElementById('backup-error');
  err.textContent = '';
  const include = [];
  if (document.getElementById('backup-db').checked) include.push('database');
  if (document.getElementById('backup-s3').checked) include.push('s3');
  if (!include.length) { err.textContent = 'Pick at least one of database / s3.'; return; }
  const btn = document.getElementById('backup-btn');
  btn.disabled = true;
  try {
    const bk = await api('/api/backups', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        sourceNamespace: currentDetail.ns,
        targetNamespace: document.getElementById('backup-target').value,
        include,
      }),
    });
    toast(`Backup ${bk.name} created - the controller will run the copy job`, true);
    try {
      const targets = JSON.parse(localStorage.getItem('kubo.backupTargets') || '{}');
      targets[currentDetail.ns] = bk.target;
      localStorage.setItem('kubo.backupTargets', JSON.stringify(targets));
    } catch (e) { /* ignore */ }
    document.getElementById('backup').close();
    await loadBackups(currentDetail.ns);
  } catch (e) { err.textContent = e.message; }
  btn.disabled = false;
}

