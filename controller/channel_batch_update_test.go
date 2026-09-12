package controller

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
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
			name:    "ids 含 0",
			req:     ChannelBatchUpdateRequest{Ids: []int{1, 0}, Weight: ptr(1)},
			wantErr: "参数错误",
		},
		{
			name:    "ids 含负数",
			req:     ChannelBatchUpdateRequest{Ids: []int{-5}, Weight: ptr(1)},
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
		ids := make([]int, 200)
		for i := range ids {
			ids[i] = i + 1
		}
		req := ChannelBatchUpdateRequest{Ids: ids, Weight: ptr(1)}
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

// setupBatchUpdateHandlerTestDB 建立批量编辑 handler 用例的库：渠道/abilities 夹具库
// 额外迁移审计日志表（handler 成功后写 channel.update_batch）。
func setupBatchUpdateHandlerTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Log{}))
	return db
}

// seedBatchUpdateChannel 插入一条启用渠道，weight/models 决定后续 abilities 重建路径。
func seedBatchUpdateChannel(t *testing.T, db *gorm.DB, weight uint, models string) *model.Channel {
	t.Helper()
	w := weight
	channel := &model.Channel{
		Name:   "batch-target",
		Key:    "sk-test",
		Status: common.ChannelStatusEnabled,
		Models: models,
		Group:  "default",
		Weight: &w,
	}
	require.NoError(t, db.Create(channel).Error)
	return channel
}

func callBatchUpdateChannels(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/channel/batch/update", strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	BatchUpdateChannels(ctx)
	return recorder
}

type batchUpdateResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Data    struct {
		Count  int                               `json:"count"`
		Failed []model.ChannelBatchUpdateFailure `json:"failed"`
	} `json:"data"`
}

func TestBatchUpdateChannelsHandlerRejectsEmptyFields(t *testing.T) {
	t.Run("无生效字段", func(t *testing.T) {
		setupBatchUpdateHandlerTestDB(t)

		recorder := callBatchUpdateChannels(t, `{"ids":[1,2]}`)

		assert.Equal(t, http.StatusOK, recorder.Code)
		assert.JSONEq(t, `{"success":false,"message":"批量编辑至少需要填写一个字段"}`, recorder.Body.String())
	})

	t.Run("请求体非法 JSON", func(t *testing.T) {
		setupBatchUpdateHandlerTestDB(t)

		recorder := callBatchUpdateChannels(t, `{"ids":[1,2]`)

		assert.Equal(t, http.StatusOK, recorder.Code)
		assert.JSONEq(t, `{"success":false,"message":"参数错误"}`, recorder.Body.String())
	})

	// ids 元素须为正整数，非法元素在校验阶段即拒绝，不落审计日志（03 文档 2.2.1）
	t.Run("ids 含非正整数", func(t *testing.T) {
		db := setupBatchUpdateHandlerTestDB(t)

		recorder := callBatchUpdateChannels(t, `{"ids":[0,-5],"weight":3}`)

		assert.Equal(t, http.StatusOK, recorder.Code)
		assert.JSONEq(t, `{"success":false,"message":"参数错误"}`, recorder.Body.String())

		var auditCount int64
		require.NoError(t, db.Model(&model.Log{}).Where("content LIKE ?", "%Batch updated%").Count(&auditCount).Error)
		assert.Zero(t, auditCount)
	})
}

func TestBatchUpdateChannelsHandlerUpdatesAndReportsCount(t *testing.T) {
	db := setupBatchUpdateHandlerTestDB(t)
	first := seedBatchUpdateChannel(t, db, 5, "gpt-4o")
	second := seedBatchUpdateChannel(t, db, 5, "gpt-4o")

	body := fmt.Sprintf(`{"ids":[%d,%d],"weight":7,"remark":"batch"}`, first.Id, second.Id)
	recorder := callBatchUpdateChannels(t, body)

	require.Equal(t, http.StatusOK, recorder.Code)
	var response batchUpdateResponse
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	assert.True(t, response.Success)
	assert.Empty(t, response.Message)
	assert.Equal(t, 2, response.Data.Count)

	for _, id := range []int{first.Id, second.Id} {
		var stored model.Channel
		require.NoError(t, db.First(&stored, id).Error)
		require.NotNil(t, stored.Remark)
		assert.Equal(t, "batch", *stored.Remark)
		assert.Equal(t, 7, stored.GetWeight())
	}
}

func TestBatchUpdateChannelsHandlerAuditsFieldsAndValues(t *testing.T) {
	db := setupBatchUpdateHandlerTestDB(t)
	first := seedBatchUpdateChannel(t, db, 5, "gpt-4o")
	second := seedBatchUpdateChannel(t, db, 5, "gpt-4o")

	body := fmt.Sprintf(`{"ids":[%d,%d],"weight":7,"remark":"batch"}`, first.Id, second.Id)
	callBatchUpdateChannels(t, body)

	var auditLog model.Log
	require.NoError(t, db.Order("id desc").First(&auditLog).Error)
	var other struct {
		Op struct {
			Action string         `json:"action"`
			Params map[string]any `json:"params"`
		} `json:"op"`
	}
	require.NoError(t, common.UnmarshalJsonStr(auditLog.Other, &other))
	assert.Equal(t, "channel.update_batch", other.Op.Action)
	assert.Equal(t, float64(2), other.Op.Params["count"])
	// content 由 handler 实参形态渲染而来，守护模板 ${updated_fields} 的逗号连接形态（03 文档 2.1 处理流程 6）
	// 字段名按 03 文档 2.2 声明顺序生成：remark 早于 weight
	assert.Equal(t, "Batch updated 2 channels (fields: remark, weight)", auditLog.Content)

	var channelIDs []int
	raw, err := common.Marshal(other.Op.Params["channel_ids"])
	require.NoError(t, err)
	require.NoError(t, common.Unmarshal(raw, &channelIDs))
	assert.ElementsMatch(t, []int{first.Id, second.Id}, channelIDs)

	assert.Equal(t, "remark, weight", other.Op.Params["updated_fields"])

	var values map[string]any
	raw, err = common.Marshal(other.Op.Params["values"])
	require.NoError(t, err)
	require.NoError(t, common.Unmarshal(raw, &values))
	assert.Equal(t, float64(7), values["weight"])
	assert.Equal(t, "batch", values["remark"])
}

// TestBatchUpdateChannelsHandlerReportsFailureAndRollsBack 守护事务全成全败分支：
// 任一渠道失败即整体回滚，响应携带失败渠道明细（4.2.4 规则3）。
func TestBatchUpdateChannelsHandlerReportsFailureAndRollsBack(t *testing.T) {
	db := setupBatchUpdateHandlerTestDB(t)
	first := seedBatchUpdateChannel(t, db, 5, "gpt-4o")
	second := seedBatchUpdateChannel(t, db, 5, "gpt-4o")
	// 移除 abilities 表，令事务内的路由索引重建失败；weight 生效即走重建路径。
	require.NoError(t, db.Migrator().DropTable(&model.Ability{}))

	body := fmt.Sprintf(`{"ids":[%d,%d],"weight":7}`, first.Id, second.Id)
	recorder := callBatchUpdateChannels(t, body)

	require.Equal(t, http.StatusOK, recorder.Code)
	var response batchUpdateResponse
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	assert.False(t, response.Success)
	assert.Equal(t, "批量编辑失败，已全部回滚", response.Message)
	require.Len(t, response.Data.Failed, 1)
	assert.Contains(t, response.Data.Failed[0].Reason, "abilities")

	for _, id := range []int{first.Id, second.Id} {
		var stored model.Channel
		require.NoError(t, db.First(&stored, id).Error)
		assert.Equal(t, 5, stored.GetWeight())
	}
}

// 计划 T4 核心断言：审计模板按 action 渲染英文兜底文本，${updated_fields} 取逗号连接形态
func TestAuditContentENRendersBatchUpdate(t *testing.T) {
	assert.Equal(t,
		"Batch updated 3 channels (fields: weight, tag)",
		auditContentEN("channel.update_batch", map[string]interface{}{"count": 3, "updated_fields": "weight, tag"}))
}
