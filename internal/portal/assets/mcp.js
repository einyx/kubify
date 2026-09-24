// ── MCP ──────────────────────────────────────────────────────────────
let mcpTools = [];

function showMCP() {
  hideDetail();
  clearInterval(afw.timer);
  document.getElementById('agent-view').style.display = 'none';
  document.getElementById('list-view').style.display = 'none';
  const v = document.getElementById('mcp-view');
  v.style.display = 'block';
  loadMCP();
}

function hideMCP() {
  clearInterval(afw.timer);
  document.getElementById('mcp-view').style.display = 'none';
  document.getElementById('list-view').style.display = 'block';
}

async function loadMCP() {
  try {
    const [info, tools] = await Promise.all([
      api('/api/mcp'),
      fetch('/api/mcp/tools').then(r => r.ok ? r.json() : []),
    ]);
    mcpTools = Array.isArray(tools) ? tools : [];
    document.getElementById('mcp-sub').textContent =
      `${mcpTools.length} tools · auth: ${info.authMode}`;
    const grid = document.getElementById('mcp-tools');
    grid.innerHTML = mcpTools.map(t =>
      `<button type="button" class="mcp-tool" data-tool="${esc(t.name)}" onclick="pickTool('${esc(t.name)}')">
         <span class="mcp-tool-name">${esc(t.name)}</span>
         <span class="mcp-tool-description">${esc(t.description || '')}</span>
       </button>`).join('') || '<span class="muted">No tools available. Check the MCP server connection.</span>';
    const sel = document.getElementById('mcp-tool');
    sel.innerHTML = mcpTools.map(t => `<option value="${esc(t.name)}">${esc(t.name)}</option>`).join('');
    renderToolForm();
  } catch (e) {
    document.getElementById('mcp-sub').textContent = 'unavailable: ' + e.message;
  }
}

function toolSchema(name) {
  const t = mcpTools.find(t => t.name === name);
  return (t && t.inputSchema) || { type: 'object', properties: {} };
}

function pickTool(name) {
  document.getElementById('mcp-tool').value = name;
  renderToolForm();
  document.querySelector('.mcp-playground').scrollIntoView({ behavior: matchMedia('(prefers-reduced-motion: reduce)').matches ? 'auto' : 'smooth', block: 'start' });
  document.getElementById('mcp-tool').focus({ preventScroll: true });
}

function renderToolForm() {
  const name = document.getElementById('mcp-tool').value;
  const sch = toolSchema(name);
  const form = document.getElementById('mcp-form');
  const props = sch.properties || {};
  const req = sch.required || [];
  document.querySelectorAll('.mcp-tool').forEach(el => el.setAttribute('aria-pressed', String(el.dataset.tool === name)));
  document.getElementById('mcp-run').disabled = !name;
  document.getElementById('mcp-error').textContent = '';
  document.getElementById('mcp-result').style.display = 'none';
  document.getElementById('mcp-run-status').textContent = '';
  form.innerHTML = Object.keys(props).map(k => `
    <div class="field">
       <label for="f-${esc(k)}">${esc(k.replaceAll('_', ' '))}${req.includes(k) ? ' *' : ' (optional)'}</label>
      ${props[k].type === 'boolean'
        ? `<select id="f-${esc(k)}"><option value="">unset</option><option value="true">true</option><option value="false">false</option></select>`
        : props[k].type === 'array'
          ? `<input id="f-${esc(k)}" placeholder='["a","b"] (JSON array)'>`
           : `<input id="f-${esc(k)}" ${['number', 'integer'].includes(props[k].type) ? 'type="number"' : 'type="text"'}>`}
       ${props[k].description ? `<p class="field-help" id="help-${esc(k)}">${esc(props[k].description)}</p>` : ''}
    </div>`).join('') || '<span class="muted">no arguments</span>';
  for (const k of Object.keys(props)) {
    const input = document.getElementById('f-' + k);
    input.required = req.includes(k);
    if (props[k].description) input.setAttribute('aria-describedby', 'help-' + k);
  }
}

async function runTool() {
  const err = document.getElementById('mcp-error');
  err.textContent = '';
  const name = document.getElementById('mcp-tool').value;
  const sch = toolSchema(name);
  const args = {};
  const button = document.getElementById('mcp-run');
  if (button.disabled || !name) return;
  for (const [k, def] of Object.entries(sch.properties || {})) {
    const el = document.getElementById('f-' + k);
    if (el && !el.reportValidity()) return;
    if (!el || el.value === '') continue;
    try {
      args[k] = ['number', 'integer'].includes(def.type) ? Number(el.value)
        : ['array', 'object'].includes(def.type) ? JSON.parse(el.value)
        : def.type === 'boolean' ? el.value === 'true' : el.value;
      if (def.type === 'array' && !Array.isArray(args[k])) throw new Error('Expected a JSON array');
      if (def.type === 'object' && (!args[k] || Array.isArray(args[k]) || typeof args[k] !== 'object')) throw new Error('Expected a JSON object');
      if (def.type === 'integer' && !Number.isInteger(args[k])) throw new Error('Expected a whole number');
    } catch (e) { err.textContent = `${k}: ${e.message}`; el.focus(); return; }
  }
  button.disabled = true;
  button.textContent = 'Running…';
  document.getElementById('mcp-tool').disabled = true;
  document.querySelectorAll('.mcp-tool').forEach(el => el.disabled = true);
  const status = document.getElementById('mcp-run-status');
  status.textContent = 'Waiting for response';
  document.getElementById('mcp-result').style.display = 'none';
  try {
    const r = await fetch('/api/mcp/call', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name, arguments: args }),
    });
    const body = await r.json();
    const pre = document.getElementById('mcp-result');
    pre.style.display = 'block';
    pre.textContent = JSON.stringify(body, null, 2);
    if (!r.ok) throw new Error(body.error || r.statusText);
    status.textContent = body.isError ? 'Tool returned an error' : 'Response received';
  } catch (e) { err.textContent = e.message; status.textContent = 'Request failed'; }
  finally {
    button.disabled = false;
    button.textContent = 'Run tool';
    document.getElementById('mcp-tool').disabled = false;
    document.querySelectorAll('.mcp-tool').forEach(el => el.disabled = false);
  }
}
