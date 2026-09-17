package api

// 渠道上游密钥的健康管理：连续失败触发指数冷却，冷却结束自动回到候选
// （半开：下一次真实请求或后台巡检即探测），成功即复位。运行态在内存；
// 冷却状态以 JSON 快照落 settings 表（仅状态变化时写入，低频），重启后
// 恢复未到期的冷却，避免每次发版对坏上游惊群重试一轮。持久化的另一部分
// 是手动启停（channel_keys.enabled）。

import (
	"context"
	"encoding/json"
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
	keyCooldownSnap  = "key_cooldowns"  // settings 表里的快照键
)

type keyHealthManager struct {
	mu     sync.Mutex
	st     *store.Store
	alerts *alertManager
	fails  map[string]int
	until  map[string]time.Time
}

// khSnapshot 是冷却快照的单条形态（settings 表 JSON）。
type khSnapshot struct {
	Until int64 `json:"until"` // Unix 秒
	Fails int   `json:"fails"`
}

func khKey(channelID, keyID int64) string { return fmt.Sprintf("%d/%d", channelID, keyID) }

func newKeyHealthManager(st *store.Store, alerts *alertManager) *keyHealthManager {
	return &keyHealthManager{st: st, alerts: alerts, fails: map[string]int{}, until: map[string]time.Time{}}
}

// reportFailure 记录一次失败并按需进入冷却。label 为「渠道名 + 打码密钥」的
// 展示文案（首次进入冷却时随告警推送；测试调用传空串跳过告警）。
func (m *keyHealthManager) reportFailure(channelID, keyID int64, label string) {
	m.mu.Lock()
	var cooling time.Duration
	changed := false
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
		_, wasCooling := m.until[id]
		if !wasCooling {
			cooling = d // 首次进入冷却才告警，续期不打扰
		}
		m.until[id] = time.Now().Add(d)
		changed = true
	}
	m.mu.Unlock()
	if cooling > 0 && label != "" && m.alerts != nil {
		m.alerts.fireKeyCooldown(label, cooling)
	}
	if changed {
		m.saveSnapshot()
	}
}

// saveSnapshot 把未到期的冷却状态写入 settings 表（状态变化时低频调用）。
func (m *keyHealthManager) saveSnapshot() {
	if m.st == nil {
		return
	}
	m.mu.Lock()
	snap := map[string]khSnapshot{}
	for id, t := range m.until {
		if time.Now().Before(t) {
			snap[id] = khSnapshot{Until: t.Unix(), Fails: m.fails[id]}
		}
	}
	m.mu.Unlock()
	b, err := json.Marshal(snap)
	if err != nil {
		return
	}
	if err := m.st.SetSetting(keyCooldownSnap, string(b)); err != nil {
		log.Printf("persist key cooldowns: %v", err)
	}
}

// restore 启动时恢复未到期的冷却状态（修重启惊群：坏上游不再立即被重试）。
func (m *keyHealthManager) restore(st *store.Store) {
	if st == nil {
		return
	}
	if m.st == nil {
		m.st = st
	}
	v, err := st.GetSetting(keyCooldownSnap)
	if err != nil || v == "" {
		return
	}
	snap := map[string]khSnapshot{}
	if err := json.Unmarshal([]byte(v), &snap); err != nil {
		return
	}
	now := time.Now()
	m.mu.Lock()
	for id, s := range snap {
		t := time.Unix(s.Until, 0)
		if now.Before(t) {
			m.until[id] = t
			if s.Fails > 0 {
				m.fails[id] = s.Fails
			}
		}
	}
	n := len(m.until)
	m.mu.Unlock()
	if n > 0 {
		log.Printf("已恢复 %d 把渠道密钥的冷却状态（重启前遗留）", n)
	}
}

// coolingCount 返回当前处于冷却中的渠道密钥数量（指标暴露用）。
func (m *keyHealthManager) coolingCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	n := 0
	for _, t := range m.until {
		if now.Before(t) {
			n++
		}
	}
	return n
}

func (m *keyHealthManager) reportSuccess(channelID, keyID int64) {
	m.mu.Lock()
	id := khKey(channelID, keyID)
	_, hadUntil := m.until[id]
	hadFails := m.fails[id] > 0
	delete(m.fails, id)
	delete(m.until, id)
	m.mu.Unlock()
	if hadUntil || hadFails { // 状态真有变化才落快照，成功请求的常态路径零写
		m.saveSnapshot()
	}
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
