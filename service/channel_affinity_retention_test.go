package service

import (
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAffinityRetainsPreviousChannelBeyondTwoBindingTTLs(t *testing.T) {
	for _, renew := range []bool{false, true} {
		name := "首次绑定"
		if renew {
			name = "成功续期"
		}
		t.Run(name, func(t *testing.T) {
			server := useExclusiveRedisMode(t)
			useSoftPlacementFixture(t, true)
			setting := operation_setting.GetChannelAffinitySetting()
			original := *setting
			t.Cleanup(func() { *setting = original })
			require.NoError(t, common.UnmarshalJsonStr(`{"last_bind_ttl_seconds":604800}`, setting))

			ctx := newAffinityRequestCtx("retained")
			id, found := GetPreferredChannelByAffinity(ctx, "gpt-4", "default")
			require.True(t, found)
			if renew {
				server.FastForward(30 * time.Second)
				RecordChannelAffinity(ctx, id)
			}
			server.FastForward(121 * time.Second)
			_, found, err := getChannelAffinityCache().Get(affinityKeySuffix("retained"))
			require.NoError(t, err)
			require.False(t, found, "有效绑定应已过期")
			record, found, err := lastBindGet(affinityKeySuffix("retained"))
			require.NoError(t, err)
			require.True(t, found, "最近绑定记录应超过两个绑定 TTL 后仍保留")
			assert.Equal(t, id, record.ChannelID)
			next, found := GetPreferredChannelByAffinity(newAffinityRequestCtx("retained"), "gpt-4", "default")
			require.True(t, found)
			assert.Equal(t, id, next, "原渠道可用且空闲时应回绑")
		})
	}
}

func TestAffinityRetentionConfigurationAndExpiry(t *testing.T) {
	for _, tc := range []struct {
		name    string
		seconds int
		want    time.Duration
	}{
		{"自定义一天", 86400, 24 * time.Hour},
		{"至少两个绑定周期", 1, 120 * time.Second},
		{"旧配置默认七天", 0, 7 * 24 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := useExclusiveRedisMode(t)
			useSoftPlacementFixture(t, true)
			setting := operation_setting.GetChannelAffinitySetting()
			previous := setting.LastBindTTLSeconds
			t.Cleanup(func() { setting.LastBindTTLSeconds = previous })
			setting.LastBindTTLSeconds = tc.seconds
			_, found := GetPreferredChannelByAffinity(newAffinityRequestCtx("expiry"), "gpt-4", "default")
			require.True(t, found)
			key := getChannelAffinityLastBindCache().FullKey(affinityKeySuffix("expiry"))
			assert.Equal(t, tc.want, server.TTL(key))
			server.FastForward(tc.want + time.Second)
			_, found, err := lastBindGet(affinityKeySuffix("expiry"))
			require.NoError(t, err)
			assert.False(t, found, "历史记录应按独立期限自然过期")
		})
	}
}

func TestAffinityRetainedChannelOccupiedPrefersFreeChannel(t *testing.T) {
	server := useExclusiveRedisMode(t)
	useSoftPlacementFixture(t, true)
	ctx := newAffinityRequestCtx("returning")
	id, found := GetPreferredChannelByAffinity(ctx, "gpt-4", "default")
	require.True(t, found)
	server.FastForward(121 * time.Second)
	seedExclusiveBinding(t, "new-holder", id)
	next, found := GetPreferredChannelByAffinity(newAffinityRequestCtx("returning"), "gpt-4", "default")
	require.True(t, found)
	assert.NotEqual(t, id, next, "历史偏好应让位于当前有效占用")
	assert.Equal(t, 1, channelMemberCount(t, id), "原渠道保留新持有者的独占")
}
