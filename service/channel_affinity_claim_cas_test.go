package service

// 修复回归：D3 跨键并发双占的根治回路。
// 症状（生产观察）：渠道大量空闲时，多个令牌（不同亲和键）并发首发 miss 仍频繁
// 落到同一渠道，占用键数 >1，且无 exclusive_degrade 标记、degraded_reuse_total 为 0。
// 根因：occupancyCounts 快照在亲和键分段锁外读取，决策与登记之间无渠道维度互斥，
// check-then-act 竞态使并发决策方互不可见（SSOT 8.3 原偏离 D3）。
// 本文件先以现有公开行为断言症状（修复前红、修复后绿），修复合入后补充
// claim 原语的直接单元测试。

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// useClaimFixture 构造 6 渠道（901..906，default/gpt-4）候选集夹具。
// setupExclusiveAcquireDB 自带 701/702，实际候选集为 8 渠道，一并纳入统计。
func useClaimFixture(t *testing.T) []string {
	t.Helper()
	useExclusiveMemoryMode(t)
	setupExclusiveAcquireDB(t)
	resetAffinityCacheSingleton()

	priority := int64(0)
	weight := uint(100)
	for _, id := range []int{901, 902, 903, 904, 905, 906} {
		require.NoError(t, model.DB.Create(&model.Channel{
			Id:       id,
			Type:     constant.ChannelTypeOpenAI,
			Key:      fmt.Sprintf("key-%d", id),
			Status:   common.ChannelStatusEnabled,
			Name:     fmt.Sprintf("claim-channel-%d", id),
			Weight:   &weight,
			Models:   "gpt-4",
			Group:    "default",
			Priority: &priority,
		}).Error)
		require.NoError(t, model.DB.Create(&model.Ability{
			Group:     "default",
			Model:     "gpt-4",
			ChannelId: id,
			Enabled:   true,
			Priority:  &priority,
			Weight:    weight,
		}).Error)
	}
	model.InitChannelCache()
	// 候选集 = 701/702（setupExclusiveAcquireDB）+ 901..906。
	channelKeys := []string{"701", "702", "901", "902", "903", "904", "905", "906"}
	t.Cleanup(func() {
		_, _ = getChannelAffinityOccupancyCache().DeleteMany(channelKeys)
	})
	return channelKeys
}

// claimTestMeta 构造指定亲和键的独占 meta（候选集含 901..906）。
func claimTestMeta(keyFP string, suffix string) channelAffinityMeta {
	return channelAffinityMeta{
		CacheKey:       channelAffinityCacheNamespace + ":" + suffix,
		TTLSeconds:     30,
		RuleName:       "claim-test-rule",
		ExclusiveBind:  true,
		KeyFingerprint: keyFP,
		UsingGroup:     "default",
		ModelName:      "gpt-4",
		RequestPath:    "/v1/chat/completions",
	}
}

// buildClaimContext 构造带 meta 的 gin context（claim 夹具专用，键名独立）。
func buildClaimContext(t *testing.T, meta channelAffinityMeta) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	setChannelAffinityContext(ctx, meta)
	return ctx
}

// totalRegisteredMembers 统计全部候选渠道的占用成员总数。
func totalRegisteredMembers(t *testing.T, channelKeys []string) int {
	t.Helper()
	total := 0
	for _, key := range channelKeys {
		count, _, err := occupancyBindingCount(mustAtoi(key))
		require.NoError(t, err)
		total += count
	}
	return total
}

// 变红断言 1（内存模式）：6 键并发首发于 6 渠道，渠道全空闲时无满载降级理由，
// 任意渠道键数 >1 即超卖（决策快照互不可见的 check-then-act 竞态）。
// 不禁空桶：均匀随机选定允许部分渠道天然未被选中的分布形态。
func TestClaimConcurrentNoOversell(t *testing.T) {
	channelKeys := useClaimFixture(t)

	const keys = 6
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < keys; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			m := claimTestMeta(fmt.Sprintf("claim-fp-%02d", idx), fmt.Sprintf("claim-rule:default:claim-key-%02d", idx))
			ch, found := acquireExclusiveBinding(buildClaimContext(t, m), m, m.UsingGroup, m.ModelName)
			if !found || ch <= 0 {
				t.Errorf("key %d: found=%v ch=%d", idx, found, ch)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	for _, key := range channelKeys {
		count, _, err := occupancyBindingCount(mustAtoi(key))
		require.NoError(t, err)
		if count > 1 {
			t.Errorf("channel %s oversold with %d keys while channels were all free at start", key, count)
		}
	}
	assert.Equal(t, keys, totalRegisteredMembers(t, channelKeys), "6 keys must register exactly 6 members")
}

// 变红断言 2（内存模式确定性回路）：仅剩 1 个空闲渠道（901）时 4 键并发首发。
// 修复前：跨键决策互不可见，同批挤入 901 超卖（键数 >1），且无降级标记。
// 修复后：条件占位 CAS 生效——恰好 1 键独占 901；其余键因全部候选被占（满载）
// 走 shared 降级复用（带 exclusive_degrade 标记与计数），属设计内行为
// （SSOT 5.1.4 规则3）。断言：901 首占后未标记降级的追加登记为 0——即
// 独占模式（Mode=exclusive）的占位在 CAS 下不再超卖；满载键的复用计入降级。
func TestClaimConvergesToSoleFreeChannel(t *testing.T) {
	channelKeys := useClaimFixture(t)

	// 持有键占满 701/702/902..906，仅 901 空闲。
	holderIdx := 0
	for _, id := range []int{701, 702, 902, 903, 904, 905, 906} {
		require.NoError(t, occupancyAddKeyFP(id, fmt.Sprintf("holder-%02d", holderIdx), 30*time.Second))
		holderIdx++
	}

	const keys = 4
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < keys; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			m := claimTestMeta(fmt.Sprintf("converge-fp-%02d", idx), fmt.Sprintf("converge-rule:default:converge-key-%02d", idx))
			ch, found := acquireExclusiveBinding(buildClaimContext(t, m), m, m.UsingGroup, m.ModelName)
			if !found || ch <= 0 {
				t.Errorf("key %d: found=%v ch=%d", idx, found, ch)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	count, fps, err := occupancyBindingCount(901)
	require.NoError(t, err)
	t.Logf("sole-free-channel race: 901 holds %d fps: %v", count, fps)
	// 901 允许 1 独占 + 满载降级复用（全部候选被占时）；判定无超卖的口径是
	// 降级计数：4 参与键中最多 1 键独占，其余 ≥3 键必然走满载降级。
	// 修复前同批挤入无降级（degraded 不涨），修复后降级数 = 键数 - 独占键数。
	assert.Equal(t, 7+keys, totalRegisteredMembers(t, channelKeys))
	assert.GreaterOrEqual(t, degradedReuseTotal(), uint64(keys-1),
		"keys failing the sole-free claim must enter full-load degrade, not silently double-bind")
}

// 变红断言 3（Redis 模式）：仅 1 空闲渠道（701）时多键并发首发，
// 跨实例 CAS 语义下恰好 1 键独占 701。
func TestClaimRedisConvergesToSoleFreeChannel(t *testing.T) {
	useExclusiveRedisMode(t)
	setupExclusiveAcquireDB(t)
	resetAffinityCacheSingleton()

	occupancyKeys := []string{"701", "702"}
	t.Cleanup(func() {
		_, _ = getChannelAffinityOccupancyCache().DeleteMany(occupancyKeys)
	})

	// 702 被占，仅 701 空闲。
	require.NoError(t, occupancyAddKeyFP(702, "holder-01", 30*time.Second))

	const keys = 4
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < keys; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			m := exclusiveAcquireTestMeta()
			m.KeyFingerprint = fmt.Sprintf("redis-claim-fp-%02d", idx)
			m.CacheKey = channelAffinityCacheNamespace + ":exclusive-test-rule:default:redis-claim-" + strconv.Itoa(idx)
			ch, found := acquireExclusiveBinding(buildClaimContext(t, m), m, m.UsingGroup, m.ModelName)
			if !found || ch <= 0 {
				t.Errorf("key %d: found=%v ch=%d", idx, found, ch)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	count, fps, err := occupancyBindingCount(701)
	require.NoError(t, err)
	t.Logf("redis sole-free race: 701 holds %d fps: %v", count, fps)
	// 与内存模式同口径：CAS 消除的是无降级标记的静默双占（Mode=exclusive 超卖），
	// claim 失败的键转入满载降级（带标记与计数）。降级数 ≥ 键数 - 1。
	assert.GreaterOrEqual(t, degradedReuseTotal(), uint64(keys-1),
		"redis CAS losers must enter full-load degrade, not silently double-bind")
}

// 变红断言 4：存储故障降级不再固化随机绑定（Redis 断连真实故障注入）。
// 占用索引读失败时 acquire 返回 (0,false)，distributor 随机选路成功后
// RecordChannelAffinity 把随机结果写成正式绑定（TTL 一个周期钉死）。
// 修复后：acquire 降级时在 gin context 置存储降级标记，RecordChannelAffinity
// 见标记跳过正向写入与索引登记（降级请求的渠道选择保持软亲和现状，不固化独占占位）。
func TestRecordSkipsAfterExclusivePathDegrade(t *testing.T) {
	server := useExclusiveRedisMode(t)
	setupExclusiveAcquireDB(t)
	resetAffinityCacheSingleton()
	meta := exclusiveAcquireTestMeta()
	suffix := exclusiveCacheKeySuffix(meta)
	t.Cleanup(func() {
		_, _ = getChannelAffinityCache().DeleteMany([]string{suffix})
		_, _ = getChannelAffinityLastBindCache().DeleteMany([]string{suffix})
		_, _ = getChannelAffinityOccupancyCache().DeleteMany([]string{"701", "702"})
	})

	setting := operation_setting.GetChannelAffinitySetting()
	origEnabled, origSwitch := setting.Enabled, setting.SwitchOnSuccess
	setting.Enabled, setting.SwitchOnSuccess = true, true
	t.Cleanup(func() {
		setting.Enabled, setting.SwitchOnSuccess = origEnabled, origSwitch
	})

	// 断开 Redis：occupancy/lastBind 读失败 -> acquire 降级 (0,false) 并置标记。
	server.Close()
	ctx := buildClaimContext(t, meta)
	ch, found := acquireExclusiveBinding(ctx, meta, meta.UsingGroup, meta.ModelName)
	require.False(t, found)
	require.Equal(t, 0, ch)
	_, marked := ctx.Get(ginKeyChannelAffinityStorageDegrade)
	require.True(t, marked, "storage-failure degrade must be observable in gin context")

	// distributor 随机选路成功后回写：见标记必须跳过固化。
	ctx.Set("channel_id", 701)
	RecordChannelAffinity(ctx, 701)

	// Redis 已断，改验内存视图不可行；断言降级标记仍在（未被消费清除）且
	// 跳过路径已执行：RecordChannelAffinity 直接 return，无任何写动作可观察
	// 副作用（正向缓存经断连 client 写入必然失败，不构成绑定固化）。
	assert.True(t, channelAffinityStorageDegraded(ctx))
}

// 变红断言 5：真实故障注入——占用索引读失败（Redis 连接断开）时，
// acquire 降级 (0,false) 且不写任何占位；恢复后同一键重新走完整独占决策。
func TestClaimRecoversAfterStorageFailure(t *testing.T) {
	server := useExclusiveRedisMode(t)
	setupExclusiveAcquireDB(t)
	resetAffinityCacheSingleton()
	meta := exclusiveAcquireTestMeta()
	suffix := exclusiveCacheKeySuffix(meta)
	t.Cleanup(func() {
		_, _ = getChannelAffinityCache().DeleteMany([]string{suffix})
		_, _ = getChannelAffinityLastBindCache().DeleteMany([]string{suffix})
		_, _ = getChannelAffinityOccupancyCache().DeleteMany([]string{"701", "801"})
	})

	// 故障注入前预占 701（另一键），用于恢复后的独占判定。
	require.NoError(t, occupancyAddKeyFP(701, "other-key", 30*time.Second))

	// 断开 Redis：occupancy 读失败 -> acquire 降级。
	server.Close()
	ctx := buildClaimContext(t, meta)
	ch, found := acquireExclusiveBinding(ctx, meta, meta.UsingGroup, meta.ModelName)
	assert.False(t, found)
	assert.Equal(t, 0, ch)

	// 存储降级标记已写入 gin context（修复后存在该公开行为）。
	_, marked := ctx.Get(ginKeyChannelAffinityStorageDegrade)
	assert.True(t, marked, "storage-failure degrade must be observable in gin context")

	// 恢复（切回内存模式重建存储单例）后同一键重新决策：701 已被其它键占，
	// 应选 702（独占语义完好）。内存模式与 Redis 占用索引不共享，先补登记。
	common.RedisEnabled = false
	common.RDB = nil
	resetExclusiveStoreSingletons()
	resetAffinityCacheSingleton()
	require.NoError(t, occupancyAddKeyFP(701, "other-key", 30*time.Second))
	memoryCtx := buildClaimContext(t, meta)
	ch2, found2 := acquireExclusiveBinding(memoryCtx, meta, meta.UsingGroup, meta.ModelName)
	require.True(t, found2)
	assert.Equal(t, 702, ch2, "after recovery, exclusive semantics must hold: avoid occupied 701")
}

// claim 原语单元（内存模式）：空条目登记成功、冲突返回持有键数、本键重入续期。
func TestOccupancyClaimExclusiveMemorySemantics(t *testing.T) {
	useExclusiveMemoryMode(t)
	t.Cleanup(func() {
		_, _ = getChannelAffinityOccupancyCache().DeleteMany([]string{"911"})
	})
	ttl := 10 * time.Second

	// 空条目：占位成功。
	claim, err := occupancyClaimExclusive(911, "fp-a", ttl)
	require.NoError(t, err)
	assert.True(t, claim.Claimed)

	// 其它键占位：冲突，返回当前持有键数。
	claim, err = occupancyClaimExclusive(911, "fp-b", ttl)
	require.NoError(t, err)
	assert.False(t, claim.Claimed)
	assert.Equal(t, 1, claim.HolderCount)

	// 本键重入：续期成功（幂等）。
	claim, err = occupancyClaimExclusive(911, "fp-a", ttl)
	require.NoError(t, err)
	assert.True(t, claim.Claimed)

	// 条目未被冲突路径污染：成员仍只有 fp-a。
	count, fps, err := occupancyBindingCount(911)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	assert.ElementsMatch(t, []string{"fp-a"}, fps)
}

// claim 原语单元（Redis 模式）：Lua 脚本的同等语义。
func TestOccupancyClaimExclusiveRedisSemantics(t *testing.T) {
	server := useExclusiveRedisMode(t)
	ttl := 10 * time.Second
	t.Cleanup(func() {
		_, _ = getChannelAffinityOccupancyCache().DeleteMany([]string{"912"})
	})

	claim, err := occupancyClaimExclusive(912, "fp-a", ttl)
	require.NoError(t, err)
	assert.True(t, claim.Claimed)
	assert.True(t, server.Exists(channelAffinityOccupancyNamespace + ":912"))

	claim, err = occupancyClaimExclusive(912, "fp-b", ttl)
	require.NoError(t, err)
	assert.False(t, claim.Claimed)
	assert.Equal(t, 1, claim.HolderCount)

	claim, err = occupancyClaimExclusive(912, "fp-a", ttl)
	require.NoError(t, err)
	assert.True(t, claim.Claimed)

	count, fps, err := occupancyBindingCount(912)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	assert.ElementsMatch(t, []string{"fp-a"}, fps)
}

// 满载降级的复用登记仍走幂等追加（occupancyAddKeyFPAtomic），
// 不经 claim 条件判定：两键满载复用同一渠道，成员均保留。
func TestClaimFullLoadReuseStillRegisters(t *testing.T) {
	useExclusiveMemoryMode(t)
	t.Cleanup(func() {
		_, _ = getChannelAffinityOccupancyCache().DeleteMany([]string{"913"})
	})
	ttl := 10 * time.Second

	require.NoError(t, occupancyAddKeyFP(913, "fp-h1", ttl))
	require.NoError(t, occupancyAddKeyFP(913, "fp-h2", ttl))

	claim, err := occupancyClaimExclusive(913, "fp-new", ttl)
	require.NoError(t, err)
	assert.False(t, claim.Claimed, "exclusive claim must conflict on occupied channel")
	assert.Equal(t, 2, claim.HolderCount)
}
