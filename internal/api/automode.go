package api

// 虚拟密钥的 auto 路由：客户端请求 model="auto" 时，网关按密钥配置的策略从候选池
// 解析出真实模型，再进入常规渠道路由（渠道选择/限流/预算/日志/计费均作用于解析后的
// 模型）。候选池 = auto_models（空则跟随 allowed_models，再空则当前协议下全部启用
// 模型）∩ 有启用渠道服务的模型，且始终服从 allowed_models 白名单。

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"litegate/internal/store"
)

const autoModelName = "auto"

// autoModes 是合法的 auto 路由模式（管理面校验与数据面判定共用）。
var autoModes = map[string]bool{"latency": true, "balance": true, "priority": true, "smart": true}

// autoOrdered 返回 auto 路由的候选模型序列（已按模式排好序）：
//   - priority 模式返回完整序列，数据面据此做跨模型故障转移（首选渠道全部失败
//     自动换次选）；
//   - 其他模式返回单元素序列（解析即最终选择）。
//
// 返回值 status/errMsg 非 0/非空时表示解析失败，应直接回给下游。
func (p *proxy) autoOrdered(ctx context.Context, ak *store.APIKey, protocol string, chans []store.Channel, body []byte) ([]string, int, string) {
	if !autoModes[ak.AutoMode] {
		return nil, 400, "model 'auto' requires auto routing configured on this api key"
	}
	cands := p.autoCandidates(ctx, ak, protocol, chans)
	if len(cands) == 0 {
		return nil, 404, "auto routing: no enabled candidate model for this api key"
	}
	p.metrics.observeAuto(ak.AutoMode)
	switch ak.AutoMode {
	case "latency":
		return []string{p.pickByLatency(cands)}, 0, ""
	case "balance":
		return []string{p.pickRoundRobin(ak.ID, cands)}, 0, ""
	case "priority":
		return orderedByPriority(ak.AutoPriority, cands), 0, ""
	case "smart":
		return []string{p.pickSmart(body, cands)}, 0, ""
	}
	return cands, 0, ""
}

// autoCandidates 计算当前可用的候选池：配置池 ∩ (白名单 ∩ 有渠道服务的模型)。
// 保持配置池的声明顺序（balance 轮转与 priority 排序都依赖稳定顺序）。
func (p *proxy) autoCandidates(ctx context.Context, ak *store.APIKey, protocol string, chans []store.Channel) []string {
	pool := ak.AutoModels
	if len(pool) == 0 {
		pool = ak.AllowedModels
	}
	if len(pool) == 0 {
		// 未配置任何池：全部启用模型即候选，白名单必然放行
		return p.servedModels(ctx, protocol, chans)
	}
	served := map[string]bool{}
	for _, m := range p.servedModels(ctx, protocol, chans) {
		served[m] = true
	}
	out := make([]string, 0, len(pool))
	for _, m := range pool {
		if ak.AllowsModel(m) && served[m] {
			out = append(out, m)
		}
	}
	return out
}

// servedModels 列出当前协议下有启用渠道服务的模型：显式列表渠道取配置列表剔除
// 禁用项；通配渠道的模型集合来自聚合缓存（openai 协议，60s 缓存），anthropic
// 渠道无标准模型列表端点，通配时贡献为空（客户端显式配置 auto_models 即可）。
func (p *proxy) servedModels(ctx context.Context, protocol string, chans []store.Channel) []string {
	seen := map[string]bool{}
	var out []string
	add := func(m string, disabled []string) {
		if m == "" || seen[m] || containsModelStr(disabled, m) {
			return
		}
		seen[m] = true
		out = append(out, m)
	}
	for i := range chans {
		c := &chans[i]
		if len(c.Models) > 0 {
			for _, m := range c.Models {
				add(m, c.DisabledModels)
			}
			continue
		}
		if protocol != "openai" {
			continue
		}
		for _, it := range p.aggregateModels(ctx) {
			add(it.ID, c.DisabledModels)
		}
	}
	return out
}

// pickByLatency 选近 24h 实测平均延迟最低的候选；无样本时取声明顺序第一个。
func (p *proxy) pickByLatency(cands []string) string {
	lat := p.modelLatencies()
	best, bestMs := "", int64(-1)
	for _, m := range cands {
		if v, ok := lat[m]; ok && (bestMs < 0 || v < bestMs) {
			best, bestMs = m, v
		}
	}
	if best == "" {
		return cands[0]
	}
	return best
}

// pickRoundRobin balance 模式：每密钥一个轮转计数器，在候选池均匀分摊。
func (p *proxy) pickRoundRobin(keyID int64, cands []string) string {
	v, _ := p.rr.LoadOrStore(keyID, new(atomic.Uint64))
	n := v.(*atomic.Uint64).Add(1)
	return cands[(n-1)%uint64(len(cands))]
}

// orderedByPriority 按配置顺序排列候选；未在优先级列表中的候选按原顺序追加在末尾。
func orderedByPriority(prefs, cands []string) []string {
	out := make([]string, 0, len(cands))
	in := map[string]bool{}
	for _, m := range cands {
		in[m] = true
	}
	seen := map[string]bool{}
	for _, m := range prefs {
		if in[m] && !seen[m] {
			out = append(out, m)
			seen[m] = true
		}
	}
	for _, m := range cands {
		if !seen[m] {
			out = append(out, m)
		}
	}
	return out
}

// ---- smart 模式：按任务特征（图片/工具/输入规模）选能力档，档内取实测最快 ----

type taskSignal struct {
	images bool
	tools  bool
	chars  int // 全部文本内容的字节量（中文约 3 字节/字，阈值按字节计）
}

// analyzeTask 从 chat 请求体提取任务特征。content 兼容字符串与分块数组两种形态。
func analyzeTask(body []byte) taskSignal {
	var v struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Tools []json.RawMessage `json:"tools"`
	}
	_ = json.Unmarshal(body, &v)
	sig := taskSignal{tools: len(v.Tools) > 0}
	for _, m := range v.Messages {
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			sig.chars += len(s)
			continue
		}
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(m.Content, &parts) == nil {
			for _, pt := range parts {
				switch pt.Type {
				case "text":
					sig.chars += len(pt.Text)
				case "image_url", "input_image":
					sig.images = true
				}
			}
		}
	}
	return sig
}

// modelTier 按名字把模型分档：3 强（pro/max/large/ultra/大参数）、1 快
// （flash/mini/lite/small/nano/free/tiny）、2 标准。启发式，覆盖不到的归标准档。
func modelTier(name string) int {
	n := strings.ToLower(name)
	for _, k := range []string{"pro", "max", "large", "ultra", "120b", "405b", "opus"} {
		if strings.Contains(n, k) {
			return 3
		}
	}
	for _, k := range []string{"flash", "mini", "lite", "small", "nano", "free", "tiny"} {
		if strings.Contains(n, k) {
			return 1
		}
	}
	return 2
}

// visionCapable 按名字判断模型是否具备图像理解（与对话测试页 kindOf 同一套启发式）。
func visionCapable(name string) bool {
	n := strings.ToLower(name)
	return strings.Contains(n, "vision") || strings.Contains(n, "omni") || n == "deepseek-flash"
}

// pickSmart 任务智能分配：含图片 → 视觉模型；带工具或长输入 → 强档；短输入 → 快档；
// 目标档无候选时按 强→标准→快（简单任务反向）就近回退，档内取实测延迟最低者。
func (p *proxy) pickSmart(body []byte, cands []string) string {
	sig := analyzeTask(body)
	pool := cands
	if sig.images {
		if vp := filterModels(pool, visionCapable); len(vp) > 0 {
			pool = vp // 无视觉候选时保持原池，让上游给出明确报错
		}
	}
	want := 2
	if sig.tools || sig.chars > 6000 {
		want = 3
	} else if sig.chars <= 1500 {
		want = 1
	}
	for _, t := range tierFallback(want) {
		if tier := filterModels(pool, func(m string) bool { return modelTier(m) == t }); len(tier) > 0 {
			return p.pickByLatency(tier)
		}
	}
	return p.pickByLatency(pool)
}

// tierFallback 目标档缺候选时的回退顺序：复杂任务从高档往低退，简单任务反之。
func tierFallback(want int) []int {
	switch want {
	case 3:
		return []int{3, 2, 1}
	case 1:
		return []int{1, 2, 3}
	default:
		return []int{2, 3, 1}
	}
}

func filterModels(list []string, keep func(string) bool) []string {
	out := make([]string, 0, len(list))
	for _, m := range list {
		if keep(m) {
			out = append(out, m)
		}
	}
	return out
}

// ---- proxy 上的 auto 路由状态（延迟统计缓存 + 轮转计数器） ----

// modelLatencyCache 缓存近 24h 各模型平均延迟 60 秒，避免每笔请求聚合一次日志表。
type modelLatencyCache struct {
	mu      sync.Mutex
	stats   map[string]int64
	expires time.Time
}

func (p *proxy) modelLatencies() map[string]int64 {
	p.lat.mu.Lock()
	defer p.lat.mu.Unlock()
	if p.lat.stats != nil && time.Now().Before(p.lat.expires) {
		return p.lat.stats
	}
	stats, err := p.st.ModelLatencyStats()
	if err != nil || len(stats) == 0 {
		if p.lat.stats != nil {
			return p.lat.stats // 查询失败沿用旧值
		}
		return map[string]int64{}
	}
	p.lat.stats = stats
	p.lat.expires = time.Now().Add(60 * time.Second)
	return stats
}
