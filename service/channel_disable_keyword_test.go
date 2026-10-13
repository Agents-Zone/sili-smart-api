package service

// 自动禁用关键词判定的回归测试：覆盖默认英文关键词、自定义中文关键词（如"余额不足"）
// 与状态码范围判定，守护 ShouldDisableChannel 的关键词匹配行为。

import (
	"fmt"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/stretchr/testify/require"
)

func TestShouldDisableBalanceErrors(t *testing.T) {
	common.AutomaticDisableChannelEnabled = true
	t.Cleanup(func() { common.AutomaticDisableChannelEnabled = false })

	cases := []struct {
		name    string
		msg     string
		code    int
		want    bool
	}{
		{"default keyword hit", `{"error":{"message":"Your credit balance is too low, please add credit...","type":"insufficient_quota"}}`, 402, true},
		{"quota exceeded", "You exceeded your current quota, please check your plan and billing details", 429, true},
		{"chinese insufficient balance", `{"error":{"message":"当前分组下负载已饱和或余额不足，请检查账户余额","type":"quota_error"}}`, 402, false},
		{"unrelated 500", "internal server error", 500, false},
		{"unrelated 400", "invalid request body", 400, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := types.NewErrorWithStatusCode(fmt.Errorf("%s", tc.msg), types.ErrorCodeBadResponse, tc.code)
			require.Equal(t, tc.want, ShouldDisableChannel(err))
		})
	}
}

func TestShouldDisableWithCustomChineseKeyword(t *testing.T) {
	common.AutomaticDisableChannelEnabled = true
	t.Cleanup(func() { common.AutomaticDisableChannelEnabled = false })

	orig := operation_setting.AutomaticDisableKeywords
	operation_setting.AutomaticDisableKeywordsFromString("余额不足\ninsufficient balance")
	t.Cleanup(func() { operation_setting.AutomaticDisableKeywords = orig })

	err := types.NewErrorWithStatusCode(fmt.Errorf("上游返回: 账户余额不足，请充值后重试"), types.ErrorCodeBadResponse, 402)
	require.True(t, ShouldDisableChannel(err))
}

// 线上事故回归：管理员从消费/错误日志复制带 status_code 前缀的完整错误文案
// （如 "status_code=429, [1113][余额不足或无可用资源包,请充值。]"）作为关键词，
// 关键词匹配输入必须与日志展示口径一致，否则照抄日志的关键词永不命中。
func TestShouldDisableKeywordCopiedFromLoggedErrorText(t *testing.T) {
	common.AutomaticDisableChannelEnabled = true
	t.Cleanup(func() { common.AutomaticDisableChannelEnabled = false })

	orig := operation_setting.AutomaticDisableKeywords
	operation_setting.AutomaticDisableKeywordsFromString("status_code=429, [1113][余额不足或无可用资源包,请充值。]")
	t.Cleanup(func() { operation_setting.AutomaticDisableKeywords = orig })

	// 线上真实错误：err.Error() 为消息本体，日志展示为 ErrorWithStatusCode（带 status_code 前缀）
	err := types.NewErrorWithStatusCode(
		fmt.Errorf("[1113][余额不足或无可用资源包,请充值。][2026101314090897923053b79b4f90]"),
		types.ErrorCodeBadResponse, 429)
	require.True(t, ShouldDisableChannel(err))
}
