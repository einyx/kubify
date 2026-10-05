// ── Theme toggle: auto → light → dark ──
(function () {
  var btn = document.getElementById('theme-toggle');
  if (!btn) return;
  var order = ['auto', 'light', 'dark'];
  function apply() {
    var t = localStorage.getItem('kubo-theme');
    if (!t) t = matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
    document.documentElement.dataset.theme = t;
    btn.textContent = t === 'dark' ? '☾' : t === 'light' ? '☀' : '◐';
    btn.title = 'Theme: ' + t + ' (click to change)';
  }
  btn.addEventListener('click', function () {
    var cur = localStorage.getItem('kubo-theme') || 'auto';
    var next = order[(order.indexOf(cur) + 1) % order.length];
    if (next === 'auto') localStorage.removeItem('kubo-theme');
    else localStorage.setItem('kubo-theme', next);
    apply();
  });
  matchMedia('(prefers-color-scheme: dark)').addEventListener('change', apply);
  apply();
})();
