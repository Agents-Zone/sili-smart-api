package moonshot

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/stretchr/testify/require"
)

func TestConvertOpenAIRequestKimiK26UsesOnlyAllowedTemperature(t *testing.T) {
	request := &dto.GeneralOpenAIRequest{
		Model:       "kimi-k2.6",
		Temperature: common.GetPointer[float64](0.7),
	}
	info := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "kimi-k2.6",
		},
	}

	converted, err := (&Adaptor{}).ConvertOpenAIRequest(nil, info, request)

	require.NoError(t, err)
	convertedRequest, ok := converted.(*dto.GeneralOpenAIRequest)
	require.True(t, ok)
	require.NotNil(t, convertedRequest.Temperature)
	require.Equal(t, 1.0, *convertedRequest.Temperature)
}

func TestConvertOpenAIRequestKimiK26KeepsOmittedTemperatureOmitted(t *testing.T) {
	request := &dto.GeneralOpenAIRequest{
		Model: "kimi-k2.6",
	}
	info := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "kimi-k2.6",
		},
	}

	converted, err := (&Adaptor{}).ConvertOpenAIRequest(nil, info, request)

	require.NoError(t, err)
	convertedRequest, ok := converted.(*dto.GeneralOpenAIRequest)
	require.True(t, ok)
	require.Nil(t, convertedRequest.Temperature)
}

func TestConvertOpenAIRequestKimiFixedSamplingModelsRewriteSamplingParams(t *testing.T) {
	tests := []struct {
		name            string
		model           string
		temperature     *float64
		topP            *float64
		wantTemperature *float64
		wantTopP        *float64
	}{
		{
			name:     "kimi-k2.6 rewrites top_p",
			model:    "kimi-k2.6",
			topP:     common.GetPointer[float64](1.0),
			wantTopP: common.GetPointer[float64](0.95),
		},
		{
			name:            "kimi-k2.7-code rewrites temperature and top_p",
			model:           "kimi-k2.7-code",
			temperature:     common.GetPointer[float64](0.7),
			topP:            common.GetPointer[float64](1.0),
			wantTemperature: common.GetPointer[float64](1.0),
			wantTopP:        common.GetPointer[float64](0.95),
		},
		{
			name:     "kimi-k2.8-preview rewrites top_p",
			model:    "kimi-k2.8-preview",
			topP:     common.GetPointer[float64](0.9),
			wantTopP: common.GetPointer[float64](0.95),
		},
		{
			name:     "kimi-k3 rewrites top_p",
			model:    "kimi-k3",
			topP:     common.GetPointer[float64](1.0),
			wantTopP: common.GetPointer[float64](0.95),
		},
		{
			name:     "kimi-k3-256k rewrites top_p",
			model:    "kimi-k3-256k",
			topP:     common.GetPointer[float64](1.0),
			wantTopP: common.GetPointer[float64](0.95),
		},
		{
			name: "kimi-k2.6 keeps omitted sampling params omitted",
			model: "kimi-k2.6",
		},
		{
			name:     "kimi-k2.6 keeps exact allowed top_p",
			model:    "kimi-k2.6",
			topP:     common.GetPointer[float64](0.95),
			wantTopP: common.GetPointer[float64](0.95),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := &dto.GeneralOpenAIRequest{
				Model:       tt.model,
				Temperature: tt.temperature,
				TopP:        tt.topP,
			}
			info := &relaycommon.RelayInfo{
				ChannelMeta: &relaycommon.ChannelMeta{
					UpstreamModelName: tt.model,
				},
			}

			converted, err := (&Adaptor{}).ConvertOpenAIRequest(nil, info, request)

			require.NoError(t, err)
			convertedRequest, ok := converted.(*dto.GeneralOpenAIRequest)
			require.True(t, ok)
			require.Equal(t, tt.wantTemperature, convertedRequest.Temperature)
			require.Equal(t, tt.wantTopP, convertedRequest.TopP)
		})
	}
}

func TestConvertOpenAIRequestLegacyKimiModelKeepsSamplingParams(t *testing.T) {
	for _, model := range []string{"kimi-k2.5", "kimi-k2-0905-preview", "kimi-k2-turbo-preview"} {
		t.Run(model, func(t *testing.T) {
			request := &dto.GeneralOpenAIRequest{
				Model:       model,
				Temperature: common.GetPointer[float64](0.7),
				TopP:        common.GetPointer[float64](1.0),
			}
			info := &relaycommon.RelayInfo{
				ChannelMeta: &relaycommon.ChannelMeta{
					UpstreamModelName: model,
				},
			}

			converted, err := (&Adaptor{}).ConvertOpenAIRequest(nil, info, request)

			require.NoError(t, err)
			convertedRequest, ok := converted.(*dto.GeneralOpenAIRequest)
			require.True(t, ok)
			require.NotNil(t, convertedRequest.Temperature)
			require.Equal(t, 0.7, *convertedRequest.Temperature)
			require.NotNil(t, convertedRequest.TopP)
			require.Equal(t, 1.0, *convertedRequest.TopP)
		})
	}
}

func TestConvertClaudeRequestKimiFixedSamplingModelRewritesSamplingParams(t *testing.T) {
	request := &dto.ClaudeRequest{
		Model:       "kimi-k2.6",
		Temperature: common.GetPointer[float64](0.7),
		TopP:        common.GetPointer[float64](1.0),
	}
	info := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "kimi-k2.6",
		},
	}

	converted, err := (&Adaptor{}).ConvertClaudeRequest(nil, info, request)

	require.NoError(t, err)
	convertedRequest, ok := converted.(*dto.ClaudeRequest)
	require.True(t, ok)
	require.NotNil(t, convertedRequest.Temperature)
	require.Equal(t, 1.0, *convertedRequest.Temperature)
	require.NotNil(t, convertedRequest.TopP)
	require.Equal(t, 0.95, *convertedRequest.TopP)
}

func TestConvertOpenAIRequestOtherMoonshotModelKeepsTemperature(t *testing.T) {
	request := &dto.GeneralOpenAIRequest{
		Model:       "kimi-k2.5",
		Temperature: common.GetPointer[float64](0.7),
	}
	info := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "kimi-k2.5",
		},
	}

	converted, err := (&Adaptor{}).ConvertOpenAIRequest(nil, info, request)

	require.NoError(t, err)
	convertedRequest, ok := converted.(*dto.GeneralOpenAIRequest)
	require.True(t, ok)
	require.NotNil(t, convertedRequest.Temperature)
	require.Equal(t, 0.7, *convertedRequest.Temperature)
}
