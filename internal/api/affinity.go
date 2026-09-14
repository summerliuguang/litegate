package api

// 缓存感知路由：记录「会话前缀 → 上次成功的渠道+密钥」，让同一会话的后续请求
// 优先回到同一把上游密钥，最大化上游 prompt cache 命中率（DeepSeek 缓存命中价
// 约为未命中价的 1/50，多轮对话收益显著）。
// 纯内存、无配置：重启即失效重新学习；表满时先清过期项、仍满则整表重置。

import (
	"encoding/json"
	"hash/fnv"
	"strconv"
	"sync"
	"time"
)

const (
	affinityMaxEntries = 8192
	affinityTTL        = time.Hour
)

type affEntry struct {
	ch, key int64
	at      time.Time
}

type affinityMap struct {
	mu sync.Mutex
	m  map[string]affEntry
}

func (a *affinityMap) get(k string) (affEntry, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.m[k]
	if !ok {
		return e, false
	}
	if time.Since(e.at) > affinityTTL {
		delete(a.m, k)
		return affEntry{}, false
	}
	return e, true
}

func (a *affinityMap) put(k string, ch, key int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.m == nil {
		a.m = map[string]affEntry{}
	}
	if len(a.m) >= affinityMaxEntries {
		now := time.Now()
		for kk, e := range a.m {
			if now.Sub(e.at) > affinityTTL {
				delete(a.m, kk)
			}
		}
		if len(a.m) >= affinityMaxEntries {
			a.m = map[string]affEntry{} // 仍满则整表重置（重新学习，代价可忽略）
		}
	}
	a.m[k] = affEntry{ch: ch, key: key, at: time.Now()}
}

// conversationAffinityKey 生成会话前缀亲和键：模型名 + 除最后一条外的全部消息
// （末条是本次新增内容，不构成可复用前缀）。单条消息的一次性请求没有可复用
// 前缀，返回空串表示不做亲和（保持既有轮转均摊）。
func conversationAffinityKey(model string, body []byte) string {
	var v struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if json.Unmarshal(body, &v) != nil || len(v.Messages) < 2 {
		return ""
	}
	h := fnv.New64a()
	h.Write([]byte(model))
	h.Write([]byte{0})
	for i := 0; i < len(v.Messages)-1; i++ {
		b := v.Messages[i]
		if len(b) > 2048 {
			b = b[:2048]
		}
		h.Write(b)
		h.Write([]byte{0})
	}
	return strconv.FormatUint(h.Sum64(), 16)
}
