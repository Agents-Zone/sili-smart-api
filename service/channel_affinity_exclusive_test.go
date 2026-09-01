package service

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ChannelAffinityRule.ExclusiveBind 的 JSON 反序列化行为：
// 显式 true 落 true；旧 JSON 缺字段落零值 false（向后兼容）；
// 显式 false 落 false。
func TestChannelAffinityRuleExclusiveBindUnmarshal(t *testing.T) {
	tests := []struct {
		name string
		json string
		want bool
	}{
		{
			name: "explicit true",
			json: `{"name":"r1","model_regex":["^a$"],"path_regex":[],"key_sources":[{"type":"gjson","path":"metadata.user_id"}],"exclusive_bind":true}`,
			want: true,
		},
		{
			name: "missing field defaults to false",
			json: `{"name":"r1","model_regex":["^a$"],"path_regex":[],"key_sources":[{"type":"gjson","path":"metadata.user_id"}]}`,
			want: false,
		},
		{
			name: "explicit false",
			json: `{"name":"r1","model_regex":["^a$"],"path_regex":[],"key_sources":[{"type":"gjson","path":"metadata.user_id"}],"exclusive_bind":false}`,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var rule operation_setting.ChannelAffinityRule
			require.NoError(t, common.Unmarshal([]byte(tt.json), &rule))
			assert.Equal(t, tt.want, rule.ExclusiveBind)
		})
	}
}

// 默认内置规则（codex/claude cli trace）不启用独占绑定，保持零值 false。
func TestChannelAffinityBuiltinRulesExclusiveBindDefaultOff(t *testing.T) {
	setting := operation_setting.GetChannelAffinitySetting()
	require.NotEmpty(t, setting.Rules)
	for _, rule := range setting.Rules {
		assert.False(t, rule.ExclusiveBind, "builtin rule %s should default exclusive_bind to false", rule.Name)
	}
}
