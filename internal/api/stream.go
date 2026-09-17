package api

// 上游传输层：HTTP 客户端构造、带空闲看门狗的上游请求、SSE 流复制与
// 中断信号。与渠道路由/重试逻辑（proxy.go）分离。

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"litegate/internal/store"
)

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

// writeStreamErrorEvent 在 SSE 流中断后补发协议对应的错误终止事件再结束响应：
// Anthropic 协议有原生 error 事件；OpenAI 协议发 error 数据块 + [DONE]（主流
// 客户端能识别或安全忽略）。客户端已断开时写入静默失败，无副作用。
func writeStreamErrorEvent(w http.ResponseWriter, protocol string) {
	var evt string
	switch protocol {
	case "anthropic":
		evt = "event: error\ndata: " +
			`{"type":"error","error":{"type":"api_error","message":"upstream stream interrupted"}}` + "\n\n"
	case "openai":
		evt = "data: " +
			`{"error":{"message":"upstream stream interrupted","type":"api_error"}}` + "\n\ndata: [DONE]\n\n"
	default:
		return
	}
	if _, werr := io.WriteString(w, evt); werr == nil {
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
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

func (p *proxy) attemptUpstream(r *http.Request, c *store.Channel, key, path string, body []byte) (*http.Response, error) {
	upstream := c.BaseURL + path
	if r.URL.RawQuery != "" {
		// 客户端查询串原样透传（如 Azure 的 api-version、beta 开关）
		upstream += "?" + r.URL.RawQuery
	}
	// 看门狗派生自下游请求 context、挂在上游请求上：cancel 是 transport 正在
	// 监视的那把，触发时会中断阻塞中的 Body.Read（cancel 子 context 起不到
	// 这个效果，必须派生后传给 NewRequestWithContext）。
	reqCtx, cancel := context.WithCancel(r.Context())
	req, err := http.NewRequestWithContext(reqCtx, r.Method, upstream, bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, err
	}
	setUpstreamHeaders(req, r, c, key)
	resp, err := p.client.Do(req)
	if err != nil || resp == nil || resp.Body == nil {
		cancel()
		return resp, err
	}
	// cancel 交给 Body 的 Close 统一释放（幂等）；启用看门狗时再挂空闲计时器。
	var timer *time.Timer
	if p.idleTimeout > 0 {
		timer = time.AfterFunc(p.idleTimeout, cancel)
	}
	resp.Body = &watchdogBody{ReadCloser: resp.Body, timer: timer, cancel: cancel, idle: p.idleTimeout}
	return resp, nil
}

// upstreamIdleTimeout 读 LITEGATE_STREAM_IDLE_TIMEOUT（单位秒）；默认 0 =
// 关闭看门狗（上游挂起时依赖下游客户端断开来传播中断）。需要防挂起的部署
// 显式设正数秒启用。
func upstreamIdleTimeout() time.Duration {
	if s := os.Getenv("LITEGATE_STREAM_IDLE_TIMEOUT"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 0
}

// watchdogBody 每次读到字节就重置空闲计时器；连续 idle 无进展（上游挂起/
// 网络黑洞）时 cancel 请求上下文中断连接，避免 Read 永久阻塞占住连接与
// goroutine。默认关闭（idle=0 时 timer 为 nil），仅显式配置后生效。
type watchdogBody struct {
	io.ReadCloser
	timer  *time.Timer
	cancel context.CancelFunc
	idle   time.Duration
}

func (b *watchdogBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 && b.timer != nil {
		b.timer.Reset(b.idle)
	}
	if err != nil && b.timer != nil {
		b.timer.Stop()
	}
	return n, err
}

func (b *watchdogBody) Close() error {
	if b.timer != nil {
		b.timer.Stop()
	}
	b.cancel() // 释放派生 context 的资源（幂等）
	return b.ReadCloser.Close()
}

// respond 把上游响应回写给客户端；进入此函数后不再故障转移。
// 同时被动提取 usage：流式靠 sseUsageScanner 逐行嗅探，非流式保留响应体
// 末尾 64KB（usage 位于 JSON 尾部）等复制完成后再解析。
// reqBody 非空时按采样配置留存正文（回放排障用）。

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
