package controller

import (
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func ptr[T any](v T) *T {
	return &v
}

func TestBuildBatchUpdateFieldsRejectsInvalidInput(t *testing.T) {
	cases := []struct {
		name    string
		req     ChannelBatchUpdateRequest
		wantErr string
	}{
		{
			name:    "ids 为空数组",
			req:     ChannelBatchUpdateRequest{Ids: []int{}, Weight: ptr(1)},
			wantErr: "参数错误",
		},
		{
			name:    "ids 为 nil",
			req:     ChannelBatchUpdateRequest{Weight: ptr(1)},
			wantErr: "参数错误",
		},
		{
			name:    "ids 超过 200 条",
			req:     ChannelBatchUpdateRequest{Ids: make([]int, 201), Weight: ptr(1)},
			wantErr: "单次批量编辑上限 200 条，请分批操作",
		},
		{
			name:    "全部字段跳过",
			req:     ChannelBatchUpdateRequest{Ids: []int{1}},
			wantErr: "批量编辑至少需要填写一个字段",
		},
		{
			name:    "空白字符串视为未填写",
			req:     ChannelBatchUpdateRequest{Ids: []int{1}, Tag: ptr("  ")},
			wantErr: "批量编辑至少需要填写一个字段",
		},
		{
			name:    "group 超过 64 字符",
			req:     ChannelBatchUpdateRequest{Ids: []int{1}, Group: ptr(strings.Repeat("a", 65))},
			wantErr: "分组长度不能超过 64 字符",
		},
		{
			name:    "group 存在空白段",
			req:     ChannelBatchUpdateRequest{Ids: []int{1}, Group: ptr("default,,vip")},
			wantErr: "分组格式错误",
		},
		{
			name:    "group 尾随逗号产生空白段",
			req:     ChannelBatchUpdateRequest{Ids: []int{1}, Group: ptr("default,")},
			wantErr: "分组格式错误",
		},
		{
			name:    "tag 超过 191 字符",
			req:     ChannelBatchUpdateRequest{Ids: []int{1}, Tag: ptr(strings.Repeat("t", 192))},
			wantErr: "标签长度不能超过 191 字符",
		},
		{
			name:    "remark 超过 255 字符",
			req:     ChannelBatchUpdateRequest{Ids: []int{1}, Remark: ptr(strings.Repeat("r", 256))},
			wantErr: "备注长度不能超过 255 字符",
		},
		{
			name:    "test_model 超过 255 字符",
			req:     ChannelBatchUpdateRequest{Ids: []int{1}, TestModel: ptr(strings.Repeat("m", 256))},
			wantErr: "测试模型长度不能超过 255 字符",
		},
		{
			name:    "models 存在空白段",
			req:     ChannelBatchUpdateRequest{Ids: []int{1}, Models: ptr("a,,b")},
			wantErr: "模型列表格式错误",
		},
		{
			name:    "models 单个模型名超过 255 字符",
			req:     ChannelBatchUpdateRequest{Ids: []int{1}, Models: ptr(strings.Repeat("m", 256))},
			wantErr: "模型名称过长: " + strings.Repeat("m", 256),
		},
		{
			name:    "model_mapping 为 JSON 数组",
			req:     ChannelBatchUpdateRequest{Ids: []int{1}, ModelMapping: ptr("[1,2]")},
			wantErr: "模型重定向必须是合法的 JSON 对象",
		},
		{
			name:    "model_mapping 为 JSON 标量",
			req:     ChannelBatchUpdateRequest{Ids: []int{1}, ModelMapping: ptr("123")},
			wantErr: "模型重定向必须是合法的 JSON 对象",
		},
		{
			name:    "model_mapping 非法 JSON",
			req:     ChannelBatchUpdateRequest{Ids: []int{1}, ModelMapping: ptr("{oops")},
			wantErr: "模型重定向必须是合法的 JSON 对象",
		},
		{
			name:    "model_mapping 为 null",
			req:     ChannelBatchUpdateRequest{Ids: []int{1}, ModelMapping: ptr("null")},
			wantErr: "模型重定向必须是合法的 JSON 对象",
		},
		{
			name:    "model_mapping 空串",
			req:     ChannelBatchUpdateRequest{Ids: []int{1}, ModelMapping: ptr("")},
			wantErr: "模型重定向不能为空（如需清空请在单个编辑中操作）",
		},
		{
			name:    "model_mapping 空白串",
			req:     ChannelBatchUpdateRequest{Ids: []int{1}, ModelMapping: ptr("  ")},
			wantErr: "模型重定向不能为空（如需清空请在单个编辑中操作）",
		},
		{
			name:    "weight 为负",
			req:     ChannelBatchUpdateRequest{Ids: []int{1}, Weight: ptr(-1)},
			wantErr: "权重必须在 0-4294967295 之间",
		},
		{
			name:    "weight 超过 uint32 上限",
			req:     ChannelBatchUpdateRequest{Ids: []int{1}, Weight: ptr(4294967296)},
			wantErr: "权重必须在 0-4294967295 之间",
		},
		{
			name:    "auto_ban 取值非 0/1",
			req:     ChannelBatchUpdateRequest{Ids: []int{1}, AutoBan: ptr(2)},
			wantErr: "自动封禁取值错误",
		},
		{
			name:    "auto_ban 为负",
			req:     ChannelBatchUpdateRequest{Ids: []int{1}, AutoBan: ptr(-1)},
			wantErr: "自动封禁取值错误",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.req.buildBatchUpdateFields()
			require.Error(t, err)
			assert.Equal(t, tc.wantErr, err.Error())
		})
	}
}

func TestBuildBatchUpdateFieldsAcceptsValidInput(t *testing.T) {
	t.Run("归一化生效值并保留零值", func(t *testing.T) {
		req := ChannelBatchUpdateRequest{
			Ids:     []int{1},
			Group:   ptr(" default,vip "),
			Weight:  ptr(0),
			AutoBan: ptr(0),
		}
		fields, err := req.buildBatchUpdateFields()
		require.NoError(t, err)

		require.NotNil(t, fields.Group)
		assert.Equal(t, "default,vip", *fields.Group)
		require.NotNil(t, fields.Weight)
		assert.Equal(t, uint(0), *fields.Weight)
		require.NotNil(t, fields.AutoBan)
		assert.Equal(t, 0, *fields.AutoBan)
		assert.Nil(t, fields.Tag)
		assert.Nil(t, fields.Remark)
		assert.Nil(t, fields.Models)
		assert.Nil(t, fields.ModelMapping)
		assert.Nil(t, fields.Priority)
		assert.Nil(t, fields.TestModel)
	})

	t.Run("空白字段跳过 非空白字段 trim 后生效", func(t *testing.T) {
		req := ChannelBatchUpdateRequest{
			Ids:       []int{1},
			Remark:    ptr("  "),
			TestModel: ptr(" t "),
		}
		fields, err := req.buildBatchUpdateFields()
		require.NoError(t, err)

		assert.Nil(t, fields.Remark)
		require.NotNil(t, fields.TestModel)
		assert.Equal(t, "t", *fields.TestModel)
	})

	t.Run("边界值被接受", func(t *testing.T) {
		req := ChannelBatchUpdateRequest{
			Ids:          []int{1},
			Group:        ptr(strings.Repeat("a", 64)),
			Tag:          ptr(strings.Repeat("t", 191)),
			Remark:       ptr(strings.Repeat("r", 255)),
			TestModel:    ptr(strings.Repeat("m", 255)),
			Models:       ptr("gpt-4o, claude-3-5-sonnet"),
			ModelMapping: ptr(`{"gpt-4o":"gpt-4o-mini"}`),
			Weight:       ptr(4294967295),
		}
		fields, err := req.buildBatchUpdateFields()
		require.NoError(t, err)

		require.NotNil(t, fields.Group)
		assert.Len(t, *fields.Group, 64)
		require.NotNil(t, fields.Tag)
		assert.Len(t, *fields.Tag, 191)
		require.NotNil(t, fields.Remark)
		assert.Len(t, *fields.Remark, 255)
		require.NotNil(t, fields.TestModel)
		assert.Len(t, *fields.TestModel, 255)
		require.NotNil(t, fields.Models)
		assert.Equal(t, "gpt-4o, claude-3-5-sonnet", *fields.Models)
		require.NotNil(t, fields.ModelMapping)
		assert.Equal(t, `{"gpt-4o":"gpt-4o-mini"}`, *fields.ModelMapping)
		require.NotNil(t, fields.Weight)
		assert.Equal(t, uint(4294967295), *fields.Weight)
	})

	t.Run("priority 保留 int64 全域精度", func(t *testing.T) {
		const exact = int64(9007199254740993) // 2^53 + 1，经 float64 中转会变成 9007199254740992
		req := ChannelBatchUpdateRequest{Ids: []int{1}, Priority: ptr(exact)}
		fields, err := req.buildBatchUpdateFields()
		require.NoError(t, err)
		require.NotNil(t, fields.Priority)
		assert.Equal(t, exact, *fields.Priority)

		req = ChannelBatchUpdateRequest{Ids: []int{1}, Priority: ptr(int64(-9223372036854775808))}
		fields, err = req.buildBatchUpdateFields()
		require.NoError(t, err)
		require.NotNil(t, fields.Priority)
		assert.Equal(t, int64(-9223372036854775808), *fields.Priority)
	})

	t.Run("ids 恰好 200 条被接受", func(t *testing.T) {
		req := ChannelBatchUpdateRequest{Ids: make([]int, 200), Weight: ptr(1)}
		_, err := req.buildBatchUpdateFields()
		require.NoError(t, err)
	})
}

func TestChannelBatchUpdateRequestParsesIntegersExactly(t *testing.T) {
	t.Run("priority 大整数不经 float64 中转", func(t *testing.T) {
		var req ChannelBatchUpdateRequest
		require.NoError(t, common.UnmarshalJsonStr(`{"priority":9007199254740993}`, &req))
		require.NotNil(t, req.Priority)
		assert.Equal(t, int64(9007199254740993), *req.Priority)
	})

	t.Run("非整数输入被解析阶段拒绝", func(t *testing.T) {
		var req ChannelBatchUpdateRequest
		assert.Error(t, common.UnmarshalJsonStr(`{"priority":10.5}`, &req))
	})

	t.Run("未声明字段被忽略", func(t *testing.T) {
		var req ChannelBatchUpdateRequest
		require.NoError(t, common.UnmarshalJsonStr(`{"ids":[1],"weight":3,"key":"secret","type":1,"name":"n"}`, &req))
		require.NotNil(t, req.Weight)
		assert.Equal(t, 3, *req.Weight)
		fields, err := req.buildBatchUpdateFields()
		require.NoError(t, err)
		assert.Equal(t, uint(3), *fields.Weight)
	})
}
