// Package history persists trace summaries + detail, partitioned by UTC day, so LiveProbe can
// answer "what happened on this day" after traces have aged out of the live window. The schema
// is identical to the TS server's, so databases written by either are interchangeable.
package history

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"liveprobe/internal/core"

	_ "modernc.org/sqlite"
)

type ErrorGroup struct {
	Endpoint      string `json:"endpoint"` // the trace's root operation
	Label         string `json:"label"`    // "<service>: <exception|HTTP status|operation>"
	Count         int    `json:"count"`
	LastSeen      int64  `json:"lastSeen"` // micros
	SampleTraceID string `json:"sampleTraceId"`
}

type LatencyBucket struct {
	Minute int64 `json:"minute"` // epoch minutes (UTC)
	Count  int   `json:"count"`
	P50    int64 `json:"p50"`
	P95    int64 `json:"p95"`
	P99    int64 `json:"p99"`
}

type HeatmapRow struct {
	Minute int64 `json:"minute"`
	Counts []int `json:"counts"`
}

type LatencyDistribution struct {
	Histogram []int        `json:"histogram"` // total count per latency bucket
	Heatmap   []HeatmapRow `json:"heatmap"`   // per-minute bucket counts
}

type DayInfo struct {
	Day      string `json:"day"`
	Requests int    `json:"requests"`
	Errors   int    `json:"errors"`
}

// SearchOptions filters are all optional and ANDed. Zero values mean "no filter".
type SearchOptions struct {
	Q         string // substring of the operation/endpoint
	Service   string // trace involves this service
	Attr      string // "key=value" (precise) or any value substring
	Error     *bool  // only errored / only clean
	MinMicros int64
	MaxMicros int64
	MinSpans  int
	TraceID   string
	Sort      string // "recent" (default) | "slowest"
	Limit     int    // default 100
}

type EndpointRollup struct {
	Operation string `json:"operation"`
	Calls     int    `json:"calls"`
	Errors    int    `json:"errors"`
	AvgMicros int64  `json:"avgMicros"`
	MaxMicros int64  `json:"maxMicros"`
}

type ThroughputBucket struct {
	Minute int64 `json:"minute"`
	Count  int   `json:"count"`
	Errors int   `json:"errors"`
}

type DaySummary struct {
	Day          string              `json:"day"`
	Requests     int                 `json:"requests"`
	Errors       int                 `json:"errors"`
	ErrorRate    float64             `json:"errorRate"`
	P50Micros    int64               `json:"p50Micros"`
	P95Micros    int64               `json:"p95Micros"`
	P99Micros    int64               `json:"p99Micros"`
	Throughput   []ThroughputBucket  `json:"throughput"`
	TopEndpoints []EndpointRollup    `json:"topEndpoints"`
	Slowest      []core.TraceSummary `json:"slowest"`
}

const summaryCols = "trace_id, root_operation, services, span_count, start_time, duration_micros, has_error"

// Store wraps one SQLite connection. Queries never nest (rows are fully read before the next
// statement), so a single connection serializes access the way the single-threaded TS server did.
type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	if path == "" {
		path = ":memory:"
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // also required for ":memory:" (each connection is its own database)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)
	s := &Store{db: db}
	if err := s.init(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) init() error {
	if _, err := s.db.Exec(`
      CREATE TABLE IF NOT EXISTS traces (
        trace_id        TEXT PRIMARY KEY,
        day             TEXT NOT NULL,
        start_time      INTEGER NOT NULL,
        duration_micros INTEGER NOT NULL,
        root_operation  TEXT NOT NULL,
        span_count      INTEGER NOT NULL,
        service_count   INTEGER NOT NULL,
        has_error       INTEGER NOT NULL,
        services        TEXT NOT NULL,
        detail_json     TEXT NOT NULL
      );
      CREATE INDEX IF NOT EXISTS idx_traces_day ON traces(day, start_time);
      CREATE INDEX IF NOT EXISTS idx_traces_day_dur ON traces(day, duration_micros);
      CREATE INDEX IF NOT EXISTS idx_traces_day_op ON traces(day, root_operation);`); err != nil {
		return err
	}
	// Added after the fact; ALTER for existing DBs (idempotent).
	for _, col := range []string{"error_label", "attrs_text"} {
		if err := s.ensureColumn("traces", col, "TEXT"); err != nil {
			return err
		}
	}
	_, err := s.db.Exec("CREATE INDEX IF NOT EXISTS idx_traces_error ON traces(has_error, start_time)")
	return err
}

func (s *Store) ensureColumn(table, col, typ string) error {
	rows, err := s.db.Query("SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return err
	}
	var cols []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		cols = append(cols, name)
	}
	rows.Close()
	if slices.Contains(cols, col) {
		return nil
	}
	_, err = s.db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, col, typ))
	return err
}

func (s *Store) Close() error { return s.db.Close() }

// DayOf is the UTC day partition ("YYYY-MM-DD") for an epoch-microsecond timestamp. Never
// trusts its input: an out-of-range value falls back to the epoch day.
func DayOf(startMicros int64) string {
	ms := startMicros / 1000
	if ms > 8_640_000_000_000_000 || ms < -8_640_000_000_000_000 { // JS Date range (ms)
		return "1970-01-01"
	}
	return time.UnixMilli(ms).UTC().Format("2006-01-02")
}

// attrsTextFor is a compact, searchable projection of a trace's span attributes: the distinct
// "key=value" pairs across all spans, bounded to 400. Keys are visited in sorted order per span.
func attrsTextFor(events []core.Event) string {
	seen := map[string]bool{}
	var pairs []string
	for _, e := range events {
		keys := make([]string, 0, len(e.Attributes))
		for k := range e.Attributes {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			p := k + "=" + attrString(e.Attributes[k])
			if !seen[p] {
				seen[p] = true
				pairs = append(pairs, p)
			}
			if len(pairs) >= 400 {
				return strings.Join(pairs, " | ")
			}
		}
	}
	return strings.Join(pairs, " | ")
}

func attrString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return core.FormatJSNumber(x)
	case bool:
		return strconv.FormatBool(x)
	}
	return fmt.Sprint(v)
}

func firstPresent(a core.Attributes, keys ...string) (any, bool) {
	for _, k := range keys {
		if v, ok := a[k]; ok {
			return v, true
		}
	}
	return nil, false
}

// errorLabelFor is a human error label from the first errored span: an exception type, then an
// HTTP status, else the failing operation.
func errorLabelFor(events []core.Event) (string, bool) {
	i := slices.IndexFunc(events, func(e core.Event) bool { return e.Status == core.StatusError })
	if i < 0 {
		return "", false
	}
	e := events[i]
	if exc, _ := firstPresent(e.Attributes, "exception.type", "exception_name", "error.type"); exc != nil {
		if s, ok := exc.(string); ok && s != "" {
			return e.Participant + ": " + s, true
		}
	}
	if status, ok := firstPresent(e.Attributes, "http.status_code", "http.response.status_code"); ok {
		var n float64
		switch x := status.(type) {
		case float64:
			n = x
		case string:
			n, _ = strconv.ParseFloat(strings.TrimSpace(x), 64)
		case bool:
			if x {
				n = 1
			}
		}
		if n >= 400 {
			return e.Participant + ": HTTP " + attrString(status), true
		}
	}
	return e.Participant + ": " + e.Operation, true
}

// UpsertMany writes the latest snapshot of each trace (a trace grows as spans stream in) in one
// transaction; on error the transaction rolls back.
func (s *Store) UpsertMany(traces []*core.AssembledTrace) (err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()
	stmt, err := tx.Prepare(`
      INSERT INTO traces (trace_id, day, start_time, duration_micros, root_operation,
                          span_count, service_count, has_error, services, detail_json, error_label, attrs_text)
      VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
      ON CONFLICT(trace_id) DO UPDATE SET
        day=excluded.day, start_time=excluded.start_time, duration_micros=excluded.duration_micros,
        root_operation=excluded.root_operation, span_count=excluded.span_count,
        service_count=excluded.service_count, has_error=excluded.has_error,
        services=excluded.services, detail_json=excluded.detail_json, error_label=excluded.error_label,
        attrs_text=excluded.attrs_text`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, t := range traces {
		d := core.Detail(t)
		sum := d.Summary
		services, _ := json.Marshal(sum.Services)
		detailJSON, jerr := json.Marshal(d)
		if jerr != nil {
			return jerr
		}
		var label any
		if sum.HasError {
			if l, ok := errorLabelFor(t.Events); ok {
				label = l
			}
		}
		hasErr := 0
		if sum.HasError {
			hasErr = 1
		}
		if _, err = stmt.Exec(sum.TraceID, DayOf(sum.StartTime), sum.StartTime, sum.DurationMicros, sum.RootOperation,
			sum.SpanCount, len(sum.Services), hasErr, string(services), string(detailJSON), label, attrsTextFor(t.Events)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Prune drops whole day-partitions older than the retention window (the N most recent UTC days,
// including today). retentionDays <= 0 disables. VACUUMs when rows were dropped.
func (s *Store) Prune(retentionDays int, nowMicros int64) (int, error) {
	if retentionDays <= 0 {
		return 0, nil
	}
	cutoff := DayOf(nowMicros - int64(retentionDays-1)*86_400_000_000)
	res, err := s.db.Exec("DELETE FROM traces WHERE day < ?", cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		if _, err := s.db.Exec("VACUUM"); err != nil {
			return int(n), err
		}
	}
	return int(n), nil
}

// queryAll runs a query and scans every row with scan before returning (never leaves rows open).
func queryAll[T any](db *sql.DB, scan func(*sql.Rows) (T, error), query string, args ...any) ([]T, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []T{}
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) Days() ([]DayInfo, error) {
	return queryAll(s.db, func(r *sql.Rows) (DayInfo, error) {
		var d DayInfo
		err := r.Scan(&d.Day, &d.Requests, &d.Errors)
		return d, err
	}, "SELECT day, count(*), COALESCE(sum(has_error), 0) FROM traces GROUP BY day ORDER BY day DESC")
}

// GetDetail returns the stored detail JSON verbatim (older rows may predate newer fields), or nil.
func (s *Store) GetDetail(traceID string) (json.RawMessage, error) {
	var raw string
	err := s.db.QueryRow("SELECT detail_json FROM traces WHERE trace_id = ?", traceID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return json.RawMessage(raw), nil
}

func scanSummary(r *sql.Rows) (core.TraceSummary, error) {
	var s core.TraceSummary
	var services string
	var hasErr int
	if err := r.Scan(&s.TraceID, &s.RootOperation, &services, &s.SpanCount, &s.StartTime, &s.DurationMicros, &hasErr); err != nil {
		return s, err
	}
	s.HasError = hasErr == 1
	if err := json.Unmarshal([]byte(services), &s.Services); err != nil || s.Services == nil {
		s.Services = []string{}
	}
	return s, nil
}

func (s *Store) DayTraces(day string, limit int) ([]core.TraceSummary, error) {
	return queryAll(s.db, scanSummary, "SELECT "+summaryCols+" FROM traces WHERE day = ? ORDER BY start_time DESC LIMIT ?", day, limit)
}

// ErrorGroups returns errored traces grouped by endpoint + error label, with a sample trace.
func (s *Store) ErrorGroups(limit int) ([]ErrorGroup, error) {
	return queryAll(s.db, func(r *sql.Rows) (ErrorGroup, error) {
		var g ErrorGroup
		var sample sql.NullString
		err := r.Scan(&g.Endpoint, &g.Label, &g.Count, &g.LastSeen, &sample)
		g.SampleTraceID = sample.String
		return g, err
	}, `SELECT root_operation AS endpoint,
                COALESCE(error_label, 'error') AS label,
                count(*) AS count,
                max(start_time) AS lastSeen,
                (SELECT s.trace_id FROM traces s
                 WHERE s.has_error = 1 AND s.root_operation = traces.root_operation
                   AND IFNULL(s.error_label, '') = IFNULL(traces.error_label, '')
                 ORDER BY s.start_time DESC LIMIT 1) AS sampleTraceId
         FROM traces
         WHERE has_error = 1
         GROUP BY root_operation, error_label
         ORDER BY count DESC
         LIMIT ?`, limit)
}

type minuteDur struct{ minute, d int64 }

func pctOf(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[min(len(sorted)-1, int(math.Floor(float64(len(sorted))*p)))]
}

// EndpointLatency is p50/p95/p99 per minute for one endpoint on a day.
func (s *Store) EndpointLatency(day, endpoint string) ([]LatencyBucket, error) {
	rows, err := queryAll(s.db, func(r *sql.Rows) (minuteDur, error) {
		var m minuteDur
		err := r.Scan(&m.minute, &m.d)
		return m, err
	}, "SELECT (start_time/60000000) AS minute, duration_micros AS d FROM traces WHERE day = ? AND root_operation = ? ORDER BY minute", day, endpoint)
	if err != nil {
		return nil, err
	}
	byMinute := map[int64][]int64{}
	var minutes []int64
	for _, r := range rows {
		if _, ok := byMinute[r.minute]; !ok {
			minutes = append(minutes, r.minute)
		}
		byMinute[r.minute] = append(byMinute[r.minute], r.d)
	}
	slices.Sort(minutes)
	out := []LatencyBucket{}
	for _, m := range minutes {
		ds := byMinute[m]
		slices.Sort(ds)
		out = append(out, LatencyBucket{Minute: m, Count: len(ds), P50: pctOf(ds, 0.5), P95: pctOf(ds, 0.95), P99: pctOf(ds, 0.99)})
	}
	return out, nil
}

// EndpointDistribution is an overall histogram plus a per-minute heatmap over the shared latency
// buckets. Bucketing happens in SQL; the CASE mirrors core.LatencyBucketIndex (lower-exclusive
// edges). Edges are numeric constants, so interpolating them is injection-safe.
func (s *Store) EndpointDistribution(day, endpoint string) (LatencyDistribution, error) {
	var b strings.Builder
	b.WriteString("CASE ")
	for i, e := range core.LatencyEdgesMicros {
		fmt.Fprintf(&b, "WHEN duration_micros < %d THEN %d ", e, i)
	}
	fmt.Fprintf(&b, "ELSE %d END", len(core.LatencyEdgesMicros))
	type row struct {
		minute int64
		bucket int
		c      int
	}
	rows, err := queryAll(s.db, func(r *sql.Rows) (row, error) {
		var x row
		err := r.Scan(&x.minute, &x.bucket, &x.c)
		return x, err
	}, "SELECT (start_time/60000000) AS minute, "+b.String()+" AS bucket, count(*) AS c FROM traces WHERE day = ? AND root_operation = ? GROUP BY minute, bucket ORDER BY minute", day, endpoint)
	if err != nil {
		return LatencyDistribution{}, err
	}
	dist := LatencyDistribution{Histogram: make([]int, core.NumLatencyBuckets), Heatmap: []HeatmapRow{}}
	byMinute := map[int64]int{} // minute -> index in heatmap
	for _, r := range rows {
		dist.Histogram[r.bucket] += r.c
		i, ok := byMinute[r.minute]
		if !ok {
			i = len(dist.Heatmap)
			byMinute[r.minute] = i
			dist.Heatmap = append(dist.Heatmap, HeatmapRow{Minute: r.minute, Counts: make([]int, core.NumLatencyBuckets)})
		}
		dist.Heatmap[i].Counts[r.bucket] += r.c
	}
	slices.SortFunc(dist.Heatmap, func(a, b HeatmapRow) int { return int(a.Minute - b.Minute) })
	return dist, nil
}

// EndpointFlamegraph folds the most recent `limit` traces of one root operation on a day into a
// single tree of where time goes.
func (s *Store) EndpointFlamegraph(day, endpoint string, limit int) (*core.FlameNode, error) {
	ids, err := queryAll(s.db, func(r *sql.Rows) (string, error) {
		var id string
		err := r.Scan(&id)
		return id, err
	}, "SELECT trace_id FROM traces WHERE day = ? AND root_operation = ? ORDER BY start_time DESC LIMIT ?", day, endpoint, limit)
	if err != nil {
		return nil, err
	}
	var traces [][]core.FlameSpan
	for _, id := range ids {
		raw, err := s.GetDetail(id)
		if err != nil || raw == nil {
			continue
		}
		var d struct {
			Spans []core.SpanRow `json:"spans"`
		}
		if json.Unmarshal(raw, &d) != nil {
			continue
		}
		spans := make([]core.FlameSpan, len(d.Spans))
		for i, r := range d.Spans {
			spans[i] = r.FlameSpan()
		}
		traces = append(traces, spans)
	}
	return core.AggregateFlamegraph(traces), nil
}

// Search across all persisted traces.
func (s *Store) Search(o SearchOptions) ([]core.TraceSummary, error) {
	var where []string
	var params []any
	add := func(clause string, v any) {
		where = append(where, clause)
		params = append(params, v)
	}
	if o.Q != "" {
		add("root_operation LIKE ?", "%"+o.Q+"%")
	}
	if o.Service != "" {
		add("services LIKE ?", `%"`+o.Service+`"%`) // services is a JSON array; match the quoted name
	}
	if o.Attr != "" {
		add("attrs_text LIKE ?", "%"+o.Attr+"%")
	}
	if o.Error != nil {
		v := 0
		if *o.Error {
			v = 1
		}
		add("has_error = ?", v)
	}
	if o.MinMicros > 0 {
		add("duration_micros >= ?", o.MinMicros)
	}
	if o.MaxMicros > 0 {
		add("duration_micros <= ?", o.MaxMicros)
	}
	if o.MinSpans > 0 {
		add("span_count >= ?", o.MinSpans)
	}
	if o.TraceID != "" {
		add("trace_id = ?", o.TraceID)
	}
	clause := ""
	if len(where) > 0 {
		clause = "WHERE " + strings.Join(where, " AND ")
	}
	orderBy := "start_time DESC"
	if o.Sort == "slowest" {
		orderBy = "duration_micros DESC"
	}
	limit := o.Limit
	if limit == 0 {
		limit = 100
	}
	return queryAll(s.db, scanSummary, "SELECT "+summaryCols+" FROM traces "+clause+" ORDER BY "+orderBy+" LIMIT ?", append(params, limit)...)
}

func (s *Store) percentile(day string, count int, p float64) (int64, error) {
	if count == 0 {
		return 0, nil
	}
	offset := min(count-1, int(math.Floor(float64(count)*p)))
	var d int64
	err := s.db.QueryRow("SELECT duration_micros FROM traces WHERE day = ? ORDER BY duration_micros ASC LIMIT 1 OFFSET ?", day, offset).Scan(&d)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return d, err
}

func (s *Store) DaySummary(day string) (DaySummary, error) {
	out := DaySummary{Day: day}
	if err := s.db.QueryRow("SELECT count(*), COALESCE(sum(has_error), 0) FROM traces WHERE day = ?", day).Scan(&out.Requests, &out.Errors); err != nil {
		return out, err
	}
	var err error
	if out.Throughput, err = queryAll(s.db, func(r *sql.Rows) (ThroughputBucket, error) {
		var t ThroughputBucket
		err := r.Scan(&t.Minute, &t.Count, &t.Errors)
		return t, err
	}, "SELECT (start_time/60000000) minute, count(*) count, COALESCE(sum(has_error), 0) errors FROM traces WHERE day = ? GROUP BY minute ORDER BY minute", day); err != nil {
		return out, err
	}
	if out.TopEndpoints, err = queryAll(s.db, func(r *sql.Rows) (EndpointRollup, error) {
		var e EndpointRollup
		var avg float64
		err := r.Scan(&e.Operation, &e.Calls, &e.Errors, &avg, &e.MaxMicros)
		e.AvgMicros = int64(math.Floor(avg + 0.5))
		return e, err
	}, `SELECT root_operation operation, count(*) calls, COALESCE(sum(has_error), 0) errors,
                avg(duration_micros) avgMicros, max(duration_micros) maxMicros
         FROM traces WHERE day = ? GROUP BY root_operation ORDER BY calls DESC LIMIT 15`, day); err != nil {
		return out, err
	}
	if out.Slowest, err = queryAll(s.db, scanSummary, "SELECT "+summaryCols+" FROM traces WHERE day = ? ORDER BY duration_micros DESC LIMIT 15", day); err != nil {
		return out, err
	}
	if out.Requests > 0 {
		out.ErrorRate = float64(out.Errors) / float64(out.Requests)
	}
	for _, p := range []struct {
		dst *int64
		p   float64
	}{{&out.P50Micros, 0.5}, {&out.P95Micros, 0.95}, {&out.P99Micros, 0.99}} {
		if *p.dst, err = s.percentile(day, out.Requests, p.p); err != nil {
			return out, err
		}
	}
	return out, nil
}
