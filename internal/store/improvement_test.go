package store

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"testing"
	"time"
)

func TestTrendSeparatesCurrency(t *testing.T) {
	s := openAt(t, filepath.Join(t.TempDir(), "s.db"), nil)
	cid, err := s.CreateChannel(&Channel{Name: "mixed", Type: "openai", BaseURL: "http://x", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []ModelPrice{{Model: "usd", Currency: "USD"}, {Model: "cny", Currency: "CNY"}} {
		if err := s.UpsertModelPrice(&p); err != nil {
			t.Fatal(err)
		}
	}
	for _, l := range []RequestLog{{ChannelID: cid, Model: "usd", PromptTokens: 10, CostUSD: 1, Currency: "USD"}, {ChannelID: cid, Model: "cny", PromptTokens: 20, CostUSD: 7, Currency: "CNY"}} {
		if err := s.InsertRequestLog(&l); err != nil {
			t.Fatal(err)
		}
	}
	_, series, err := s.UsageTrend("channel", "today")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(series)
	var got []struct {
		Points []struct {
			Tokens int64
			Cost   float64
			Costs  map[string]float64 `json:"cost_by_currency"`
		}
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0].Points) != 1 {
		t.Fatalf("unexpected series: %s", b)
	}
	p := got[0].Points[0]
	if p.Tokens != 30 || p.Costs["USD"] != 1 || p.Costs["CNY"] != 7 || p.Cost != 0 {
		t.Fatalf("mixed currencies: %s", b)
	}
}

func TestLogCurrencySnapshot(t *testing.T) {
	s := openAt(t, filepath.Join(t.TempDir(), "s.db"), nil)
	if err := s.InsertRequestLog(&RequestLog{Model: "m", Currency: "CNY", CostUSD: 7}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertModelPrice(&ModelPrice{Model: "m", Currency: "USD"}); err != nil {
		t.Fatal(err)
	}
	page, err := s.ListLogs(LogFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if page.Items[0].Currency != "CNY" {
		t.Fatalf("lost snapshot: %+v", page.Items[0])
	}
	d, err := s.Dashboard()
	if err != nil {
		t.Fatal(err)
	}
	if d.TodayCostByCurrency["CNY"] != 7 || d.TodayCostByCurrency["USD"] != 0 {
		t.Fatalf("relabelled cost: %+v", d.TodayCostByCurrency)
	}
	if d.ByModel[0].CostByCurrency["CNY"] != 7 {
		t.Fatalf("model relabelled: %+v", d.ByModel)
	}
}

func TestCurrencyMigrationLegacyFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	s := openAt(t, path, nil)
	if err := s.UpsertModelPrice(&ModelPrice{Model: "legacy", Currency: "CNY"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertRequestLog(&RequestLog{Model: "legacy", CostUSD: 7}); err != nil {
		t.Fatal(err)
	}
	// Recreate the pre-snapshot schema and then run the real startup migration twice.
	if _, err := s.DB.Exec(`DROP TRIGGER analytics_logs_insert; DROP TRIGGER analytics_logs_update; DROP TRIGGER analytics_logs_delete;
 DROP TABLE analytics_meta; DROP TABLE request_usage_hour; DROP TABLE request_latency_day;`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`ALTER TABLE request_logs DROP COLUMN currency`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	for i := 0; i < 2; i++ {
		reopened := openAt(t, path, nil)
		page, err := reopened.ListLogs(LogFilter{})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 1 || page.Items[0].Currency != "" {
			t.Fatalf("invented legacy snapshot: %+v", page.Items)
		}
		d, err := reopened.Dashboard()
		if err != nil {
			t.Fatal(err)
		}
		if d.TodayCostByCurrency["CNY"] != 7 {
			t.Fatalf("legacy fallback: %+v", d.TodayCostByCurrency)
		}
		reopened.Close()
	}
}

func TestTrendOtherPreservesCurrencies(t *testing.T) {
	s := openAt(t, filepath.Join(t.TempDir(), "s.db"), nil)
	for i := 0; i < 10; i++ {
		currency := "USD"
		if i%2 == 1 {
			currency = "CNY"
		}
		l := RequestLog{Model: fmt.Sprintf("m%02d", i), PromptTokens: 10, CompletionTokens: 5, LatencyMs: 1000, CostUSD: 1, Currency: currency}
		if err := s.InsertRequestLog(&l); err != nil {
			t.Fatal(err)
		}
	}
	_, series, err := s.UsageTrend("model", "today")
	if err != nil {
		t.Fatal(err)
	}
	if len(series) != 9 {
		t.Fatalf("series=%d", len(series))
	}
	for _, sr := range series {
		if sr.Name == "其他" {
			p := sr.Points[0]
			if p.Tokens != 30 || p.Tps != 5 || p.CostByCurrency["USD"] != 1 || p.CostByCurrency["CNY"] != 1 || p.Cost != 0 {
				t.Fatalf("other=%+v", p)
			}
			return
		}
	}
	t.Fatal("missing other")
}

func TestDashboardDoesNotQueueOnPrimaryConnection(t *testing.T) {
	s := openAt(t, filepath.Join(t.TempDir(), "s.db"), nil)
	conn, err := s.DB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	done := make(chan error, 1)
	go func() { _, err := s.Dashboard(); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		conn.Close()
		<-done
		t.Fatal("dashboard queued on data-plane connection")
	}
}

func TestAnalyticsReadOnlyAndConcurrentWrite(t *testing.T) {
	s := openAt(t, filepath.Join(t.TempDir(), "s.db"), nil)
	if _, err := s.analyticsDB().Exec(`DELETE FROM request_logs`); err == nil {
		t.Fatal("analytics connection accepted write")
	}
	tx, err := s.analyticsDB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM request_logs`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO request_logs(model) VALUES('concurrent')`); err != nil {
		t.Fatalf("read transaction blocked write: %v", err)
	}
	if err := tx.QueryRow(`SELECT COUNT(*) FROM request_logs`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("unstable snapshot count=%d err=%v", count, err)
	}
	tx.Rollback()
	if err := s.analyticsDB().QueryRow(`SELECT COUNT(*) FROM request_logs`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("new writes invisible count=%d err=%v", count, err)
	}
}

func TestBackupRestoresCurrencySnapshot(t *testing.T) {
	s := openAt(t, filepath.Join(t.TempDir(), "s.db"), nil)
	if err := s.InsertRequestLog(&RequestLog{Model: "m", CostUSD: 7, Currency: "CNY"}); err != nil {
		t.Fatal(err)
	}
	backup, err := s.Backup()
	if err != nil {
		t.Fatal(err)
	}
	restored := openAt(t, filepath.Join(s.BackupDir(), backup.File), nil)
	page, err := restored.ListLogs(LogFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Currency != "CNY" || page.Items[0].CostUSD != 7 {
		t.Fatalf("restore mismatch: %+v", page.Items)
	}
}

func TestDashboardLatencyHistogram(t *testing.T) {
	s := openAt(t, filepath.Join(t.TempDir(), "s.db"), nil)
	for _, latency := range []int64{10, 10, 10, 100, 200} {
		if err := s.InsertRequestLog(&RequestLog{Status: 200, LatencyMs: latency, TtfbMs: 5, CompletionTokens: 5}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.InsertRequestLog(&RequestLog{Status: 500, LatencyMs: 9999, CompletionTokens: 10000}); err != nil {
		t.Fatal(err)
	}
	d, err := s.Dashboard()
	if err != nil {
		t.Fatal(err)
	}
	if d.LatencyP50Ms != 10 || d.LatencyP95Ms != 200 {
		t.Fatalf("incorrect exact percentiles: %d/%d", d.LatencyP50Ms, d.LatencyP95Ms)
	}
	if math.Abs(d.AvgTps-25.0/0.305) > 1e-9 {
		t.Fatalf("weighted speed=%f", d.AvgTps)
	}
}
