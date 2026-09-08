package service

// 回归回路：Redis 模式下 occupancyRemoveKeyFP 的读-改-写竞态（成员丢失）。
// 症状（生产观察）：渠道大量空闲时，多个亲和键并发迁移/回滚/登记同一渠道，
// 移除路径的 GET→过滤→SET 回写覆盖并发登记，成员丢失后该渠道被误判空闲，
// 新键独占 claim 成功，与既有持有键真实同占一个渠道（无 exclusive_degrade 标记）。
// 断言（修复前红、修复后绿）：N 个移除与 M 个登记并发完成后，成员守恒：
// 最终成员 = 预登记的 N 个被移除指纹之外，并发登记的 M 个指纹全部存活。

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOccupancyRemoveVsAddRedisMemberConservation 并发移除与登记：成员守恒。
// 预登记 20 个持有指纹，随后 20 个并发 remove（各自移除自己的指纹）与
// 20 个并发原子 add（occupancyAddKeyFPAtomic，模拟其它键的登记/续期）交错。
// 修复前：remove 的 GET→SET 回写覆盖并发 add 的成员 → 最终成员 < 期望。
func TestOccupancyRemoveVsAddRedisMemberConservation(t *testing.T) {
	server := useExclusiveRedisMode(t)
	_ = server
	ttl := 30 * time.Second
	t.Cleanup(func() {
		_, _ = getChannelAffinityOccupancyCache().DeleteMany([]string{"951"})
	})

	const holders = 20
	const joiners = 20

	// 预登记 holders 个持有指纹。
	for i := 0; i < holders; i++ {
		require.NoError(t, occupancyAddKeyFP(951, fmt.Sprintf("holder-%02d", i), ttl))
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	// 并发移除：每个持有键迁移离开 951（失败重试迁移场景）。
	for i := 0; i < holders; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			_ = occupancyRemoveKeyFP(951, fmt.Sprintf("holder-%02d", idx))
		}(i)
	}
	// 并发登记：joiners 个新键满载降级/软亲和登记到 951。
	for i := 0; i < joiners; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			_ = occupancyAddKeyFPAtomic(951, fmt.Sprintf("joiner-%02d", idx), ttl)
		}(i)
	}
	close(start)
	wg.Wait()

	count, fps, err := occupancyBindingCount(951)
	require.NoError(t, err)
	t.Logf("redis remove-vs-add race: 951 final members=%d: %v", count, fps)
	// 期望：joiner 全部存活（holder 已全部移除）。
	found := make(map[string]bool, len(fps))
	for _, fp := range fps {
		found[fp] = true
	}
	for i := 0; i < joiners; i++ {
		assert.True(t, found[fmt.Sprintf("joiner-%02d", i)],
			"joiner-%02d member lost by concurrent remove read-modify-write: channel=951", i)
	}
}
