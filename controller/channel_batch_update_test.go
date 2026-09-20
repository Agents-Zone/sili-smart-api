package controller

import (
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/i18n"
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
	require.NoError(t, i18n.Init())
	cases := []struct {
		name    string
		req     ChannelBatchUpdateRequest
		wantErr string
	}{
		{
			name:    "ids 为空数组",
			req:     ChannelBatchUpdateRequest{Ids: []int{}, Weight: ptr(1)},
			wantErr: i18n.MsgInvalidParams,
		},
		{
			name:    "ids 为 nil",
			req:     ChannelBatchUpdateRequest{Weight: ptr(1)},
			wantErr: i18n.MsgInvalidParams,
		},
		{
			name:    "ids 含 0",
			req:     ChannelBatchUpdateRequest{Ids: []int{1, 0}, Weight: ptr(1)},
			wantErr: i18n.MsgInvalidParams,
		},
		{
			name:    "ids 含负数",
			req:     ChannelBatchUpdateRequest{Ids: []int{-5}, Weight: ptr(1)},
			wantErr: i18n.MsgInvalidParams,
		},
		{
			name:    "ids 超过 200 条",
			req:     ChannelBatchUpdateRequest{Ids: make([]int, 201), Weight: ptr(1)},
			wantErr: i18n.MsgChannelBatchLimit,
		},
		{
			name:    "全部字段跳过",
			req:     ChannelBatchUpdateRequest{Ids: []int{1}},
			wantErr: i18n.MsgChannelBatchNoFields,
		},
		{
			name:    "空白字符串视为未填写",
			req:     ChannelBatchUpdateRequest{Ids: []int{1}, Tag: ptr("  ")},
			wantErr: i18n.MsgChannelBatchNoFields,
		},
		{
			name:    "group 超过 64 字符",
			req:     ChannelBatchUpdateRequest{Ids: []int{1}, Group: ptr(strings.Repeat("a", 65))},
			wantErr: i18n.MsgChannelBatchGroupTooLong,
		},
		{
			name:    "group 超过 64 个汉字",
			req:     ChannelBatchUpdateRequest{Ids: []int{1}, Group: ptr(strings.Repeat("中", 65))},
			wantErr: i18n.MsgChannelBatchGroupTooLong,
		},
		{
			name:    "group 存在空白段",
			req:     ChannelBatchUpdateRequest{Ids: []int{1}, Group: ptr("default,,vip")},
			wantErr: i18n.MsgChannelBatchGroupInvalid,
		},
		{
			name:    "group 尾随逗号产生空白段",
			req:     ChannelBatchUpdateRequest{Ids: []int{1}, Group: ptr("default,")},
			wantErr: i18n.MsgChannelBatchGroupInvalid,
		},
		{
			name:    "tag 超过 191 字符",
			req:     ChannelBatchUpdateRequest{Ids: []int{1}, Tag: ptr(strings.Repeat("t", 192))},
			wantErr: i18n.MsgChannelBatchTagTooLong,
		},
		{
			name:    "remark 超过 255 字符",
			req:     ChannelBatchUpdateRequest{Ids: []int{1}, Remark: ptr(strings.Repeat("r", 256))},
			wantErr: i18n.MsgChannelBatchRemarkTooLong,
		},
		{
			name:    "test_model 超过 255 字符",
			req:     ChannelBatchUpdateRequest{Ids: []int{1}, TestModel: ptr(strings.Repeat("m", 256))},
			wantErr: i18n.MsgChannelBatchTestModelLong,
		},
		{
			name:    "models 存在空白段",
			req:     ChannelBatchUpdateRequest{Ids: []int{1}, Models: ptr("a,,b")},
			wantErr: i18n.MsgChannelBatchModelsInvalid,
		},
		{
			name:    "models 单个模型名超过 255 字符",
			req:     ChannelBatchUpdateRequest{Ids: []int{1}, Models: ptr(strings.Repeat("m", 256))},
			wantErr: i18n.MsgChannelBatchModelTooLong,
		},
		{
			name:    "model_mapping 为 JSON 数组",
			req:     ChannelBatchUpdateRequest{Ids: []int{1}, ModelMapping: ptr("[1,2]")},
			wantErr: i18n.MsgChannelBatchMappingInvalid,
		},
		{
			name:    "model_mapping 为 JSON 标量",
			req:     ChannelBatchUpdateRequest{Ids: []int{1}, ModelMapping: ptr("123")},
			wantErr: i18n.MsgChannelBatchMappingInvalid,
		},
		{
			name:    "model_mapping 非法 JSON",
			req:     ChannelBatchUpdateRequest{Ids: []int{1}, ModelMapping: ptr("{oops")},
			wantErr: i18n.MsgChannelBatchMappingInvalid,
		},
		{
			name:    "model_mapping 为 null",
			req:     ChannelBatchUpdateRequest{Ids: []int{1}, ModelMapping: ptr("null")},
			wantErr: i18n.MsgChannelBatchMappingInvalid,
		},
		{
			name:    "model_mapping 值非字符串",
			req:     ChannelBatchUpdateRequest{Ids: []int{1}, ModelMapping: ptr(`{"gpt-4o":123}`)},
			wantErr: i18n.MsgChannelBatchMappingValues,
		},
		{
			name:    "model_mapping 值为嵌套对象",
			req:     ChannelBatchUpdateRequest{Ids: []int{1}, ModelMapping: ptr(`{"gpt-4o":{"a":"b"}}`)},
			wantErr: i18n.MsgChannelBatchMappingValues,
		},
		{
			name:    "model_mapping 空串",
			req:     ChannelBatchUpdateRequest{Ids: []int{1}, ModelMapping: ptr("")},
			wantErr: i18n.MsgChannelBatchMappingEmpty,
		},
		{
			name:    "model_mapping 空白串",
			req:     ChannelBatchUpdateRequest{Ids: []int{1}, ModelMapping: ptr("  ")},
			wantErr: i18n.MsgChannelBatchMappingEmpty,
		},
		{
			name:    "weight 为负",
			req:     ChannelBatchUpdateRequest{Ids: []int{1}, Weight: ptr(-1)},
			wantErr: i18n.MsgChannelBatchWeightRange,
		},
		{
			name:    "weight 超过 uint32 上限",
			req:     ChannelBatchUpdateRequest{Ids: []int{1}, Weight: ptr(4294967296)},
			wantErr: i18n.MsgChannelBatchWeightRange,
		},
		{
			name:    "auto_ban 取值非 0/1",
			req:     ChannelBatchUpdateRequest{Ids: []int{1}, AutoBan: ptr(2)},
			wantErr: i18n.MsgChannelBatchAutoBanInvalid,
		},
		{
			name:    "auto_ban 为负",
			req:     ChannelBatchUpdateRequest{Ids: []int{1}, AutoBan: ptr(-1)},
			wantErr: i18n.MsgChannelBatchAutoBanInvalid,
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
			Group:   ptr(" default , vip "),
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

	t.Run("逗号列表段级归一化，段内空格不落库", func(t *testing.T) {
		req := ChannelBatchUpdateRequest{
			Ids:    []int{1},
			Group:  ptr(" vip , default "),
			Models: ptr(" gpt-4o , claude "),
		}
		fields, err := req.buildBatchUpdateFields()
		require.NoError(t, err)

		require.NotNil(t, fields.Group)
		assert.Equal(t, "vip,default", *fields.Group)
		require.NotNil(t, fields.Models)
		assert.Equal(t, "gpt-4o,claude", *fields.Models)
	})

	t.Run("边界值被接受且九个字段全量搬运", func(t *testing.T) {
		req := ChannelBatchUpdateRequest{
			Ids:          []int{1},
			Group:        ptr(strings.Repeat("a", 64)),
			Tag:          ptr(strings.Repeat("t", 191)),
			Remark:       ptr(strings.Repeat("r", 255)),
			TestModel:    ptr(strings.Repeat("m", 255)),
			Models:       ptr("gpt-4o, claude-3-5-sonnet"),
			ModelMapping: ptr(`{"gpt-4o":"gpt-4o-mini"}`),
			Weight:       ptr(4294967295),
			Priority:     ptr(int64(-1)),
			AutoBan:      ptr(1),
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
		// 段内空格经归一化去除：段级 trim 后逗号紧邻
		assert.Equal(t, "gpt-4o,claude-3-5-sonnet", *fields.Models)
		require.NotNil(t, fields.ModelMapping)
		assert.Equal(t, `{"gpt-4o":"gpt-4o-mini"}`, *fields.ModelMapping)
		require.NotNil(t, fields.Weight)
		assert.Equal(t, uint(4294967295), *fields.Weight)
		require.NotNil(t, fields.Priority)
		assert.Equal(t, int64(-1), *fields.Priority)
		require.NotNil(t, fields.AutoBan)
		assert.Equal(t, 1, *fields.AutoBan)
		// 九列全部进入生效集合，缺一列都会让审计 updated_fields 少一项
		assert.Equal(t,
			[]string{"group", "tag", "remark", "models", "model_mapping", "weight", "priority", "test_model", "auto_ban"},
			fields.EffectiveColumns())
	})

	t.Run("多字节字符按码点计数：上限内汉字放行", func(t *testing.T) {
		req := ChannelBatchUpdateRequest{
			Ids:       []int{1},
			Group:     ptr(strings.Repeat("中", 64)),
			Tag:       ptr(strings.Repeat("中", 191)),
			Remark:    ptr(strings.Repeat("中", 255)),
			TestModel: ptr(strings.Repeat("中", 255)),
			Models:    ptr(strings.Repeat("中", 255)),
		}
		fields, err := req.buildBatchUpdateFields()
		require.NoError(t, err)

		require.NotNil(t, fields.Group)
		assert.Equal(t, 64, utf8.RuneCountInString(*fields.Group))
		require.NotNil(t, fields.Tag)
		assert.Equal(t, 191, utf8.RuneCountInString(*fields.Tag))
		require.NotNil(t, fields.Remark)
		assert.Equal(t, 255, utf8.RuneCountInString(*fields.Remark))
		require.NotNil(t, fields.TestModel)
		assert.Equal(t, 255, utf8.RuneCountInString(*fields.TestModel))
		// 模型名与其余文本字段同口径：255 个汉字（765 字节）按码点计数放行
		require.NotNil(t, fields.Models)
		assert.Equal(t, 255, utf8.RuneCountInString(*fields.Models))
	})

	t.Run("priority 按 int64 全域精确保留", func(t *testing.T) {
		// 03 文档 2.2.5 声明 int64 全域：单渠道编辑同样无上限，两条入口同口径
		for _, exact := range []int64{9007199254740993, math.MaxInt64, math.MinInt64} {
			req := ChannelBatchUpdateRequest{Ids: []int{1}, Priority: ptr(exact)}
			fields, err := req.buildBatchUpdateFields()
			require.NoError(t, err)
			require.NotNil(t, fields.Priority)
			assert.Equal(t, exact, *fields.Priority)
		}
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
		require.NoError(t, common.UnmarshalJsonStr(`{"ids":[1],"priority":9007199254740993}`, &req))
		require.NotNil(t, req.Priority)
		fields, err := req.buildBatchUpdateFields()
		require.NoError(t, err)
		require.NotNil(t, fields.Priority)
		// 解析层保留 int64 全精度：值经 float64 中转会变成 9007199254740992
		assert.Equal(t, int64(9007199254740993), *fields.Priority)
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
	require.NoError(t, i18n.Init())
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
		Count  int                                `json:"count"`
		Failed []channelBatchUpdateFailurePayload `json:"failed"`
	} `json:"data"`
}

// TestChannelBatchUpdateRequestMirrorsDomainFields 请求 DTO 与领域字段必须逐字段对应：
// 两份结构体各自声明时，新增字段只改一侧会让该字段被静默丢弃（空即跳过语义下无法
// 从行为上察觉）。字段名与类型由反射逐一对齐，请求侧只多一个 ids。
func TestChannelBatchUpdateRequestMirrorsDomainFields(t *testing.T) {
	// weight 是唯一允许的类型差异：请求侧用 *int 才能对负数回「权重必须在 0-4294967295
	// 之间」（03 文档 2.2.5），领域侧用 *uint 对齐 channels.weight 列
	allowedTypeDivergence := map[string]bool{"Weight": true}

	reqType := reflect.TypeOf(ChannelBatchUpdateRequest{})
	domainType := reflect.TypeOf(model.ChannelBatchEditFields{})
	for i := 0; i < domainType.NumField(); i++ {
		domainField := domainType.Field(i)
		reqField, ok := reqType.FieldByName(domainField.Name)
		require.True(t, ok, "请求 DTO 缺少领域字段 %s", domainField.Name)
		if allowedTypeDivergence[domainField.Name] {
			continue
		}
		assert.Equal(t, domainField.Type, reqField.Type, "字段 %s 的类型与领域字段不一致", domainField.Name)
	}
	assert.Equal(t, domainType.NumField()+1, reqType.NumField(), "请求 DTO 只应比领域字段多一个 ids")
}

func TestBatchUpdateChannelsHandlerRejectsEmptyFields(t *testing.T) {
	t.Run("无生效字段", func(t *testing.T) {
		setupBatchUpdateHandlerTestDB(t)

		recorder := callBatchUpdateChannels(t, `{"ids":[1,2]}`)

		assert.Equal(t, http.StatusOK, recorder.Code)
		assert.JSONEq(t, `{"success":false,"message":"Batch edit requires at least one field"}`, recorder.Body.String())
	})

	t.Run("请求体非法 JSON", func(t *testing.T) {
		setupBatchUpdateHandlerTestDB(t)

		recorder := callBatchUpdateChannels(t, `{"ids":[1,2]`)

		assert.Equal(t, http.StatusOK, recorder.Code)
		assert.JSONEq(t, `{"success":false,"message":"Invalid parameters"}`, recorder.Body.String())
	})

	// ids 元素须为正整数，非法元素在校验阶段即拒绝，不落审计日志（03 文档 2.2.1）
	t.Run("ids 含非正整数", func(t *testing.T) {
		db := setupBatchUpdateHandlerTestDB(t)

		recorder := callBatchUpdateChannels(t, `{"ids":[0,-5],"weight":3}`)

		assert.Equal(t, http.StatusOK, recorder.Code)
		assert.JSONEq(t, `{"success":false,"message":"Invalid parameters"}`, recorder.Body.String())

		var auditCount int64
		require.NoError(t, db.Model(&model.Log{}).Where("content LIKE ?", "%Batch updated%").Count(&auditCount).Error)
		assert.Zero(t, auditCount)
	})
}

// TestBatchUpdateChannelsHandlerMissingTargets ids 全部不存在时返回明确失败而非「0 条成功」。
func TestBatchUpdateChannelsHandlerMissingTargets(t *testing.T) {
	setupBatchUpdateHandlerTestDB(t)

	recorder := callBatchUpdateChannels(t, `{"ids":[404],"weight":3}`)

	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.JSONEq(t, `{"success":false,"message":"Channel does not exist"}`, recorder.Body.String())
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
	assert.Equal(t, "Batch edit failed, all changes were rolled back", response.Message)
	require.Len(t, response.Data.Failed, 1)
	// 失败明细精确指向触发回滚的渠道（按 id 顺序串行执行，首个渠道即失败方），
	// 且文案为 i18n 结果而非驱动层原始错误
	assert.Equal(t, first.Id, response.Data.Failed[0].Id)
	assert.Equal(t, "Failed to rebuild abilities", response.Data.Failed[0].Reason)

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
