package controller

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
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

type channelKeyTestResponse struct {
	Success   bool    `json:"success"`
	Message   string  `json:"message"`
	Time      float64 `json:"time"`
	ErrorCode string  `json:"error_code"`
}

// TestTestChannelKeyValidation 覆盖 TestChannelKey 的业务校验分支：
// 参数解析失败、渠道不存在、非多密钥模式、密钥序号越界、渠道类型不支持测试。
// 上述分支均在发起上游请求前返回，无需真实上游。
// 共同断言：响应为 HTTP 200 + success=false + time=0 + 无 error_code，
// 且渠道记录的 MultiKeyPollingIndex 与 MultiKeyStatusList 保持原值（无写回）。
func TestTestChannelKeyValidation(t *testing.T) {
	setupRelayErrorDB(t)
	gin.SetMode(gin.TestMode)

	// CacheGetChannel 走 DB 读取
	origCacheEnabled := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() { common.MemoryCacheEnabled = origCacheEnabled })

	// 模拟渠道路由组注册形态：/test/:id 与 /test/:id/:key_index 同树共存
	withTestSession := func(h gin.HandlerFunc) gin.HandlerFunc {
		return func(c *gin.Context) {
			c.Set("id", 1) // AdminAuth 已解析的管理员会话，满足测试用户解析
			h(c)
		}
	}
	router := gin.New()
	router.GET("/test/:id", withTestSession(TestChannel))
	router.GET("/test/:id/:key_index", withTestSession(TestChannelKey))

	newChannel := func(mutate func(ch *model.Channel)) *model.Channel {
		ch := &model.Channel{
			Name:   "mk-test",
			Key:    "sk-a\nsk-b",
			Status: common.ChannelStatusEnabled,
			Models: "gpt-test",
			Group:  "default",
			ChannelInfo: model.ChannelInfo{
				IsMultiKey:           true,
				MultiKeySize:         2,
				MultiKeyMode:         "polling",
				MultiKeyPollingIndex: 1,
				MultiKeyStatusList:   map[int]int{1: common.ChannelStatusManuallyDisabled},
			},
		}
		if mutate != nil {
			mutate(ch)
		}
		require.NoError(t, model.DB.Create(ch).Error)
		return ch
	}

	tests := []struct {
		name       string
		channel    *model.Channel // nil 表示该场景不建渠道
		path       func(ch *model.Channel) string
		wantMsg    string // 精确匹配
		wantMsgSub string // 包含匹配
	}{
		{
			name:       "bad_param_key_index",
			path:       func(*model.Channel) string { return "/test/1/abc" },
			wantMsgSub: "invalid syntax",
		},
		{
			name:       "bad_param_channel_id",
			path:       func(*model.Channel) string { return "/test/abc/0" },
			wantMsgSub: "invalid syntax",
		},
		{
			name:    "channel_not_found",
			path:    func(*model.Channel) string { return "/test/999/0" },
			wantMsg: "渠道不存在",
		},
		{
			name: "not_multi_key",
			channel: newChannel(func(ch *model.Channel) {
				ch.Key = "sk-a"
				ch.ChannelInfo.IsMultiKey = false
			}),
			path:    func(ch *model.Channel) string { return fmt.Sprintf("/test/%d/0", ch.Id) },
			wantMsg: "该渠道不是多密钥模式",
		},
		{
			name:    "index_out_of_range",
			channel: newChannel(nil),
			path:    func(ch *model.Channel) string { return fmt.Sprintf("/test/%d/5", ch.Id) },
			wantMsg: "密钥索引超出范围",
		},
		{
			name:    "index_negative",
			channel: newChannel(nil),
			path:    func(ch *model.Channel) string { return fmt.Sprintf("/test/%d/-1", ch.Id) },
			wantMsg: "密钥索引超出范围",
		},
		{
			name: "unsupported_type",
			channel: newChannel(func(ch *model.Channel) {
				ch.Type = constant.ChannelTypeMidjourney
			}),
			path:       func(ch *model.Channel) string { return fmt.Sprintf("/test/%d/0", ch.Id) },
			wantMsgSub: "channel test is not supported",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tt.path(tt.channel), nil))

			require.Equal(t, http.StatusOK, w.Code)
			var resp channelKeyTestResponse
			require.NoError(t, common.Unmarshal(w.Body.Bytes(), &resp))
			assert.False(t, resp.Success)
			assert.Zero(t, resp.Time)
			if tt.wantMsg != "" {
				assert.Equal(t, tt.wantMsg, resp.Message)
			} else {
				assert.Contains(t, resp.Message, tt.wantMsgSub)
			}
			assert.Empty(t, resp.ErrorCode, "业务校验失败不携带 error_code")

			// 校验分支不写回 ChannelInfo：轮询索引与密钥状态列表保持原值
			if tt.channel != nil {
				after, err := model.GetChannelById(tt.channel.Id, true)
				require.NoError(t, err)
				assert.Equal(t, 1, after.ChannelInfo.MultiKeyPollingIndex, "轮询索引保持原值")
				assert.Equal(t, map[int]int{1: common.ChannelStatusManuallyDisabled}, after.ChannelInfo.MultiKeyStatusList, "密钥状态列表保持原值")
			}
		})
	}
}
