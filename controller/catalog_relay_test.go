package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	c "github.com/QuantumNous/new-api/pkg/catalogcontract"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/service"
	integration "github.com/QuantumNous/new-api/service/catalogintegration"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func catalogRelayFixture(t *testing.T, path string, operation c.Operation, upstream http.HandlerFunc) (*integration.Engine, integration.Principal, *catalogRelayAdapter, *atomic.Int32) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	oldDB, oldLog := model.DB, model.LOG_DB
	oldRedis, oldConsume, oldBatch := common.RedisEnabled, common.LogConsumeEnabled, common.BatchUpdateEnabled
	oldPath, oldMaster := common.SQLitePath, common.IsMasterNode
	common.SQLitePath = filepath.Join(t.TempDir(), "relay.db")
	common.IsMasterNode = false
	t.Setenv("SQL_DSN", "local")
	require.NoError(t, model.InitDB())
	db := model.DB
	model.LOG_DB = db
	t.Cleanup(func() { common.SQLitePath, common.IsMasterNode = oldPath, oldMaster })
	common.RedisEnabled = false
	common.LogConsumeEnabled = false
	common.BatchUpdateEnabled = false
	t.Cleanup(func() {
		model.DB, model.LOG_DB = oldDB, oldLog
		common.RedisEnabled, common.LogConsumeEnabled, common.BatchUpdateEnabled = oldRedis, oldConsume, oldBatch
		sql, _ := db.DB()
		_ = sql.Close()
	})
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.Channel{}, &model.Ability{}, &model.Option{}, &model.Log{}))
	require.NoError(t, db.AutoMigrate(model.CatalogIntegrationModels()...))
	user := model.User{Username: "catalog-real-relay", Setting: `{"billing_preference":"wallet_only"}`, Status: common.UserStatusEnabled, Group: "default", Quota: 10000000}
	require.NoError(t, db.Create(&user).Error)
	token := model.Token{UserId: user.Id, Key: "local-test-only-catalog", Status: common.TokenStatusEnabled, Group: "default", ExpiredTime: -1, RemainQuota: 10000000}
	require.NoError(t, db.Create(&token).Error)
	calls := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); upstream(w, r) }))
	t.Cleanup(server.Close)
	channel := model.Channel{Type: constant.ChannelTypeOpenAI, Key: "local-fake-provider", Status: common.ChannelStatusEnabled, BaseURL: &server.URL, Models: "fixture/catalog", Group: "default"}
	require.NoError(t, db.Create(&channel).Error)
	require.NoError(t, db.Create(&model.Ability{Group: "default", Model: "fixture/catalog", ChannelId: channel.Id, Enabled: true}).Error)
	setting := config.GlobalConfig.Get("billing_setting").(*billing_setting.BillingSetting)
	oldSetting := *setting
	*setting = billing_setting.BillingSetting{BillingMode: map[string]string{"fixture/catalog": "tiered_expr"}, BillingExpr: map[string]string{"fixture/catalog": `tier("request", fixed(0.01))`}, PluginBillingExpr: map[string]string{}}
	t.Cleanup(func() { *setting = oldSetting })
	require.NoError(t, db.Create(&model.Option{Key: "billing_setting.billing_mode", Value: `{"fixture/catalog":"tiered_expr"}`}).Error)
	require.NoError(t, db.Create(&model.Option{Key: "billing_setting.billing_expr", Value: `{"fixture/catalog":"tier(\"request\", fixed(0.01))"}`}).Error)
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })
	service.InitHttpClient()
	definition := c.OperationDefinition{Operation: operation, OperationRevision: "fixture-1", PricingRevision: "fixture-price", InputSchema: common.RawMessage(`{"type":"object","required":["model"],"properties":{"model":{"const":"fixture/catalog"}}}`), OutputSchema: common.RawMessage(`{"type":"object"}`), Limits: common.RawMessage(`{"maxInputTokens":4096,"maxOutputTokens":128}`), UsageSchema: common.RawMessage(`{"type":"object"}`), InputModalities: []string{"text"}, OutputModalities: []string{"text"}, ExecutionMode: []string{"sync"}, Protocol: "openai", SubmitPath: path}
	adapter := &catalogRelayAdapter{config: CatalogRelayOperation{Model: "fixture/catalog", ChannelID: channel.Id, Definition: definition, MaxInputTokens: 4096, MaxOutputTokens: 128}, token: &token, db: db, root: t.TempDir()}
	engine := &integration.Engine{DB: db, SourceID: "dantoken", ArtifactRoot: adapter.root, Adapters: []integration.AdapterRegistration{{Key: "relay-v1", Model: "fixture/catalog", Operation: operation, Adapter: adapter}}}
	principal := integration.Principal{ID: "fixture-principal", ResolveScope: func(context.Context) (integration.Scope, error) {
		return integration.Scope{PrincipalID: "fixture-principal", Group: "default", ScopeID: "fixture-scope", Models: []string{"fixture/catalog"}}, nil
	}, CanSubmit: func(context.Context) error { return nil }}
	return engine, principal, adapter, calls
}
func quoteCatalogNative(t *testing.T, engine *integration.Engine, p integration.Principal, operation c.Operation, requestID string, raw []byte) c.Quote {
	t.Helper()
	snap, err := engine.Catalog(context.Background(), p, "")
	require.NoError(t, err)
	require.Len(t, snap.Models[0].Operations, 1)
	q, err := engine.Quote(context.Background(), p, c.QuoteRequest{SnapshotID: snap.SnapshotID, ClientRequestID: requestID, Model: "fixture/catalog", Operation: operation, OperationRevision: snap.Models[0].Operations[0].OperationRevision, Request: raw})
	require.NoError(t, err)
	return q
}
func TestCatalogRealRelayFrozenPriceAndRecovery(t *testing.T) {
	engine, p, adapter, calls := catalogRelayFixture(t, "/v1/chat/completions", c.TextGeneration, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/chat/completions", r.URL.Path)
		assert.Equal(t, "Bearer local-fake-provider", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chat-fixture","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}}`))
	})
	raw := []byte(`{"model":"fixture/catalog","messages":[{"role":"user","content":"hello"}],"max_tokens":16}`)
	q := quoteCatalogNative(t, engine, p, c.TextGeneration, "stable-request", raw)
	setting := config.GlobalConfig.Get("billing_setting").(*billing_setting.BillingSetting)
	setting.BillingExpr["fixture/catalog"] = `tier("request", fixed(9))`
	oldUnit := common.QuotaPerUnit
	common.QuotaPerUnit = oldUnit * 2
	t.Cleanup(func() { common.QuotaPerUnit = oldUnit })
	oldGroup := ratio_setting.GroupRatio2JSONString()
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":2}`))
	t.Cleanup(func() { require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(oldGroup)) })
	invocation, err := engine.Submit(context.Background(), p, "/v1/chat/completions", q.QuoteID, "stable-request", raw)
	require.NoError(t, err)
	require.Equal(t, "succeeded", invocation.ExecutionState)
	require.NotNil(t, invocation.Receipt)
	assert.Equal(t, "10000", invocation.Receipt.Cost.AmountMicros)
	var user model.User
	require.NoError(t, adapter.db.First(&user, adapter.token.UserId).Error)
	assert.Equal(t, 9995000, user.Quota)
	engine.Now = func() time.Time { return time.Now().Add(time.Hour) }
	again, err := engine.Submit(context.Background(), p, "/v1/chat/completions", q.QuoteID, "stable-request", raw)
	require.NoError(t, err)
	assert.Equal(t, invocation.InvocationID, again.InvocationID)
	recovered, err := engine.Invocation(context.Background(), p, "stable-request", true)
	require.NoError(t, err)
	assert.Equal(t, "settled", recovered.BillingState)
	assert.EqualValues(t, 1, calls.Load())
	require.NoError(t, adapter.db.First(&user, user.Id).Error)
	assert.Equal(t, 9995000, user.Quota)
	// A real stream/binary result must not be mistaken for a JSON object merely
	// because a parser happens to accept its first byte. Native replay is separate.

}

func TestCatalogRealRelayMediaAndStream(t *testing.T) {
	cases := []struct {
		name, path                   string
		op                           c.Operation
		input, response, contentType string
		artifact                     bool
	}{
		{"responses", "/v1/responses", c.TextGeneration, `{"model":"fixture/catalog","input":"hello","max_output_tokens":16}`, `{"id":"resp_fixture","object":"response","status":"completed","output":[{"type":"message","id":"msg_fixture","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":4,"output_tokens":2,"total_tokens":6}}`, "application/json", false},
		{"multimodal", "/v1/chat/completions", c.MultimodalGeneration, `{"model":"fixture/catalog","messages":[{"role":"user","content":[{"type":"text","text":"describe"},{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aJ1cAAAAASUVORK5CYII=","detail":"low"}}]}],"max_tokens":16}`, `{"id":"vision_fixture","choices":[{"index":0,"message":{"role":"assistant","content":"pixel"},"finish_reason":"stop"}],"usage":{"prompt_tokens":85,"completion_tokens":2,"total_tokens":87}}`, "application/json", false},
		{"embedding", "/v1/embeddings", c.Embedding, `{"model":"fixture/catalog","input":["hello"]}`, `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2]}],"usage":{"prompt_tokens":2,"total_tokens":2}}`, "application/json", false},
		{"image", "/v1/images/generations", c.ImageGeneration, `{"model":"fixture/catalog","prompt":"tiny","n":1}`, `{"created":1,"data":[{"b64_json":"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aJ1cAAAAASUVORK5CYII="}],"usage":{"input_tokens":2,"output_tokens":2,"total_tokens":4}}`, "application/json", true},
		{"speech", "/v1/audio/speech", c.SpeechSynthesis, `{"model":"fixture/catalog","input":"hello","voice":"alloy","response_format":"pcm"}`, string(make([]byte, 4800)), "audio/pcm", true},
		{"stream", "/v1/chat/completions", c.TextGeneration, `{"model":"fixture/catalog","messages":[{"role":"user","content":"hello"}],"max_tokens":16,"stream":true,"stream_options":{"include_usage":true}}`, "data: {\"id\":\"stream-fixture\",\"model\":\"fixture/catalog\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"stream-fixture\",\"model\":\"fixture/catalog\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":2,\"total_tokens\":6}}\n\ndata: [DONE]\n\n", "text/event-stream", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine, p, _, calls := catalogRelayFixture(t, tc.path, tc.op, func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, tc.path, r.URL.Path)
				w.Header().Set("Content-Type", tc.contentType)
				_, _ = w.Write([]byte(tc.response))
			})
			raw := []byte(tc.input)
			q := quoteCatalogNative(t, engine, p, tc.op, "media-request", raw)
			invocation, err := engine.Submit(context.Background(), p, tc.path, q.QuoteID, "media-request", raw)
			require.NoError(t, err)
			require.Equal(t, "succeeded", invocation.ExecutionState)
			require.NotNil(t, invocation.Receipt)
			if tc.artifact {
				require.Len(t, invocation.Artifacts, 1)
				artifact := invocation.Artifacts[0]
				req := httptest.NewRequest(http.MethodGet, artifact.ReadPath, nil)
				capture := httptest.NewRecorder()
				gc, _ := gin.CreateTestContext(capture)
				gc.Request = req
				engine.Handle(gc, p)
				require.Equal(t, http.StatusOK, capture.Code, capture.Body.String())
				assert.EqualValues(t, artifact.Size, capture.Body.Len())
				other := p
				other.ID = "another-principal"
				capture = httptest.NewRecorder()
				gc, _ = gin.CreateTestContext(capture)
				gc.Request = req
				engine.Handle(gc, other)
				assert.Equal(t, http.StatusNotFound, capture.Code)
			} else {
				require.NotEmpty(t, invocation.Result)
			}
			if tc.name == "stream" {
				assert.Contains(t, string(invocation.Result), `"content":"hello"`)
			}
			_, err = engine.Invocation(context.Background(), p, "media-request", true)
			require.NoError(t, err)
			assert.EqualValues(t, 1, calls.Load())
		})
	}
}

func TestCatalogRelayDoesNotReplayLostHTTPResponse(t *testing.T) {
	var requests int
	engine, p, adapter, calls := catalogRelayFixture(t, "/v1/chat/completions", c.TextGeneration, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		r.Body.Close()
		requests++
		if requests == 2 {
			connection, _, err := w.(http.Hijacker).Hijack()
			require.NoError(t, err)
			_ = connection.Close()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`))
	})
	require.NoError(t, adapter.db.Model(&model.Channel{}).Where("id = ?", adapter.config.ChannelID).Update("header_override", `{"Idempotency-Key":"provider-test-key"}`).Error)
	raw := []byte(`{"model":"fixture/catalog","messages":[{"role":"user","content":"hello"}],"max_tokens":16}`)
	for i, id := range []string{"warm-connection", "lost-response"} {
		q := quoteCatalogNative(t, engine, p, c.TextGeneration, id, raw)
		_, err := engine.Submit(context.Background(), p, "/v1/chat/completions", q.QuoteID, id, raw)
		if i == 0 {
			require.NoError(t, err)
		} else {
			require.ErrorContains(t, err, "SUBMISSION_UNKNOWN")
		}
	}
	recovered, err := engine.Invocation(context.Background(), p, "lost-response", true)
	require.NoError(t, err)
	assert.Equal(t, "unknown", recovered.ExecutionState)
	assert.Equal(t, "pending", recovered.BillingState)
	assert.EqualValues(t, 2, calls.Load())
}

func TestCatalogRealTaskPluginSubmissionObservationAndArtifacts(t *testing.T) {
	var posts, queries, downloads atomic.Int32
	engine, p, adapter, _ := catalogRelayFixture(t, "/v1/videos", c.VideoGeneration, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/submit":
			posts.Add(1)
			_, _ = w.Write([]byte(`{"id":"provider-task"}`))
		case "/tasks/provider-task":
			phase := queries.Add(1)
			if phase == 1 {
				http.NotFound(w, r)
				return
			}
			if phase == 2 {
				_, _ = w.Write([]byte(`{"id":"provider-task","status":"IN_PROGRESS","seconds":3}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":"provider-task","status":"SUCCESS","seconds":3}`))
		case "/content":
			if downloads.Add(1) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write([]byte("\x00\x00\x00\x18ftypmp42fixture-video"))
		default:
			http.NotFound(w, r)
		}
	})
	oldRegistry := pluginruntime.DefaultRegistry
	pluginruntime.DefaultRegistry = pluginruntime.NewRegistry()
	t.Cleanup(func() { pluginruntime.DefaultRegistry = oldRegistry })
	_, err := pluginruntime.DefaultRegistry.Register(`
 export const meta={apiVersion:1,key:"catalog-video",name:"Catalog video fixture",version:"1.0.0",author:{name:"Test"},models:["fixture/catalog"],fetchMode:"per_task",protocols:["openai_video"],usageSchema:{seconds:{type:"number",unit:"second",description:{en:"Video generation unit price",zh:"视频生成单价"}}}};
 export function buildSubmitRequest(ctx){return {url:ctx.baseUrl+"/submit",method:"POST",body:ctx.requestBody,action:"text_to_video",model:ctx.model};}
 export function parseSubmitResponse(ctx,response){return {taskId:response.body.id,taskData:response.body};}
 export function extractUsage(ctx){return {seconds:ctx.requestBody.seconds};}
 export function buildQueryRequest(ctx){return {url:ctx.baseUrl+"/tasks/"+ctx.taskId,method:"GET"};}
 export function parseTaskResult(ctx,body){return {taskId:body.id,status:body.status,progress:"100%"};}
 export function extractUsageOnComplete(ctx,result,data){return {seconds:data.seconds};}
 export function listArtifacts(ctx){return [{key:"video",type:"video",mimeType:"video/mp4"}];}
 export function buildContentRequest(ctx){return {url:ctx.baseUrl+"/content",method:"GET"};}
 export const protocols={openai_video:{decodeRequest:function(ctx){return {kind:"submit",model:ctx.model,requestBody:ctx.body.value};},render:function(ctx,task){return {id:task.task_id};}}};
 `, pluginruntime.Options{})
	require.NoError(t, err)
	require.NoError(t, adapter.db.Model(&model.Channel{}).Where("id = ?", adapter.config.ChannelID).Updates(map[string]any{"type": constant.ChannelTypeTaskPlugin, "setting": `{"task_plugin_key":"catalog-video"}`}).Error)
	setting := config.GlobalConfig.Get("billing_setting").(*billing_setting.BillingSetting)
	setting.BillingExpr["fixture/catalog"] = `tier("video", u("seconds") * 0.001)`
	adapter.config.UsageBounds = map[string]billingexpr.NumericRange{"seconds": {Min: 0, Max: 10}}
	fetch := system_setting.GetFetchSetting()
	oldFetch := *fetch
	fetch.EnableSSRFProtection = false
	t.Cleanup(func() { *fetch = oldFetch }) // local test server only
	model.InitChannelCache()
	raw := []byte(`{"model":"fixture/catalog","prompt":"tiny","seconds":3}`)
	q := quoteCatalogNative(t, engine, p, c.VideoGeneration, "video-request", raw)
	invocation, err := engine.Submit(context.Background(), p, "/v1/videos", q.QuoteID, "video-request", raw)
	require.NoError(t, err)
	assert.Equal(t, "running", invocation.ExecutionState)
	assert.Equal(t, "pending", invocation.BillingState)
	// Process restart with no active operation manifest retains frozen recovery.
	t.Setenv("DANTOKEN_CATALOG_OPERATIONS_FILE", "")
	t.Setenv("DANTOKEN_ARTIFACT_DIR", adapter.root)
	restarted, err := NewCatalogRelayEngine(nil, adapter.token)
	require.NoError(t, err)
	invocation, err = restarted.Invocation(context.Background(), p, "video-request", true)
	require.NoError(t, err)
	assert.Equal(t, "running", invocation.ExecutionState)
	invocation, err = restarted.Invocation(context.Background(), p, "video-request", true)
	require.NoError(t, err)
	assert.Equal(t, "succeeded", invocation.ExecutionState)
	assert.Equal(t, "settled", invocation.BillingState)
	assert.Empty(t, invocation.Artifacts)
	require.NotNil(t, invocation.Receipt)
	receipt := *invocation.Receipt
	invocation, err = restarted.Invocation(context.Background(), p, "video-request", true)
	require.NoError(t, err)
	assert.Equal(t, receipt, *invocation.Receipt)
	require.NotNil(t, invocation.Receipt)
	assert.Equal(t, "3000", invocation.Receipt.Cost.AmountMicros)
	require.Len(t, invocation.Artifacts, 1)
	_, err = engine.Invocation(context.Background(), p, "video-request", true)
	require.NoError(t, err)
	assert.EqualValues(t, 1, posts.Load())
	assert.EqualValues(t, 3, queries.Load())
	assert.EqualValues(t, 2, downloads.Load())
	var user model.User
	require.NoError(t, adapter.db.First(&user, adapter.token.UserId).Error)
	assert.Equal(t, 9998500, user.Quota)
}

func TestCatalogNativeMultipartRealRelay(t *testing.T) {
	for _, tc := range []struct {
		path, field, filename, mime, response string
		op                                    c.Operation
	}{
		{"/v1/audio/transcriptions", "file", "input.wav", "audio/wav", `{"text":"hello","duration":1}`, c.AudioTranscription},
		{"/v1/images/edits", "image", "input.png", "image/png", `{"data":[{"b64_json":"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aJ1cAAAAASUVORK5CYII="}],"usage":{"input_tokens":2,"output_tokens":2,"total_tokens":4}}`, c.ImageEdit},
	} {
		t.Run(string(tc.op), func(t *testing.T) {
			data := []byte("local-fixture-content")
			engine, p, _, calls := catalogRelayFixture(t, tc.path, tc.op, func(w http.ResponseWriter, r *http.Request) {
				require.NoError(t, r.ParseMultipartForm(1<<20))
				assert.Equal(t, "fixture/catalog", r.FormValue("model"))
				file, header, err := r.FormFile(tc.field)
				require.NoError(t, err)
				defer file.Close()
				actual, err := io.ReadAll(file)
				require.NoError(t, err)
				assert.Equal(t, data, actual)
				assert.Equal(t, tc.filename, header.Filename)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.response))
			})
			hash := sha256.Sum256(data)
			file := integration.NativeFile{SHA256: hex.EncodeToString(hash[:]), Size: int64(len(data)), MIME: tc.mime, Filename: tc.filename}
			raw, err := common.Marshal(map[string]any{"model": "fixture/catalog", tc.field: file, "prompt": "describe", "response_format": "json"})
			require.NoError(t, err)
			q := quoteCatalogNative(t, engine, p, tc.op, "multipart-request", raw)
			var buffer bytes.Buffer
			writer := multipart.NewWriter(&buffer)
			require.NoError(t, writer.WriteField("model", `"fixture/catalog"`))
			require.NoError(t, writer.WriteField("prompt", `"describe"`))
			require.NoError(t, writer.WriteField("response_format", `"json"`))
			header := make(textproto.MIMEHeader)
			header.Set("Content-Disposition", `form-data; name="`+tc.field+`"; filename="`+tc.filename+`"`)
			header.Set("Content-Type", tc.mime)
			part, err := writer.CreatePart(header)
			require.NoError(t, err)
			_, err = part.Write(data)
			require.NoError(t, err)
			require.NoError(t, writer.Close())
			request := httptest.NewRequest(http.MethodPost, tc.path, &buffer)
			request.Header.Set("Content-Type", writer.FormDataContentType())
			native, files, err := integration.DecodeNativeInput(request)
			require.NoError(t, err)
			result, err := engine.Submit(integration.WithNativeFiles(context.Background(), files), p, tc.path, q.QuoteID, "multipart-request", native)
			require.NoError(t, err)
			assert.Equal(t, "succeeded", result.ExecutionState)
			require.NotNil(t, result.Receipt)
			assert.EqualValues(t, 1, calls.Load())
			q2 := quoteCatalogNative(t, engine, p, tc.op, "missing-file", raw)
			result, err = engine.Submit(context.Background(), p, tc.path, q2.QuoteID, "missing-file", raw)
			require.NoError(t, err)
			assert.Equal(t, "not_charged", result.BillingState)
			assert.EqualValues(t, 1, calls.Load())
		})
	}
}

func TestCatalogLegacyPricingIsFrozen(t *testing.T) {
	engine, p, adapter, calls := catalogRelayFixture(t, "/v1/chat/completions", c.TextGeneration, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"legacy","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}}`))
	})
	setting := config.GlobalConfig.Get("billing_setting").(*billing_setting.BillingSetting)
	setting.BillingMode = map[string]string{}
	old := ratio_setting.ModelRatio2JSONString()
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"fixture/catalog":2}`))
	t.Cleanup(func() { require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(old)) })
	raw := []byte(`{"model":"fixture/catalog","messages":[{"role":"user","content":"hello"}],"max_tokens":16}`)
	quote := quoteCatalogNative(t, engine, p, c.TextGeneration, "legacy-price", raw)
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"fixture/catalog":200}`))
	result, err := engine.Submit(context.Background(), p, "/v1/chat/completions", quote.QuoteID, "legacy-price", raw)
	require.NoError(t, err)
	require.NotNil(t, result.Receipt)
	assert.Equal(t, "12", result.Receipt.RawQuota)
	var user model.User
	require.NoError(t, adapter.db.First(&user, adapter.token.UserId).Error)
	assert.Equal(t, 9999988, user.Quota)
	assert.EqualValues(t, 1, calls.Load())
}

func TestCatalogTruncatedStreamCannotSettle(t *testing.T) {
	engine, p, _, calls := catalogRelayFixture(t, "/v1/chat/completions", c.TextGeneration, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"partial\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n"))
	})
	raw := []byte(`{"model":"fixture/catalog","messages":[{"role":"user","content":"hello"}],"max_tokens":16,"stream":true}`)
	quote := quoteCatalogNative(t, engine, p, c.TextGeneration, "truncated", raw)
	_, err := engine.Submit(context.Background(), p, "/v1/chat/completions", quote.QuoteID, "truncated", raw)
	require.ErrorContains(t, err, "SUBMISSION_UNKNOWN")
	recovered, err := engine.Invocation(context.Background(), p, "truncated", true)
	require.NoError(t, err)
	assert.Equal(t, "unknown", recovered.ExecutionState)
	assert.Equal(t, "pending", recovered.BillingState)
	assert.EqualValues(t, 1, calls.Load())
}

func TestCatalogOverrunBlocksOnlyTheAffectedOperation(t *testing.T) {
	engine, p, adapter, calls := catalogRelayFixture(t, "/v1/chat/completions", c.TextGeneration, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"overrun","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":6000,"completion_tokens":1,"total_tokens":6001}}`))
	})
	setting := config.GlobalConfig.Get("billing_setting").(*billing_setting.BillingSetting)
	setting.BillingExpr["fixture/catalog"] = `tier("tokens", p * 2)`
	raw := []byte(`{"model":"fixture/catalog","messages":[{"role":"user","content":"hello"}],"max_tokens":16}`)
	quote := quoteCatalogNative(t, engine, p, c.TextGeneration, "overrun", raw)
	result, err := engine.Submit(context.Background(), p, "/v1/chat/completions", quote.QuoteID, "overrun", raw)
	require.NoError(t, err)
	assert.Equal(t, "reconciliation_required", result.BillingState)
	require.NotNil(t, result.Receipt)
	assert.False(t, result.Receipt.IsFinal)
	other := *adapter
	other.config.Definition.Operation = c.Embedding
	other.config.Definition.SubmitPath = "/v1/embeddings"
	engine.Adapters = append(engine.Adapters, integration.AdapterRegistration{Key: "other", Model: other.config.Model, Operation: c.Embedding, Adapter: &other})
	catalog, err := engine.Catalog(context.Background(), p, "")
	require.NoError(t, err)
	require.Len(t, catalog.Models[0].Operations, 1)
	assert.Equal(t, c.Embedding, catalog.Models[0].Operations[0].Operation)
	recovered, err := engine.Invocation(context.Background(), p, "overrun", true)
	require.NoError(t, err)
	assert.Equal(t, "reconciliation_required", recovered.BillingState)
	assert.EqualValues(t, 1, calls.Load())
}

func TestCatalogMaintenancePublishRetireAndDependencyDrift(t *testing.T) {
	_, p, adapter, calls := catalogRelayFixture(t, "/v1/chat/completions", c.TextGeneration, func(http.ResponseWriter, *http.Request) { t.Fatal("maintenance must not invoke provider") })
	adapter.config.Definition.Protocol = "openai.chat_completions"
	adapter.config.Definition.Capabilities = common.RawMessage(`{"reasoningSupported":false}`)
	t.Setenv("DANTOKEN_ARTIFACT_DIR", adapter.root)
	raw, err := common.Marshal(adapter.config)
	require.NoError(t, err)
	entries := []model.CatalogMaintenanceEntry{{Model: adapter.config.Model, Owner: "fixture-owner", Operations: []model.CatalogMaintenanceOperation{{Operation: c.TextGeneration, Config: raw, Evidence: "local fixture only"}}}}
	require.NoError(t, normalizeCatalogMaintenance(entries))
	require.Empty(t, validateCatalogMaintenance(entries))
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(ctx *gin.Context) { ctx.Set("id", 1) })
	router.PUT("/draft", SaveCatalogMaintenanceDraft)
	router.POST("/publish", PublishCatalogMaintenance)
	router.POST("/retire", RetireCatalogMaintenance)
	router.GET("/", GetCatalogMaintenance)
	invoke := func(method, path string, body any) *httptest.ResponseRecorder {
		payload, er := common.Marshal(body)
		require.NoError(t, er)
		req := httptest.NewRequest(method, path, bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		res := httptest.NewRecorder()
		router.ServeHTTP(res, req)
		return res
	}
	request := catalogMaintenanceRequest{Entries: entries, Reason: "fixture review", ActivateDatabase: true}
	require.Equal(t, 200, invoke("PUT", "/draft", request).Code)
	state, err := model.ReadCatalogDirectory(model.DB)
	require.NoError(t, err)
	assert.False(t, state.DatabaseEnabled)
	// Stale writes and missing evidence cannot publish.
	assert.Equal(t, 409, invoke("PUT", "/draft", request).Code)
	request.DraftRevision = 1
	request.Selections = []catalogSelection{{Model: adapter.config.Model, Operation: c.TextGeneration}}
	withoutActivation := request
	withoutActivation.ActivateDatabase = false
	assert.Equal(t, 409, invoke("POST", "/publish", withoutActivation).Code)
	require.Equal(t, 200, invoke("POST", "/publish", request).Code)
	engine, err := NewCatalogRelayEngine(nil, adapter.token)
	require.NoError(t, err)
	require.NoError(t, engine.AdmissionError)
	snap, err := engine.Catalog(context.Background(), p, "")
	require.NoError(t, err)
	require.Len(t, snap.Models, 1)
	// Database mode cannot fall back to a changed/broken manifest.
	t.Setenv("DANTOKEN_CATALOG_OPERATIONS_FILE", "/nonexistent/should-not-be-read")
	engine, err = NewCatalogRelayEngine(nil, adapter.token)
	require.NoError(t, err)
	require.NoError(t, engine.AdmissionError)
	require.NoError(t, model.DB.Model(&model.Option{}).Where("key = ?", "billing_setting.billing_expr").Update("value", `{"fixture/catalog":"tier(\"request\", fixed(0.01))","unrelated":"tier(\"request\", fixed(9))"}`).Error)
	unchanged, checkErr := engine.Catalog(context.Background(), p, "")
	require.NoError(t, checkErr)
	require.Len(t, unchanged.Models, 1)
	request.DraftRevision = 2
	request.PublishedRevision = 1
	require.Equal(t, 200, invoke("POST", "/retire", request).Code)
	_, err = engine.Catalog(context.Background(), p, "")
	require.ErrorContains(t, err, "CATALOG_CONFLICT")
	engine, err = NewCatalogRelayEngine(nil, adapter.token)
	require.NoError(t, err)
	empty, err := engine.Catalog(context.Background(), p, "")
	require.NoError(t, err)
	assert.Empty(t, empty.Models)
	assert.Equal(t, "supply_empty", empty.EmptyReason)
	require.Len(t, empty.Removals, 1)
	request.DraftRevision = 3
	request.PublishedRevision = 2
	require.Equal(t, 200, invoke("POST", "/publish", request).Code)
	engine, err = NewCatalogRelayEngine(nil, adapter.token)
	require.NoError(t, err)
	require.NoError(t, model.DB.Model(&model.Option{}).Where("key = ?", "billing_setting.billing_expr").Update("value", `{"fixture/catalog":"tier(\"request\", fixed(0.02))"}`).Error)
	changed, err := engine.Catalog(context.Background(), p, "")
	require.NoError(t, err)
	assert.Empty(t, changed.Models)
	// A deleted channel is an operation-local removal, not a broken whole catalog.
	require.NoError(t, model.DB.Delete(&model.Channel{}, adapter.config.ChannelID).Error)
	missing, err := engine.Catalog(context.Background(), p, "")
	require.NoError(t, err)
	assert.Empty(t, missing.Models)
	assert.Zero(t, calls.Load())
	if path := os.Getenv("CATALOG_WIRE_EXPORT"); path != "" {
		wire, marshalErr := common.Marshal(map[string]c.CatalogSnapshot{"published": snap, "unchanged": unchanged, "retired": empty, "dependencyChanged": changed})
		require.NoError(t, marshalErr)
		require.NoError(t, os.WriteFile(path, wire, 0600))
	}
	candidates := catalogCandidateEntries()
	require.Len(t, candidates, 25)
	require.NoError(t, normalizeCatalogMaintenance(candidates))
	for _, candidate := range candidates {
		assert.NotEmpty(t, validateCatalogMaintenance([]model.CatalogMaintenanceEntry{candidate}))
	}
}
