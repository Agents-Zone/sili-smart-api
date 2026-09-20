package service

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExclusiveAffinityFailurePreservesBinding(t *testing.T) {
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
			seedExclusiveBinding(t, "stable", 921)
			ctx := newAffinityRequestCtx("stable")
			id, found := GetPreferredChannelByAffinity(ctx, "gpt-4", "default")
			require.True(t, found)
			require.Equal(t, 921, id)
			assert.True(t, ShouldSkipRetryAfterChannelAffinityFailure(ctx), "独占请求应停止跨渠道重试")
			RollbackChannelAffinityOnFinalFailure(ctx)
			assert.Equal(t, 921, forwardBinding(t, "stable"))
			assert.Equal(t, 1, channelMemberCount(t, 921))
			record, found, err := lastBindGet(affinityKeySuffix("stable"))
			require.NoError(t, err)
			require.True(t, found)
			assert.Equal(t, 921, record.ChannelID)
			assert.True(t, channelAffinityFinalFailed(ctx))
		})
	}
}

func TestExclusiveAffinityRebindsDisabledChannel(t *testing.T) {
	for _, mode := range []string{"memory", "redis", "realredis"} {
		for _, status := range []int{common.ChannelStatusManuallyDisabled, common.ChannelStatusAutoDisabled} {
			name := mode + "/manual"
			if status == common.ChannelStatusAutoDisabled {
				name = mode + "/auto"
			}
			t.Run(name, func(t *testing.T) {
				if mode == "realredis" {
					useRealRedisMode(t)
				} else if mode == "redis" {
					useExclusiveRedisMode(t)
				} else {
					useExclusiveMemoryMode(t)
				}
				useSoftPlacementFixture(t, true)
				operation_setting.GetChannelAffinitySetting().KeepOnChannelDisabled = true
				seedExclusiveBinding(t, "disabled", 921)
				seedExclusiveBinding(t, "other", 922)
				oldRequest, oldMeta := raceHitCtx("disabled", 921)
				require.True(t, model.UpdateChannelStatus(921, "", status, "测试禁用"))
				id, found := GetPreferredChannelByAffinity(newAffinityRequestCtx("disabled"), "gpt-4", "default")
				require.True(t, found)
				require.NotContains(t, []int{921, 922}, id)
				assert.Equal(t, 0, channelMemberCount(t, 921))
				assert.Equal(t, id, forwardBinding(t, "disabled"))
				// 另一请求仍携带旧渠道快照，延迟释放应保留已经建立的新绑定。
				disabled, err := releaseDisabledAffinityBinding(oldMeta, 921)
				require.NoError(t, err)
				assert.True(t, disabled)
				assert.Equal(t, id, forwardBinding(t, "disabled"))
				oldRequest.Set("channel_id", 921)
				RecordChannelAffinity(oldRequest, 921)
				RollbackChannelAffinityOnFinalFailure(oldRequest)
				assert.Equal(t, id, forwardBinding(t, "disabled"), "旧请求成功或失败均应保留新绑定")
				assert.Equal(t, 0, channelMemberCount(t, 921))
				setChannelsEnabled(t, 921)
				next, found := GetPreferredChannelByAffinity(newAffinityRequestCtx("disabled"), "gpt-4", "default")
				require.True(t, found)
				assert.Equal(t, id, next, "原渠道恢复后继续使用新绑定")
			})
		}
	}
}

func TestExclusiveAffinityInFlightCannotRecreateClearedBinding(t *testing.T) {
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
			seedExclusiveBinding(t, "cleared", 921)
			ctx, _ := raceHitCtx("cleared", 921)
			require.True(t, ClearCurrentChannelAffinityCache(ctx))
			ctx.Set("channel_id", 921)
			RecordChannelAffinity(ctx, 921)
			_, found, err := getChannelAffinityCache().Get(affinityKeySuffix("cleared"))
			require.NoError(t, err)
			assert.False(t, found)
			assert.Equal(t, 0, channelMemberCount(t, 921))
		})
	}
}
