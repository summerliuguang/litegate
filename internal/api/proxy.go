package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"litegate/internal/store"
)

const maxBodyBytes = 32 << 20

type proxy struct {
	st     *store.Store
	client *http.Client
	cache  modelsCache
	pc     priceCache
}

// priceCache 缓存价格表 60s，避免每笔请求都查一次库（SQLite 是单连接串行化）。
type priceCache struct {
	mu      sync.Mutex
	prices  []store.ModelPrice
	expires time.Time
}

func newUpstreamClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   16,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 300 * time.Second, // LLM 首字节可能很慢，不做整体超时
		},
	}
}

// upstreamClientForTest 供管理面连通性测试复用同一套 Transport 配置。
var upstreamClientForTest = newUpstreamClient()

func (p *proxy) register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/chat/completions", p.serveOpenAI)
	mux.HandleFunc("POST /v1/embeddings", p.serveOpenAI)
	mux.HandleFunc("POST /v1/messages", p.serveAnthropic)
	mux.HandleFunc("GET /v1/models", p.serveModels)
}

// serveOpenAI 把 /v1 下的资源路径原样映射到 openai 渠道（base_url 需含版本前缀）。
func (p *proxy) serveOpenAI(w http.ResponseWriter, r *http.Request) {
	p.serve(w, r, "openai", strings.TrimPrefix(r.URL.Path, "/v1"))
}

func (p *proxy) serveAnthropic(w http.ResponseWriter, r *http.Request) {
	p.serve(w, r, "anthropic", strings.TrimPrefix(r.URL.Path, "/v1"))
}

// serve 是数据面主流程：鉴权 → 选渠道 → 带故障转移地转发 → 回写响应并记日志。
func (p *proxy) serve(w http.ResponseWriter, r *http.Request, protocol, upstreamPath string) {
	ak, err := p.authenticate(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid api key"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large or unreadable"})
		return
	}
	model := jsonModel(body)

	// openai 流式请求补 stream_options.include_usage 以获取 usage；上游不识别时回退重试
	attemptBody := body
	usageInjected := false
	if protocol == "openai" {
		if b, ok := injectStreamUsage(body); ok {
			attemptBody = b
			usageInjected = true
		}
	}

	chans, err := p.st.ListChannels(protocol)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	chans = filterByModel(chans, model)
	if len(chans) == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no enabled channel serves model: " + model})
		return
	}

	start := time.Now()
	attempts := orderCandidates(chans)
	if len(attempts) > 3 {
		attempts = attempts[:3]
	}

	var lastErr error
	i := 0
	for i < len(attempts) {
		c := &attempts[i]
		resp, err := p.attemptUpstream(r, c, upstreamPath, attemptBody)
		if err != nil {
			lastErr = fmt.Errorf("channel %q: %w", c.Name, err)
			i++
			continue
		}
		if usageInjected && resp.StatusCode == http.StatusBadRequest {
			// 上游不识别 stream_options.include_usage：去掉该字段对同一渠道重试一次
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			attemptBody = body
			usageInjected = false
			continue
		}
		if isRetryableStatus(resp.StatusCode) && i < len(attempts)-1 {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			lastErr = fmt.Errorf("channel %q: upstream status %d", c.Name, resp.StatusCode)
			i++
			continue
		}
		p.respond(w, ak, c, protocol, model, resp, start)
		return
	}

	status := http.StatusBadGateway
	msg := "all channels failed"
	if lastErr != nil {
		msg += ": " + lastErr.Error()
	}
	writeJSON(w, status, map[string]string{"error": msg})
	p.logRequest(ak, nil, protocol, model, status, time.Since(start), time.Since(start), tokenUsage{}, msg)
}

func (p *proxy) attemptUpstream(r *http.Request, c *store.Channel, path string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(r.Context(), r.Method, c.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	setUpstreamHeaders(req, r, c)
	return p.client.Do(req)
}

// respond 把上游响应回写给客户端；进入此函数后不再故障转移。
// 同时被动提取 usage：流式靠 sseUsageScanner 逐行嗅探，非流式保留响应体
// 末尾 64KB（usage 位于 JSON 尾部）等复制完成后再解析。
func (p *proxy) respond(w http.ResponseWriter, ak *store.APIKey, c *store.Channel, protocol, model string, resp *http.Response, start time.Time) {
	defer resp.Body.Close()
	ttfb := time.Since(start) // 上游返回响应头的耗时，近似上游首包延迟
	ct := resp.Header.Get("Content-Type")
	if ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)

	var u tokenUsage
	var err error
	if strings.HasPrefix(ct, "text/event-stream") {
		var scan sseUsageScanner
		_, err = streamCopy(w, resp.Body, &scan)
		u = scan.usage
	} else {
		tail := &tailBuffer{cap: maxUsageTail}
		_, err = io.Copy(w, io.TeeReader(resp.Body, tail))
		u, _ = usageFromJSON(tail.bytes())
	}
	errMsg := ""
	if err != nil {
		errMsg = err.Error()
	}
	p.logRequest(ak, c, protocol, model, resp.StatusCode, time.Since(start), ttfb, u, errMsg)
}

// authenticate 校验下游虚拟密钥：Authorization: Bearer 或 X-Api-Key（Claude Code 风格）。
func (p *proxy) authenticate(r *http.Request) (*store.APIKey, error) {
	h := r.Header.Get("Authorization")
	key := ""
	if strings.HasPrefix(h, "Bearer ") {
		key = strings.TrimSpace(h[len("Bearer "):])
	} else {
		key = r.Header.Get("X-Api-Key")
	}
	if key == "" {
		return nil, errors.New("missing api key")
	}
	return p.st.LookupAPIKey(key)
}

func setUpstreamHeaders(req *http.Request, inbound *http.Request, c *store.Channel) {
	ct := inbound.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Accept", inbound.Header.Get("Accept"))
	switch c.Type {
	case "anthropic":
		req.Header.Set("X-Api-Key", c.APIKey)
		v := inbound.Header.Get("Anthropic-Version")
		if v == "" {
			v = "2023-06-01"
		}
		req.Header.Set("Anthropic-Version", v)
	default:
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
}

func jsonModel(body []byte) string {
	var v struct {
		Model string `json:"model"`
	}
	// model 缺失时返回空串，由“无可用渠道”逻辑兜底
	_ = json.Unmarshal(body, &v)
	return v.Model
}

// filterByModel 保留声明了该模型的渠道；models 为空表示通配。
func filterByModel(chans []store.Channel, model string) []store.Channel {
	if model == "" {
		return chans
	}
	out := chans[:0:0]
	for _, c := range chans {
		if len(c.Models) == 0 {
			out = append(out, c)
			continue
		}
		for _, m := range c.Models {
			if m == model {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

// orderCandidates 最高优先级组内做无放回加权随机，其余渠道按优先级兜底。
func orderCandidates(chans []store.Channel) []store.Channel {
	maxPrio := chans[0].Priority
	for _, c := range chans {
		if c.Priority > maxPrio {
			maxPrio = c.Priority
		}
	}
	var top, rest []store.Channel
	for _, c := range chans {
		if c.Priority == maxPrio {
			top = append(top, c)
		} else {
			rest = append(rest, c)
		}
	}
	return append(weightedOrder(top), rest...)
}

func weightedOrder(items []store.Channel) []store.Channel {
	pool := append([]store.Channel(nil), items...)
	out := make([]store.Channel, 0, len(pool))
	for len(pool) > 0 {
		total := 0
		for _, c := range pool {
			total += max(c.Weight, 1)
		}
		x := rand.IntN(total)
		idx := 0
		for i, c := range pool {
			x -= max(c.Weight, 1)
			if x < 0 {
				idx = i
				break
			}
		}
		out = append(out, pool[idx])
		pool = append(pool[:idx], pool[idx+1:]...)
	}
	return out
}

// isRetryableStatus 判定可故障转移的状态码；401/400/404 等视为确定性失败直接透传。
func isRetryableStatus(code int) bool {
	return code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code >= 500
}

// streamCopy 边读边写边 Flush，保证 SSE 首字节延迟与断流传播；
// scan 非空时把透传的字节喂给 usage 嗅探器。
func streamCopy(w http.ResponseWriter, src io.Reader, scan *sseUsageScanner) (int64, error) {
	buf := make([]byte, 32*1024)
	var total int64
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return total, werr
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			if scan != nil {
				_, _ = scan.Write(buf[:n])
			}
			total += int64(n)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return total, nil
			}
			return total, err
		}
	}
}

// 尾部环形缓冲：非流式响应只保留最后 cap 字节用于解析 usage，
// 内存有上界，不随响应体大小增长。
const maxUsageTail = 64 << 10

type tailBuffer struct {
	buf []byte
	cap int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.cap {
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-t.cap:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) bytes() []byte { return t.buf }

func (p *proxy) logRequest(ak *store.APIKey, c *store.Channel, protocol, model string, status int, total, ttfb time.Duration, u tokenUsage, errMsg string) {
	if len(errMsg) > 512 {
		errMsg = errMsg[:512]
	}
	var price *store.ModelPrice
	if u.prompt > 0 || u.completion > 0 {
		price = p.lookupPrice(model)
	}
	l := &store.RequestLog{
		Model: model, Protocol: protocol, Status: status,
		LatencyMs: total.Milliseconds(), TtfbMs: ttfb.Milliseconds(),
		PromptTokens: u.prompt, CompletionTokens: u.completion,
		CostUSD: store.CostOf(price, u.prompt, u.completion),
		Error:   errMsg,
	}
	if ak != nil {
		l.APIKeyID = ak.ID
	}
	if c != nil {
		l.ChannelID = c.ID
	}
	if err := p.st.InsertRequestLog(l); err != nil {
		log.Printf("insert request log: %v", err)
	}
}

// lookupPrice 带 60s 缓存的价格匹配，规则见 matchPrice；查不到返回 nil（成本记 0）。
func (p *proxy) lookupPrice(model string) *store.ModelPrice {
	p.pc.mu.Lock()
	defer p.pc.mu.Unlock()
	if time.Now().After(p.pc.expires) {
		if prices, err := p.st.ListModelPrices(); err == nil {
			p.pc.prices = prices
			p.pc.expires = time.Now().Add(60 * time.Second)
		}
	}
	return matchPrice(p.pc.prices, model)
}
