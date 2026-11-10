package store

import (
	"database/sql"
	"fmt"
	"strings"
)

// These materialized aggregates are maintained in the same SQLite transaction as
// their source log. No TTL, goroutine, second database, or loss-prone write queue.
// Keeping the original currency (including legacy "") preserves price fallback.
const analyticsSchema = `
CREATE TABLE IF NOT EXISTS analytics_meta (version INTEGER PRIMARY KEY);
CREATE TABLE IF NOT EXISTS request_usage_hour (
 ts TEXT NOT NULL, model TEXT NOT NULL, channel_id INTEGER NOT NULL,
 api_key_id INTEGER NOT NULL, app TEXT NOT NULL, currency TEXT NOT NULL,
 requests INTEGER NOT NULL, errors INTEGER NOT NULL, failed_requests INTEGER NOT NULL,
 prompt_tokens INTEGER NOT NULL, completion_tokens INTEGER NOT NULL,
 cache_tokens INTEGER NOT NULL, latency_ms INTEGER NOT NULL, cost REAL NOT NULL,
 PRIMARY KEY(ts,model,channel_id,api_key_id,app,currency)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS request_latency_day (
 ts TEXT NOT NULL, latency_ms INTEGER NOT NULL, requests INTEGER NOT NULL,
 gen_ms INTEGER NOT NULL, gen_tokens INTEGER NOT NULL,
 PRIMARY KEY(ts,latency_ms)
) WITHOUT ROWID;
`

func migrateAnalytics(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(analyticsSchema); err != nil {
		return err
	}
	var initialized int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM analytics_meta WHERE version=1`).Scan(&initialized); err != nil {
		return err
	}
	if initialized == 0 {
		// One atomic backfill before installing triggers; restart cannot double-count.
		if _, err = tx.Exec(`DELETE FROM request_usage_hour; DELETE FROM request_latency_day;
 INSERT INTO request_usage_hour
 SELECT substr(ts,1,13)||':00:00',model,channel_id,api_key_id,app,currency,
 COUNT(*),SUM(status>=400),SUM(status>=400 OR error!=''),SUM(prompt_tokens),SUM(completion_tokens),SUM(cache_tokens),SUM(latency_ms),SUM(cost)
 FROM request_logs GROUP BY 1,2,3,4,5,6;
 INSERT INTO request_latency_day
 SELECT substr(ts,1,10)||' 00:00:00',latency_ms,COUNT(*),
 SUM(CASE WHEN completion_tokens>0 AND latency_ms>ttfb_ms THEN latency_ms-ttfb_ms ELSE 0 END),
 SUM(CASE WHEN completion_tokens>0 AND latency_ms>ttfb_ms THEN completion_tokens ELSE 0 END)
 FROM request_logs WHERE status<400 AND error='' GROUP BY 1,2;
 INSERT INTO analytics_meta(version) VALUES(1);`); err != nil {
			return fmt.Errorf("backfill analytics: %w", err)
		}
	}
	for _, event := range []string{"INSERT", "UPDATE", "DELETE"} {
		var body string
		if event != "INSERT" {
			body += analyticsDelta("OLD", -1)
		}
		if event != "DELETE" {
			body += analyticsDelta("NEW", 1)
		}
		if _, err = tx.Exec(`CREATE TRIGGER IF NOT EXISTS analytics_logs_` + strings.ToLower(event) + ` AFTER ` + event + ` ON request_logs BEGIN ` + body + ` END`); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Both UPDATE images are accounted for, including timestamp, dimension, currency,
// error status and latency changes. Retention DELETEs remove the same aggregates.
func analyticsDelta(row string, sign int) string {
	n := row + "."
	hour := "substr(" + n + "ts,1,13)||':00:00'"
	day := "substr(" + n + "ts,1,10)||' 00:00:00'"
	key := fmt.Sprintf("ts=%s AND model=%smodel AND channel_id=%schannel_id AND api_key_id=%sapi_key_id AND app=%sapp AND currency=%scurrency", hour, n, n, n, n, n)
	metrics := []string{"requests", "errors", "failed_requests", "prompt_tokens", "completion_tokens", "cache_tokens", "latency_ms", "cost"}
	var assignments []string
	for _, col := range metrics {
		assignments = append(assignments, col+"="+col+"+excluded."+col)
	}
	sql := fmt.Sprintf(`INSERT INTO request_usage_hour VALUES(%s,%smodel,%schannel_id,%sapi_key_id,%sapp,%scurrency,
 %d,%d*(%sstatus>=400),%d*(%sstatus>=400 OR %serror!=''),%d*%sprompt_tokens,%d*%scompletion_tokens,%d*%scache_tokens,%d*%slatency_ms,%d*%scost)
 ON CONFLICT(ts,model,channel_id,api_key_id,app,currency) DO UPDATE SET %s;
 INSERT INTO request_latency_day
 SELECT %s,%slatency_ms,%d,
 %d*CASE WHEN %scompletion_tokens>0 AND %slatency_ms>%sttfb_ms THEN %slatency_ms-%sttfb_ms ELSE 0 END,
 %d*CASE WHEN %scompletion_tokens>0 AND %slatency_ms>%sttfb_ms THEN %scompletion_tokens ELSE 0 END
 WHERE %sstatus<400 AND %serror=''
 ON CONFLICT(ts,latency_ms) DO UPDATE SET requests=requests+excluded.requests,gen_ms=gen_ms+excluded.gen_ms,gen_tokens=gen_tokens+excluded.gen_tokens;
 `, hour, n, n, n, n, n, sign, sign, n, sign, n, n, sign, n, sign, n, sign, n, sign, n, sign, n, strings.Join(assignments, ","),
		day, n, sign, sign, n, n, n, n, n, sign, n, n, n, n, n, n)
	if sign < 0 {
		sql += `DELETE FROM request_usage_hour WHERE ` + key + ` AND requests=0;
 DELETE FROM request_latency_day WHERE ts=` + day + ` AND latency_ms=` + n + `latency_ms AND requests=0;`
	}
	return sql
}
