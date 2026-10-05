async function loadTemplates() {
  try {
    const templates = await api('/api/templates');
    const sel = document.getElementById('template');
    sel.innerHTML = templates.map(t =>
      `<option value="${esc(t.id)}">${esc(t.name || t.id)}</option>`).join('');
    const syncDesc = () => {
      const t = templates.find(t => t.id === sel.value);
      document.getElementById('template-desc').textContent =
        t ? (t.description || '') + (t.source ? ` - source: ${t.source}` : '') : '';
    };
    sel.onchange = syncDesc;
    try { // remember the last template the operator used
      const last = localStorage.getItem('kubo.lastTemplate');
      if (last && [...sel.options].some(o => o.value === last)) sel.value = last;
    } catch (e) { /* ignore */ }
    syncDesc();
  } catch (e) { /* non-fatal */ }
}

async function preview() {
  const err = document.getElementById('create-error');
  err.textContent = '';
  try {
    const params = new URLSearchParams({
      template: document.getElementById('template').value,
      tenant: tenantVal(),
    });
    const mode = document.getElementById('mode').value;
    if (mode) params.set('mode', mode);
    const yaml = await fetch('/api/template?' + params).then(async r => {
      if (!r.ok) throw new Error((await r.json().catch(() => ({}))).error || r.statusText);
      return r.text();
    });
    const pre = document.getElementById('preview');
    pre.style.display = 'block';
    pre.textContent = yaml;
  } catch (e) { err.textContent = e.message; }
}

async function create() {
  const err = document.getElementById('create-error');
  const tenant = tenantVal();
  if (!validTenantSlug(tenant)) {
    err.textContent = "Tenant must be a lowercase RFC-1123 label (a-z, 0-9, '-').";
    return;
  }
  err.textContent = '';
  const btn = document.getElementById('create-btn');
  btn.disabled = true;
  try {
    const body = {
      template: document.getElementById('template').value,
      tenant,
    };
    const mode = document.getElementById('mode').value;
    if (mode) body.mode = mode;
    await api('/api/stacks', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    try { localStorage.setItem('kubo.lastTemplate', body.template); } catch (e) {}
    toast('Stack created - opening it…', true);
    document.getElementById('create').close();
    document.getElementById('preview').style.display = 'none';
    // Deep-link into the new stack once the list sees it.
    await refresh();
    const created = lastStacks.find(s => s.namespace.endsWith('-' + tenant) || s.namespace === tenant);
    if (created) {
      showDetail(created.namespace, created.name);
    } else {
      toast('Created - but it is not visible yet; the list will catch up', true);
    }
  } catch (e) { err.textContent = e.message; }
  btn.disabled = false;
}


