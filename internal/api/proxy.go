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
	"time"

	"litegate/internal/store"
)

const maxBodyBytes = 32 << 20

type proxy struct {
	st     *store.Store
	client *http.Client
	cache  modelsCache
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
	for i := range attempts {
		c := &attempts[i]
		resp, err := p.attemptUpstream(r, c, upstreamPath, body)
		if err != nil {
			lastErr = fmt.Errorf("channel %q: %w", c.Name, err)
			continue
		}
		if isRetryableStatus(resp.StatusCode) && i < len(attempts)-1 {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			lastErr = fmt.Errorf("channel %q: upstream status %d", c.Name, resp.StatusCode)
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
	p.logRequest(ak, nil, protocol, model, status, time.Since(start), msg)
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
func (p *proxy) respond(w http.ResponseWriter, ak *store.APIKey, c *store.Channel, protocol, model string, resp *http.Response, start time.Time) {
	defer resp.Body.Close()
	ct := resp.Header.Get("Content-Type")
	if ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)

	var err error
	if strings.HasPrefix(ct, "text/event-stream") {
		_, err = streamCopy(w, resp.Body)
	} else {
		_, err = io.Copy(w, resp.Body)
	}
	errMsg := ""
	if err != nil {
		errMsg = err.Error()
	}
	p.logRequest(ak, c, protocol, model, resp.StatusCode, time.Since(start), errMsg)
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

// streamCopy 边读边写边 Flush，保证 SSE 首字节延迟与断流传播。
func streamCopy(w http.ResponseWriter, src io.Reader) (int64, error) {
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

func (p *proxy) logRequest(ak *store.APIKey, c *store.Channel, protocol, model string, status int, d time.Duration, errMsg string) {
	if len(errMsg) > 512 {
		errMsg = errMsg[:512]
	}
	l := &store.RequestLog{
		Model: model, Protocol: protocol, Status: status,
		LatencyMs: d.Milliseconds(), Error: errMsg,
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
