package agentfw

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite; keeps CGO_ENABLED=0 distroless builds
)

// Archive is the persistent, searchable record of every LLM call that
// crossed the proxy — agentsview's core idea, applied to firewall traffic.
// It is safe for concurrent use.
type Archive struct {
	db *sql.DB
	mu sync.Mutex // serializes writes; SQLite likes one writer

	fts bool // FTS5 available (probed at Open); search falls back to LIKE
}

// Record is one proxied LLM call stored in the archive.
type Record struct {
	ID          int64     `json:"id"`
	SessionID   string    `json:"session_id"`
	Time        time.Time `json:"time"`
	Method      string    `json:"method"`
	URL         string    `json:"url"`
	Host        string    `json:"host"`
	Model       string    `json:"model,omitempty"`
	Status      int       `json:"status"`
	DurationMS  int64     `json:"duration_ms"`
	ReqBytes    int       `json:"req_bytes"`
	RespBytes   int       `json:"resp_bytes"`
	Action      string    `json:"action"`
	ReqBody     string    `json:"req_body,omitempty"`
	RespBody    string    `json:"resp_body,omitempty"`
	Findings    []Finding `json:"findings,omitempty"`
	Usage       *Usage    `json:"usage,omitempty"`
}

// SessionSummary is a rolled-up session row for list views.
type SessionSummary struct {
	SessionID   string    `json:"session_id"`
	FirstSeen   time.Time `json:"first_seen"`
	LastSeen    time.Time `json:"last_seen"`
	Requests    int       `json:"requests"`
	Blocked     int       `json:"blocked"`
	Findings    int       `json:"findings"`
	InputTokens int64     `json:"input_tokens"`
	OutTokens   int64     `json:"output_tokens"`
	CostMicro   int64     `json:"cost_micro"`
	Models      []string  `json:"models"`
}

// Stats is the dashboard payload — agentsview's front page, firewall flavor.
type Stats struct {
	TotalRequests   int            `json:"total_requests"`
	TotalSessions   int            `json:"total_sessions"`
	Blocked         int            `json:"blocked"`
	Redacted        int            `json:"redacted"`
	FindingsByKind  map[string]int `json:"findings_by_kind"`
	TopModels       []ModelUsage   `json:"top_models"`
	InputTokens     int64          `json:"input_tokens"`
	OutputTokens    int64          `json:"output_tokens"`
	CostMicro       int64          `json:"cost_micro"`
	RecentBlocked   []Record       `json:"recent_blocked,omitempty"`
}

// ModelUsage is per-model token/cost rollup.
type ModelUsage struct {
	Model       string `json:"model"`
	Requests    int    `json:"requests"`
	InputTokens int64  `json:"input_tokens"`
	OutTokens   int64  `json:"output_tokens"`
	CostMicro   int64  `json:"cost_micro"`
}

const archiveSchema = `
CREATE TABLE IF NOT EXISTS requests (
	id          INTEGER PRIMARY KEY,
	session_id  TEXT NOT NULL,
	ts          INTEGER NOT NULL,
	method      TEXT NOT NULL,
	url         TEXT NOT NULL,
	host        TEXT NOT NULL,
	model       TEXT NOT NULL DEFAULT '',
	status      INTEGER NOT NULL,
	duration_ms INTEGER NOT NULL,
	req_bytes   INTEGER NOT NULL,
	resp_bytes  INTEGER NOT NULL,
	action      TEXT NOT NULL,
	req_body    TEXT NOT NULL DEFAULT '',
	resp_body   TEXT NOT NULL DEFAULT '',
	input_tokens  INTEGER NOT NULL DEFAULT 0,
	output_tokens INTEGER NOT NULL DEFAULT 0,
	cost_micro    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_requests_session ON requests(session_id, ts);
CREATE INDEX IF NOT EXISTS idx_requests_ts      ON requests(ts);
CREATE INDEX IF NOT EXISTS idx_requests_action  ON requests(action);

CREATE TABLE IF NOT EXISTS findings (
	id         INTEGER PRIMARY KEY,
	request_id INTEGER NOT NULL REFERENCES requests(id) ON DELETE CASCADE,
	kind       TEXT NOT NULL,
	pattern    TEXT NOT NULL,
	excerpt    TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_findings_request ON findings(request_id);
`

// OpenArchive opens (creating if needed) the archive at path and applies
// the schema. A nil return value from callers is handled gracefully by the
// recorder, so a disabled or failed archive never blocks traffic.
func OpenArchive(path string) (*Archive, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("archive open: %w", err)
	}
	// Modern SQLite is fine with these; busy_timeout avoids spurious
	// SQLITE_BUSY when the viewer reads during a write burst.
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("archive pragma: %w", err)
		}
	}
	a := &Archive{db: db}
	if err := a.init(); err != nil {
		db.Close()
		return nil, err
	}
	return a, nil
}

func (a *Archive) init() error {
	if _, err := a.db.Exec(archiveSchema); err != nil {
		return fmt.Errorf("archive schema: %w", err)
	}
	// Probe FTS5 once; modernc ships it in most builds. Fall back to LIKE.
	if _, err := a.db.Exec(`CREATE VIRTUAL TABLE IF NOT EXISTS requests_fts USING fts5(url, req_body, resp_body, content='requests', content_rowid='id')`); err != nil {
		a.fts = false
		return nil
	}
	// Keep FTS in step with the content table.
	a.db.Exec(`CREATE TRIGGER IF NOT EXISTS requests_ai AFTER INSERT ON requests BEGIN
		INSERT INTO requests_fts(rowid, url, req_body, resp_body)
		VALUES (new.id, new.url, new.req_body, new.resp_body);
	END`) //nolint:errcheck
	a.db.Exec(`CREATE TRIGGER IF NOT EXISTS requests_ad AFTER DELETE ON requests BEGIN
		INSERT INTO requests_fts(requests_fts, rowid, url, req_body, resp_body)
		VALUES ('delete', old.id, old.url, old.req_body, old.resp_body);
	END`) //nolint:errcheck
	a.fts = true
	return nil
}

// Close closes the underlying database.
func (a *Archive) Close() error { return a.db.Close() }

// Insert stores one record with its findings. Bodies are truncated to
// keep a single row from dominating the file; the caps match the
// scanner's own read limits.
func (a *Archive) Insert(rec Record) (int64, error) {
	const maxBody = 1 << 20 // 1 MB per side
	if len(rec.ReqBody) > maxBody {
		rec.ReqBody = rec.ReqBody[:maxBody]
	}
	if len(rec.RespBody) > maxBody {
		rec.RespBody = rec.RespBody[:maxBody]
	}
	if rec.Time.IsZero() {
		rec.Time = time.Now()
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	tx, err := a.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck

	var in, out, cost int64
	if rec.Usage != nil {
		in, out, cost = rec.Usage.InputTokens, rec.Usage.OutputTokens, rec.Usage.CostMicro
	}
	res, err := tx.Exec(`INSERT INTO requests
		(session_id, ts, method, url, host, model, status, duration_ms, req_bytes, resp_bytes, action, req_body, resp_body, input_tokens, output_tokens, cost_micro)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		rec.SessionID, rec.Time.Unix(), rec.Method, rec.URL, rec.Host, rec.Model,
		rec.Status, rec.DurationMS, rec.ReqBytes, rec.RespBytes, rec.Action,
		rec.ReqBody, rec.RespBody, in, out, cost)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	for _, f := range rec.Findings {
		if _, err := tx.Exec(`INSERT INTO findings (request_id, kind, pattern, excerpt) VALUES (?,?,?,?)`,
			id, f.Kind, f.Pattern, f.Excerpt); err != nil {
			return 0, err
		}
	}
	return id, tx.Commit()
}

// SearchOptions controls List/Search.
type SearchOptions struct {
	Query     string // FTS5 or LIKE query; empty = browse
	SessionID string
	Action    string // "allow" | "redact" | "block"; empty = all
	Limit     int
	Offset    int
}

// List returns matching records newest-first, without bodies (use Get).
func (a *Archive) List(opt SearchOptions) ([]Record, error) {
	if opt.Limit <= 0 || opt.Limit > 500 {
		opt.Limit = 100
	}
	var rows *sql.Rows
	var err error

	if opt.Query != "" && a.fts {
		rows, err = a.db.Query(`SELECT r.id, r.session_id, r.ts, r.method, r.url, r.host, r.model, r.status, r.duration_ms, r.req_bytes, r.resp_bytes, r.action,
			COALESCE(r.input_tokens,0), COALESCE(r.output_tokens,0), COALESCE(r.cost_micro,0),
			(SELECT COUNT(*) FROM findings f WHERE f.request_id = r.id)
			FROM requests_fts q JOIN requests r ON r.id = q.rowid
			WHERE requests_fts MATCH ?
			AND (? = '' OR r.session_id = ?) AND (? = '' OR r.action = ?)
			ORDER BY r.ts DESC LIMIT ? OFFSET ?`,
			ftsQuery(opt.Query), opt.SessionID, opt.SessionID, opt.Action, opt.Action, opt.Limit, opt.Offset)
	} else {
		where, args := listWhere(opt)
		rows, err = a.db.Query(`SELECT id, session_id, ts, method, url, host, model, status, duration_ms, req_bytes, resp_bytes, action,
			input_tokens, output_tokens, cost_micro,
			0
			FROM requests `+where+` ORDER BY ts DESC LIMIT ? OFFSET ?`,
			append(args, opt.Limit, opt.Offset)...)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRecords(rows)
}

// SearchCount returns the total hits for SearchOptions (for pagination UI).
func (a *Archive) SearchCount(opt SearchOptions) (int, error) {
	if opt.Query != "" && a.fts {
		return countQuery(a.db, `SELECT COUNT(*) FROM requests_fts q JOIN requests r ON r.id = q.rowid
			WHERE requests_fts MATCH ? AND (? = '' OR r.session_id = ?) AND (? = '' OR r.action = ?)`,
			ftsQuery(opt.Query), opt.SessionID, opt.SessionID, opt.Action, opt.Action)
	}
	where, args := listWhere(opt)
	return countQuery(a.db, `SELECT COUNT(*) FROM requests `+where, args...)
}

func listWhere(opt SearchOptions) (string, []any) {
	var conds []string
	var args []any
	if opt.SessionID != "" {
		conds = append(conds, "session_id = ?")
		args = append(args, opt.SessionID)
	}
	if opt.Action != "" {
		conds = append(conds, "action = ?")
		args = append(args, opt.Action)
	}
	if len(conds) == 0 {
		return "", nil
	}
	return "WHERE " + strings.Join(conds, " AND "), args
}

// ftsQuery sanitizes user input into a prefix-matching FTS5 query.
// User quotes/operators are dropped; each word becomes word*.
func ftsQuery(q string) string {
	words := strings.Fields(q)
	var out []string
	for _, w := range words {
		var b strings.Builder
		for _, r := range w {
			if r == '"' || r == '*' || r == '(' || r == ')' || r == ':' {
				continue
			}
			b.WriteRune(r)
		}
		if s := b.String(); s != "" {
			out = append(out, `"`+s+`"*`)
		}
	}
	return strings.Join(out, " ")
}

// Get returns one record with bodies and findings.
func (a *Archive) Get(id int64) (*Record, error) {
	row := a.db.QueryRow(`SELECT id, session_id, ts, method, url, host, model, status, duration_ms, req_bytes, resp_bytes, action, req_body, resp_body,
		input_tokens, output_tokens, cost_micro FROM requests WHERE id = ?`, id)
	var r Record
	var ts int64
	var in, out, cost int64
	var usage Usage
	if err := row.Scan(&r.ID, &r.SessionID, &ts, &r.Method, &r.URL, &r.Host, &r.Model, &r.Status, &r.DurationMS, &r.ReqBytes, &r.RespBytes, &r.Action, &r.ReqBody, &r.RespBody, &in, &out, &cost); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	r.Time = time.Unix(ts, 0)
	if in != 0 || out != 0 {
		usage = Usage{Model: r.Model, InputTokens: in, OutputTokens: out, CostMicro: cost, HasUsage: true}
		r.Usage = &usage
	}
	findings, err := a.findingsFor(id)
	if err != nil {
		return nil, err
	}
	r.Findings = findings
	return &r, nil
}

func (a *Archive) findingsFor(requestID int64) ([]Finding, error) {
	rows, err := a.db.Query(`SELECT kind, pattern, excerpt FROM findings WHERE request_id = ? ORDER BY id`, requestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Finding
	for rows.Next() {
		var f Finding
		if err := rows.Scan(&f.Kind, &f.Pattern, &f.Excerpt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Sessions returns rolled-up session summaries newest-active-first.
func (a *Archive) Sessions(limit int) ([]SessionSummary, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := a.db.Query(`SELECT session_id, MIN(ts), MAX(ts), COUNT(*),
			SUM(CASE WHEN action = 'block' THEN 1 ELSE 0 END),
			(SELECT COUNT(*) FROM findings f JOIN requests r2 ON r2.id = f.request_id WHERE r2.session_id = r.session_id),
			SUM(input_tokens), SUM(output_tokens), SUM(cost_micro),
			COALESCE(GROUP_CONCAT(DISTINCT CASE WHEN model != '' THEN model END), '')
		FROM requests r GROUP BY session_id ORDER BY MAX(ts) DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionSummary
	for rows.Next() {
		var s SessionSummary
		var first, last int64
		var models string
		if err := rows.Scan(&s.SessionID, &first, &last, &s.Requests, &s.Blocked, &s.Findings,
			&s.InputTokens, &s.OutTokens, &s.CostMicro, &models); err != nil {
			return nil, err
		}
		s.FirstSeen = time.Unix(first, 0)
		s.LastSeen = time.Unix(last, 0)
		if models != "" {
			s.Models = strings.Split(models, ",")
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Stats returns the dashboard rollup.
func (a *Archive) Stats() (*Stats, error) {
	s := &Stats{FindingsByKind: map[string]int{}}

	var q = [][2]any{}
	_ = q
	if err := a.db.QueryRow(`SELECT COUNT(*), COUNT(DISTINCT session_id),
			COALESCE(SUM(CASE WHEN action='block' THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN action='redact' THEN 1 ELSE 0 END),0),
			COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0), COALESCE(SUM(cost_micro),0)
			FROM requests`).Scan(&s.TotalRequests, &s.TotalSessions, &s.Blocked, &s.Redacted, &s.InputTokens, &s.OutputTokens, &s.CostMicro); err != nil {
		return nil, err
	}

	frows, err := a.db.Query(`SELECT kind, COUNT(*) FROM findings GROUP BY kind`)
	if err != nil {
		return nil, err
	}
	for frows.Next() {
		var kind string
		var n int
		if err := frows.Scan(&kind, &n); err != nil {
			frows.Close()
			return nil, err
		}
		s.FindingsByKind[kind] = n
	}
	frows.Close()

	mrows, err := a.db.Query(`SELECT model, COUNT(*), SUM(input_tokens), SUM(output_tokens), SUM(cost_micro)
		FROM requests WHERE model != '' GROUP BY model ORDER BY cost_micro DESC LIMIT 10`)
	if err != nil {
		return nil, err
	}
	defer mrows.Close()
	for mrows.Next() {
		var m ModelUsage
		if err := mrows.Scan(&m.Model, &m.Requests, &m.InputTokens, &m.OutTokens, &m.CostMicro); err != nil {
			return nil, err
		}
		s.TopModels = append(s.TopModels, m)
	}

	blocked, err := a.List(SearchOptions{Action: "block", Limit: 10})
	if err != nil {
		return nil, err
	}
	s.RecentBlocked = blocked
	return s, nil
}

func scanRecords(rows *sql.Rows) ([]Record, error) {
	var out []Record
	for rows.Next() {
		var r Record
		var ts int64
		var findings int
		var inTok, outTok, cost int64
		if err := rows.Scan(&r.ID, &r.SessionID, &ts, &r.Method, &r.URL, &r.Host, &r.Model, &r.Status, &r.DurationMS, &r.ReqBytes, &r.RespBytes, &r.Action,
			&inTok, &outTok, &cost, &findings); err != nil {
			return nil, err
		}
		r.Time = time.Unix(ts, 0)
		if inTok != 0 || outTok != 0 {
			r.Usage = &Usage{Model: r.Model, InputTokens: inTok, OutputTokens: outTok, CostMicro: cost, HasUsage: true}
		}
		if findings > 0 {
			r.Findings = make([]Finding, findings) // count only; detail via Get
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func countQuery(db *sql.DB, q string, args ...any) (int, error) {
	var n int
	err := db.QueryRow(q, args...).Scan(&n)
	return n, err
}
