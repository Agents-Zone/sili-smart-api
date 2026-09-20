package service

// 生产缺陷回归：占用索引成员级无过期 + 软规则漂移迁移缺失，导致占用虚高。
// 症状（生产观察）：渠道大量空闲时多个亲和键共享同一渠道（无满载理由）。
// 根因 1（成员级无过期）：occupancy 条目 TTL 按最近一次登记续期（04 文档 §4.1
//   设计内"成员残留随 TTL 收敛"），但渠道上有任意活跃键时条目被无限续期，
//   已漂移键的指纹永不过期。残留累积 -> 占用虚高 -> 独占判定误判满载 ->
//   shared 降级按最少绑定数集中复用 -> 堆积。
// 根因 2（软规则漂移不迁移）：软规则键正向缓存过期后随机选路固化新渠道，
//   RecordChannelAffinity 以 old=0 登记（不迁移旧渠道指纹）。旧渠道残留。
// 两个用例修复前红、修复后绿。

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newAffinityRequestCtx 构造带亲和 header 的裸请求上下文（走真实规则匹配）。
func newAffinityRequestCtx(key string) *gin.Context {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Request.Header.Set("X-Affinity-Key", key)
	return ctx
}

// 根因 2 复现：软规则键 K 绑 C1（occupancy[C1]+=fpK），正向缓存过期（lastBind
// 仍在两周期寿命内），K 再请求随机固化到 C2。旧渠道 C1 的 fpK 必须迁移移除，
// 否则 C1 占用虚高，后续独占键误判 C1 被占。
func TestSoftRuleRebindMigratesLegacyOccupancy(t *testing.T) {
	useSoftPlacementFixture(t, false)

	const key = "drift-soft-1"
	first := mirrorDistributorRequest(t, key)
	require.Contains(t, softPlacementChannels, first)
	require.Equal(t, 1, channelMemberCount(t, first))

	// 模拟正向 TTL 过期：删正向键，lastBind（两周期 TTL）保留。
	_, err := getChannelAffinityCache().DeleteMany([]string{affinityKeySuffix(key)})
	require.NoError(t, err)

	// 再请求：亲和 miss（软规则），随机固化到指定新渠道。
	ctx := newAffinityRequestCtx(key)
	_, found := GetPreferredChannelByAffinity(ctx, "gpt-4", "default")
	require.False(t, found, "forward cache expired, affinity must miss")

	var next int
	for _, id := range softPlacementChannels {
		if id != first {
			next = id
			break
		}
	}
	ctx.Set("channel_id", next)
	RecordChannelAffinity(ctx, next)

	assert.Equal(t, next, forwardBinding(t, key), "rebind must pin the new channel")
	assert.Equal(t, 1, channelMemberCount(t, next))
	// 核心断言：旧渠道指纹迁移移除（修复前残留，红）。
	count, fps, err := occupancyBindingCount(first)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "legacy occupancy on the old channel must be migrated away")
	assert.Empty(t, fps)
}

// 根因 1 复现：短寿命成员与长寿命成员同渠道，短成员逻辑过期后不得继续占用
// 计数、不得阻碍后续独占 claim。修复前条目被长成员续期、短成员残留（红）。
func TestExpiredOccupancyMemberDoesNotBlockClaim(t *testing.T) {
	for _, mode := range []string{"memory", "redis"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "redis" {
				useExclusiveRedisMode(t)
			} else {
				useExclusiveMemoryMode(t)
			}
			resetAffinityCacheSingleton()
			t.Cleanup(func() {
				_, _ = getChannelAffinityOccupancyCache().DeleteMany([]string{"801"})
			})

			require.NoError(t, occupancyAddKeyFP(801, "fp-short", 80*time.Millisecond))

			time.Sleep(200 * time.Millisecond)

			count, fps, err := occupancyBindingCount(801)
			require.NoError(t, err)
			assert.Equal(t, 0, count, "expired member must not inflate occupancy count")
			assert.Empty(t, fps)

			claim, err := occupancyClaimExclusive(801, "fp-new", 30*time.Second)
			require.NoError(t, err)
			assert.True(t, claim.Claimed,
				"expired member must not block exclusive claim on the channel")
		})
	}
}

// 端到端复现（生产形态）：软规则键集反复"正向过期 + 随机重绑"后，
// occupancy 渠道分布必须与正向绑定分布精确一致（无残留虚高）。
// 修复前漂移键残留旧渠道，占用总量 > 键数（红）。
func TestSoftRuleChurnKeepsOccupancyConsistent(t *testing.T) {
	useSoftPlacementFixture(t, false)

	keys := []string{"cc-1", "cc-2", "cc-3", "cc-4", "cc-5", "cc-6", "cc-7", "cc-8"}
	for _, key := range keys {
		mirrorDistributorRequest(t, key)
	}

	// 漂移轮：每轮挑一半键过期正向（保 lastBind），再请求随机重绑。
	for round := 0; round < 4; round++ {
		for i, key := range keys {
			if (i+round)%2 != 0 {
				continue
			}
			_, err := getChannelAffinityCache().DeleteMany([]string{affinityKeySuffix(key)})
			require.NoError(t, err)
			ctx := newAffinityRequestCtx(key)
			_, found := GetPreferredChannelByAffinity(ctx, "gpt-4", "default")
			require.False(t, found)
			// 随机固化：与 distributor 随机选路同分布（等权随机）。
			pick := softPlacementChannels[common.GetRandomInt(len(softPlacementChannels))]
			ctx.Set("channel_id", pick)
			RecordChannelAffinity(ctx, pick)
		}
	}

	// 终态断言：occupancy 分布 == 正向绑定分布。
	forward := map[int]int{}
	for _, key := range keys {
		forward[forwardBinding(t, key)]++
	}
	total := 0
	for _, id := range softPlacementChannels {
		c := channelMemberCount(t, id)
		assert.Equal(t, forward[id], c,
			"occupancy on channel %d must equal forward bindings (no stale members)", id)
		total += c
	}
	assert.Equal(t, len(keys), total, "total occupancy must equal key count (each key registered once)")
}
