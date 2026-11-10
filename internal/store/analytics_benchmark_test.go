package store

import (
	"path/filepath"
	"testing"
	"time"
)

func BenchmarkAnalyticsMillion(b *testing.B) {
	s, err := Open(filepath.Join(b.TempDir(), "bench.db"), nil)
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	start := time.Now()
	_, err = s.DB.Exec(`WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<1000000)
 INSERT INTO request_logs(model,status,prompt_tokens,completion_tokens,latency_ms,cost,currency)
 SELECT 'm',200,10,5,10,0.00004,'USD' FROM n`)
	if err != nil {
		b.Fatal(err)
	}
	b.Logf("seed one million: %s", time.Since(start))
	b.Run("backfill", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			b.StopTimer()
			if _, err := s.DB.Exec(`DROP TRIGGER analytics_logs_insert;DROP TRIGGER analytics_logs_update;DROP TRIGGER analytics_logs_delete;DROP TABLE request_usage_hour;DROP TABLE request_latency_day;DROP TABLE analytics_meta`); err != nil {
				b.Fatal(err)
			}
			b.StartTimer()
			if err := migrateAnalytics(s.DB); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("dashboard", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			d, err := s.Dashboard()
			if err != nil {
				b.Fatal(err)
			}
			if d.TodayRequests != 1000000 {
				b.Fatal(d.TodayRequests)
			}
		}
	})
	b.Run("trend", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_, sr, err := s.UsageTrend("model", "7d")
			if err != nil {
				b.Fatal(err)
			}
			if len(sr) != 1 || sr[0].Points[0].Tokens != 15000000 {
				b.Fatal("wrong aggregate")
			}
		}
	})
}

// Less compressible fixture: 20 models x 50 keys x 5 apps x 24 UTC hours.
// This documents the cardinality tradeoff instead of extrapolating one bucket.
func BenchmarkAnalyticsDiverseMillion(b *testing.B) {
	s, err := Open(filepath.Join(b.TempDir(), "diverse.db"), nil)
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	if _, err := s.CreateChannel(&Channel{Name: "bench", Type: "openai", BaseURL: "http://local-mock", Enabled: true}); err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		if err := s.CreateAPIKey(&APIKey{Name: "bench"}); err != nil {
			b.Fatal(err)
		}
	}
	start := time.Now()
	_, err = s.DB.Exec(`WITH RECURSIVE n(x) AS (VALUES(0) UNION ALL SELECT x+1 FROM n WHERE x<999999)
 INSERT INTO request_logs(ts,model,channel_id,api_key_id,app,status,error,prompt_tokens,completion_tokens,cache_tokens,latency_ms,ttfb_ms,cost,currency)
 SELECT strftime('%Y-%m-%d %H:00:00','now',printf('-%d hours',(x/5000)%24)),'model-'||(x%20),1,(x/20)%50+1,'app-'||((x/1000)%5),
 CASE WHEN x%50=0 THEN 500 ELSE 200 END,CASE WHEN x%70=0 THEN 'partial' ELSE '' END,
 10,5,x%3,(x%1000)+10,5,0.00004,CASE WHEN x%2=0 THEN 'USD' ELSE 'CNY' END FROM n`)
	if err != nil {
		b.Fatal(err)
	}
	var groups int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM request_usage_hour`).Scan(&groups); err != nil {
		b.Fatal(err)
	}
	b.Logf("seed one million: %s; hourly groups: %d", time.Since(start), groups)
	var expectedToday int64
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM request_logs WHERE ts>=datetime('now','start of day')`).Scan(&expectedToday); err != nil {
		b.Fatal(err)
	}
	b.Run("dashboard", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			d, err := s.Dashboard()
			if err != nil {
				b.Fatal(err)
			}
			if d.TodayRequests != expectedToday {
				b.Fatal(d.TodayRequests)
			}
		}
	})
	b.Run("trend", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_, sr, err := s.UsageTrend("model", "7d")
			if err != nil {
				b.Fatal(err)
			}
			var total int64
			for _, s := range sr {
				for _, p := range s.Points {
					total += p.Tokens
				}
			}
			if total != 15000000 {
				b.Fatal(total)
			}
		}
	})
}
