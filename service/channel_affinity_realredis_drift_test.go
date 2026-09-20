package service

// 真实 Redis 下的生产缺陷形态复现：软规则键集反复"正向过期 + 随机重绑"（Codex/
// Claude 会话漂移的生产形态）后，occupancy 分布必须与正向绑定分布精确一致。
// 修复前漂移键在旧渠道残留指纹，且条目被活跃键续期导致残留永不过期，占用虚高
// 使独占判定误判满载，shared 集中复用形成"渠道富裕却多人共享"的堆积。
// 通过 AFFINITY_REAL_REDIS 启用，未设置时跳过。

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRealRedisSoftRuleDriftConsistency 8 键 6 渠道、4 轮漂移（每轮一半键过期
// 重绑随机渠道）后：occupancy == 正向分布、总量守恒、无单渠道堆积。
func TestRealRedisSoftRuleDriftConsistency(t *testing.T) {
	useRealRedisMode(t)
	useSoftPlacementFixture(t, false)

	keys := []string{"rd-1", "rd-2", "rd-3", "rd-4", "rd-5", "rd-6", "rd-7", "rd-8"}
	for _, key := range keys {
		mirrorDistributorRequest(t, key)
	}

	for round := 0; round < 4; round++ {
		for i, key := range keys {
			if (i+round)%2 != 0 {
				continue
			}
			// 正向 TTL 过期（删正向键，lastBind 两周期寿命保留）。
			_, err := getChannelAffinityCache().DeleteMany([]string{affinityKeySuffix(key)})
			require.NoError(t, err)
			ctx := realRedisRequestCtx(key)
			_, found := GetPreferredChannelByAffinity(ctx, "gpt-4", "default")
			require.False(t, found)
			// 随机重绑（distributor 随机选路 + RecordChannelAffinity 固化）。
			pick := softPlacementChannels[common.GetRandomInt(len(softPlacementChannels))]
			ctx.Set("channel_id", pick)
			RecordChannelAffinity(ctx, pick)
		}
	}

	forward := map[int]int{}
	for _, key := range keys {
		forward[forwardBinding(t, key)]++
	}
	total := 0
	dist := map[int]int{}
	for _, id := range softPlacementChannels {
		c := channelMemberCount(t, id)
		dist[id] = c
		total += c
		assert.Equal(t, forward[id], c,
			"occupancy on channel %d must equal forward bindings after drift", id)
	}
	t.Logf("real-redis drift distribution: %v", dist)

	assert.Equal(t, len(keys), total, "total occupancy must equal key count after drift rounds")
}
