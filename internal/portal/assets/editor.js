async function doReconcile() {
  if (!currentDetail) return;
  const { ns, name } = currentDetail;
  const btn = event && event.target instanceof Element ? event.target.closest('button') : null;
  if (btn) btn.disabled = true;
  try {
    await fetch(`/api/stacks/${ns}/${name}/reconcile`, { method: 'POST' })
      .then(async r => { if (!r.ok) throw new Error((await r.json().catch(() => ({}))).error || r.statusText); });
    toast(`Reconcile triggered for ${ns}/${name}`, true);
    fetchDetail(ns, name);
  } catch (e) { toast(e.message, false); }
  if (btn) btn.disabled = false;
}

const OPERATORS = ['agentFW', 'vault', 'istio', 'spark', 'certManager', 'kafka', 'postgres', 'kubeflow'];

function openEdit() {
  if (!currentData) return;
  const d = currentData;
  const wrap = document.getElementById('edit-operators');
  wrap.innerHTML = OPERATORS.map(k => {
    const on = d.operators && d.operators[k];
    return `<label style="display:flex;gap:6px;align-items:center;cursor:pointer;text-transform:none;font-size:12px;font-weight:500;
      padding:5px 12px;border-radius:9999px;border:1px solid ${on ? 'rgba(20,148,106,.35)' : 'var(--border)'};
      background:${on ? 'rgba(20,148,106,.08)' : 'rgba(15,18,25,.03)'};color:${on ? 'var(--ok)' : 'var(--muted2)'}">
      <input type="checkbox" data-op="${k}" ${on ? 'checked' : ''} style="width:auto"
        onchange="this.parentElement.style.borderColor=this.checked?'rgba(20,148,106,.35)':'var(--border)';
        this.parentElement.style.background=this.checked?'rgba(20,148,106,.08)':'rgba(15,18,25,.03)';
        this.parentElement.style.color=this.checked?'var(--ok)':'var(--muted2)'">
      ${esc(k)}</label>`;
  }).join('');
  document.getElementById('edit-exclude').value = (d.exclude || []).join(', ');
  document.getElementById('edit-bundle').value = d.bundle || '';
  document.getElementById('edit-mode').value = '';
  document.getElementById('edit-error').textContent = '';
  document.getElementById('edit').showModal();
}

async function savePatch() {
  if (!currentDetail) return;
  const { ns, name } = currentDetail;
  const err = document.getElementById('edit-error');
  err.textContent = '';
  const body = {};
  const exclude = document.getElementById('edit-exclude').value
    .split(',').map(s => s.trim()).filter(Boolean);
  if (JSON.stringify(exclude) !== JSON.stringify(currentData.exclude || [])) body.exclude = exclude;
  const bundle = document.getElementById('edit-bundle').value.trim();
  if (bundle !== (currentData.bundle || '')) body.bundle = bundle;
  const mode = document.getElementById('edit-mode').value;
  if (mode) body.mode = mode;
  const operators = {};
  let opsChanged = false;
  document.querySelectorAll('#edit-operators input[data-op]').forEach(cb => {
    const was = !!(currentData.operators && currentData.operators[cb.dataset.op]);
    if (cb.checked !== was) { operators[cb.dataset.op] = cb.checked; opsChanged = true; }
  });
  if (opsChanged) body.operators = operators;
  if (!Object.keys(body).length) { document.getElementById('edit').close(); return; }
  const btn = document.getElementById('edit-btn');
  btn.disabled = true;
  try {
    await fetch(`/api/stacks/${ns}/${name}`, {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    }).then(async r => { if (!r.ok) throw new Error((await r.json().catch(() => ({}))).error || r.statusText); });
    toast('Spec patched - operator will reconcile', true);
    document.getElementById('edit').close();
    fetchDetail(ns, name);
  } catch (e) { err.textContent = e.message; }
  btn.disabled = false;
}

