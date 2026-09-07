package api

import (
	"bytes"
	"encoding/json"
	"strings"

	"litegate/internal/store"
)

// tokenUsage 是一次请求的 token 消耗；prompt 为总输入（含缓存命中/写入）。
// cacheRead/cacheWrite 用于按缓存价折算成本（读 1/10 价、写 1.25 倍价）。
type tokenUsage struct {
	prompt     int64
	completion int64
	cacheRead  int64
	cacheWrite int64
}

// usageFields 兼容 openai（prompt_tokens/completion_tokens/prompt_tokens_details）
// 与 anthropic（input_tokens/output_tokens/cache_*_input_tokens）两套字段名。
type usageFields struct {
	PromptTokens     *int64 `json:"prompt_tokens"`
	CompletionTokens *int64 `json:"completion_tokens"`
	InputTokens      *int64 `json:"input_tokens"`
	OutputTokens     *int64 `json:"output_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CacheReadTokens    *int64 `json:"cache_read_input_tokens"`
	CacheCreateTokens  *int64 `json:"cache_creation_input_tokens"`
}

// values 归一化两种口径：openai 的 prompt_tokens 本身就是总输入；anthropic 的
// input_tokens 不含缓存部分，这里加总成总输入，让成本公式只有一套。
func (u *usageFields) values() (t tokenUsage) {
	t.prompt = valueOr(u.PromptTokens, u.InputTokens)
	t.completion = valueOr(u.CompletionTokens, u.OutputTokens)
	if u.PromptTokensDetails != nil {
		t.cacheRead = u.PromptTokensDetails.CachedTokens
	}
	if t.cacheRead == 0 {
		t.cacheRead = valueOr(u.CacheReadTokens, nil)
	}
	t.cacheWrite = valueOr(u.CacheCreateTokens, nil)
	if u.PromptTokens == nil && u.InputTokens != nil {
		t.prompt += t.cacheRead + t.cacheWrite
	}
	return t
}

func valueOr(a, b *int64) int64 {
	if a != nil {
		return *a
	}
	if b != nil {
		return *b
	}
	return 0
}

// usageFromJSON 从一段 JSON（完整响应体或单条 SSE data 行）里提取 usage。
// 覆盖三种形态：openai 响应/末尾 chunk 的 usage、anthropic 的 usage、
// anthropic message_start 事件里嵌在 message 下的 usage。
func usageFromJSON(b []byte) (tokenUsage, bool) {
	var v struct {
		Usage   *usageFields `json:"usage"`
		Message *struct {
			Usage *usageFields `json:"usage"`
		} `json:"message"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return tokenUsage{}, false
	}
	var u tokenUsage
	switch {
	case v.Usage != nil:
		u = v.Usage.values()
	case v.Message != nil && v.Message.Usage != nil:
		u = v.Message.Usage.values()
	default:
		return tokenUsage{}, false
	}
	if u.prompt <= 0 && u.completion <= 0 {
		return tokenUsage{}, false
	}
	return u, true
}

// sseUsageScanner 在透传 SSE 字节流的同时被动嗅探 usage：不缓冲整条事件、
// 不改写任何字节，对首包延迟零影响。openai 流式仅末尾 chunk 带 usage，
// anthropic 的 message_start/message_delta 各带一半，故按「prompt 覆盖、completion 取大」合并。
type sseUsageScanner struct {
	carry []byte // 跨 Write 的半行残留
	usage tokenUsage
	found bool
}

func (s *sseUsageScanner) Write(p []byte) (int, error) {
	data := p
	if len(s.carry) > 0 {
		data = append(s.carry, p...)
		s.carry = s.carry[:0]
	}
	start := 0
	for {
		idx := bytes.IndexByte(data[start:], '\n')
		if idx < 0 {
			break
		}
		s.scanLine(data[start : start+idx])
		start += idx + 1
	}
	s.carry = append(s.carry, data[start:]...)
	return len(p), nil
}

func (s *sseUsageScanner) scanLine(line []byte) {
	if !bytes.Contains(line, []byte("usage")) {
		return
	}
	b := bytes.TrimSpace(line)
	b = bytes.TrimPrefix(b, []byte("data:"))
	b = bytes.TrimSpace(b)
	if len(b) == 0 || bytes.Equal(b, []byte("[DONE]")) {
		return
	}
	if u, ok := usageFromJSON(b); ok {
		if u.prompt > 0 {
			s.usage.prompt = u.prompt
			s.usage.cacheRead = u.cacheRead
			s.usage.cacheWrite = u.cacheWrite
		}
		if u.completion > s.usage.completion {
			s.usage.completion = u.completion
		}
		s.found = true
	}
}

// injectStreamUsage 给 openai 协议的流式请求补 stream_options.include_usage，
// 否则 OpenAI 官方接口不在流式响应里报告 usage。客户端已自带 stream_options、
// 或请求体不是 JSON 对象/非流式时不动作；上游不识别该字段时由 serve 里的
// 400 回退逻辑去掉它对同一渠道重试一次。
// 解析必须用 UseNumber：默认 float64 重组会把 >2^53 的整数改写成科学计数法。
func injectStreamUsage(body []byte) ([]byte, bool) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v map[string]any
	if dec.Decode(&v) != nil || v == nil {
		return body, false
	}
	if stream, _ := v["stream"].(bool); !stream {
		return body, false
	}
	if _, exists := v["stream_options"]; exists {
		return body, false
	}
	v["stream_options"] = map[string]any{"include_usage": true}
	out, err := json.Marshal(v)
	if err != nil {
		return body, false
	}
	return out, true
}

// matchPrice 在价格表里找模型单价：先精确匹配，再退回最长的段边界前缀，
// 如 "gpt-4o" 覆盖 "gpt-4o-2024-08-06"；"gpt-4" 不会匹配 "gpt-4o"。
func matchPrice(prices []store.ModelPrice, model string) *store.ModelPrice {
	if model == "" {
		return nil
	}
	var best *store.ModelPrice
	for i := range prices {
		p := &prices[i]
		if p.Model == model {
			return p
		}
		if strings.HasPrefix(model, p.Model+"-") ||
			strings.HasPrefix(model, p.Model+".") ||
			strings.HasPrefix(model, p.Model+"/") {
			if best == nil || len(p.Model) > len(best.Model) {
				best = p
			}
		}
	}
	return best
}
