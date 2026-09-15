package api

// 轻量 Prometheus 文本指标：手写 text 格式，不引入 client_golang 依赖。
// 计数在请求完成（logRequest / autoOrdered）时累加到内存，/metrics 时输出。
// 进程生命周期内的累计值（重启归零）；历史趋势以请求日志表为准。

import (
	"fmt"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type reqKey struct {
	model, protocol, code string
}

type metricsState struct {
	mu            sync.Mutex
	requests      map[reqKey]int64
	latencySumMs  map[reqKey]int64
	tokPrompt     map[string]int64 // by model
	tokCompletion map[string]int64
	tokCache      map[string]int64
	costByCur     map[string]float64
	autoByMode    map[string]int64
}

func newMetricsState() *metricsState {
	return &metricsState{
		requests:      map[reqKey]int64{},
		latencySumMs:  map[reqKey]int64{},
		tokPrompt:     map[string]int64{},
		tokCompletion: map[string]int64{},
		tokCache:      map[string]int64{},
		costByCur:     map[string]float64{},
		autoByMode:    map[string]int64{},
	}
}

// observe 在每笔请求落日志后累计指标。
func (m *metricsState) observe(model, protocol string, code int, latencyMs, prompt, completion, cache int64, cost float64, currency string) {
	if m == nil {
		return
	}
	k := reqKey{model: model, protocol: protocol, code: strconv.Itoa(code)}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[k]++
	m.latencySumMs[k] += latencyMs
	if model != "" {
		m.tokPrompt[model] += prompt
		m.tokCompletion[model] += completion
		m.tokCache[model] += cache
	}
	if cost > 0 {
		m.costByCur[currency] += cost
	}
}

func (m *metricsState) observeAuto(mode string) {
	if m == nil || mode == "" {
		return
	}
	m.mu.Lock()
	m.autoByMode[mode]++
	m.mu.Unlock()
}

// serveMetrics 输出 Prometheus text 格式。访问控制在 server.go 的包装 handler：
// 本机直连放行，经 nginx 等反代（带 X-Real-IP/X-Forwarded-For）要求管理令牌。
func (p *proxy) serveMetrics(w http.ResponseWriter, _ *http.Request) {
	var b strings.Builder
	wr := func(s string) { b.WriteString(s) }

	wr("# HELP litegate_requests_total Requests proxied by model/protocol/status.\n# TYPE litegate_requests_total counter\n")
	p.metrics.mu.Lock()
	keys := make([]reqKey, 0, len(p.metrics.requests))
	total := int64(0)
	for k, v := range p.metrics.requests {
		keys = append(keys, k)
		total += v
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].model != keys[j].model {
			return keys[i].model < keys[j].model
		}
		if keys[i].protocol != keys[j].protocol {
			return keys[i].protocol < keys[j].protocol
		}
		return keys[i].code < keys[j].code
	})
	for _, k := range keys {
		wr(fmt.Sprintf("litegate_requests_total{model=%q,protocol=%q,code=%q} %d\n",
			k.model, k.protocol, k.code, p.metrics.requests[k]))
		wr(fmt.Sprintf("litegate_request_latency_seconds_sum{model=%q,protocol=%q,code=%q} %.3f\n",
			k.model, k.protocol, k.code, float64(p.metrics.latencySumMs[k])/1000))
	}
	wr("# HELP litegate_requests_total_all Total requests across all models.\n# TYPE litegate_requests_total_all counter\n")
	wr(fmt.Sprintf("litegate_requests_total_all %d\n", total))

	models := map[string]bool{}
	for k := range p.metrics.requests {
		if k.model != "" {
			models[k.model] = true
		}
	}
	for m := range p.metrics.tokPrompt {
		models[m] = true
	}
	names := make([]string, 0, len(models))
	for m := range models {
		names = append(names, m)
	}
	sort.Strings(names)

	wr("# HELP litegate_tokens_total Tokens by kind and model.\n# TYPE litegate_tokens_total counter\n")
	for _, m := range names {
		wr(fmt.Sprintf("litegate_tokens_total{model=%q,kind=%q} %d\n", m, "prompt", p.metrics.tokPrompt[m]))
		wr(fmt.Sprintf("litegate_tokens_total{model=%q,kind=%q} %d\n", m, "completion", p.metrics.tokCompletion[m]))
		if c := p.metrics.tokCache[m]; c > 0 {
			wr(fmt.Sprintf("litegate_tokens_total{model=%q,kind=%q} %d\n", m, "cache_read", c))
		}
	}

	curs := make([]string, 0, len(p.metrics.costByCur))
	for c := range p.metrics.costByCur {
		curs = append(curs, c)
	}
	sort.Strings(curs)
	wr("# HELP litegate_cost_total Accrued cost by currency (per price-table currency, not summed).\n# TYPE litegate_cost_total counter\n")
	for _, c := range curs {
		wr(fmt.Sprintf("litegate_cost_total{currency=%q} %.6f\n", c, p.metrics.costByCur[c]))
	}

	wr("# HELP litegate_auto_route_total Auto-route resolutions by mode.\n# TYPE litegate_auto_route_total counter\n")
	modes := make([]string, 0, len(p.metrics.autoByMode))
	for m := range p.metrics.autoByMode {
		modes = append(modes, m)
	}
	sort.Strings(modes)
	for _, m := range modes {
		wr(fmt.Sprintf("litegate_auto_route_total{mode=%q} %d\n", m, p.metrics.autoByMode[m]))
	}
	p.metrics.mu.Unlock()

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	wr("# HELP litegate_go_goroutines Current goroutines.\n# TYPE litegate_go_goroutines gauge\n")
	wr(fmt.Sprintf("litegate_go_goroutines %d\n", runtime.NumGoroutine()))
	wr("# HELP litegate_go_heap_alloc_bytes Heap allocated bytes.\n# TYPE litegate_go_heap_alloc_bytes gauge\n")
	wr(fmt.Sprintf("litegate_go_heap_alloc_bytes %d\n", ms.HeapAlloc))
	wr("# HELP litegate_uptime_seconds Process uptime.\n# TYPE litegate_uptime_seconds gauge\n")
	wr(fmt.Sprintf("litegate_uptime_seconds %d\n", int64(time.Since(startedAt).Seconds())))
	wr("# HELP litegate_keys_cooling Upstream channel keys currently in cooldown.\n# TYPE litegate_keys_cooling gauge\n")
	wr(fmt.Sprintf("litegate_keys_cooling %d\n", p.keys.coolingCount()))

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}
