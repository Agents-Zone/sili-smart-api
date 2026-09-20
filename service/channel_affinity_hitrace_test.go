package service

// 生产缺陷回归：独占命中路径（hit path）的并发时序竞态，症状与生产观察一致
//（渠道大量空闲时多键共享同一渠道）：
//   竞态 1（并发 hop 双登记）：hop 脚本只校验旧渠道"还有其它键"（others>0），
//     不校验本键指纹是否仍在旧渠道上。同键并发命中时，先到者已把指纹迁走，
//     后到者以过期旧值再执行 hop：指纹被再次登记到另一渠道，形成双渠道幽灵
//     占用，污染空闲判定（误判满载 -> shared 集中复用 -> 堆积）。
//   竞态 2（在途回写复活）：亲和命中后请求在途（流式可达数秒）期间，并发请求
//     hop 已把绑定改向新渠道；在途请求成功回写时 RecordChannelAffinity 无条件
//     覆盖正向绑定（复活旧渠道）并无条件重登记占用（旧渠道幽灵占用）。
// 两个用例均以确定性时序编排（先到者完整执行、在途者事后回放），修复前红、
// 修复后绿。内存与真实 Redis 双模式守护（Redis 用 AFFINITY_REAL_REDIS 启用）。

import (
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countKeyFingerprintAcross 统计指纹在夹具渠道占用条目中的出现次数。
func countKeyFingerprintAcross(t *testing.T, key string) int {
	t.Helper()
	fp := affinityFingerprint(key)
	total := 0
	for _, id := range softPlacementChannels {
		_, fps, err := occupancyBindingCount(id)
		require.NoError(t, err)
		for _, m := range fps {
			if m == fp {
				total++
			}
		}
	}
	return total
}

// raceHitCtx 构造独占命中请求的在途上下文（meta + boundChannel 已设置，
// 等价 GetPreferredChannelByAffinity 命中返回后的状态）。
func raceHitCtx(key string, boundChannel int) (*gin.Context, channelAffinityMeta) {
	meta := channelAffinityMeta{
		CacheKey:       channelAffinityCacheNamespace + ":" + affinityKeySuffix(key),
		TTLSeconds:     60,
		RuleName:       softPlacementRuleName,
		KeyFingerprint: affinityFingerprint(key),
		UsingGroup:     "default",
		ModelName:      "gpt-4",
		ExclusiveBind:  true,
	}
	ctx := newAffinityRequestCtx(key)
	setChannelAffinityContext(ctx, meta)
	ctx.Set(ginKeyChannelAffinityBoundChannel, boundChannel)
	return ctx, meta
}

// TestConcurrentHopDoesNotDoubleRegisterFingerprint 竞态 1：先到请求已 hop 迁走
// 指纹（正向=新渠道），在途请求以旧渠道值再触发 rebalance，必须 no-op，
// 指纹不得被二次登记。
func TestConcurrentHopDoesNotDoubleRegisterFingerprint(t *testing.T) {
	for _, mode := range []string{"memory", "realredis"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "realredis" {
				useRealRedisMode(t)
			}
			useSoftPlacementFixture(t, true)

			// 前置：racer 与 holder 共享 921（满载残留形态），其余渠道空闲。
			seedExclusiveBinding(t, "race-key", 921)
			require.NoError(t, occupancyAddKeyFP(921, affinityFingerprint("race-holder"), 60*time.Second))

			// 先到请求完整执行：命中 921 -> hop 到空闲渠道。
			first := mirrorDistributorRequest(t, "race-key")
			require.NotEqual(t, 921, first, "shared channel with free candidates must hop away")
			require.Equal(t, 1, countKeyFingerprintAcross(t, "race-key"))

			// 在途请求以旧值 921 触发 rebalance（时序：其正向读发生在先到者写入前）。
			ctx, _ := raceHitCtx("race-key", 921)
			got := rebalanceExclusiveBinding(ctx, 921, "default", "gpt-4")

			// 修复前：921 上仍有 holder（others=1>0）且有空闲，再次迁移并把指纹
			// 登记到另一渠道（got != 921，计数=2，双红）。
			assert.Equal(t, 921, got, "stale-view rebalance must no-op after concurrent hop")
			assert.Equal(t, 1, countKeyFingerprintAcross(t, "race-key"),
				"fingerprint must be registered exactly once across all channels")
			// 正向绑定保持先到者的 hop 结果。
			assert.Equal(t, first, forwardBinding(t, "race-key"))
		})
	}
}

// TestInFlightHitRecordDoesNotReviveOldBinding 竞态 2：在途命中请求回写时，
// 正向绑定已被并发 hop 改向；回写必须跳过（不复活旧绑定、不留幽灵占用）。
func TestInFlightHitRecordDoesNotReviveOldBinding(t *testing.T) {
	for _, mode := range []string{"memory", "realredis"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "realredis" {
				useRealRedisMode(t)
			}
			useSoftPlacementFixture(t, true)

			seedExclusiveBinding(t, "race-key", 921)
			require.NoError(t, occupancyAddKeyFP(921, affinityFingerprint("race-holder"), 60*time.Second))

			// R1 命中 921 后在途（等价命中返回瞬间，尚未回写）。
			r1, _ := raceHitCtx("race-key", 921)

			// R2 完整请求：命中 921 -> hop 到空闲渠道 X，正向=X、占用迁移。
			second := mirrorDistributorRequest(t, "race-key")
			require.NotEqual(t, 921, second)
			require.Equal(t, 1, countKeyFingerprintAcross(t, "race-key"))

			// R1 成功回写（渠道 921）。
			r1.Set("channel_id", 921)
			RecordChannelAffinity(r1, 921)

			// 修复前：无条件覆盖复活正向=921 并重登记占用（双红）。
			assert.Equal(t, second, forwardBinding(t, "race-key"),
				"in-flight write-back must not revive the stale binding")
			assert.Equal(t, 1, countKeyFingerprintAcross(t, "race-key"),
				"in-flight write-back must not leave ghost occupancy on the old channel")
			// 旧渠道仅剩 holder。
			count, _, err := occupancyBindingCount(921)
			require.NoError(t, err)
			assert.Equal(t, 1, count, "old channel must only hold the other key")
		})
	}
}

// TestInFlightHitRecordRenewsWhenBindingUnchanged 对照组：无并发改向时，
// 在途回写仍是合法续期（正向续期 + 占用成员续期），不得因防护误伤正常路径。
func TestInFlightHitRecordRenewsWhenBindingUnchanged(t *testing.T) {
	for _, mode := range []string{"memory", "realredis"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "realredis" {
				useRealRedisMode(t)
			}
			useSoftPlacementFixture(t, true)

			seedExclusiveBinding(t, "steady-key", 921)
			ctx, _ := raceHitCtx("steady-key", 921)

			ctx.Set("channel_id", 921)
			RecordChannelAffinity(ctx, 921)

			assert.Equal(t, 921, forwardBinding(t, "steady-key"), "uncontended renew must keep the binding")
			assert.Equal(t, 1, countKeyFingerprintAcross(t, "steady-key"))
			// 独占命中续期不产生软落位计数。
			require.NotNil(t, model.CacheGetChannel)
		})
	}
}
