package controller

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	c "github.com/QuantumNous/new-api/pkg/catalogcontract"
	"github.com/stretchr/testify/require"
)

// Opt-in subprocess fixture for the Host/Account/DanToken HTTP chain. It only
// accepts a newly allocated local SQLite file, with a caller-owned fake upstream.
func TestCatalogCrossServiceServer(t *testing.T) {
	path := os.Getenv("CATALOG_PROCESS_CHILD_CONFIG")
	if path == "" {
		return
	}
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var cfg struct {
		catalogProcessConfig
		Upstream string
	}
	require.NoError(t, json.Unmarshal(raw, &cfg))
	require.Equal(t, "local", cfg.DSN)
	require.Contains(t, cfg.Upstream, "http://127.0.0.1:")
	db := catalogProcessOpen(t, cfg.catalogProcessConfig)
	require.NoError(t, db.AutoMigrate(append([]any{&model.User{}, &model.Token{}, &model.Channel{}, &model.Ability{}, &model.Option{}, &model.Log{}}, model.CatalogIntegrationModels()...)...))
	var count int64
	require.NoError(t, db.Model(&model.User{}).Count(&count).Error)
	if count == 0 {
		user := model.User{Username: "cross-service-fixture", Setting: `{"billing_preference":"wallet_only"}`, Status: common.UserStatusEnabled, Group: "default", Quota: 10000000}
		require.NoError(t, db.Create(&user).Error)
		token := model.Token{UserId: user.Id, Key: "localprocessTokenA", Status: common.TokenStatusEnabled, Group: "default", ExpiredTime: -1, RemainQuota: 10000000}
		require.NoError(t, db.Create(&token).Error)
		require.NoError(t, model.SaveCatalogBinding(db, model.CatalogIntegrationBinding{TokenID: token.Id, UserID: user.Id, PrincipalID: "process-A", FixedGroup: "default", Enabled: true, AllowedOperations: `["text_generation"]`}, 0, 1, "isolated HTTP fixture"))
		channel := model.Channel{Type: constant.ChannelTypeOpenAI, Key: "local-provider", Status: common.ChannelStatusEnabled, BaseURL: &cfg.Upstream, Models: "fixture/catalog", Group: "default"}
		require.NoError(t, db.Create(&channel).Error)
		require.NoError(t, db.Create(&model.Ability{Group: "default", Model: "fixture/catalog", ChannelId: channel.Id, Enabled: true}).Error)
		require.NoError(t, db.Create(&model.Option{Key: "billing_setting.billing_mode", Value: `{"fixture/catalog":"tiered_expr"}`}).Error)
		require.NoError(t, db.Create(&model.Option{Key: "billing_setting.billing_expr", Value: `{"fixture/catalog":"tier(\"request\", fixed(0.01))"}`}).Error)
		op := c.OperationDefinition{Operation: c.TextGeneration, OperationRevision: "fixture-1", PricingRevision: "fixture-price", InputSchema: common.RawMessage(`{"type":"object","required":["model"],"properties":{"model":{"const":"fixture/catalog"}}}`), OutputSchema: common.RawMessage(`{"type":"object"}`), Limits: common.RawMessage(`{"maxInputTokens":4096,"maxOutputTokens":128}`), UsageSchema: common.RawMessage(`{"type":"object"}`), Capabilities: common.RawMessage(`{"reasoningSupported":false}`), InputModalities: []string{"text"}, OutputModalities: []string{"text"}, ExecutionMode: []string{"sync"}, Protocol: "openai.chat_completions", SubmitPath: "/v1/chat/completions"}
		manifest, err := json.Marshal([]CatalogRelayOperation{{Model: "fixture/catalog", ChannelID: channel.Id, Definition: op, MaxInputTokens: 4096, MaxOutputTokens: 128}})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(cfg.Manifest, manifest, 0600))
	} else {
		require.EqualValues(t, 1, count)
		var user model.User
		require.NoError(t, db.First(&user).Error)
		require.Equal(t, "cross-service-fixture", user.Username)
	}
	TestCatalogProcessServer(t)
}
