# Portal frontend

The Go binary embeds and serves these assets under `/assets/`. No Node runtime or
bundle step is required. `../index.html` contains the page and dialog markup.

- `portal.css`: feature and component styles, including light/dark themes.
- `console.css`: console design tokens, layout, responsive and accessibility rules.
- `meshx-design-system.css`: vendored scoped tokens from
  `meshxdata/meshx-design-system`; refresh with
  `hack/sync-design-system.sh /path/to/meshx-design-system`.
- `fonts/`: self-hosted Stack Sans and JetBrains Mono assets from the design system.
- `stacks.js`: API helper, stack list, detail tabs, component inspection, values.
- `backups.js`: backup listing and operations.
- `editor.js`: reconcile and Stack spec editing.
- `templates.js`: template selection, preview, and creation.
- `mcp.js`, `agents.js`, `vault.js`: feature-specific views and operations.
- `ui.js`: validation, notifications, clipboard, and table sorting.
- `theme.js`: theme preference controls.
- `runtime.js`: refresh lifecycle, SSE, routing, keyboard shortcuts, and startup.

Scripts use ordered `defer` loading and share the page's classic-script scope to
support existing HTML event handlers. Keep startup in `runtime.js`, loaded last,
so all feature state is initialized before routing or requests start.

Run `go test ./internal/portal` after changes. Rebuild/restart the portal to pick
up embedded asset changes.
