package service

// 满载残留收敛（rebalance hop）的内存模式回归：禁用扰动把键挤到少数渠道后，
// 渠道恢复、存活键命中续期时发现"共享 + 更优落点"即原子 hop，堆积在一个
// 请求周期内排空，而非等整 TTL 到期（生产症状：渠道大量空闲时多键集中共享
// 同一渠道）。Redis 模式的同等语义由真实 Redis 集成测试守护
//（TestRealRedisProductionSimChurn），此处守护内存模式路径。

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMemoryHopDrainsStackingAfterRestore：6 键绑 6 渠道后禁 2 渠道（键挤到
// 4 渠道），恢复后再各请求一轮，堆积必须显著排空（最大渠道键数下降），
// 且每键恰登记一次。
func TestMemoryHopDrainsStackingAfterRestore(t *testing.T) {
	useSoftPlacementFixture(t, true)

	keys := []string{"hop-m-1", "hop-m-2", "hop-m-3", "hop-m-4", "hop-m-5", "hop-m-6"}
	for _, k := range keys {
		mirrorDistributorRequest(t, k)
	}
	dist, total := func() (map[int]int, int) {
		d := map[int]int{}
		tt := 0
		for _, id := range softPlacementChannels {
			c := channelMemberCount(t, id)
			d[id] = c
			tt += c
		}
		return d, tt
	}()
	require.Equal(t, 6, total)
	for _, c := range dist {
		require.LessOrEqual(t, c, 1, "6 keys over 6 free channels must be pure exclusive")
	}

	// 禁 923/926：其上的键清占位重绑，6 键挤进 4 渠道（各 2）。
	setChannelsDisabled(t, 923, 926)
	for _, k := range keys {
		mirrorDistributorRequest(t, k)
	}
	// 恢复后每键再请求一轮：共享渠道（2 键）上的键应 hop 到空闲的 923/926，
	// 单轮后回到纯独占分布。
	setChannelsEnabled(t, 923, 926)
	for _, k := range keys {
		mirrorDistributorRequest(t, k)
	}

	maxC := 0
	total = 0
	final := map[int]int{}
	for _, id := range softPlacementChannels {
		c := channelMemberCount(t, id)
		final[id] = c
		total += c
		if c > maxC {
			maxC = c
		}
	}
	t.Logf("after restore round: %v", final)
	assert.Equal(t, 6, total, "every key must stay registered exactly once")
	assert.LessOrEqual(t, maxC, 1, "stacking must drain within one request round after channels return")
}
