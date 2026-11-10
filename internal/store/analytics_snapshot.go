package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Every dashboard/trend response reads one consistent snapshot. Cancellation and
// a finite lifetime prevent abandoned/high-cardinality queries pinning WAL forever.
const analyticsTimeout = 10 * time.Second

type queryer interface {
	Query(string, ...any) (*sql.Rows, error)
	QueryRow(string, ...any) *sql.Row
}
type analyticsSnapshot struct {
	tx  *sql.Tx
	ctx context.Context
}

func (r analyticsSnapshot) Query(q string, args ...any) (*sql.Rows, error) {
	return r.tx.QueryContext(r.ctx, q, args...)
}
func (r analyticsSnapshot) QueryRow(q string, args ...any) *sql.Row {
	return r.tx.QueryRowContext(r.ctx, q, args...)
}

func (s *Store) Dashboard() (*Dashboard, error) { return s.DashboardContext(context.Background()) }
func (s *Store) DashboardContext(parent context.Context) (*Dashboard, error) {
	ctx, cancel := context.WithTimeout(parent, analyticsTimeout)
	defer cancel()
	tx, err := s.analyticsDB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	d, err := s.dashboard(analyticsSnapshot{tx, ctx}, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return d, nil
}
func (s *Store) UsageTrend(dim, rng string) (string, []TrendSeries, error) {
	return s.UsageTrendContext(context.Background(), dim, rng)
}
func (s *Store) UsageTrendContext(parent context.Context, dim, rng string) (string, []TrendSeries, error) {
	ctx, cancel := context.WithTimeout(parent, analyticsTimeout)
	defer cancel()
	tx, err := s.analyticsDB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return "", nil, err
	}
	defer tx.Rollback()
	bucket, series, err := s.usageTrend(analyticsSnapshot{tx, ctx}, time.Now().UTC(), dim, rng)
	if err != nil {
		return "", nil, err
	}
	if err := tx.Commit(); err != nil {
		return "", nil, err
	}
	return bucket, series, nil
}

func utcDayStart(now time.Time) time.Time {
	now = now.UTC()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
}

// Only static internal column names and time.Time values enter this SQL fragment.
func analyticsRange(column string, from, until time.Time) string {
	return fmt.Sprintf("%s >= '%s' AND %s < '%s'", column, from.UTC().Format("2006-01-02 15:04:05"), column, until.UTC().Format("2006-01-02 15:04:05"))
}
