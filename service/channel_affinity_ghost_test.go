package service

// 生产缺陷回归：Redis 模式 SetNX 竞争失败（loser）路径的堆积来源。
// 症状（生产观察）：渠道大量空闲时，多个亲和键共享同一渠道，无满载降级理由。
// 缺陷 1（幽灵占用）：跨实例同键并发 miss，败者先在渠道 Y 上 claim 占位成功，
//   随后正向 SetNX 失败（胜者实例抢先写入绑定渠道 X），败者读胜者结果返回 X，
//   但 Y 条目中残留本键指纹不回滚。占用索引高估 Y 被占，后续键的空闲判定被
//   污染，误判满载进入 shared 降级后按最少绑定数集中复用，形成堆积。
// 缺陷 2（败者确认失败缺降级标记）：SetNX 失败后读胜者结果又遇存储故障，
//   返回 (0,false) 但未置存储降级标记；distributor 随机选路成功后
//   RecordChannelAffinity 把随机渠道固化为正式绑定（old=0 追加占用登记）。
// 两个用例均以 preHook 注入跨实例竞争时序（内存模式被分段锁内 double-check
// 挡住，无法复现），修复前红、修复后绿。

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ghostAcquireCtx 构造带独占 meta 的请求上下文。
func ghostAcquireCtx(t *testing.T, meta channelAffinityMeta) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	setChannelAffinityContext(ctx, meta)
	return ctx
}

// injectWinnerOnSetNX 在败者执行正向 SET NX 时把胜者绑定写入 Redis：
// 模拟跨实例竞争中胜者实例恰在败者 claim 之后、SetNX 之前完成正向写入。
// 返回注入是否已发生（仅注入一次）。
func injectWinnerOnSetNX(t *testing.T, srv *miniredis.Miniredis, fullForwardKey string, winnerID int) *int32 {
	t.Helper()
	var injected int32
	srv.Server().SetPreHook(func(c *server.Peer, cmd string, args ...string) bool {
		if cmd != "SET" || len(args) < 2 {
			return false
		}
		if args[0] != fullForwardKey {
			return false
		}
		hasNX := false
		for _, opt := range args[2:] {
			if strings.EqualFold(opt, "NX") {
				hasNX = true
				break
			}
		}
		if hasNX && atomic.CompareAndSwapInt32(&injected, 0, 1) {
			srv.Set(fullForwardKey, fmt.Sprintf("%d", winnerID))
		}
		return false
	})
	return &injected
}

// 变红断言 1（幽灵占用）：迟到实例 claim 702 后 SetNX 失败，702 上的占位必须回滚。
// 构造确定性决策：701 被胜者指纹 + 满载第三键占据（对本键计数 1，非空闲），
// 702 空闲，迟到实例的空闲集只剩 702。
func TestSetNXLoserRollsBackClaimOnOtherChannel(t *testing.T) {
	srv := useExclusiveRedisMode(t)
	setupExclusiveAcquireDB(t)
	resetAffinityCacheSingleton()
	meta := exclusiveAcquireTestMeta()
	meta.CacheKey = channelAffinityCacheNamespace + ":exclusive-test-rule:default:ghost-key"
	suffix := exclusiveCacheKeySuffix(meta)
	fullForwardKey := channelAffinityCacheNamespace + ":" + suffix
	t.Cleanup(func() {
		_, _ = getChannelAffinityCache().DeleteMany([]string{suffix})
		_, _ = getChannelAffinityLastBindCache().DeleteMany([]string{suffix})
		_, _ = getChannelAffinityOccupancyCache().DeleteMany([]string{"701", "702"})
	})

	// 胜者实例已完成 claim 701（同键指纹）；另一键满载挤入 701，使迟到实例
	// 的空闲集只剩 702。lastBind 留空（胜者尚未写完的窗口）。
	require.NoError(t, occupancyAddKeyFP(701, meta.KeyFingerprint, 30*time.Second))
	require.NoError(t, occupancyAddKeyFP(701, "shared-holder", 30*time.Second))

	injected := injectWinnerOnSetNX(t, srv, fullForwardKey, 701)

	ch, found := acquireExclusiveBinding(ghostAcquireCtx(t, meta), meta, meta.UsingGroup, meta.ModelName)

	require.Equal(t, int32(1), atomic.LoadInt32(injected), "winner injection must have fired on loser SetNX")
	require.True(t, found, "loser must read and return the winner binding")
	assert.Equal(t, 701, ch, "loser converges to winner channel")

	// 核心断言：迟到实例在 702 上的 claim 占位必须回滚（修复前幽灵残留，红）。
	count, _, err := occupancyBindingCount(702)
	require.NoError(t, err)
	assert.Equal(t, 0, count,
		"loser claim on the losing channel must be rolled back, ghost occupancy poisons free-channel detection")

	// 回滚不得误删胜者在 701 上的登记（同键指纹，decision!=winner 才回滚的依据）。
	count, fps, err := occupancyBindingCount(701)
	require.NoError(t, err)
	assert.Equal(t, 2, count)
	assert.ElementsMatch(t, []string{meta.KeyFingerprint, "shared-holder"}, fps)
}

// 变红断言 2（败者确认失败缺降级标记）：SetNX 失败后读胜者结果遇存储故障，
// 必须置存储降级标记，防止 distributor 随机选路结果被固化为正式绑定。
func TestSetNXLoserConfirmFailureMarksDegrade(t *testing.T) {
	srv := useExclusiveRedisMode(t)
	setupExclusiveAcquireDB(t)
	resetAffinityCacheSingleton()
	meta := exclusiveAcquireTestMeta()
	meta.CacheKey = channelAffinityCacheNamespace + ":exclusive-test-rule:default:ghost-confirm-key"
	suffix := exclusiveCacheKeySuffix(meta)
	fullForwardKey := channelAffinityCacheNamespace + ":" + suffix
	t.Cleanup(func() {
		_, _ = getChannelAffinityCache().DeleteMany([]string{suffix})
		_, _ = getChannelAffinityLastBindCache().DeleteMany([]string{suffix})
		_, _ = getChannelAffinityOccupancyCache().DeleteMany([]string{"701", "702"})
	})

	require.NoError(t, occupancyAddKeyFP(701, meta.KeyFingerprint, 30*time.Second))
	require.NoError(t, occupancyAddKeyFP(701, "shared-holder", 30*time.Second))

	var injected int32
	srv.Server().SetPreHook(func(c *server.Peer, cmd string, args ...string) bool {
		if cmd == "SET" && len(args) >= 2 && args[0] == fullForwardKey {
			hasNX := false
			for _, opt := range args[2:] {
				if strings.EqualFold(opt, "NX") {
					hasNX = true
					break
				}
			}
			if hasNX && atomic.CompareAndSwapInt32(&injected, 0, 1) {
				srv.Set(fullForwardKey, "701")
			}
			return false
		}
		if cmd == "GET" && len(args) >= 1 && args[0] == fullForwardKey && atomic.LoadInt32(&injected) == 1 {
			c.WriteError("LOADING Redis is loading the dataset in memory")
			return true
		}
		return false
	})

	ctx := ghostAcquireCtx(t, meta)
	ch, found := acquireExclusiveBinding(ctx, meta, meta.UsingGroup, meta.ModelName)

	require.Equal(t, int32(1), atomic.LoadInt32(&injected), "winner injection must have fired on loser SetNX")
	require.False(t, found, "loser confirm failure must degrade to soft affinity")
	assert.Equal(t, 0, ch)

	// 核心断言：存储降级标记必须置位（修复前缺失，红）——
	// 否则随机选路结果会被 RecordChannelAffinity 固化为正式绑定。
	_, marked := ctx.Get(ginKeyChannelAffinityStorageDegrade)
	assert.True(t, marked, "loser confirm failure must mark storage degrade so random routing is not pinned")

	// 幽灵占位同样必须回滚。
	count, _, err := occupancyBindingCount(702)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "loser claim on the losing channel must be rolled back on confirm failure too")
}

// 变红断言 3（路径 F）：正向缓存读故障时 GetPreferredChannelByAffinity 必须
// 置存储降级标记，否则随机选路结果会被固化为正式绑定（meta 已设置、
// boundChannel 未设置，软落位与重试切换检测全部落空）。
func TestForwardCacheReadFailureMarksDegrade(t *testing.T) {
	srv := useExclusiveRedisMode(t)
	setupExclusiveAcquireDB(t)
	resetAffinityCacheSingleton()
	// suffix 由规则构成（rule:group:affinity_value）推出，亲和值取 header 值。
	const affinityValue = "forward-fail"
	suffix := "exclusive-test-rule:default:" + affinityValue
	fullForwardKey := channelAffinityCacheNamespace + ":" + suffix
	t.Cleanup(func() {
		_, _ = getChannelAffinityCache().DeleteMany([]string{suffix})
		_, _ = getChannelAffinityOccupancyCache().DeleteMany([]string{"701", "702"})
		_, _ = getChannelAffinityLastBindCache().DeleteMany([]string{suffix})
	})

	// 正向键已存在（绑定 701），但读取时存储故障。
	// 键构成与真实规则一致：rule:group:header 值。
	require.NoError(t, getChannelAffinityCache().SetWithTTL(suffix, 701, 30*time.Second))
	srv.Server().SetPreHook(func(c *server.Peer, cmd string, args ...string) bool {
		if cmd == "GET" && len(args) >= 1 && args[0] == fullForwardKey {
			c.WriteError("LOADING Redis is loading the dataset in memory")
			return true
		}
		return false
	})

	// 注入独占规则（header 键源，命中本请求），并使 setting.Enabled=true。
	settingRules := operation_setting.GetChannelAffinitySetting()
	origRules, origEnabled, origSwitch := settingRules.Rules, settingRules.Enabled, settingRules.SwitchOnSuccess
	settingRules.Rules = []operation_setting.ChannelAffinityRule{{
		Name:              "exclusive-test-rule",
		ModelRegex:        []string{"^gpt-4$"},
		KeySources:        []operation_setting.ChannelAffinityKeySource{{Type: "request_header", Key: "X-Affinity-Key"}},
		TTLSeconds:        30,
		IncludeRuleName:   true,
		IncludeUsingGroup: true,
		ExclusiveBind:     true,
	}}
	settingRules.Enabled = true
	settingRules.SwitchOnSuccess = true
	t.Cleanup(func() {
		settingRules.Rules = origRules
		settingRules.Enabled = origEnabled
		settingRules.SwitchOnSuccess = origSwitch
	})

	// 构造走真实规则匹配的请求（独占规则命中本请求）。
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Request.Header.Set("X-Affinity-Key", "forward-fail")

	_, found := GetPreferredChannelByAffinity(ctx, "gpt-4", "default")
	require.False(t, found, "read failure must degrade to soft affinity")

	_, marked := ctx.Get(ginKeyChannelAffinityStorageDegrade)
	assert.True(t, marked, "forward read failure must mark storage degrade so random routing is not pinned")

	// 标记置位后 RecordChannelAffinity 跳过固化：随机渠道不得追加占用登记。
	ctx.Set("channel_id", 702)
	RecordChannelAffinity(ctx, 702)
	count, _, err := occupancyBindingCount(702)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "random routing must not be pinned after forward read failure")
}

// 变红断言 4（Redis 模式批量分布）：8 键 6 渠道，全部首发（无禁用扰动）后，
// 独占语义要求任意渠道键数 ≤1（6 渠道容纳 8 键会触发 2 次满载降级，但降级
// 键经 shared 判定分散到各渠道，不得堆积在同一渠道）。
// 这是生产现象（渠道富裕却多人共享）的直接断言：修复前的幽灵占用污染
// 空闲判定，使后续键误判满载集中复用。
func TestRedisBatchFirstBindBalanced(t *testing.T) {
	useExclusiveRedisMode(t)
	useSoftPlacementFixture(t, true)

	keys := []string{"rb-1", "rb-2", "rb-3", "rb-4", "rb-5", "rb-6", "rb-7", "rb-8"}
	for _, key := range keys {
		mirrorDistributorRequest(t, key)
	}

	counts := map[int]int{}
	total := 0
	for _, id := range softPlacementChannels {
		c := channelMemberCount(t, id)
		counts[id] = c
		total += c
	}
	maxC := 0
	for _, c := range counts {
		if c > maxC {
			maxC = c
		}
	}
	t.Logf("redis first-bind distribution: %v", counts)

	assert.Equal(t, len(keys), total, "every key must be registered exactly once")
	// 8 键 6 渠道全空闲首发：前 6 键各占一渠道（独占），后 2 键满载降级
	// 分散复用。允许降级键与独占键共享，但单渠道至多 2（1 独占 + 1 满载降级，
	// 满载层在剩余计数相同时均匀随机，两个降级键挤同一渠道才会到 3）。
	assert.LessOrEqual(t, maxC, 2,
		"with all channels free, 8 keys over 6 channels must not stack 3+ on one channel")
}

// 收官验证（Redis 模式并发批量）：8 键并发首发于 6 渠道（渠道全空闲）。
// 独占引擎在跨实例 CAS 下不得超卖：任意渠道键数 ≤2（1 独占 + 满载降级上限），
// 总登记数守恒 = 8。并发是生产现象的形态（多用户同时发起请求）。
func TestRedisConcurrentBatchNoStacking(t *testing.T) {
	useExclusiveRedisMode(t)
	useSoftPlacementFixture(t, true)

	const n = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			mirrorDistributorRequest(t, fmt.Sprintf("conc-%02d", idx))
		}(i)
	}
	close(start)
	wg.Wait()

	counts := map[int]int{}
	total := 0
	for _, id := range softPlacementChannels {
		c := channelMemberCount(t, id)
		counts[id] = c
		total += c
	}
	maxC := 0
	for _, c := range counts {
		if c > maxC {
			maxC = c
		}
	}
	t.Logf("redis concurrent distribution: %v", counts)

	assert.Equal(t, n, total, "every concurrent key must be registered exactly once")
	assert.LessOrEqual(t, maxC, 2, "concurrent first bind over free channels must not stack 3+ on one channel")
}
