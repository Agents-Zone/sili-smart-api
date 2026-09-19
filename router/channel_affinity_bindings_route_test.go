package router

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChannelAffinityBindingsRouteUsesRootAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	SetApiRouter(engine)

	var route *gin.RouteInfo
	for i := range engine.Routes() {
		candidate := engine.Routes()[i]
		if candidate.Method == http.MethodGet && candidate.Path == "/api/option/channel_affinity_bindings" {
			route = &candidate
			break
		}
	}
	require.NotNil(t, route)

	req := httptest.NewRequest(http.MethodGet, "/api/option/channel_affinity_bindings", nil)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	assert.GreaterOrEqual(t, rec.Code, http.StatusUnauthorized)
	assert.Less(t, rec.Code, http.StatusInternalServerError)
}
