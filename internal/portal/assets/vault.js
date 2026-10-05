// ── Vault ──────────────────────────────────────────────────────────────
let vaultPath = '';

async function loadVault(ns) {
  const healthEl = document.getElementById('vault-health');
  const rows = document.getElementById('vault-rows');
  try {
    const health = await api(`/api/namespaces/${ns}/vault/health`);
    setBadge('badge-secrets', 0, false);
    if (!health.installed) {
      healthEl.textContent = '- not installed';
      rows.innerHTML = '<tr><td colspan="4" class="muted">No Vault in this namespace (operators.vault is off).</td></tr>';
      return;
    }
    if (health.error) {
      setBadge('badge-secrets', 1, true);
      healthEl.textContent = '- unreachable';
      rows.innerHTML = `<tr><td colspan="4" class="muted">${esc(health.error)}<br>
        The portal routes Vault calls through the Kubernetes API server automatically -
        check your kubeconfig context and RBAC (services/proxy on the Vault namespace).</td></tr>`;
      return;
    }
    if (health.sealed) {
      healthEl.textContent = '- SEALED';
      rows.innerHTML = '<tr><td colspan="4" class="muted">Vault is sealed. Unseal it to manage secrets.</td></tr>';
      return;
    }
    healthEl.textContent = '- unsealed';
  } catch (e) {
    healthEl.textContent = '- unreachable';
    rows.innerHTML = `<tr><td colspan="4" class="muted">${esc(String(e.message || e))}</td></tr>`;
    return;
  }

  try {
    const t = await api(`/api/namespaces/${ns}/vault/tree?path=${encodeURIComponent(vaultPath)}`);
    const prefix = vaultPath ? vaultPath + '/' : '';
    let html = '';
    if (vaultPath) {
      const parent = vaultPath.split('/').slice(0, -1).join('/');
      html += `<tr><td colspan="4"><a href="#" onclick="vaultNav('${esc(parent)}');return false" class="muted">← ..</a></td></tr>`;
    }
    for (const fo of (t.folders || [])) {
      html += `<tr><td colspan="4"><a href="#" onclick="vaultNav('${esc(prefix + fo)}');return false">📁 ${esc(fo)}</a></td></tr>`;
    }
    for (const en of (t.entries || [])) {
      const full = prefix + en;
      html += `<tr>
        <td><a href="#" onclick="viewVaultEntry('${esc(full)}');return false">${esc(en)}</a></td>
        <td class="cell-count" colspan="3" style="text-align:right">
          <button class="btn secondary" style="padding:2px 8px;font-size:11px" onclick="openVaultEntry('${esc(full)}')">Edit</button>
          <button class="btn secondary" style="padding:2px 8px;font-size:11px;color:var(--err)" onclick="deleteVaultEntry('${esc(full)}')">Delete…</button>
        </td>
      </tr>`;
    }
    rows.innerHTML = html || '<tr><td colspan="4" class="muted">Empty</td></tr>';
    // Clickable breadcrumb instead of a flat "secret/…" string.
    const segs = (vaultPath ? vaultPath.split('/') : []);
    let crumb = `<a href="#" onclick="vaultNav('');return false">secret</a>`;
    let acc = '';
    for (const seg of segs) {
      acc = acc ? acc + '/' + seg : seg;
      crumb += ` / <a href="#" onclick="vaultNav('${esc(acc)}');return false">${esc(seg)}</a>`;
    }
    document.getElementById('vault-path').innerHTML = crumb;
  } catch (e) {
    rows.innerHTML = `<tr><td colspan="4" class="muted">${esc(String(e.message || e))}</td></tr>`;
  }
}

function vaultNav(path) {
  vaultPath = path.replace(/\/$/, '');
  loadVault(currentDetail.ns);
}

async function viewVaultEntry(path) {
  const e = await api(`/api/namespaces/${currentDetail.ns}/vault/entry?path=${encodeURIComponent(path)}&reveal=true`);
  openVaultDialog(path, e.data || {});
}

let vaultDirty = false;

function openVaultEntry(path = '') {
  openVaultDialog(path, null);
}

function openVaultDialog(path, data) {
  document.getElementById('vault-entry-title').textContent = path ? 'Edit Vault secret' : 'Add Vault secret';
  document.getElementById('vault-path-input').value = path;
  document.getElementById('vault-kv').value = data ? JSON.stringify(data, null, 2) : '{\n  \n}';
  document.getElementById('vault-error').textContent = '';
  vaultDirty = false;
  document.getElementById('vault-entry').showModal();
}

// Dirty tracking: warn before losing edits (dialog cancel, Esc).
document.getElementById('vault-kv').addEventListener('input', () => { vaultDirty = true; });
document.getElementById('vault-path-input').addEventListener('input', () => { vaultDirty = true; });
document.getElementById('vault-entry').addEventListener('cancel', (e) => {
  if (vaultDirty && !confirm('Discard unsaved changes to this secret?')) e.preventDefault();
});
document.getElementById('vault-entry').addEventListener('keydown', (e) => {
  if ((e.metaKey || e.ctrlKey) && e.key === 's') {
    e.preventDefault();
    saveVaultEntry();
  }
});

async function saveVaultEntry() {
  const ns = currentDetail.ns;
  const path = document.getElementById('vault-path-input').value.trim().replace(/^\/+|\/+$/g, '');
  let data;
  try {
    data = JSON.parse(document.getElementById('vault-kv').value || '{}');
  } catch (e) {
    document.getElementById('vault-error').textContent = 'Invalid JSON: ' + e.message;
    return;
  }
  try {
    await api(`/api/namespaces/${ns}/vault/entry`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ path, data }),
    });
    document.getElementById('vault-entry').close();
    toast('Saved ' + path, true);
    vaultDirty = false;
    vaultPath = path.includes('/') ? path.split('/').slice(0, -1).join('/') : '';
    loadVault(ns);
  } catch (e) {
    document.getElementById('vault-error').textContent = e.message;
  }
}

async function deleteVaultEntry(path) {
  const ns = currentDetail.ns;
  const answer = prompt(`Permanent delete of "${path}" destroys ALL versions.\nType the namespace (${ns}) to confirm:`);
  if (answer !== ns) return;
  try {
    await api(`/api/namespaces/${ns}/vault/entry?path=${encodeURIComponent(path)}&permanent=true&confirm=${encodeURIComponent(ns)}`, { method: 'DELETE' });
    toast('Deleted ' + path, true);
  } catch (e) {
    toast(e.message, false);
  }
  loadVault(ns);
}

