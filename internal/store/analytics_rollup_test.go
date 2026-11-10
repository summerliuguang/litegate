package store

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Compare every materialized key and metric against an independent raw-log GROUP
// BY after each mutation, including empty groups. Bidirectional EXCEPT catches
// missing/extra keys; round only monetary REAL values to the API's six decimals.
func assertAnalyticsMatchesLogs(t *testing.T, s *Store) {
	t.Helper()
	queries := []string{
		`WITH expected AS (SELECT substr(ts,1,13)||':00:00' ts,model,channel_id,api_key_id,app,currency,COUNT(*) requests,SUM(status>=400) errors,SUM(status>=400 OR error!='') failed_requests,SUM(prompt_tokens) prompt_tokens,SUM(completion_tokens) completion_tokens,SUM(cache_tokens) cache_tokens,SUM(latency_ms) latency_ms,ROUND(SUM(cost),6) cost FROM request_logs GROUP BY 1,2,3,4,5,6), actual AS (SELECT ts,model,channel_id,api_key_id,app,currency,requests,errors,failed_requests,prompt_tokens,completion_tokens,cache_tokens,latency_ms,ROUND(cost,6) cost FROM request_usage_hour), missing AS (SELECT * FROM expected EXCEPT SELECT * FROM actual), extra AS (SELECT * FROM actual EXCEPT SELECT * FROM expected) SELECT (SELECT COUNT(*) FROM missing)+(SELECT COUNT(*) FROM extra)`,
		`WITH expected AS (SELECT substr(ts,1,10)||' 00:00:00' ts,latency_ms,COUNT(*) requests,SUM(CASE WHEN completion_tokens>0 AND latency_ms>ttfb_ms THEN latency_ms-ttfb_ms ELSE 0 END) gen_ms,SUM(CASE WHEN completion_tokens>0 AND latency_ms>ttfb_ms THEN completion_tokens ELSE 0 END) gen_tokens FROM request_logs WHERE status<400 AND error='' GROUP BY 1,2), missing AS (SELECT * FROM expected EXCEPT SELECT * FROM request_latency_day), extra AS (SELECT * FROM request_latency_day EXCEPT SELECT * FROM expected) SELECT (SELECT COUNT(*) FROM missing)+(SELECT COUNT(*) FROM extra)`,
	}
	for _, q := range queries {
		var differences int
		if err := s.DB.QueryRow(q).Scan(&differences); err != nil {
			t.Fatal(err)
		}
		if differences != 0 {
			t.Fatalf("materialized totals differ from raw logs: %d", differences)
		}
	}
}

func TestAnalyticsInsertUpdateDeleteRollback(t *testing.T) {
	s := openAt(t, filepath.Join(t.TempDir(), "s.db"), nil)
	for i := 0; i < 90; i++ {
		status := 200
		errorText := ""
		currency := "USD"
		if i%3 == 0 {
			status = 500
		}
		if i%5 == 0 {
			errorText = "interrupted"
		}
		if i%2 == 0 {
			currency = "CNY"
		}
		if err := s.InsertRequestLog(&RequestLog{Model: "m", ChannelID: int64(i % 4), APIKeyID: int64(i % 3), App: "app", Currency: currency, Status: status, Error: errorText, PromptTokens: int64(i), CompletionTokens: 5, CacheTokens: 2, LatencyMs: int64(i + 1), TtfbMs: 3, CostUSD: 0.000123}); err != nil {
			t.Fatal(err)
		}
	}
	assertAnalyticsMatchesLogs(t, s)
	if _, err := s.DB.Exec(`UPDATE request_logs SET ts='2026-01-02 23:59:59',currency='EUR',cost=7,model='renamed',app='other',channel_id=9,api_key_id=8,status=200,error='',latency_ms=300,ttfb_ms=20 WHERE id%2=0`); err != nil {
		t.Fatal(err)
	}
	assertAnalyticsMatchesLogs(t, s)
	var hour, day string
	if err := s.DB.QueryRow(`SELECT ts FROM request_usage_hour WHERE model='renamed' LIMIT 1`).Scan(&hour); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRow(`SELECT ts FROM request_latency_day WHERE latency_ms=300`).Scan(&day); err != nil {
		t.Fatal(err)
	}
	if hour != "2026-01-02 23:00:00" || day != "2026-01-02 00:00:00" {
		t.Fatalf("UTC bucket shifted %s %s", hour, day)
	}
	tx, err := s.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`DELETE FROM request_logs; INSERT INTO request_logs(model,cost,currency) VALUES('rolled-back',99,'USD')`); err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	assertAnalyticsMatchesLogs(t, s)
	if _, err := s.PruneLogs(7); err != nil {
		t.Fatal(err)
	}
	assertAnalyticsMatchesLogs(t, s)
	if _, err := s.DB.Exec(`DELETE FROM request_logs`); err != nil {
		t.Fatal(err)
	}
	assertAnalyticsMatchesLogs(t, s)
}

func TestAnalyticsBackfillAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.db")
	s := openAt(t, path, nil)
	if err := s.InsertRequestLog(&RequestLog{Model: "m", Currency: "CNY", CostUSD: 7, PromptTokens: 10, CompletionTokens: 5, Status: 200, LatencyMs: 20}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`DROP TRIGGER analytics_logs_insert;DROP TRIGGER analytics_logs_update;DROP TRIGGER analytics_logs_delete;DROP TABLE request_usage_hour;DROP TABLE request_latency_day;DROP TABLE analytics_meta`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	for i := 0; i < 2; i++ {
		next := openAt(t, path, nil)
		assertAnalyticsMatchesLogs(t, next)
		d, err := next.Dashboard()
		if err != nil {
			t.Fatal(err)
		}
		if d.TodayRequests != 1 || d.TodayCostByCurrency["CNY"] != 7 {
			t.Fatalf("backfill counted twice: %+v", d)
		}
		next.Close()
	}
}

func TestAnalyticsUTCWindowsAndPriceChanges(t *testing.T) {
	s := openAt(t, filepath.Join(t.TempDir(), "s.db"), nil)
	for _, cur := range []string{"USD", "CNY", ""} {
		if err := s.InsertRequestLog(&RequestLog{Model: "m", Currency: cur, CostUSD: 1, PromptTokens: 10, Status: 200}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.UpsertModelPrice(&ModelPrice{Model: "m", Currency: "CNY"}); err != nil {
		t.Fatal(err)
	}
	d, err := s.Dashboard()
	if err != nil {
		t.Fatal(err)
	}
	if d.TodayCostByCurrency["USD"] != 1 || d.TodayCostByCurrency["CNY"] != 2 {
		t.Fatal(d.TodayCostByCurrency)
	}
	if err := s.UpsertModelPrice(&ModelPrice{Model: "m", Currency: "USD"}); err != nil {
		t.Fatal(err)
	}
	d, err = s.Dashboard()
	if err != nil {
		t.Fatal(err)
	}
	if d.TodayCostByCurrency["USD"] != 2 || d.TodayCostByCurrency["CNY"] != 1 {
		t.Fatal("price fallback stale", d.TodayCostByCurrency)
	}
	if _, err := s.DB.Exec(`UPDATE request_logs SET ts=datetime('now','start of day','-1 second') WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	d, err = s.Dashboard()
	if err != nil {
		t.Fatal(err)
	}
	if d.TodayRequests != 2 {
		t.Fatal("midnight bucket stale", d.TodayRequests)
	}
	_, today, err := s.UsageTrend("model", "today")
	if err != nil {
		t.Fatal(err)
	}
	_, yesterday, err := s.UsageTrend("model", "yesterday")
	if err != nil {
		t.Fatal(err)
	}
	if today[0].Points[0].Tokens != 20 || yesterday[0].Points[0].Tokens != 10 || math.Abs(yesterday[0].Points[0].CostByCurrency["USD"]-1) > 1e-9 {
		t.Fatal("wrong UTC windows")
	}
	assertAnalyticsMatchesLogs(t, s)
}

func TestAnalyticsMetadataChangesVisible(t *testing.T) {
	s := openAt(t, filepath.Join(t.TempDir(), "s.db"), nil)
	cid, err := s.CreateChannel(&Channel{Name: "before", Type: "openai", BaseURL: "http://local", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	key := &APIKey{Name: "key-before"}
	if err := s.CreateAPIKey(key); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertRequestLog(&RequestLog{ChannelID: cid, APIKeyID: key.ID, Model: "m", PromptTokens: 10, CostUSD: 1, Currency: "USD"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`UPDATE channels SET name='after' WHERE id=?`, cid); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`UPDATE api_keys SET name='key-after' WHERE id=?`, key.ID); err != nil {
		t.Fatal(err)
	}
	for dim, want := range map[string]string{"channel": "after", "key": "key-after"} {
		_, series, err := s.UsageTrend(dim, "today")
		if err != nil {
			t.Fatal(err)
		}
		if len(series) != 1 || series[0].Name != want || series[0].Points[0].Tokens != 10 {
			t.Fatalf("stale %s metadata: %+v", dim, series)
		}
	}
	if _, err := s.DB.Exec(`DELETE FROM channels WHERE id=?`, cid); err != nil {
		t.Fatal(err)
	}
	_, series, err := s.UsageTrend("channel", "today")
	if err != nil {
		t.Fatal(err)
	}
	if len(series) != 0 {
		t.Fatal("deleted channel still cached")
	}
	d, err := s.Dashboard()
	if err != nil {
		t.Fatal(err)
	}
	if d.TodayRequests != 1 || d.TodayCostByCurrency["USD"] != 1 {
		t.Fatal("metadata deletion erased historical totals")
	}
}

func TestAnalyticsConcurrentReadsAndWrites(t *testing.T) {
	s := openAt(t, filepath.Join(t.TempDir(), "s.db"), nil)
	done := make(chan error, 1)
	go func() {
		for i := 0; i < 200; i++ {
			if err := s.InsertRequestLog(&RequestLog{Model: "m", Status: 200, PromptTokens: 10, CompletionTokens: 5, CostUSD: 0.000004, Currency: "CNY", LatencyMs: 10}); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	var previous int64
	for i := 0; i < 20; i++ {
		d, err := s.Dashboard()
		if err != nil {
			t.Fatal(err)
		}
		if d.TodayRequests < previous {
			t.Fatal("aggregate count went backwards")
		}
		previous = d.TodayRequests
		if d.TodayRequests > 0 && (d.TodayRequests != d.Daily[0].Requests || d.TodayRequests != d.ByModel[0].Requests) {
			t.Fatal("inconsistent response snapshot")
		}
		if _, _, err := s.UsageTrend("model", "today"); err != nil {
			t.Fatal(err)
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	assertAnalyticsMatchesLogs(t, s)
	d, err := s.Dashboard()
	if err != nil {
		t.Fatal(err)
	}
	if d.TodayRequests != 200 || math.Abs(d.TodayCostByCurrency["CNY"]-0.0008) > 1e-9 {
		t.Fatalf("concurrent totals lost: %+v", d)
	}
}

func TestWALReclaimsAfterReaderRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.db")
	s := openAt(t, path, nil)
	var limit int64
	if err := s.DB.QueryRow(`PRAGMA journal_size_limit`).Scan(&limit); err != nil || limit != maxIdleWALBytes {
		t.Fatalf("WAL limit %d: %v", limit, err)
	}
	// A smaller limit makes the same reclamation path observable without a large fixture.
	if _, err := s.DB.Exec(`PRAGMA journal_size_limit=65536; PRAGMA wal_autocheckpoint=10; PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	reader, err := s.analyticsDB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Rollback()
	var count int
	if err := reader.QueryRow(`SELECT COUNT(*) FROM request_logs`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if _, err := s.DB.Exec(`INSERT INTO request_logs(model,error) VALUES('wal',?)`, strings.Repeat("x", 4096)); err != nil {
			t.Fatal(err)
		}
	}
	info, err := os.Stat(path + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() <= 65536 {
		t.Fatal("fixture did not pin an oversized WAL")
	}
	if err := reader.Rollback(); err != nil {
		t.Fatal(err)
	}
	// Automatic checkpoints and the next WAL reset reclaim the retained high water
	// mark without forcing a blocking TRUNCATE during application writes.
	for i := 0; i < 3; i++ {
		if _, err := s.DB.Exec(`INSERT INTO request_logs(model) VALUES('after')`); err != nil {
			t.Fatal(err)
		}
	}
	info, err = os.Stat(path + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > 65536 {
		t.Fatalf("WAL not reclaimed after reader release: %d", info.Size())
	}
	assertAnalyticsMatchesLogs(t, s)
}

func TestAnalyticsCancellationReleasesReader(t *testing.T) {
	s := openAt(t, filepath.Join(t.TempDir(), "s.db"), nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.DashboardContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("dashboard cancellation: %v", err)
	}
	if _, _, err := s.UsageTrendContext(ctx, "model", "today"); !errors.Is(err, context.Canceled) {
		t.Fatalf("trend cancellation: %v", err)
	}
	if _, err := s.Dashboard(); err != nil {
		t.Fatalf("reader not reusable: %v", err)
	}
}

func TestAnalyticsFrozenUTCWindow(t *testing.T) {
	s := openAt(t, filepath.Join(t.TempDir(), "s.db"), nil)
	if _, err := s.DB.Exec(`INSERT INTO request_logs(ts,model,prompt_tokens,cost,currency,status) VALUES
 ('2026-01-02 23:59:59','m',10,1,'USD',200),('2026-01-03 00:00:00','m',20,7,'CNY',200)`); err != nil {
		t.Fatal(err)
	}
	before := time.Date(2026, 1, 3, 7, 59, 59, 0, time.FixedZone("UTC+8", 8*3600))
	d, err := s.dashboard(s.analyticsDB(), before)
	if err != nil {
		t.Fatal(err)
	}
	if d.TodayRequests != 1 || d.TodayCostByCurrency["USD"] != 1 || d.TodayCostByCurrency["CNY"] != 0 || d.RPM != 1 {
		t.Fatalf("UTC pre-midnight mixed days: %+v", d)
	}
	_, series, err := s.usageTrend(s.analyticsDB(), before, "model", "today")
	if err != nil {
		t.Fatal(err)
	}
	if len(series) != 1 || series[0].Points[0].Tokens != 10 {
		t.Fatal("future day included")
	}
	after := before.Add(time.Second)
	d, err = s.dashboard(s.analyticsDB(), after)
	if err != nil {
		t.Fatal(err)
	}
	if d.TodayRequests != 1 || d.TodayCostByCurrency["CNY"] != 7 || d.RPM != 2 {
		t.Fatalf("UTC rollover stale: %+v", d)
	}
}

func TestAnalyticsInterruptsActiveSQLiteQuery(t *testing.T) {
	s := openAt(t, filepath.Join(t.TempDir(), "s.db"), nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	tx, err := s.analyticsDB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := analyticsSnapshot{tx: tx, ctx: ctx}
	var count int64
	err = snapshot.QueryRow(`WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<100000000) SELECT SUM(x) FROM n`).Scan(&count)
	if err == nil {
		t.Fatal("active query ignored cancellation")
	}
	if ctx.Err() == nil {
		t.Fatalf("query failed before cancellation: %v", err)
	}
	tx.Rollback()
	if _, err := s.Dashboard(); err != nil {
		t.Fatalf("cancelled snapshot retained connection: %v", err)
	}
}

func TestBudgetBackfillSnapshotCurrencyAndAtomicity(t *testing.T) {
	s := openAt(t, filepath.Join(t.TempDir(), "s.db"), nil)
	if err := s.UpsertModelPrice(&ModelPrice{Model: "m", Currency: "CNY"}); err != nil {
		t.Fatal(err)
	}
	for _, l := range []RequestLog{
		{APIKeyID: 1, Model: "m", Currency: "USD", CostUSD: 2, PromptTokens: 10},
		{APIKeyID: 1, Model: "m", Currency: "CNY", CostUSD: 7, PromptTokens: 10},
		{APIKeyID: 2, Model: "m", Currency: "", CostUSD: 3, PromptTokens: 10},
	} {
		if err := s.InsertRequestLog(&l); err != nil {
			t.Fatal(err)
		}
	}
	// Abort the second insert regardless of Go map iteration order.
	if _, err := s.DB.Exec(`CREATE TRIGGER fail_budget_backfill BEFORE INSERT ON key_usage_day WHEN (SELECT COUNT(*) FROM key_usage_day)>0 BEGIN SELECT RAISE(ABORT,'test failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BackfillKeyUsageFromLogs(); err == nil {
		t.Fatal("expected injected failure")
	}
	var count int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM key_usage_day`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("partial migration committed: %d", count)
	}
	if _, err := s.DB.Exec(`DROP TRIGGER fail_budget_backfill`); err != nil {
		t.Fatal(err)
	}
	if n, err := s.BackfillKeyUsageFromLogs(); err != nil || n != 2 {
		t.Fatalf("retry: %d %v", n, err)
	}
	usd, cny, tokens, err := s.KeyUsageSince(1, "2000-01-01")
	if err != nil || usd != 2 || cny != 7 || tokens != 20 {
		t.Fatalf("snapshot currency lost: %f %f %d %v", usd, cny, tokens, err)
	}
	usd, cny, _, err = s.KeyUsageSince(2, "2000-01-01")
	if err != nil || usd != 0 || cny != 3 {
		t.Fatalf("legacy currency fallback: %f %f %v", usd, cny, err)
	}
	if n, err := s.BackfillKeyUsageFromLogs(); err != nil || n != 0 {
		t.Fatalf("not idempotent: %d %v", n, err)
	}
}
