package controller

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"math"
	"net"
	"net/http"
	"path/filepath"

	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	c "github.com/QuantumNous/new-api/pkg/catalogcontract"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	relaytypes "github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	integration "github.com/QuantumNous/new-api/service/catalogintegration"

	hosttypes "github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// CatalogRelayOperation is server-owned admission metadata. Neither a public
// pricing label nor a caller can select a channel or invent supported operations.
// The file contains no credentials or replacement prices: pricing is resolved
// through the existing engine and credentials through the selected channel.
type CatalogRelayOperation struct {
	DependencyHash  string                              `json:"dependencyHash,omitempty"`
	Model           string                              `json:"model"`
	ChannelID       int                                 `json:"channelId"`
	Definition      c.OperationDefinition               `json:"definition"`
	MaxInputTokens  int                                 `json:"maxInputTokens"`
	MaxOutputTokens int                                 `json:"maxOutputTokens"`
	UsageBounds     map[string]billingexpr.NumericRange `json:"usageBounds,omitempty"`
}
type catalogFrozenRelay struct {
	AudioInputPrice float64                      `json:"audioInputPrice"`
	Operation       CatalogRelayOperation        `json:"operation"`
	ChannelID       int                          `json:"channelId"`
	RouteHash       string                       `json:"routeHash"`
	Price           hosttypes.PriceData          `json:"price"`
	OtherRatios     map[string]float64           `json:"otherRatios,omitempty"`
	Tiered          *billingexpr.BillingSnapshot `json:"tiered,omitempty"`
	RequestInput    *billingexpr.RequestInput    `json:"requestInput,omitempty"`
	MaximumQuota    int                          `json:"maximumQuota"`
	QuotaPerUnit    float64                      `json:"quotaPerUnit"`
	EstimateInput   int                          `json:"estimateInput"`
}
type catalogRelayAdapter struct {
	config CatalogRelayOperation
	token  *model.Token
	root   string
	db     *gorm.DB
}

func NewCatalogRelayEngine(_ *gin.Context, token *model.Token) (*integration.Engine, error) {
	engine := &integration.Engine{DB: model.DB, SourceID: "dantoken", ArtifactRoot: os.Getenv("DANTOKEN_ARTIFACT_DIR")}
	engine.RecoveryAdapter = func(row model.CatalogStoredQuote) (integration.Adapter, error) {
		var frozen catalogFrozenRelay
		if err := common.Unmarshal(row.FrozenPricing, &frozen); err != nil {
			return nil, err
		}
		if frozen.Operation.Model != row.ModelName || string(frozen.Operation.Definition.Operation) != row.Operation || frozen.Operation.ChannelID != frozen.ChannelID {
			return nil, fmt.Errorf("frozen recovery route unavailable")
		}
		return &catalogRelayAdapter{config: frozen.Operation, token: token, root: engine.ArtifactRoot, db: model.DB}, nil
	}
	engine.AdmissionError = loadCatalogRelayOperations(engine, token)
	engine.SnapshotGate = func(ctx context.Context, tx *gorm.DB, scope integration.Scope, models []c.CatalogModel) error {
		for _, m := range models {
			for _, expected := range m.Operations {
				for _, registration := range engine.Adapters {
					if registration.Model != m.ID || registration.Operation != expected.Operation {
						continue
					}
					adapter := registration.Adapter.(*catalogRelayAdapter)
					if err := model.LockCatalogChannel(tx, adapter.config.ChannelID); err != nil {
						return err
					}
					copy := *adapter
					copy.db = tx
					actual, err := copy.Definition(ctx, scope, m.ID)
					if err != nil {
						return c.Invalid("CATALOG_CONFLICT", "operation changed during snapshot", "retry a fresh snapshot")
					}
					a, _ := c.Hash(actual)
					b, _ := c.Hash(expected)
					if a != b {
						return c.Invalid("CATALOG_CONFLICT", "operation changed during snapshot", "retry a fresh snapshot")
					}
				}
			}
		}
		return nil
	}
	engine.ValidateAcceptance = func(ctx context.Context, tx *gorm.DB, scope integration.Scope, row model.CatalogStoredQuote, quote c.Quote) error {
		for _, registration := range engine.Adapters {
			if registration.Model != row.ModelName || string(registration.Operation) != row.Operation {
				continue
			}
			adapter, ok := registration.Adapter.(*catalogRelayAdapter)
			if !ok {
				return fmt.Errorf("invalid relay registration")
			}
			if err := model.LockCatalogChannel(tx, adapter.config.ChannelID); err != nil {
				return err
			}
			copy := *adapter
			copy.db = tx
			current, err := copy.Definition(ctx, scope, row.ModelName)
			if err != nil {
				return err
			}
			if current.OperationRevision != quote.OperationRevision {
				return c.Invalid("OPERATION_NOT_ADMITTED", "operation changed before acceptance", "refresh operation")
			}
			return nil
		}
		return c.Invalid("OPERATION_NOT_ADMITTED", "operation retired", "refresh catalog")
	}
	if engine.AdmissionError != nil {
		engine.Adapters = nil
	}
	return engine, nil
}

func loadCatalogRelayOperations(engine *integration.Engine, token *model.Token) error {
	if common.QuotaPerUnit <= 0 || math.IsNaN(common.QuotaPerUnit) || math.IsInf(common.QuotaPerUnit, 0) {
		return fmt.Errorf("invalid supplier quota unit")
	}
	directory, err := model.ReadCatalogDirectory(model.DB)
	if err != nil {
		return err
	}
	var configs []CatalogRelayOperation
	if directory.DatabaseEnabled {
		var entries []model.CatalogMaintenanceEntry
		if err = common.Unmarshal(directory.Published, &entries); err != nil {
			return err
		}
		engine.CheckDirectory = func(tx *gorm.DB) error {
			current, er := model.ReadCatalogDirectory(tx)
			if er != nil {
				return er
			}
			if !current.DatabaseEnabled || current.PublishedRevision != directory.PublishedRevision {
				return c.Invalid("CATALOG_CONFLICT", "directory changed during request", "refresh the published catalog")
			}
			return nil
		}
		engine.DisplayNames = map[string]string{}
		for _, entry := range entries {
			engine.DisplayNames[entry.Model] = entry.DisplayName
			for _, op := range entry.Operations {
				if op.State != "published" {
					continue
				}
				var config CatalogRelayOperation
				if err = common.Unmarshal(op.Config, &config); err != nil {
					return err
				}
				config.DependencyHash = op.DependencyHash
				if config.DependencyHash == "" {
					return fmt.Errorf("published dependency fingerprint missing")
				}
				configs = append(configs, config)
			}
		}
	} else {
		path := os.Getenv("DANTOKEN_CATALOG_OPERATIONS_FILE")
		if path == "" {
			return nil
		}
		raw, er := os.ReadFile(path)
		if er != nil {
			return er
		}
		if len(raw) > 2<<20 {
			return fmt.Errorf("catalog operation manifest exceeds limit")
		}
		if err = common.Unmarshal(raw, &configs); err != nil {
			return err
		}
		digest, _ := c.Hash(configs)
		engine.CheckDirectory = func(tx *gorm.DB) error {
			current, er := model.ReadCatalogDirectory(tx)
			if er != nil {
				return er
			}
			if current.DatabaseEnabled {
				return c.Invalid("CATALOG_CONFLICT", "database catalog was activated", "refresh catalog")
			}
			data, er := os.ReadFile(path)
			if er != nil {
				return er
			}
			var latest []CatalogRelayOperation
			if er = common.Unmarshal(data, &latest); er != nil {
				return er
			}
			hash, er := c.Hash(latest)
			if er != nil {
				return er
			}
			if hash != digest {
				return c.Invalid("CATALOG_CONFLICT", "manifest changed during request", "refresh catalog")
			}
			return nil
		}
	}
	return registerCatalogRelayOperations(engine, token, configs)
}

func registerCatalogRelayOperations(engine *integration.Engine, token *model.Token, configs []CatalogRelayOperation) error {
	var err error
	seen := map[string]bool{}
	for _, config := range configs {
		key := config.Model + "/" + string(config.Definition.Operation)
		if seen[key] || config.ChannelID <= 0 || config.Model == "" {
			return fmt.Errorf("invalid or duplicate catalog operation")
		}
		seen[key] = true
		if err = config.Definition.Validate(); err != nil {
			return err
		}
		if _, err = catalogRelayFormat(config.Definition.SubmitPath); err != nil && config.Definition.SubmitPath != "/v1/videos" {
			return err
		}
		expectedPaths := map[c.Operation][]string{
			c.TextGeneration: {"/v1/chat/completions", "/v1/responses"}, c.MultimodalGeneration: {"/v1/chat/completions", "/v1/responses"},
			c.ImageGeneration: {"/v1/images/generations"}, c.ImageEdit: {"/v1/images/edits"}, c.VideoGeneration: {"/v1/videos"},
			c.SpeechSynthesis: {"/v1/audio/speech"}, c.AudioTranscription: {"/v1/audio/transcriptions"}, c.Embedding: {"/v1/embeddings"},
		}
		if !slices.Contains(expectedPaths[config.Definition.Operation], config.Definition.SubmitPath) {
			return fmt.Errorf("operation and native path disagree")
		}
		for name, bound := range config.UsageBounds {
			if name == "" || bound.Min < 0 || bound.Max < bound.Min || math.IsNaN(bound.Min) || math.IsNaN(bound.Max) || math.IsInf(bound.Min, 0) || math.IsInf(bound.Max, 0) {
				return fmt.Errorf("invalid finite usage bound")
			}
		}
		for _, schema := range []common.RawMessage{config.Definition.InputSchema, config.Definition.OutputSchema, config.Definition.UsageSchema} {
			if err = validateCatalogInput(schema, []byte(`{}`)); err != nil {
				var validationError *jsonschema.ValidationError
				if !errors.As(err, &validationError) {
					return err
				}
			}
		}
		if config.Definition.Operation == c.ImageGeneration || config.Definition.Operation == c.ImageEdit || config.Definition.Operation == c.SpeechSynthesis || config.Definition.Operation == c.VideoGeneration {
			if !filepath.IsAbs(engine.ArtifactRoot) {
				return fmt.Errorf("media operations require an absolute private artifact directory")
			}
		}
		if (config.Definition.Operation == c.TextGeneration || config.Definition.Operation == c.MultimodalGeneration || config.Definition.Operation == c.Embedding) && config.MaxInputTokens == 0 {
			return fmt.Errorf("token operations require a proven context limit")
		}
		if (config.Definition.Operation == c.TextGeneration || config.Definition.Operation == c.MultimodalGeneration) && config.MaxOutputTokens == 0 {
			return fmt.Errorf("generation requires a proven output limit")
		}
		if config.MaxInputTokens < 0 || config.MaxInputTokens > math.MaxInt32/2 || config.MaxOutputTokens < 0 || config.MaxOutputTokens > math.MaxInt32/2 {
			return fmt.Errorf("invalid model token bounds")
		}
		a := &catalogRelayAdapter{config: config, token: token, root: engine.ArtifactRoot, db: model.DB}
		engine.Adapters = append(engine.Adapters, integration.AdapterRegistration{Key: "relay-v1/" + strconv.Itoa(config.ChannelID), Model: config.Model, Operation: config.Definition.Operation, Adapter: a})
	}
	return nil
}
func catalogRelayFormat(path string) (relaytypes.RelayFormat, error) {
	switch path {
	case "/v1/chat/completions":
		return relaytypes.RelayFormatOpenAI, nil
	case "/v1/responses":
		return relaytypes.RelayFormatOpenAIResponses, nil
	case "/v1/embeddings":
		return relaytypes.RelayFormatEmbedding, nil
	case "/v1/images/generations", "/v1/images/edits":
		return relaytypes.RelayFormatOpenAIImage, nil
	case "/v1/audio/speech", "/v1/audio/transcriptions":
		return relaytypes.RelayFormatOpenAIAudio, nil
	}
	return "", fmt.Errorf("unsupported catalog native path")
}

func (a *catalogRelayAdapter) channel(scope integration.Scope) (*model.Channel, string, error) {
	var channel model.Channel
	if err := a.db.Where("id = ?", a.config.ChannelID).Take(&channel).Error; err != nil {
		return nil, "", err
	}
	var count int64
	if err := a.db.Model(&model.Ability{}).Where(&model.Ability{ChannelId: channel.Id, Group: scope.Group, Model: a.config.Model}).Where("enabled = ?", true).Count(&count).Error; err != nil {
		return nil, "", err
	}
	if channel.Status != common.ChannelStatusEnabled || count == 0 {
		return nil, "", c.Invalid("OPERATION_NOT_ADMITTED", "configured channel is unavailable for this scope", "restore the admitted route")
	}
	hash, err := catalogChannelFingerprint(&channel)
	return &channel, hash, err
}

func catalogChannelFingerprint(channel *model.Channel) (string, error) {
	// Credentials may rotate without changing execution identity. Everything that
	// can change endpoint, mapping, parameters or plugin semantics is fingerprinted.
	if channel.Setting != nil && *channel.Setting != "" {
		var setting map[string]any
		if err := common.Unmarshal([]byte(*channel.Setting), &setting); err != nil {
			return "", fmt.Errorf("invalid channel settings")
		}
	}
	pluginDigest := ""
	generation := pluginruntime.DefaultRegistry.Generation()
	plugin, ok := generation.Get(channel.GetSetting().TaskPluginKey)
	if !ok {
		plugin, ok = generation.GetByChannelType(channel.Type)
	}
	if ok {
		pluginDigest = plugin.Engine.SourceDigest()
	}
	return c.Hash(struct {
		ID, Type                                                         int
		BaseURL, Other, Mapping, Setting, OtherSettings, Params, Headers string
	}{channel.Id, channel.Type, channel.GetBaseURL(), channel.Other + "/plugin=" + pluginDigest, channel.GetModelMapping(), catalogOptionalString(channel.Setting), channel.OtherSettings, catalogOptionalString(channel.ParamOverride), catalogOptionalString(channel.HeaderOverride)})
}

type catalogDenySchemaLoader struct{}

func (catalogDenySchemaLoader) Load(string) (any, error) {
	return nil, fmt.Errorf("external catalog schema references are forbidden")
}
func validateCatalogInput(schema, raw common.RawMessage) error {
	var doc, input any
	if err := common.Unmarshal(schema, &doc); err != nil {
		return err
	}
	if err := common.Unmarshal(raw, &input); err != nil {
		return err
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.UseLoader(catalogDenySchemaLoader{})
	if err := compiler.AddResource("https://catalog.invalid/native.json", doc); err != nil {
		return err
	}
	compiled, err := compiler.Compile("https://catalog.invalid/native.json")
	if err != nil {
		return err
	}
	return compiled.Validate(input)
}

func (a *catalogRelayAdapter) Definition(ctx context.Context, scope integration.Scope, _ string) (c.OperationDefinition, error) {
	def := a.config.Definition
	if a.config.DependencyHash != "" {
		current, err := catalogDependencyFingerprint(a.db, a.config)
		if err != nil {
			return def, err
		}
		if current != a.config.DependencyHash {
			return def, c.Invalid("OPERATION_NOT_ADMITTED", "published route or pricing dependencies changed", "review and republish this operation")
		}
	}
	_, routeHash, err := a.channel(scope)
	if err != nil {
		return def, err
	}
	if err = validateCatalogInput(common.RawMessage(`{"type":"object"}`), def.InputSchema); err != nil {
		return def, err
	}
	def.OperationRevision, err = c.Hash(struct {
		Definition    c.OperationDefinition
		Route         string
		Input, Output int
		Usage         map[string]billingexpr.NumericRange
	}{def, routeHash, a.config.MaxInputTokens, a.config.MaxOutputTokens, a.config.UsageBounds})
	if err != nil {
		return def, err
	}
	// A broken bound disables this exact execution variant until its limits or
	// schema are revised. Other operations and historical recovery remain valid.
	var debts []struct{ Data []byte }
	if err = a.db.Table("catalog_executions").Select("catalog_stored_quotes.data").Joins("JOIN catalog_stored_quotes ON catalog_stored_quotes.quote_id = catalog_executions.quote_id").Where("catalog_executions.channel_id = ? AND catalog_executions.billing_state = ? AND catalog_stored_quotes.model_name = ? AND catalog_stored_quotes.operation = ?", a.config.ChannelID, "reconciliation_required", a.config.Model, string(def.Operation)).Find(&debts).Error; err != nil {
		return def, err
	}
	for _, debt := range debts {
		var quote c.Quote
		if err = common.Unmarshal(debt.Data, &quote); err != nil {
			return def, err
		}
		if quote.OperationRevision == def.OperationRevision {
			return def, c.Invalid("OPERATION_NOT_ADMITTED", "this execution variant exceeded its frozen quote", "reconcile evidence and revise the proven bound before new submissions")
		}
	}
	prices, err := model.GetModelPricingSnapshotWithDB(a.db, []string{a.config.Model})
	if err != nil {
		return def, err
	}
	user := &model.User{}
	err = a.db.Where("id = ?", a.token.UserId).Take(user).Error
	if err != nil {
		return def, err
	}
	if common.NormalizeBillingPreference(user.GetSetting().BillingPreference) != "wallet_only" {
		return def, c.Invalid("OPERATION_NOT_ADMITTED", "integration supplier identity must explicitly use wallet_only funding", "configure a dedicated wallet-funded integration identity")
	}
	if a.config.DependencyHash != "" {
		prices.Options = nil
	}
	ratio := ratio_setting.GetGroupRatio(scope.Group)
	if special, ok := ratio_setting.GetGroupGroupRatio(user.Group, scope.Group); ok {
		ratio = special
	}
	def.PricingRevision, err = c.Hash(struct {
		Prices *model.ModelPricingSnapshot
		Group  string
		Ratio  float64
		Unit   float64
	}{prices, scope.Group, ratio, common.QuotaPerUnit})
	if err == nil && a.config.DependencyHash != "" {
		def.OperationRevision, err = c.Hash([]string{def.OperationRevision, def.PricingRevision})
	}
	return def, err
}

func (a *catalogRelayAdapter) relayContext(ctx context.Context, scope integration.Scope, raw common.RawMessage) (*gin.Context, *relaycommon.RelayInfo, *catalogResponse, error) {
	body, contentType, err := integration.NativeBody(ctx, raw)
	if err != nil {
		return nil, nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.config.Definition.SubmitPath, bytes.NewReader(body))
	if err != nil {
		return nil, nil, nil, err
	}
	req.Header.Set("Content-Type", contentType)
	recorder := &catalogResponse{header: make(http.Header), status: 200, stream: integration.NativeStreamFromContext(ctx)}
	gc := &gin.Context{Request: req, Writer: recorder}
	completed := false
	defer func() {
		if !completed {
			common.CleanupBodyStorage(gc)
		}
	}()
	user, err := model.GetUserById(a.token.UserId, false)
	if err != nil {
		return nil, nil, nil, err
	}
	for key, value := range map[string]any{string(constant.ContextKeyUserId): user.Id, string(constant.ContextKeyUserGroup): user.Group, string(constant.ContextKeyUsingGroup): scope.Group, string(constant.ContextKeyUserQuota): user.Quota, string(constant.ContextKeyTokenId): a.token.Id, string(constant.ContextKeyTokenKey): a.token.Key, string(constant.ContextKeyTokenUnlimited): a.token.UnlimitedQuota, string(constant.ContextKeyTokenGroup): scope.Group, string(constant.ContextKeyUserSetting): user.GetSetting(), string(common.RequestIdKey): common.NewRequestId()} {
		gc.Set(key, value)
	}
	channel, _, err := a.channel(scope)
	if err != nil {
		return nil, nil, nil, err
	}
	if apiErr := middleware.SetupContextForSelectedChannel(gc, channel, a.config.Model); apiErr != nil {
		return nil, nil, nil, apiErr
	}
	if a.config.Definition.SubmitPath == "/v1/videos" {
		info, err := relaycommon.GenRelayInfo(gc, relaytypes.RelayFormatTask, nil, nil)
		if err != nil {
			return nil, nil, nil, err
		}
		info.InitChannelMeta(gc)
		info.LockedChannel = channel
		if err = helper.ModelMappedHelper(gc, info, nil); err != nil {
			return nil, nil, nil, err
		}
		generation := pluginruntime.DefaultRegistry.Generation()
		plugin, ok := generation.Get(channel.GetSetting().TaskPluginKey)
		if !ok {
			plugin, ok = generation.GetByChannelType(channel.Type)
		}
		if !ok {
			return nil, nil, nil, fmt.Errorf("task plugin unavailable")
		}
		var selected *pluginruntime.ProtocolBinding
		for _, candidate := range generation.LookupEndpointCandidates(http.MethodPost, "/v1/videos", info.UpstreamModelName) {
			if candidate.Plugin == plugin {
				copy := candidate
				selected = &copy
				break
			}
		}
		if selected == nil {
			return nil, nil, nil, fmt.Errorf("plugin does not declare this model on openai_video")
		}
		pinned := pluginruntime.PinnedEndpoint{Generation: generation, Plugin: plugin, Protocol: selected.Protocol, Operation: selected.Operation, Model: a.config.Model, MappedModel: info.UpstreamModelName, Candidates: []pluginruntime.ProtocolBinding{*selected}}
		gc.Set(pluginruntime.ContextKeyPinnedEndpoint, pinned)
		gc.Set(pluginruntime.ContextKeyPinnedPlugin, pluginruntime.PinnedPlugin{Generation: generation, Plugin: plugin})
		gc.Set("platform", plugin.Meta.Key)
		middleware.PrepareTaskPluginEndpoint()(gc)
		if gc.IsAborted() {
			return nil, nil, nil, fmt.Errorf("task decoder rejected native request: %s", recorder.body.String())
		}
		completed = true
		return gc, info, recorder, nil
	}
	format, err := catalogRelayFormat(a.config.Definition.SubmitPath)
	if err != nil {
		return nil, nil, nil, err
	}
	request, err := helper.GetAndValidateRequest(gc, format)
	if err != nil {
		return nil, nil, nil, err
	}
	info, err := relaycommon.GenRelayInfo(gc, format, request, nil)
	if err != nil {
		return nil, nil, nil, err
	}
	info.InitChannelMeta(gc)
	completed = true
	return gc, info, recorder, nil
}

func (a *catalogRelayAdapter) Prepare(ctx context.Context, scope integration.Scope, r c.QuoteRequest) (integration.Bound, error) {
	var bound integration.Bound
	if err := validateCatalogInput(a.config.Definition.InputSchema, r.Request); err != nil {
		return bound, c.Invalid("REQUEST_INVALID", err.Error(), "use the operation input schema")
	}
	quoteFiles, err := integration.NativeQuoteFiles(a.config.Definition.SubmitPath, r.Request)
	if err != nil {
		return bound, err
	}
	ctx = integration.WithNativeFiles(ctx, quoteFiles)
	// Built-in provider tools can add independently priced requests. Until an
	// explicit finite tool budget exists, only client function tools are admitted.
	var requestFields map[string]common.RawMessage
	if err = common.Unmarshal(r.Request, &requestFields); err != nil {
		return bound, err
	}
	if strings.HasSuffix(a.config.Model, "search-preview") || len(requestFields["web_search_options"]) > 0 {
		return bound, fmt.Errorf("built-in search has no frozen cost bound")
	}
	var toolDefs []struct {
		Type string `json:"type"`
	}
	if raw := requestFields["tools"]; len(raw) > 0 {
		if err = common.Unmarshal(raw, &toolDefs); err != nil {
			return bound, err
		}
		for _, tool := range toolDefs {
			if tool.Type != "function" {
				return bound, fmt.Errorf("built-in tool has no frozen cost bound")
			}
		}
	}
	gc, info, _, err := a.relayContext(ctx, scope, r.Request)
	if err != nil {
		return bound, err
	}
	defer common.CleanupBodyStorage(gc)
	if a.config.Definition.SubmitPath == "/v1/videos" {
		return a.prepareTaskBound(gc, info, scope, r)
	}
	meta := info.Request.GetTokenCountMeta()
	estimated, err := service.EstimateRequestToken(gc, meta, info)
	if err != nil {
		return bound, err
	}
	info.SetEstimatePromptTokens(estimated)
	if a.config.MaxInputTokens > 0 && estimated > a.config.MaxInputTokens {
		return bound, c.Invalid("REQUEST_INVALID", "input exceeds admitted context limit", "reduce the input")
	}
	if (r.Operation == c.TextGeneration || r.Operation == c.MultimodalGeneration) && (meta.MaxTokens < 0 || meta.MaxTokens > a.config.MaxOutputTokens) {
		return bound, c.Invalid("REQUEST_INVALID", "output exceeds admitted limit", "use the operation output limit")
	}
	if (r.Operation == c.TextGeneration || r.Operation == c.MultimodalGeneration) && meta.MaxTokens == 0 {
		return bound, c.Invalid("REQUEST_INVALID", "an explicit output token limit is required", "set the native max token field")
	}
	price, err := helper.ModelPriceHelper(gc, info, estimated, meta)
	if err != nil {
		return bound, err
	}
	// Chat n counts independent completions. Its token ceiling must cover
	// every choice, even when a permissive native schema allows n > 1.
	choices := 1
	if r.Operation == c.TextGeneration || r.Operation == c.MultimodalGeneration {
		if value, exists := requestFields["n"]; exists {
			if err := common.Unmarshal(value, &choices); err != nil || choices < 1 || choices > dto.MaxImageN {
				return bound, fmt.Errorf("invalid completion count")
			}
		}
	}
	var maximum float64
	if snap := info.TieredBillingSnapshot; snap != nil {
		vars := map[string]billingexpr.NumericRange{}
		for _, name := range []string{"p", "len", "cr", "cc", "cc1h", "img", "img_cr", "ai"} {
			vars[name] = billingexpr.NumericRange{Min: 0, Max: float64(a.config.MaxInputTokens)}
		}
		for _, name := range []string{"c", "img_o", "ao"} {
			vars[name] = billingexpr.NumericRange{Min: 0, Max: float64(a.config.MaxOutputTokens) * float64(choices)}
		}
		if meta.MaxTokens > 0 {
			for _, name := range []string{"c", "img_o", "ao"} {
				vars[name] = billingexpr.NumericRange{Min: 0, Max: float64(meta.MaxTokens) * float64(choices)}
			}
		}
		vars["image_count"] = billingexpr.NumericRange{Min: 1, Max: dto.MaxImageN}
		if snap.EstimatedImageCount != nil {
			vars["image_count"] = billingexpr.NumericRange{Min: 1, Max: float64(*snap.EstimatedImageCount)}
		}
		requestInput := billingexpr.RequestInput{}
		if info.BillingRequestInput != nil {
			requestInput = *info.BillingRequestInput
		}
		upper, err := billingexpr.CostUpperBound(snap.ExprString, vars, a.config.UsageBounds, requestInput)
		if err != nil {
			return bound, c.Invalid("QUOTE_BOUND_UNAVAILABLE", err.Error(), "provide finite operation limits and supported pricing arithmetic")
		}
		maximum = upper / 1e6 * snap.QuotaPerUnit * snap.GroupRatio
	} else {
		// Preserve the legacy engine's frozen policy rather than silently converting
		// it to expression rounding. Sum independent positive categories conservatively.
		if price.UsePrice {
			maximum = price.ModelPrice * common.QuotaPerUnit * price.GroupRatioInfo.GroupRatio * dto.MaxImageN
		} else {
			maximum = (float64(a.config.MaxInputTokens)*(1+price.CacheRatio+price.CacheCreation5mRatio+price.CacheCreation1hRatio+price.ImageRatio+price.AudioRatio) + float64(a.config.MaxOutputTokens)*float64(choices)*(price.CompletionRatio+price.AudioRatio*price.AudioCompletionRatio)) * price.ModelRatio * price.GroupRatioInfo.GroupRatio
		}
	}
	maximum += float64(a.config.MaxInputTokens) * operation_setting.GetGeminiInputAudioPricePerMillionTokens(info.GetBillingModelName()) / 1e6 * common.QuotaPerUnit * price.GroupRatioInfo.GroupRatio
	maxQuota, err := common.QuotaFromFloatStrict(math.Ceil(maximum))
	if err != nil {
		return bound, c.Invalid("QUOTE_BOUND_UNAVAILABLE", "quoted bound exceeds the supported quota range", "narrow the declared operation limits")
	}
	if maxQuota < price.QuotaToPreConsume {
		maxQuota = price.QuotaToPreConsume
	}
	_, routeHash, err := a.channel(scope)
	if err != nil {
		return bound, err
	}
	frozen := catalogFrozenRelay{Operation: a.config, ChannelID: a.config.ChannelID, RouteHash: routeHash, Price: price, AudioInputPrice: operation_setting.GetGeminiInputAudioPricePerMillionTokens(info.GetBillingModelName()), OtherRatios: price.OtherRatios(), Tiered: info.TieredBillingSnapshot, RequestInput: info.BillingRequestInput, MaximumQuota: maxQuota, QuotaPerUnit: common.QuotaPerUnit, EstimateInput: estimated}
	payload, err := common.Marshal(frozen)
	if err != nil {
		return bound, err
	}
	bound = integration.Bound{Estimated: catalogQuotaMoney(price.QuotaToPreConsume, common.QuotaPerUnit), Maximum: catalogQuotaMoney(maxQuota, common.QuotaPerUnit), PricingSnapshot: payload, RoundingVersion: "supplier-quota-v1-usd-micros-ceil"}
	return bound, nil
}
func catalogQuotaMoney(quota int, unit float64) c.Money {
	amount := decimal.NewFromInt(int64(quota)).Mul(decimal.NewFromInt(1e6)).Div(decimal.NewFromFloat(unit)).Ceil()
	return c.Money{Currency: "USD", AmountMicros: amount.StringFixed(0)}
}

type catalogBillingSession struct {
	maximum int
	actual  *int
}

func (s *catalogBillingSession) Settle(actual int) error  { s.actual = &actual; return nil } // commit only after durable native result
func (s *catalogBillingSession) Refund(*gin.Context)      {}                                 // uncertainty never proves a free failure
func (s *catalogBillingSession) NeedsRefund() bool        { return false }
func (s *catalogBillingSession) GetPreConsumedQuota() int { return s.maximum }
func (s *catalogBillingSession) Reserve(quota int) error {
	if quota < 0 || quota > s.maximum {
		return fmt.Errorf("execution exceeds frozen quote maximum")
	}
	return nil
}

func (a *catalogRelayAdapter) Submit(ctx context.Context, row model.CatalogStoredQuote, q c.Quote, raw common.RawMessage) (integration.Outcome, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
	defer cancel()
	var frozen catalogFrozenRelay
	if err := common.Unmarshal(row.FrozenPricing, &frozen); err != nil {
		return integration.Outcome{}, err
	}
	scope := integration.Scope{PrincipalID: row.PrincipalID, Group: row.FixedGroup}
	if err := integration.ValidateNativeFiles(ctx, a.config.Definition.SubmitPath, raw); err != nil {
		return integration.Outcome{State: "failed", NotCharged: true}, nil
	}
	_, routeHash, err := a.channel(scope)
	if err != nil {
		return integration.Outcome{}, err
	}
	if routeHash != frozen.RouteHash {
		return integration.Outcome{State: "failed", NotCharged: true}, nil
	}
	gc, info, recorder, err := a.relayContext(ctx, scope, raw)
	if err != nil {
		return integration.Outcome{}, err
	}
	defer common.CleanupBodyStorage(gc)
	info.CatalogFrozenPricing = true
	info.CatalogQuotaUnit = frozen.QuotaPerUnit
	info.CatalogResponseLimit = integration.MediaByteLimit()
	info.CatalogAudioInputPrice = frozen.AudioInputPrice
	info.DisablePing = true
	info.PriceData = frozen.Price
	info.PriceData.ReplaceOtherRatios(frozen.OtherRatios)
	info.TieredBillingSnapshot = frozen.Tiered
	info.BillingRequestInput = frozen.RequestInput
	info.SetEstimatePromptTokens(frozen.EstimateInput)
	execution := model.CatalogExecution{QuoteID: q.QuoteID, UserID: a.token.UserId, TokenID: a.token.Id, ChannelID: frozen.ChannelID, Reserved: frozen.MaximumQuota, BillingState: "reserved", ExecutionState: "unknown"}
	if err = model.ReserveCatalogExecution(a.db, &execution); err != nil {
		// No provider I/O has occurred. Confirm rollback or release a committed
		// hold before reporting a definite uncharged rejection.
		var existing model.CatalogExecution
		readErr := a.db.Where("quote_id = ?", q.QuoteID).Take(&existing).Error
		if errors.Is(readErr, gorm.ErrRecordNotFound) {
			return integration.Outcome{State: "failed", NotCharged: true}, nil
		}
		if readErr == nil && len(existing.Outcome) == 0 && len(existing.TaskData) == 0 {
			if releaseErr := model.SettleCatalogExecution(a.db, q.QuoteID, 0); releaseErr == nil {
				return integration.Outcome{State: "failed", NotCharged: true}, nil
			}
		}
		return integration.Outcome{}, err
	}
	session := &catalogBillingSession{maximum: frozen.MaximumQuota}
	info.Billing = session
	info.FinalPreConsumedQuota = frozen.MaximumQuota
	info.BillingSource = service.BillingSourceWallet
	if a.config.Definition.SubmitPath == "/v1/videos" {
		return a.submitTask(ctx, gc, info, row, q, frozen)
	}
	var usage *dto.Usage
	info.CatalogUsageObserver = func(u *dto.Usage) { usage, _ = common.DeepCopy(u) }
	apiErr := relayHandler(gc, info) // one pinned real relay attempt, never controller retries
	if apiErr != nil {
		return integration.Outcome{}, apiErr
	}
	native := recorder.body.Bytes()
	if len(native) == 0 || recorder.overflow || int64(len(native)) > integration.MediaByteLimit() {
		return integration.Outcome{}, fmt.Errorf("native output exceeds artifact limit")
	}
	outcome := integration.Outcome{State: "succeeded"}
	contentType := recorder.Header().Get("Content-Type")
	if strings.Contains(contentType, "application/json") || common.GetJsonType(native) == "object" {
		outcome.Result = append([]byte(nil), native...)
	}
	if strings.Contains(contentType, "text/event-stream") {
		if info.StreamStatus != nil {
			stream := info.StreamStatus.OutcomeSnapshot()
			if !info.StreamStatus.IsNormalEnd() || stream.HasErrors || stream.Response == relaycommon.ResponseOutcomeFailed || stream.Response == relaycommon.ResponseOutcomeIncomplete || (stream.ExpectsTerminal && stream.Response != relaycommon.ResponseOutcomeCompleted) {
				return integration.Outcome{}, fmt.Errorf("provider stream did not complete authoritatively")
			}
		}
		outcome.Result, err = aggregateCatalogSSE(row.SubmitPath, native)
		if err != nil {
			return integration.Outcome{}, err
		}
	}
	execution.NativeBody = append([]byte(nil), native...)
	execution.ContentType = contentType
	execution.ExecutionState = "succeeded"
	if usage == nil || session.actual == nil {
		saved, saveErr := common.Marshal(outcome)
		if saveErr != nil {
			return outcome, saveErr
		}
		saveErr = a.db.Model(&model.CatalogExecution{}).Where("quote_id = ?", q.QuoteID).Updates(map[string]any{"outcome": saved, "native_body": native, "content_type": contentType, "execution_state": "succeeded"}).Error
		return outcome, saveErr // Result is recoverable; billing deliberately stays pending.
	}
	usageJSON, err := common.Marshal(usage)
	if err != nil {
		return outcome, err
	}
	// Persist response and the exact proposed quota before touching the wallet.
	// A crash after this write is recoverable by Observe without another inference.
	outcome.Receipt = &c.Receipt{SourceID: "dantoken", IntegrationPrincipalID: row.PrincipalID, ReceiptID: q.QuoteID, Revision: 1, AmountMode: "cumulative", IsFinal: true, QuoteID: q.QuoteID, PricingRevision: q.PricingRevision, RawUsage: usageJSON, Cost: catalogQuotaMoney(*session.actual, frozen.QuotaPerUnit), RawQuota: strconv.Itoa(*session.actual), QuotaPerUnit: strconv.FormatFloat(frozen.QuotaPerUnit, 'f', 0, 64), RoundingVersion: q.RoundingVersion}
	saved, err := common.Marshal(outcome)
	if err != nil {
		return outcome, err
	}
	if err = a.db.Model(&model.CatalogExecution{}).Where("quote_id = ?", q.QuoteID).Updates(map[string]any{"outcome": saved, "native_body": execution.NativeBody, "content_type": contentType, "execution_state": "succeeded"}).Error; err != nil {
		return outcome, err
	}
	return a.Observe(ctx, row, c.Invocation{ExecutionState: "unknown"})
}
func (a *catalogRelayAdapter) Observe(ctx context.Context, row model.CatalogStoredQuote, previous c.Invocation) (integration.Outcome, error) {
	var execution model.CatalogExecution
	err := a.db.Where("quote_id = ?", row.QuoteID).Take(&execution).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return integration.Outcome{State: previous.ExecutionState, Result: previous.Result, Artifacts: previous.Artifacts, Receipt: previous.Receipt}, nil
	}
	if err != nil {
		return integration.Outcome{}, err
	}
	if len(execution.TaskData) > 0 && len(execution.Outcome) == 0 {
		return a.observeTask(ctx, row, execution)
	}
	if len(execution.Outcome) == 0 {
		return integration.Outcome{State: "unknown"}, nil
	}
	var out integration.Outcome
	if err = common.Unmarshal(execution.Outcome, &out); err != nil {
		return out, err
	}
	if len(execution.TaskData) > 0 && out.State == "succeeded" && len(out.Artifacts) == 0 {
		refreshed, taskErr := a.observeTask(ctx, row, execution)
		if taskErr != nil {
			return out, nil
		} // Keep already durable receipts visible.
		return refreshed, nil
	}
	if out.Receipt != nil {
		quota, err := strconv.Atoi(out.Receipt.RawQuota)
		if err != nil {
			return out, err
		}
		if err = model.SettleCatalogExecution(a.db, row.QuoteID, quota); err != nil {
			if !errors.Is(err, model.ErrCatalogReconciliation) {
				return integration.Outcome{State: out.State, Result: out.Result}, err
			}
			out.Receipt.IsFinal = false // Debt evidence is explicit; no automatic overcharge.
		}
		if err = a.db.Where("quote_id = ?", row.QuoteID).Take(&execution).Error; err != nil {
			return out, err
		}
		out.Receipt.SettledAt = execution.SettledAt
	}
	if out.State == "succeeded" && len(out.Artifacts) == 0 {
		if err = a.archiveNativeMedia(ctx, row, execution, &out); err != nil {
			out.Artifacts = nil
		} // Retry artifact reads independently of settlement.
	}

	saved, err := common.Marshal(out)
	if err != nil {
		return out, err
	}
	if err = a.db.Model(&model.CatalogExecution{}).Where("quote_id = ?", row.QuoteID).Update("outcome", saved).Error; err != nil {
		return out, err
	}
	return out, nil
}

func catalogOptionalString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// catalogResponse bounds the internal native response without an HTTP loopback.
// Flushed SSE is retained for durable replay; it never triggers a second relay.
type catalogResponse struct {
	stream   *integration.NativeStream
	header   http.Header
	body     bytes.Buffer
	status   int
	written  bool
	overflow bool
}

func (w *catalogResponse) Header() http.Header { return w.header }
func (w *catalogResponse) WriteHeader(status int) {
	if status > 0 && !w.written {
		w.status = status
	}
}
func (w *catalogResponse) WriteHeaderNow() { w.written = true }
func (w *catalogResponse) Write(data []byte) (int, error) {
	w.written = true
	if int64(w.body.Len()+len(data)) > integration.MediaByteLimit() {
		w.overflow = true
		return 0, fmt.Errorf("native result exceeds byte limit")
	}
	n, err := w.body.Write(data)
	if err == nil {
		w.stream.Write(w.header, w.status, data)
	}
	return n, err
}
func (w *catalogResponse) WriteString(value string) (int, error) { return w.Write([]byte(value)) }
func (w *catalogResponse) Status() int                           { return w.status }
func (w *catalogResponse) Size() int                             { return w.body.Len() }
func (w *catalogResponse) Written() bool                         { return w.written }
func (w *catalogResponse) Flush()                                { w.WriteHeaderNow() }
func (w *catalogResponse) Pusher() http.Pusher                   { return nil }
func (w *catalogResponse) CloseNotify() <-chan bool              { return make(chan bool) }
func (w *catalogResponse) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, fmt.Errorf("catalog websocket hijack is not supported")
}
