package api

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"testing"

	"litegate/internal/store"
)

func readLogSummaries(t *testing.T, st *store.Store, query string) []store.LogSummary {
	t.Helper()
	w := httptest.NewRecorder()
	(&admin{st: st}).listLogSummaries(w, httptest.NewRequest("GET", "/api/admin/logs/summary"+query, nil))
	if w.Code != 200 {
		t.Fatalf("summary status=%d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Items []store.LogSummary `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Items
}

func TestLogSummaryLegacyAndSnapshotCurrencies(t *testing.T) {
	for _, tc := range []struct {
		name, price, currency string
		rows                  []string
		costs                 map[string]float64
		cost                  float64
	}{
		{"same-usd", "USD", "USD", []string{"", "USD"}, map[string]float64{"USD": 2}, 2},
		{"same-cny-prefix-price", "CNY", "CNY", []string{"", "CNY"}, map[string]float64{"CNY": 2}, 2},
		{"mixed-keeps-snapshot", "CNY", "", []string{"", "CNY", "USD"}, map[string]float64{"CNY": 2, "USD": 1}, 0},
		{"snapshot-overrides-price", "CNY", "USD", []string{"USD", "USD"}, map[string]float64{"USD": 2}, 2},
		{"missing-price-default", "", "USD", []string{"", "USD"}, map[string]float64{"USD": 2}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, st := newTestServer(t)
			if tc.price != "" {
				if err := st.UpsertModelPrice(&store.ModelPrice{Model: "review", Currency: tc.price}); err != nil {
					t.Fatal(err)
				}
			}
			for _, currency := range tc.rows {
				if err := st.InsertRequestLog(&store.RequestLog{Model: "review-model", Currency: currency, Status: 200,
					PromptTokens: 10, CompletionTokens: 4, CacheTokens: 2, CostUSD: 1, Error: "interrupted"}); err != nil {
					t.Fatal(err)
				}
			}
			items := readLogSummaries(t, st, "")
			if len(items) != 1 {
				t.Fatalf("expected one complete model card, got %+v", items)
			}
			got, n := items[0], int64(len(tc.rows))
			if got.Requests != n || got.Errors != n || got.PromptTokens != 10*n || got.CompletionTokens != 4*n || got.CacheTokens != 2*n {
				t.Fatalf("lost counters: %+v", got)
			}
			if got.Currency != tc.currency || got.Cost != tc.cost || !reflect.DeepEqual(got.CostByCurrency, tc.costs) {
				t.Fatalf("incorrect currency totals: %+v", got)
			}
		})
	}
}

func TestLogSummaryFiltersOrderingAndEmpty(t *testing.T) {
	_, st := newTestServer(t)
	if got := readLogSummaries(t, st, ""); got == nil || len(got) != 0 {
		t.Fatalf("empty response should contain an empty array: %+v", got)
	}
	for _, row := range []struct{ model, currency, app, ts string }{
		{"first", "", "keep", "2026-11-08 10:00:00"},
		{"second", "USD", "keep", "2026-11-09 10:00:00"},
		{"first", "USD", "keep", "2026-11-10 10:00:00"},
		{"excluded", "USD", "other", "2026-11-11 10:00:00"},
	} {
		log := &store.RequestLog{Model: row.model, Currency: row.currency, App: row.app, Status: 200, CostUSD: 0.000001}
		if err := st.InsertRequestLog(log); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB.Exec(`UPDATE request_logs SET ts=? WHERE id=?`, row.ts, log.ID); err != nil {
			t.Fatal(err)
		}
	}
	items := readLogSummaries(t, st, "?app=keep")
	if len(items) != 2 || items[0].Model != "first" || items[1].Model != "second" || items[0].LastTs != "2026-11-10 10:00:00" || items[0].Requests != 2 || items[0].Cost != 0.000002 {
		t.Fatalf("filter/order/cost mismatch: %+v", items)
	}
	items = readLogSummaries(t, st, "?app=keep&since=2026-11-09")
	if len(items) != 2 || items[0].Requests != 1 || items[0].Cost != 0.000001 {
		t.Fatalf("time filter mismatch: %+v", items)
	}
}
