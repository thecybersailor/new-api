package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service/authz"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestCatalogCredentialRecoveryAndIsolation(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "auth.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	oldDB := model.DB
	model.DB = db
	t.Cleanup(func() { model.DB = oldDB; sql, _ := db.DB(); _ = sql.Close() })
	require.NoError(t, db.AutoMigrate(&model.Token{}, &model.User{}))
	require.NoError(t, db.AutoMigrate(model.CatalogIntegrationModels()...))
	user := model.User{Id: 1001, Username: "fixture", Status: common.UserStatusEnabled, Group: "default", Quota: 0}
	require.NoError(t, db.Create(&user).Error)
	token := model.Token{UserId: user.Id, Key: "fixturecatalogtoken", Status: common.TokenStatusExhausted, ExpiredTime: -1, Group: "default"}
	require.NoError(t, db.Create(&token).Error)
	b := model.CatalogIntegrationBinding{TokenID: token.Id, UserID: user.Id, PrincipalID: "fixture", FixedGroup: "default", Enabled: true}
	require.NoError(t, db.Create(&b).Error)
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest("GET", "/v1/invocations/request", nil)
	p, err := catalogPrincipal(ctx, &token, b)
	require.NoError(t, err)
	assert.Equal(t, "fixture", p.ID)
	require.ErrorContains(t, p.CanSubmit(context.Background()), "INTEGRATION_DISABLED")
	t.Setenv("DANTOKEN_CATALOG_ENABLED", "true")
	require.ErrorContains(t, p.CanSubmit(context.Background()), "INSUFFICIENT_POINTS")
	for _, status := range []int{common.TokenStatusDisabled, common.TokenStatusExpired, 99} {
		copy := token
		copy.Status = status
		_, err = catalogPrincipal(ctx, &copy, b)
		require.ErrorContains(t, err, "ACCESS_DENIED")
	}
	expired := token
	expired.ExpiredTime = time.Now().Unix() - 1
	_, err = catalogPrincipal(ctx, &expired, b)
	require.ErrorContains(t, err, "ACCESS_DENIED")
	wrong := b
	wrong.UserID++
	_, err = catalogPrincipal(ctx, &token, wrong)
	require.ErrorContains(t, err, "ACCESS_DENIED")
	ips := "198.51.100.0/24"
	restricted := token
	restricted.AllowIps = &ips
	_, err = catalogPrincipal(ctx, &restricted, b)
	require.ErrorContains(t, err, "ACCESS_DENIED")
	conflict := model.CatalogIntegrationBinding{TokenID: 999, UserID: user.Id + 1, PrincipalID: b.PrincipalID, FixedGroup: "default", Enabled: true}
	require.NoError(t, db.Create(&conflict).Error)
	_, err = catalogPrincipal(ctx, &token, b)
	require.ErrorContains(t, err, "ACCESS_DENIED")
	require.NoError(t, db.Delete(&conflict).Error)
	// Tokens are loaded freshly from DB, and disabling a binding cannot fall back
	// to the unquoted legacy native route, even when the feature flag is off.
	router := gin.New()
	router.Use(CatalogIntegration())
	fallback := false
	for _, path := range []string{"/v1/chat/completions", "/v1/messages", "/v1/models/test"} {
		router.POST(path, func(c *gin.Context) { fallback = true; c.Status(200) })
	}
	require.NoError(t, db.Model(&b).Update("enabled", false).Error)
	t.Setenv("DANTOKEN_CATALOG_ENABLED", "false")
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	request.Header.Set("Authorization", "Bearer sk-"+token.Key)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	assert.Equal(t, 403, response.Code)
	assert.False(t, fallback)

	for header, value := range map[string]string{"mj-api-secret": "Bearer sk-" + token.Key, "x-api-key": token.Key, "x-goog-api-key": token.Key, "Sec-WebSocket-Protocol": "openai-insecure-api-key." + token.Key} {
		path := "/v1/chat/completions"
		if header == "x-api-key" {
			path = "/v1/messages"
		}
		if header == "x-goog-api-key" {
			path = "/v1/models/test"
		}
		request = httptest.NewRequest(http.MethodPost, path, nil)
		if header == "x-api-key" || header == "x-goog-api-key" {
			request.Header.Set("Authorization", "Bearer ignored-credential")
		}
		request.Header.Set(header, value)
		response = httptest.NewRecorder()
		router.ServeHTTP(response, request)
		assert.Equal(t, 403, response.Code, header)
		assert.False(t, fallback, header)
	}

	// A normal API token has no integration binding and retains the ordinary
	// relay path with either catalog flag value; catalog control APIs stay denied.
	ordinary := model.Token{UserId: user.Id, Key: "fixtureordinarytoken", Status: common.TokenStatusEnabled, ExpiredTime: -1, Group: "default"}
	require.NoError(t, db.Create(&ordinary).Error)
	router.GET("/v1/catalog", func(c *gin.Context) { fallback = true; c.Status(200) })
	for _, enabled := range []string{"false", "true"} {
		t.Setenv("DANTOKEN_CATALOG_ENABLED", enabled)
		for _, path := range []string{"/v1/chat/completions", "/v1/messages", "/v1/models/test"} {
			fallback = false
			request = httptest.NewRequest(http.MethodPost, path, nil)
			request.Header.Set("Authorization", "Bearer "+ordinary.Key)
			response = httptest.NewRecorder()
			router.ServeHTTP(response, request)
			assert.Equal(t, http.StatusOK, response.Code, path)
			assert.True(t, fallback, path)
		}
		fallback = false
		request = httptest.NewRequest(http.MethodGet, "/v1/catalog", nil)
		request.Header.Set("Authorization", "Bearer "+ordinary.Key)
		response = httptest.NewRecorder()
		router.ServeHTTP(response, request)
		assert.Equal(t, http.StatusForbidden, response.Code)
		assert.False(t, fallback)
	}
}

func TestCatalogMaintenanceScopePolicyAndRotation(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "policy.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	old := model.DB
	model.DB = db
	t.Cleanup(func() { model.DB = old; sql, _ := db.DB(); _ = sql.Close() })
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.Ability{}))
	require.NoError(t, db.AutoMigrate(model.CatalogIntegrationModels()...))
	user := model.User{Username: "policy", Status: common.UserStatusEnabled, Group: "default", Quota: 100}
	require.NoError(t, db.Create(&user).Error)
	token := model.Token{UserId: user.Id, Key: "local-policy-token", Group: "default", ExpiredTime: -1, Status: common.TokenStatusEnabled, RemainQuota: 100}
	require.NoError(t, db.Create(&token).Error)
	binding := model.CatalogIntegrationBinding{TokenID: token.Id, UserID: user.Id, PrincipalID: "principal", FixedGroup: "default", Enabled: true}
	require.NoError(t, db.Create(&binding).Error)
	require.NoError(t, db.Create(&model.Ability{ChannelId: 1, Group: "default", Model: "A", Enabled: true}).Error)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest("GET", "/v1/catalog", nil)
	first, err := resolveCatalogScope(ctx, db, token.Id)
	require.NoError(t, err)
	require.NoError(t, db.Create(&model.Ability{ChannelId: 1, Group: "default", Model: "B", Enabled: true}).Error)
	added, err := resolveCatalogScope(ctx, db, token.Id)
	require.NoError(t, err)
	assert.Equal(t, first.ScopeID, added.ScopeID)
	assert.Equal(t, first.AuthorizationHash, added.AuthorizationHash)
	require.NoError(t, db.Model(&token).Updates(map[string]any{"model_limits_enabled": true, "model_limits": "A"}).Error)
	limited, err := resolveCatalogScope(ctx, db, token.Id)
	require.NoError(t, err)
	assert.Equal(t, first.ScopeID, limited.ScopeID)
	assert.NotEqual(t, first.AuthorizationHash, limited.AuthorizationHash)
	assert.Equal(t, []string{"A"}, limited.Models)
	binding.ModelPolicy = "allowlist"
	binding.AllowedModels = `["A"]`
	binding.AllowedOperations = `["image_generation"]`
	require.NoError(t, model.SaveCatalogBinding(db, binding, 0, 1, "review exact operations"))
	opPolicy, err := resolveCatalogScope(ctx, db, token.Id)
	require.NoError(t, err)
	assert.True(t, opPolicy.AllowsOperation("A", "image_generation"))
	assert.False(t, opPolicy.AllowsOperation("A", "text_generation"))
	rotated := token
	rotated.Id = 0
	rotated.Key = "local-rotated-token"
	require.NoError(t, db.Create(&rotated).Error)
	next := binding
	next.ID = 0
	next.TokenID = rotated.Id
	require.NoError(t, model.SaveCatalogBinding(db, next, 0, 1, "rotate same principal"))
	rotation, err := resolveCatalogScope(ctx, db, rotated.Id)
	require.NoError(t, err)
	assert.Equal(t, opPolicy.ScopeID, rotation.ScopeID)
	assert.Equal(t, opPolicy.BindingVersion, rotation.BindingVersion)
	assert.Equal(t, opPolicy.AuthorizationHash, rotation.AuthorizationHash)
	require.NoError(t, db.Model(&rotated).Update("model_limits", "B").Error)
	_, err = resolveCatalogScope(ctx, db, rotated.Id)
	require.ErrorContains(t, err, "inconsistent policies")
	require.NoError(t, db.Model(&token).Update("status", common.TokenStatusDisabled).Error)
	_, err = resolveCatalogScope(ctx, db, token.Id)
	require.ErrorContains(t, err, "ACCESS_DENIED")
}

func TestCatalogMaintenanceWriteOriginAndPermissions(t *testing.T) {
	for _, tc := range []struct {
		name, origin, content string
		role, status          int
	}{
		{"ordinary-user", "http://catalog.test", "application/json", common.RoleCommonUser, 403},
		{"admin-without-explicit-publish", "http://catalog.test", "application/json", common.RoleAdminUser, 403},
		{"root", "http://catalog.test", "application/json", common.RoleRootUser, 204},
		{"cross-origin", "https://attacker.invalid", "application/json", common.RoleRootUser, 403},
		{"form-csrf", "http://catalog.test", "text/plain", common.RoleRootUser, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router := gin.New()
			router.Use(func(c *gin.Context) { c.Set("id", 1); c.Set("role", tc.role) })
			router.POST("/publish", RequirePermission(authz.CatalogPublish), CatalogMaintenanceWrite(), func(c *gin.Context) { c.Status(204) })
			req := httptest.NewRequest("POST", "http://catalog.test/publish", nil)
			req.Header.Set("Origin", tc.origin)
			req.Header.Set("Content-Type", tc.content)
			res := httptest.NewRecorder()
			router.ServeHTTP(res, req)
			assert.Equal(t, tc.status, res.Code)
		})
	}
}
