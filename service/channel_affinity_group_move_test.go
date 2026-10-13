package service

// 生产缺陷回归：渠道被移动到其它分组后，独占亲和绑定不失效。
// 症状（生产观察）：渠道换分组后原分组请求持续 503
// "No available channel for model XXX under group YYY (distributor)"，
// 即使原分组下仍有其它可用渠道。
// 根因：GetPreferredChannelByAffinity 命中独占绑定后仅检查渠道禁用状态
// （releaseDisabledAffinityBinding），渠道启用但已移出请求分组时绑定保留，
// distributor 独占分支（middleware/distributor.go）分组校验失败直接 503，
// 且每个后续请求重复同一判定，直到正向 TTL 过期（线上配置 7 天）或手动清缓存。
// 期望：绑定渠道已服务不了请求分组时，清旧绑定并从当前分组候选集重绑。

import (
	"testing"

	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// moveChannelToOtherGroup 把渠道移入 other 分组（DB 渠道与 Ability 同步 + 刷新缓存）。
func moveChannelToOtherGroup(t *testing.T, id int) {
	t.Helper()
	require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", id).
		Update("group", "other").Error)
	require.NoError(t, model.DB.Model(&model.Ability{}).Where("channel_id = ?", id).
		Update("group", "other").Error)
	model.InitChannelCache()
	t.Cleanup(func() { model.InitChannelCache() })
}

// 复现主场景：绑定渠道 921 移出 default 分组后，亲和应清旧绑定并重绑到
// 分组内其它渠道，而非继续返回 921 让 distributor 503。
func TestExclusiveAffinityRebindsWhenBoundChannelMovedOut(t *testing.T) {
	for _, mode := range []string{"memory", "redis", "realredis"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "realredis" {
				useRealRedisMode(t)
			} else if mode == "redis" {
				useExclusiveRedisMode(t)
			} else {
				useExclusiveMemoryMode(t)
			}
			useSoftPlacementFixture(t, true)
			seedExclusiveBinding(t, "moved", 921)
			moveChannelToOtherGroup(t, 921)

			id, found := GetPreferredChannelByAffinity(newAffinityRequestCtx("moved"), "gpt-4", "default")
			require.True(t, found)
			require.Contains(t, []int{922, 923, 924, 925, 926}, id,
				"绑定渠道移出分组后应重绑到分组内其它渠道")
			assert.Equal(t, 0, channelMemberCount(t, 921), "旧渠道占用索引应清理")
			assert.Equal(t, id, forwardBinding(t, "moved"))

			// 下一请求应继续命中新绑定（自愈后稳定）。
			next, found := GetPreferredChannelByAffinity(newAffinityRequestCtx("moved"), "gpt-4", "default")
			require.True(t, found)
			assert.Equal(t, id, next)
		})
	}
}

// 边界：分组下所有渠道都被移走时，绑定释放后无候选可绑，返回 miss 让
// distributor 走随机选路（随机同样无渠道时才 503，与无亲和行为一致）。
func TestExclusiveAffinityGroupExhaustedReturnsMiss(t *testing.T) {
	useExclusiveMemoryMode(t)
	useSoftPlacementFixture(t, true)
	seedExclusiveBinding(t, "exhausted", 921)
	for _, id := range softPlacementChannels {
		moveChannelToOtherGroup(t, id)
	}

	id, found := GetPreferredChannelByAffinity(newAffinityRequestCtx("exhausted"), "gpt-4", "default")
	assert.False(t, found)
	assert.Zero(t, id)
	assert.Equal(t, 0, channelMemberCount(t, 921), "无候选时也不应残留旧渠道占用")
}
