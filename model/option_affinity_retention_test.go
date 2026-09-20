package model

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateAffinityRetentionOption(t *testing.T) {
	for _, value := range []string{"0", "-1", "1.5", "31536001", "9223372036854775807", "invalid"} {
		t.Run(value, func(t *testing.T) {
			require.Error(t, validateOptionValue("channel_affinity_setting.last_bind_ttl_seconds", value))
		})
	}
	for _, value := range []string{"1", "604800", "31536000"} {
		t.Run(value, func(t *testing.T) {
			require.NoError(t, validateOptionValue("channel_affinity_setting.last_bind_ttl_seconds", value))
		})
	}
}
