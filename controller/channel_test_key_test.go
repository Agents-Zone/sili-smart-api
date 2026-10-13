package controller

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildSingleKeyTestChannel(t *testing.T) {
	multiKeyInfo := func() model.ChannelInfo {
		return model.ChannelInfo{
			IsMultiKey:           true,
			MultiKeyMode:         "polling",
			MultiKeySize:         2,
			MultiKeyPollingIndex: 1,
		}
	}

	tests := []struct {
		name             string
		channel          *model.Channel
		keyIndex         int
		wantErr          string
		wantKey          string
		wantPollingIndex int
	}{
		{
			name: "success",
			channel: &model.Channel{
				Key:          "sk-a\nsk-b",
				ChannelInfo:  multiKeyInfo(),
			},
			keyIndex:         1,
			wantKey:          "sk-b",
			wantPollingIndex: 1,
		},
		{
			name: "success_disabled_key",
			channel: func() *model.Channel {
				info := multiKeyInfo()
				info.MultiKeyStatusList = map[int]int{1: common.ChannelStatusManuallyDisabled}
				return &model.Channel{Key: "sk-a\nsk-b", ChannelInfo: info}
			}(),
			keyIndex:         1,
			wantKey:          "sk-b",
			wantPollingIndex: 1,
		},
		{
			name: "success_with_cached_keys",
			channel: &model.Channel{
				Key:          "sk-a\nsk-b",
				Keys:         []string{"sk-a", "sk-b"},
				ChannelInfo:  multiKeyInfo(),
			},
			keyIndex:         1,
			wantKey:          "sk-b",
			wantPollingIndex: 1,
		},
		{
			name: "not_multi_key",
			channel: &model.Channel{
				Key:         "sk-a\nsk-b",
				ChannelInfo: model.ChannelInfo{IsMultiKey: false},
			},
			keyIndex: 0,
			wantErr:  "该渠道不是多密钥模式",
		},
		{
			name: "index_out_of_range_negative",
			channel: &model.Channel{
				Key:         "sk-a\nsk-b",
				ChannelInfo: multiKeyInfo(),
			},
			keyIndex: -1,
			wantErr:  "密钥索引超出范围",
		},
		{
			name: "index_out_of_range_equal_len",
			channel: &model.Channel{
				Key:         "sk-a\nsk-b",
				ChannelInfo: multiKeyInfo(),
			},
			keyIndex: 2,
			wantErr:  "密钥索引超出范围",
		},
		{
			name: "keys_shorter_than_size",
			channel: &model.Channel{
				Key: "sk-a\nsk-b",
				ChannelInfo: model.ChannelInfo{
					IsMultiKey:   true,
					MultiKeySize: 5,
				},
			},
			keyIndex: 3,
			wantErr:  "密钥索引超出范围",
		},
		{
			name: "size_shorter_than_keys",
			channel: &model.Channel{
				Key: "sk-a\nsk-b",
				ChannelInfo: model.ChannelInfo{
					IsMultiKey:   true,
					MultiKeySize: 1,
				},
			},
			keyIndex: 1,
			wantErr:  "密钥索引超出范围",
		},
		{
			name:     "nil_channel",
			channel:  nil,
			keyIndex: 0,
			wantErr:  "渠道不存在",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			view, err := buildSingleKeyTestChannel(tt.channel, tt.keyIndex)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				assert.Nil(t, view)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, view)

			// 视图直连指定序号密钥并关闭多密钥标记
			assert.Equal(t, tt.wantKey, view.Key)
			assert.Nil(t, view.Keys)
			assert.False(t, view.ChannelInfo.IsMultiKey)
			assert.Equal(t, tt.wantPollingIndex, view.ChannelInfo.MultiKeyPollingIndex)

			// 单密钥直取：不推进轮询索引，不写回 ChannelInfo
			key, idx, keyErr := view.GetNextEnabledKey()
			require.Nil(t, keyErr)
			assert.Equal(t, tt.wantKey, key)
			assert.Equal(t, 0, idx)
			assert.Equal(t, tt.wantPollingIndex, view.ChannelInfo.MultiKeyPollingIndex)

			// 原对象保持不变（缓存渠道为共享指针，禁止原地修改）
			assert.Equal(t, "sk-a\nsk-b", tt.channel.Key)
			assert.True(t, tt.channel.ChannelInfo.IsMultiKey)
		})
	}
}
