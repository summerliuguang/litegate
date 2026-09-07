package api

// 渠道上游密钥的健康管理：连续失败触发指数冷却，冷却结束自动回到候选
// （半开：下一次真实请求或后台巡检即探测），成功即复位。全部状态在内存，
// 重启即清零——持久化的只有手动启停（channel_keys.enabled）。

import (
	"context"
	"fmt"
	"log"
	"math/rand/v2"
	"sync"
	"time"

	"litegate/internal/store"
)

const (
	keyCooldownBase  = time.Minute      // 首次冷却 1 分钟
	keyCooldownMax   = 30 * time.Minute // 冷却上限，每多失败一次翻倍逼近
	keyFailThreshold = 2                // 连续失败达到该次数才进入冷却
)

type keyHealthManager struct {
	mu    sync.Mutex
	fails map[string]int
	until map[string]time.Time
}

func khKey(channelID, keyID int64) string { return fmt.Sprintf("%d/%d", channelID, keyID) }

func newKeyHealthManager() *keyHealthManager {
	return &keyHealthManager{fails: map[string]int{}, until: map[string]time.Time{}}
}

func (m *keyHealthManager) reportFailure(channelID, keyID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := khKey(channelID, keyID)
	m.fails[id]++
	n := m.fails[id]
	if n >= keyFailThreshold {
		d := keyCooldownBase
		for i := keyFailThreshold; i < n && d < keyCooldownMax; i++ {
			d *= 2
		}
		if d > keyCooldownMax {
			d = keyCooldownMax
		}
		m.until[id] = time.Now().Add(d)
	}
}

func (m *keyHealthManager) reportSuccess(channelID, keyID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := khKey(channelID, keyID)
	delete(m.fails, id)
	delete(m.until, id)
}

// available 返回渠道当前可用的启用密钥：优先未被冷却的；全部冷却中则仍返回
// 全部启用密钥（整体冷却时试一次好过必然失败）。顺序随机轮转实现均匀分摊。
func (m *keyHealthManager) available(c *store.Channel, now time.Time) []store.ChannelKey {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ready, cooling []store.ChannelKey
	for _, k := range c.APIKeys {
		if !k.Enabled {
			continue
		}
		if t, bad := m.until[khKey(c.ID, k.ID)]; bad && now.Before(t) {
			cooling = append(cooling, k)
		} else {
			ready = append(ready, k)
		}
	}
	if len(ready) == 0 {
		ready = cooling
	}
	if len(ready) > 1 {
		off := rand.IntN(len(ready))
		ready = append(append([]store.ChannelKey{}, ready[off:]...), ready[:off]...)
	}
	return ready
}

// probeTarget 是一把需要巡检的渠道密钥。
type probeTarget struct {
	channel store.Channel
	key     store.ChannelKey
}

// unhealthyKeys 列出有失败记录的启用密钥（巡检对象）。
func (m *keyHealthManager) unhealthyKeys(chans []store.Channel) []probeTarget {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []probeTarget
	for _, c := range chans {
		for _, k := range c.APIKeys {
			if !k.Enabled {
				continue
			}
			if m.fails[khKey(c.ID, k.ID)] > 0 {
				out = append(out, probeTarget{channel: c, key: k})
			}
		}
	}
	return out
}

// serverProxy 是当前进程的数据面实例（NewServer 注入；进程内仅一个）。
var serverProxy *proxy

// StartKeyHealthChecker 后台定时巡检：对有失败记录的启用密钥发 GET /models 探测，
// 成功则复位失败计数。只在主程序启动；单元测试不启动，避免后台协程触碰测试库。
func StartKeyHealthChecker(st *store.Store, interval time.Duration) {
	go func() {
		client := newUpstreamClient()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			p := serverProxy
			if p == nil {
				continue
			}
			chans, err := st.ListChannels("")
			if err != nil {
				continue
			}
			for _, t := range p.keys.unhealthyKeys(chans) {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				_, err := fetchFromChannel(ctx, client, &t.channel, t.key.Key)
				cancel()
				if err == nil {
					p.keys.reportSuccess(t.channel.ID, t.key.ID)
					log.Printf("健康巡检：渠道 %q 密钥 %s 探测恢复", t.channel.Name, t.key.Masked)
				}
			}
		}
	}()
}
