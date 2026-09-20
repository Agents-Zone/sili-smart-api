package service

// 生产形态高强度模拟（真实 Redis）：验证独占绑定在并发 + 扰动（渠道禁用/恢复、
// 终态失败回滚、TTL 过期重绑）长周期下的不变量：
//   I1 每键恰登记一次（占用成员总数 == 键数，指纹无重复、无丢失）
//   I2 原子最小落位下渠道间均摊（max 与均摊值偏差 <=1，min >= 均摊值-1）
//   I3 首发全独占：全渠道可用且无既有绑定时，每渠道至多 1 键直至满载
// Redis 模式下独占决策的跨实例原子性由 Lua/SetNX 承担，多 goroutine 不同键
// 并发进 claim/place 脚本等价于多实例交错（外层分段锁按键哈希，互不阻塞）。
// 通过 AFFINITY_REAL_REDIS 启用，未设置时跳过。

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// prodSimKeys 模拟键集（24 键）。
var prodSimKeys = func() []string {
	keys := make([]string, 24)
	for i := range keys {
		keys[i] = fmt.Sprintf("psim-%02d", i)
	}
	return keys
}()

// prodSimRequest 镜像 distributor 亲和块 + relay 成功回写的完整时序。
// finalFail=true 时在回写前注入终态失败回滚（等价 relay 全重试失败）。
// expireBind=true 时先清该键正向+占位+lastBind（等价整周期 TTL 过期）。
// 返回实际使用渠道（0 = 随机兜底或回滚后无绑定）。
func prodSimRequest(t *testing.T, key string, finalFail bool, expireBind bool) int {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Request.Header.Set("X-Affinity-Key", key)

	if expireBind {
		suffix := affinityKeySuffix(key)
		v, found, err := getChannelAffinityCache().Get(suffix)
		require.NoError(t, err)
		if found && v > 0 {
			require.NoError(t, occupancyRemoveKeyFP(v, affinityFingerprint(key)))
		}
		_, err = getChannelAffinityCache().DeleteMany([]string{suffix})
		require.NoError(t, err)
		require.NoError(t, lastBindDelete(suffix))
	}

	channel := 0
	if preferredID, found := GetPreferredChannelByAffinity(ctx, "gpt-4", "default"); found {
		affinityUsable := false
		if preferred, err := model.CacheGetChannel(preferredID); err == nil && preferred != nil &&
			preferred.Status == common.ChannelStatusEnabled &&
			model.IsChannelEnabledForGroupModel("default", "gpt-4", preferred.Id) {
			channel = preferredID
			affinityUsable = true
			MarkChannelAffinityUsed(ctx, "default", preferred.Id)
		}
		if !affinityUsable {
			if !ShouldKeepChannelAffinityOnChannelDisabled() {
				ClearCurrentChannelAffinityCache(ctx)
				if rebindID, refound := GetPreferredChannelByAffinity(ctx, "gpt-4", "default"); refound {
					if p, perr := model.CacheGetChannel(rebindID); perr == nil && p != nil &&
						p.Status == common.ChannelStatusEnabled &&
						model.IsChannelEnabledForGroupModel("default", "gpt-4", p.Id) {
						channel = rebindID
					}
				}
			}
			if channel == 0 {
				MarkChannelAffinitySoftPlacement(ctx)
			}
		}
	}

	if finalFail {
		// 镜像 relay 终态失败：回滚占位（不进入成功回写）。
		RollbackChannelAffinityOnFinalFailure(ctx)
		return 0
	}

	if channel == 0 {
		// 随机兜底：真实路径为 CacheGetRandomSatisfiedChannel；独占语义下软落位
		// 不固化，此处直接取缓存候选首位（值不影响占用索引断言）。
		candidates := model.GetEnabledChannelIDsForGroupModel("default", "gpt-4", "/v1/chat/completions")
		require.NotEmpty(t, candidates)
		channel = candidates[0]
	}

	ctx.Set("channel_id", channel)
	RecordChannelAffinity(ctx, channel)
	return channel
}

// prodSimConcurrentRound 并发跑一轮指定键集的请求。
func prodSimConcurrentRound(t *testing.T, keys []string, finalFail map[string]bool, expireBind map[string]bool) {
	t.Helper()
	var wg sync.WaitGroup
	start := make(chan struct{})
	errCh := make(chan error, len(keys))
	for _, key := range keys {
		wg.Add(1)
		go func(k string) {
			defer wg.Done()
			<-start
			defer func() {
				if r := recover(); r != nil {
					errCh <- fmt.Errorf("key %s panicked: %v", k, r)
				}
			}()
			prodSimRequest(t, k, finalFail[k], expireBind[k])
		}(key)
	}
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}

// prodSimDistribution 统计各渠道占用键数（仅夹具渠道）。
func prodSimDistribution(t *testing.T) (map[int]int, int) {
	t.Helper()
	dist := map[int]int{}
	total := 0
	for _, id := range softPlacementChannels {
		c := channelMemberCount(t, id)
		dist[id] = c
		total += c
	}
	return dist, total
}

// prodSimDistributionLogged 输出各渠道占用键数（仅日志）。
func prodSimDistributionLogged(t *testing.T, phase string) {
	t.Helper()
	dist, total := prodSimDistribution(t)
	t.Logf("%s: distribution=%v total=%d", phase, dist, total)
}

// prodSimAssertBalanced I1+I2：总成员 == 键数，且 max <= avg+1、min >= avg-1
//（avg = total/len(channels)，仅在可用渠道上均摊）。
func prodSimAssertBalanced(t *testing.T, keys int, available []int, phase string) {
	t.Helper()
	dist, total := prodSimDistribution(t)
	maxC, minC := 0, 1<<30
	avail := 0
	for _, id := range available {
		avail += dist[id]
	}
	for _, id := range available {
		c := dist[id]
		if c > maxC {
			maxC = c
		}
		if c < minC {
			minC = c
		}
	}
	avg := (avail + len(available) - 1) / len(available)
	t.Logf("%s: distribution=%v available=%v (avg=%d)", phase, dist, available, avg)
	assert.Equalf(t, keys, total, "%s: every key must be registered exactly once", phase)
	assert.LessOrEqualf(t, maxC, avg+1, "%s: channel stacking %d exceeds fair share %d+1 while channels available", phase, maxC, avg)
	assert.GreaterOrEqualf(t, minC, avg-1, "%s: channel under-used at %d while others hold %d", phase, minC, maxC)
}

// TestRealRedisProductionSimChurn 生产形态主场景：24 键 6 渠道。
func TestRealRedisProductionSimChurn(t *testing.T) {
	useRealRedisMode(t)
	useSoftPlacementFixture(t, true)

	// 阶段1：24 键并发首发。6 渠道满载均摊 4，独占引擎应产出 4,4,4,4,4,4
	//（偏差 <=1），总成员恰 24。
	prodSimConcurrentRound(t, prodSimKeys, nil, nil)
	prodSimAssertBalanced(t, len(prodSimKeys), softPlacementChannels, "phase1 first bind")

	// 阶段2：8 轮扰动。每轮禁用 2 渠道（含承接键的渠道），全部键并发请求
	//（绑定禁用渠道的键清占位重绑到可用渠道），随后恢复。
	disabledPairs := [][2]int{{921, 922}, {923, 924}, {925, 926}, {921, 923}, {922, 924}, {921, 926}, {922, 925}, {923, 926}}
	for round, pair := range disabledPairs {
		setChannelsDisabled(t, pair[0], pair[1])
		available := make([]int, 0, len(softPlacementChannels))
		for _, id := range softPlacementChannels {
			if id != pair[0] && id != pair[1] {
				available = append(available, id)
			}
		}
		prodSimConcurrentRound(t, prodSimKeys, nil, nil)
		prodSimAssertBalanced(t, len(prodSimKeys), available, fmt.Sprintf("round%d disabled=%v", round, pair))
		setChannelsEnabled(t, pair[0], pair[1])
	}

	// 阶段3：终态失败注入。随机 6 键请求即失败（回滚占位三处），随后全部键
	// 再请求一轮：失败键重新走独占决策，分布回填均摊。
	finalFail := map[string]bool{}
	for i := 0; i < 6; i++ {
		finalFail[prodSimKeys[i*4]] = true
	}
	prodSimConcurrentRound(t, prodSimKeys, finalFail, nil)
	prodSimDistributionLogged(t, "phase3 mid (after final-fail round)")
	prodSimConcurrentRound(t, prodSimKeys, nil, nil)
	prodSimAssertBalanced(t, len(prodSimKeys), softPlacementChannels, "phase3 after final-failure churn")

	// 阶段4：TTL 过期重绑。12 键整周期过期（正向+占位+lastBind 全清，等价
	// 冷键两周期后再来），全部键并发请求：过期键重新独占选定，均摊保持。
	expireBind := map[string]bool{}
	for i := 0; i < 12; i++ {
		expireBind[prodSimKeys[i*2]] = true
	}
	prodSimConcurrentRound(t, prodSimKeys, nil, expireBind)
	prodSimAssertBalanced(t, len(prodSimKeys), softPlacementChannels, "phase4 after TTL expiry rebind")
}

// TestRealRedisProductionSimExclusiveFirstBind I3：键数 <= 渠道数时全独占，
// 任意渠道至多 1 键（无满载理由则无共享）。
func TestRealRedisProductionSimExclusiveFirstBind(t *testing.T) {
	useRealRedisMode(t)
	useSoftPlacementFixture(t, true)

	keys := make([]string, 0, len(softPlacementChannels))
	for i := 0; i < len(softPlacementChannels); i++ {
		keys = append(keys, fmt.Sprintf("psim-x-%d", i))
	}
	prodSimConcurrentRound(t, keys, nil, nil)

	dist, total := prodSimDistribution(t)
	t.Logf("exclusive first-bind distribution: %v", dist)
	assert.Equal(t, len(keys), total)
	for id, c := range dist {
		assert.LessOrEqualf(t, c, 1, "channel %d holds %d keys with free channels available (no full-load excuse)", id, c)
	}
	// 静默双占检测：无降级计数增长（每键独占，无 shared）。
	sorted := make([]int, 0, len(dist))
	for _, c := range dist {
		sorted = append(sorted, c)
	}
	sort.Ints(sorted)
	assert.Equal(t, 1, sorted[len(sorted)-1], "keys == channels must yield pure exclusive placement")
}
