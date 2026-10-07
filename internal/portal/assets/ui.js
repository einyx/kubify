const tenantVal = () => document.getElementById('tenant').value.trim();
const validTenantSlug = s => /^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$/.test(s);

// Live tenant validation in the create dialog.
document.getElementById('tenant').addEventListener('input', function () {
  const v = this.value.trim();
  let hint = this.parentElement.querySelector('.tenant-hint');
  if (!hint) {
    hint = document.createElement('div');
    hint.className = 'tenant-hint';
    hint.style.cssText = 'font-size:11px;margin-top:5px;min-height:14px';
    this.parentElement.appendChild(hint);
  }
  if (!v) { hint.textContent = ''; return; }
  hint.innerHTML = validTenantSlug(v)
    ? `<span class="tenant-ok">✓ valid</span>`
    : `<span style="color:var(--err)">✕ lowercase a-z, 0-9 and '-' only</span>`;
});
const esc = s => String(s)
  .replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;')
  .replace(/'/g,'&#39;').replace(/"/g,'&quot;');

function toast(msg, ok = true) {
  const wrap = document.getElementById('toasts');
  if (!wrap) return;
  const el = document.createElement('div');
  el.className = 'toast ' + (ok ? 'ok' : 'err');
  el.innerHTML = `<span class="t-icon">${ok ? '✓' : '✕'}</span><span>${esc(msg)}</span>` +
    (ok ? '' : ' <button class="t-close" title="dismiss">✕</button>');
  const dismiss = () => { el.classList.add('out'); setTimeout(() => el.remove(), 320); };
  const btn = el.querySelector('.t-close');
  if (btn) btn.onclick = dismiss;
  wrap.appendChild(el);
  while (wrap.children.length > 4) wrap.firstChild.remove();
  if (ok) setTimeout(dismiss, 3800); // errors persist until dismissed
}

document.getElementById('search').addEventListener('input', refresh);

function copyText(t) {
  navigator.clipboard.writeText(t).then(() => toast('copied'));
}
window.copyText = copyText;

// ── Column sorting ─────────────────────────────────────────────────────
let sortState = { key: null, dir: 1 };
const PHASE_ORDER = { Ready: 0, Progressing: 1, Pending: 2, Failed: 3 };
function sortStacks(rows) {
  if (!sortState.key) return rows;
  const { key, dir } = sortState;
  return [...rows].sort((a, b) => {
    let va = a[key], vb = b[key];
    if (key === 'phase') { va = PHASE_ORDER[va] ?? 9; vb = PHASE_ORDER[vb] ?? 9; }
    if (key === 'age') { // "3d" style - compare by unit magnitude
      const parse = v => parseInt(v) * ({ s: 1, m: 60, h: 3600, d: 86400 }[String(v).slice(-1)] || 1);
      return (parse(va) - parse(vb)) * dir;
    }
    return String(va).localeCompare(String(vb)) * dir;
  });
}
function renderSortHeaders() {
  document.querySelectorAll('th.sortable').forEach(th => {
    th.classList.remove('sorted-asc', 'sorted-desc');
    if (th.dataset.sort === sortState.key) {
      th.classList.add(sortState.dir === 1 ? 'sorted-asc' : 'sorted-desc');
    }
  });
}
document.querySelectorAll('th.sortable').forEach(th =>
  th.addEventListener('click', () => {
    const k = th.dataset.sort;
    if (sortState.key === k) sortState.dir *= -1;
    else sortState = { key: k, dir: 1 };
    renderSortHeaders();
    refresh();
  }));
