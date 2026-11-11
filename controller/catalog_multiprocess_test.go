package controller

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	c "github.com/QuantumNous/new-api/pkg/catalogcontract"
	"github.com/QuantumNous/new-api/service"
	integration "github.com/QuantumNous/new-api/service/catalogintegration"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Only test children consume this file. Every server uses the production
// credential middleware, catalog engine, pinned relay and wallet journal.
type catalogProcessConfig struct {
	DSN, SQLite, Manifest, Root, Ready, Barrier, Release, Stage string
}
type catalogProcess struct {
	cmd *exec.Cmd
	URL string
	cfg catalogProcessConfig
}
type catalogHTTPResult struct {
	Status int
	Body   []byte
	Err    error
}

func catalogProcessOpen(t *testing.T, cfg catalogProcessConfig) *gorm.DB {
	t.Helper()
	t.Setenv("SQL_DSN", cfg.DSN)
	t.Setenv("DANTOKEN_CATALOG_ENABLED", "true")
	t.Setenv("DANTOKEN_CATALOG_OPERATIONS_FILE", cfg.Manifest)
	t.Setenv("DANTOKEN_ARTIFACT_DIR", cfg.Root)
	oldDB, oldLog, oldPath, oldMaster, oldType := model.DB, model.LOG_DB, common.SQLitePath, common.IsMasterNode, common.MainDatabaseType()
	oldRedis, oldConsume, oldBatch := common.RedisEnabled, common.LogConsumeEnabled, common.BatchUpdateEnabled
	common.SQLitePath = cfg.SQLite
	// The parent bootstraps the isolated database; child servers require its migrated schema.
	common.IsMasterNode = os.Getenv("CATALOG_PROCESS_CHILD_CONFIG") == ""
	require.NoError(t, model.InitDB())
	common.IsMasterNode = false
	db := model.DB
	model.LOG_DB = db
	db.Logger = logger.Default.LogMode(logger.Silent)
	common.RedisEnabled = false
	common.LogConsumeEnabled = false
	common.BatchUpdateEnabled = false
	setting := config.GlobalConfig.Get("billing_setting").(*billing_setting.BillingSetting)
	oldSetting := *setting
	*setting = billing_setting.BillingSetting{BillingMode: map[string]string{"fixture/catalog": "tiered_expr"}, BillingExpr: map[string]string{"fixture/catalog": `tier("request", fixed(0.01))`}, PluginBillingExpr: map[string]string{}}
	service.InitHttpClient()
	t.Cleanup(func() {
		sql, _ := db.DB()
		_ = sql.Close()
		model.DB, model.LOG_DB = oldDB, oldLog
		common.SQLitePath, common.IsMasterNode = oldPath, oldMaster
		common.SetMainDatabaseType(oldType)
		common.RedisEnabled, common.LogConsumeEnabled, common.BatchUpdateEnabled = oldRedis, oldConsume, oldBatch
		*setting = oldSetting
	})
	return db
}

func catalogProcessBarrier(t *testing.T, cfg catalogProcessConfig) {
	t.Helper()
	require.NoError(t, os.WriteFile(cfg.Barrier, []byte(fmt.Sprint(os.Getpid())), 0600))
	require.Eventually(t, func() bool { _, err := os.Stat(cfg.Release); return err == nil }, 20*time.Second, 10*time.Millisecond, "test barrier release")
}

func TestCatalogProcessServer(t *testing.T) {
	path := os.Getenv("CATALOG_PROCESS_CHILD_CONFIG")
	if path == "" {
		return
	}
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var cfg catalogProcessConfig
	require.NoError(t, json.Unmarshal(raw, &cfg))
	db := catalogProcessOpen(t, cfg)
	var blocked atomic.Bool
	if cfg.Stage == "outcome_persisted" || cfg.Stage == "acceptance_committed" {
		require.NoError(t, db.Callback().Update().After("gorm:commit_or_rollback_transaction").Register("test:durable_outcome", func(tx *gorm.DB) {
			values, ok := tx.Statement.Dest.(map[string]any)
			matches := ok && ((cfg.Stage == "outcome_persisted" && tx.Statement.Table == "catalog_executions" && values["outcome"] != nil) || (cfg.Stage == "acceptance_committed" && tx.Statement.Table == "catalog_invocation_records" && values["state"] == "unknown"))
			if matches && tx.Error == nil && blocked.CompareAndSwap(false, true) {
				catalogProcessBarrier(t, cfg)
			}
		}))
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/__fixture/policy/:mode", func(gc *gin.Context) {
		var binding model.CatalogIntegrationBinding
		if err := db.Where("principal_id = ?", "process-A").Take(&binding).Error; err != nil {
			gc.AbortWithStatusJSON(500, err.Error())
			return
		}
		binding.AllowedOperations = `["text_generation"]`
		if gc.Param("mode") == "deny" {
			binding.AllowedOperations = `["embedding"]`
		} else if gc.Param("mode") != "allow" {
			gc.AbortWithStatus(400)
			return
		}
		if err := model.SaveCatalogBinding(db, binding, binding.Revision, 1, "isolated process race"); err != nil {
			gc.AbortWithStatusJSON(500, err.Error())
			return
		}
		gc.Status(204)
	})
	router.POST("/__fixture/retire-directory", func(gc *gin.Context) {
		current, err := model.ReadCatalogDirectory(db)
		if err == nil {
			err = model.SaveCatalogDirectory(db, current, []byte("[]"), []byte("[]"), "retire", "isolated database takeover", 1, true)
		}
		if err != nil {
			gc.AbortWithStatusJSON(500, err.Error())
			return
		}
		gc.Status(204)
	})
	router.NoRoute(middleware.CatalogIntegration(func(gc *gin.Context, token *model.Token) (*integration.Engine, error) {
		engine, err := NewCatalogRelayEngine(gc, token)
		if err != nil {
			return nil, err
		}
		if gc.GetHeader("X-Catalog-Test-Barrier") == "yes" {
			if cfg.Stage == "before_acceptance" {
				catalogProcessBarrier(t, cfg)
			}
		}
		return engine, nil
	}))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ready, _ := json.Marshal(map[string]any{"url": "http://" + listener.Addr().String(), "pid": os.Getpid()})
	require.NoError(t, os.WriteFile(cfg.Ready, ready, 0600))
	require.NoError(t, http.Serve(listener, router))
}

func startCatalogProcess(t *testing.T, base catalogProcessConfig, stage string) *catalogProcess {
	t.Helper()
	dir := t.TempDir()
	base.Ready = filepath.Join(dir, "ready")
	base.Barrier = filepath.Join(dir, "barrier")
	base.Release = filepath.Join(dir, "release")
	base.Stage = stage
	raw, err := json.Marshal(base)
	require.NoError(t, err)
	path := filepath.Join(dir, "config")
	require.NoError(t, os.WriteFile(path, raw, 0600))
	cmd := exec.Command(os.Args[0], "-test.run=^TestCatalogProcessServer$", "-test.timeout=120s")
	cmd.Env = append(os.Environ(), "CATALOG_PROCESS_CHILD_CONFIG="+path)
	log, err := os.Create(filepath.Join(dir, "process.log"))
	require.NoError(t, err)
	cmd.Stdout = log
	cmd.Stderr = log
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = log.Close()
		if t.Failed() {
			data, _ := os.ReadFile(filepath.Join(dir, "process.log"))
			t.Log(string(data))
		}
	})
	var ready struct {
		URL string
		PID int
	}
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(base.Ready)
		return err == nil && json.Unmarshal(data, &ready) == nil
	}, 20*time.Second, 20*time.Millisecond)
	require.NotEqual(t, os.Getpid(), ready.PID)
	t.Logf("child PID=%d stage=%s", ready.PID, stage)
	return &catalogProcess{cmd: cmd, URL: ready.URL, cfg: base}
}
func catalogProcessRequest(url, method, path, key, quote, id string, body []byte, barrier bool) catalogHTTPResult {
	req, err := http.NewRequest(method, url+path, bytes.NewReader(body))
	if err != nil {
		return catalogHTTPResult{Err: err}
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-DanToken-Quote-ID", quote)
	req.Header.Set("Idempotency-Key", id)
	if barrier {
		req.Header.Set("X-Catalog-Test-Barrier", "yes")
	}
	client := http.Client{Timeout: 25 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return catalogHTTPResult{Err: err}
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	return catalogHTTPResult{Status: res.StatusCode, Body: data, Err: err}
}
func catalogProcessQuote(t *testing.T, p *catalogProcess, key, id string, raw []byte) c.Quote {
	t.Helper()
	result := catalogProcessRequest(p.URL, "GET", "/v1/catalog", key, "", "", nil, false)
	require.NoError(t, result.Err)
	require.Equal(t, 200, result.Status, string(result.Body))
	var snapshot c.CatalogSnapshot
	require.NoError(t, json.Unmarshal(result.Body, &snapshot))
	require.Len(t, snapshot.Models, 1)
	require.Len(t, snapshot.Models[0].Operations, 1)
	req := c.QuoteRequest{SnapshotID: snapshot.SnapshotID, ClientRequestID: id, Model: "fixture/catalog", Operation: c.TextGeneration, OperationRevision: snapshot.Models[0].Operations[0].OperationRevision, Request: raw}
	body, err := json.Marshal(req)
	require.NoError(t, err)
	result = catalogProcessRequest(p.URL, "POST", "/v1/quotes", key, "", "", body, false)
	require.NoError(t, result.Err)
	require.Equal(t, 200, result.Status, string(result.Body))
	var quote c.Quote
	require.NoError(t, json.Unmarshal(result.Body, &quote))
	return quote
}
func catalogProcessObserve(t *testing.T, p *catalogProcess, key, id string) c.Invocation {
	t.Helper()
	r := catalogProcessRequest(p.URL, "GET", "/v1/invocations/"+id, key, "", "", nil, false)
	require.NoError(t, r.Err)
	require.Equal(t, 200, r.Status, string(r.Body))
	var inv c.Invocation
	require.NoError(t, json.Unmarshal(r.Body, &inv))
	return inv
}

func TestCatalogMultiProcessRelay(t *testing.T) {
	if os.Getenv("CATALOG_PROCESS_RUN") != "1" {
		t.Skip("explicit isolated database opt-in required")
	}
	dir := t.TempDir()
	dsn := os.Getenv("CATALOG_PROCESS_DSN")
	if dsn == "" {
		dsn = "local"
	}
	cfg := catalogProcessConfig{DSN: dsn, SQLite: filepath.Join(dir, "catalog.db") + "?_pragma=busy_timeout(10000)", Manifest: filepath.Join(dir, "manifest.json"), Root: dir}
	db := catalogProcessOpen(t, cfg)
	tables := append([]any{&model.User{}, &model.Token{}, &model.Channel{}, &model.Ability{}, &model.Option{}, &model.Log{}}, model.CatalogIntegrationModels()...)
	require.NoError(t, db.AutoMigrate(tables...))
	var count int64
	require.NoError(t, db.Model(&model.User{}).Count(&count).Error)
	require.Zero(t, count, "use an empty dedicated test database")
	user := model.User{Username: "process-user", Setting: `{"billing_preference":"wallet_only"}`, Status: common.UserStatusEnabled, Group: "default", Quota: 10000000}
	require.NoError(t, db.Create(&user).Error)
	keyA, keyB := "localprocessTokenA", "localprocessTokenB"
	for i, key := range []string{keyA, keyB} {
		token := model.Token{UserId: user.Id, Key: key, Status: common.TokenStatusEnabled, Group: "default", ExpiredTime: -1, RemainQuota: 10000000}
		require.NoError(t, db.Create(&token).Error)
		binding := model.CatalogIntegrationBinding{TokenID: token.Id, UserID: user.Id, PrincipalID: []string{"process-A", "process-B"}[i], FixedGroup: "default", Enabled: true, AllowedOperations: `["text_generation"]`}
		require.NoError(t, model.SaveCatalogBinding(db, binding, 0, 1, "isolated fixture"))
	}
	var calls atomic.Int32
	providerEntered := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "block-provider") {
			providerEntered <- struct{}{}
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"local-provider","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}}`))
	}))
	t.Cleanup(server.Close)
	channel := model.Channel{Type: constant.ChannelTypeOpenAI, Key: "local-provider", Status: common.ChannelStatusEnabled, BaseURL: &server.URL, Models: "fixture/catalog", Group: "default"}
	require.NoError(t, db.Create(&channel).Error)
	require.NoError(t, db.Create(&model.Ability{Group: "default", Model: "fixture/catalog", ChannelId: channel.Id, Enabled: true}).Error)
	require.NoError(t, db.Create(&model.Option{Key: "billing_setting.billing_mode", Value: `{"fixture/catalog":"tiered_expr"}`}).Error)
	require.NoError(t, db.Create(&model.Option{Key: "billing_setting.billing_expr", Value: `{"fixture/catalog":"tier(\"request\", fixed(0.01))"}`}).Error)
	definition := c.OperationDefinition{Operation: c.TextGeneration, OperationRevision: "fixture-1", PricingRevision: "fixture-price", InputSchema: common.RawMessage(`{"type":"object","required":["model"],"properties":{"model":{"const":"fixture/catalog"}}}`), OutputSchema: common.RawMessage(`{"type":"object"}`), Limits: common.RawMessage(`{"maxInputTokens":4096,"maxOutputTokens":128}`), UsageSchema: common.RawMessage(`{"type":"object"}`), InputModalities: []string{"text"}, OutputModalities: []string{"text"}, ExecutionMode: []string{"sync"}, Protocol: "openai.chat_completions", SubmitPath: "/v1/chat/completions"}
	manifest, err := json.Marshal([]CatalogRelayOperation{{Model: "fixture/catalog", ChannelID: channel.Id, Definition: definition, MaxInputTokens: 4096, MaxOutputTokens: 128}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(cfg.Manifest, manifest, 0600))
	first, second := startCatalogProcess(t, cfg, ""), startCatalogProcess(t, cfg, "")
	raw := []byte(`{"model":"fixture/catalog","messages":[{"role":"user","content":"hello"}],"max_tokens":16}`)
	wallet := func(t *testing.T, expected int) {
		t.Helper()
		var u model.User
		require.NoError(t, db.First(&u, user.Id).Error)
		require.Equal(t, expected, u.Quota)
		var tokens []model.Token
		require.NoError(t, db.Find(&tokens).Error)
		used := 0
		for _, token := range tokens {
			require.Equal(t, 10000000, token.RemainQuota+token.UsedQuota)
			used += token.UsedQuota
		}
		require.Equal(t, 10000000-expected, used)
	}
	suite := t
	t.Run("concurrent_once_and_conflict", func(t *testing.T) {
		q := catalogProcessQuote(t, first, keyA, "race", raw)
		start := make(chan struct{})
		results := make(chan catalogHTTPResult, 10)
		var wg sync.WaitGroup
		for i := range 10 {
			wg.Go(func() {
				<-start
				p := []*catalogProcess{first, second}[i%2]
				results <- catalogProcessRequest(p.URL, "POST", "/v1/chat/completions", keyA, q.QuoteID, "race", raw, false)
			})
		}
		close(start)
		wg.Wait()
		close(results)
		for r := range results {
			require.NoError(t, r.Err)
			if common.MainDatabaseType() == common.DatabaseTypeSQLite && r.Status == 409 {
				require.Contains(t, string(r.Body), "INVOCATION_CONFLICT")
			} else {
				require.Contains(t, []int{200, 202}, r.Status, string(r.Body))
			}
		}
		require.EqualValues(t, 1, calls.Load())
		wallet(t, 9995000)
		inv := catalogProcessObserve(t, second, keyA, "race")
		require.Equal(t, "settled", inv.BillingState)
		require.Len(t, inv.Receipts, 1)
		conflict := bytes.ReplaceAll(raw, []byte("hello"), []byte("changed"))
		r := catalogProcessRequest(second.URL, "POST", "/v1/chat/completions", keyA, q.QuoteID, "race", conflict, false)
		require.NoError(t, r.Err)
		require.Equal(t, 409, r.Status)
		require.Contains(t, string(r.Body), "IDEMPOTENCY_CONFLICT")
		require.NoError(t, db.Model(&model.CatalogExecution{}).Count(&count).Error)
		require.EqualValues(t, 1, count)
		require.NoError(t, first.cmd.Process.Kill())
		require.NoError(t, second.cmd.Process.Kill())
		first, second = startCatalogProcess(suite, cfg, ""), startCatalogProcess(suite, cfg, "")
		again := catalogProcessObserve(t, first, keyA, "race")
		require.Equal(t, inv.InvocationID, again.InvocationID)
		require.Len(t, again.Receipts, 1)
		wallet(t, 9995000)
		require.EqualValues(t, 1, calls.Load())
		foreign := catalogProcessRequest(second.URL, "GET", "/v1/invocations/race", keyB, "", "", nil, false)
		require.Equal(t, 404, foreign.Status)
	})
	if t.Failed() {
		return
	}
	t.Run("kill_after_provider_send_unknown", func(t *testing.T) {
		request := bytes.ReplaceAll(raw, []byte("hello"), []byte("block-provider"))
		q := catalogProcessQuote(t, first, keyA, "unknown", request)
		finished := make(chan catalogHTTPResult, 1)
		go func() {
			finished <- catalogProcessRequest(first.URL, "POST", "/v1/chat/completions", keyA, q.QuoteID, "unknown", request, false)
		}()
		select {
		case <-providerEntered:
		case <-time.After(15 * time.Second):
			t.Fatal("provider not reached")
		}
		require.NoError(t, first.cmd.Process.Kill())
		require.Error(t, (<-finished).Err)
		inv := catalogProcessObserve(t, second, keyA, "unknown")
		require.Equal(t, "unknown", inv.ExecutionState)
		require.Empty(t, inv.Receipts)
		duplicate := catalogProcessRequest(second.URL, "POST", "/v1/chat/completions", keyA, q.QuoteID, "unknown", request, false)
		require.Equal(t, 202, duplicate.Status)
		require.EqualValues(t, 2, calls.Load())
		wallet(t, 9989999)
	})
	if t.Failed() {
		return
	}
	t.Run("kill_after_durable_result_before_settle", func(t *testing.T) {
		p := startCatalogProcess(t, cfg, "outcome_persisted")
		q := catalogProcessQuote(t, second, keyA, "durable", raw)
		finished := make(chan catalogHTTPResult, 1)
		go func() {
			finished <- catalogProcessRequest(p.URL, "POST", "/v1/chat/completions", keyA, q.QuoteID, "durable", raw, false)
		}()
		require.Eventually(t, func() bool { _, err := os.Stat(p.cfg.Barrier); return err == nil }, 15*time.Second, 10*time.Millisecond)
		var execution model.CatalogExecution
		require.NoError(t, db.Where("quote_id = ?", q.QuoteID).Take(&execution).Error)
		require.Equal(t, "reserved", execution.BillingState)
		require.NotEmpty(t, execution.NativeBody)
		require.NotEmpty(t, execution.Outcome)
		require.NoError(t, p.cmd.Process.Kill())
		require.Error(t, (<-finished).Err)
		observer := startCatalogProcess(t, cfg, "")
		start := make(chan struct{})
		results := make(chan catalogHTTPResult, 10)
		var wg sync.WaitGroup
		for i := range 10 {
			wg.Go(func() {
				<-start
				process := []*catalogProcess{second, observer}[i%2]
				results <- catalogProcessRequest(process.URL, "GET", "/v1/invocations/durable", keyA, "", "", nil, false)
			})
		}
		close(start)
		wg.Wait()
		close(results)
		successes := 0
		for result := range results {
			require.NoError(t, result.Err)
			require.Contains(t, []int{200, 409}, result.Status, string(result.Body))
			if result.Status == 409 {
				require.Contains(t, string(result.Body), "INVOCATION_CONFLICT")
			} else {
				successes++
			}
		}
		require.Positive(t, successes, "at least one authoritative recovery must finish")
		inv := catalogProcessObserve(t, second, keyA, "durable")
		require.Equal(t, "succeeded", inv.ExecutionState)
		require.Equal(t, "settled", inv.BillingState)
		require.Len(t, inv.Receipts, 1)
		inv = catalogProcessObserve(t, second, keyA, "durable")
		require.Len(t, inv.Receipts, 1)
		require.EqualValues(t, 3, calls.Load())
		wallet(t, 9984999)
	})
	if t.Failed() {
		return
	}
	t.Run("revoke_before_acceptance_and_other_principal", func(t *testing.T) {
		p := startCatalogProcess(t, cfg, "before_acceptance")
		q := catalogProcessQuote(t, second, keyA, "revoked", raw)
		finished := make(chan catalogHTTPResult, 1)
		go func() {
			finished <- catalogProcessRequest(p.URL, "POST", "/v1/chat/completions", keyA, q.QuoteID, "revoked", raw, true)
		}()
		require.Eventually(t, func() bool { _, err := os.Stat(p.cfg.Barrier); return err == nil }, 15*time.Second, 10*time.Millisecond)
		r := catalogProcessRequest(second.URL, "POST", "/__fixture/policy/deny", "", "", "", nil, false)
		require.Equal(t, 204, r.Status, string(r.Body))
		require.NoError(t, os.WriteFile(p.cfg.Release, []byte("release"), 0600))
		r = <-finished
		require.NoError(t, r.Err)
		require.Contains(t, []int{409, 422}, r.Status, string(r.Body))
		require.EqualValues(t, 3, calls.Load())
		wallet(t, 9984999)
		require.NoError(t, db.Model(&model.CatalogInvocationRecord{}).Where("client_request_id = ?", "revoked").Count(&count).Error)
		require.Zero(t, count)
		qB := catalogProcessQuote(t, second, keyB, "other-principal", raw)
		r = catalogProcessRequest(second.URL, "POST", "/v1/chat/completions", keyB, qB.QuoteID, "other-principal", raw, false)
		require.Equal(t, 200, r.Status, string(r.Body))
		require.EqualValues(t, 4, calls.Load())
		wallet(t, 9979999)
		r = catalogProcessRequest(second.URL, "POST", "/__fixture/policy/allow", "", "", "", nil, false)
		require.Equal(t, 204, r.Status)
	})
	if t.Failed() {
		return
	}
	t.Run("acceptance_before_revoke", func(t *testing.T) {
		p := startCatalogProcess(t, cfg, "acceptance_committed")
		q := catalogProcessQuote(t, second, keyA, "accepted-first", raw)
		finished := make(chan catalogHTTPResult, 1)
		go func() {
			finished <- catalogProcessRequest(p.URL, "POST", "/v1/chat/completions", keyA, q.QuoteID, "accepted-first", raw, true)
		}()
		require.Eventually(t, func() bool { _, err := os.Stat(p.cfg.Barrier); return err == nil }, 15*time.Second, 10*time.Millisecond)
		// The claim and unknown send marker are committed before the policy edit.
		// Await the edit's commit before allowing any upstream network request.
		r := catalogProcessRequest(second.URL, "POST", "/__fixture/policy/deny", "", "", "", nil, false)
		require.NoError(t, r.Err)
		require.Equal(t, 204, r.Status, string(r.Body))
		require.EqualValues(t, 4, calls.Load())
		require.NoError(t, os.WriteFile(p.cfg.Release, []byte("release"), 0600))
		r = <-finished
		require.NoError(t, r.Err)
		require.Equal(t, 200, r.Status, string(r.Body))
		inv := catalogProcessObserve(t, second, keyA, "accepted-first")
		require.Equal(t, "succeeded", inv.ExecutionState)
		require.Equal(t, "settled", inv.BillingState)
		require.Len(t, inv.Receipts, 1)
		require.EqualValues(t, 5, calls.Load())
		wallet(t, 9974999)
		// Full credential revocation still prevents observation of even known IDs.
		require.NoError(t, db.Model(&model.CatalogIntegrationBinding{}).Where("principal_id = ?", "process-A").Update("enabled", false).Error)
		denied := catalogProcessRequest(second.URL, "GET", "/v1/invocations/accepted-first", keyA, "", "", nil, false)
		require.Equal(t, 403, denied.Status)
		require.EqualValues(t, 5, calls.Load())
		wallet(t, 9974999)
	})
	if t.Failed() {
		return
	}
	t.Run("directory_takeover_before_acceptance", func(t *testing.T) {
		p := startCatalogProcess(t, cfg, "before_acceptance")
		q := catalogProcessQuote(t, second, keyB, "retired-directory", raw)
		finished := make(chan catalogHTTPResult, 1)
		go func() {
			finished <- catalogProcessRequest(p.URL, "POST", "/v1/chat/completions", keyB, q.QuoteID, "retired-directory", raw, true)
		}()
		require.Eventually(t, func() bool { _, err := os.Stat(p.cfg.Barrier); return err == nil }, 15*time.Second, 10*time.Millisecond)
		changed := catalogProcessRequest(second.URL, "POST", "/__fixture/retire-directory", "", "", "", nil, false)
		require.NoError(t, changed.Err)
		require.Equal(t, 204, changed.Status, string(changed.Body))
		require.NoError(t, os.WriteFile(p.cfg.Release, []byte("release"), 0600))
		denied := <-finished
		require.NoError(t, denied.Err)
		require.Equal(t, 400, denied.Status, string(denied.Body))
		require.Contains(t, string(denied.Body), "CATALOG_CONFLICT")
		require.EqualValues(t, 5, calls.Load())
		wallet(t, 9974999)
		require.NoError(t, db.Model(&model.CatalogInvocationRecord{}).Where("client_request_id = ?", "retired-directory").Count(&count).Error)
		require.Zero(t, count)
		empty := catalogProcessRequest(second.URL, "GET", "/v1/catalog", keyB, "", "", nil, false)
		require.Equal(t, 200, empty.Status, string(empty.Body))
		var snapshot c.CatalogSnapshot
		require.NoError(t, json.Unmarshal(empty.Body, &snapshot))
		require.Empty(t, snapshot.Models)
		require.Equal(t, "supply_empty", snapshot.EmptyReason)
	})
	t.Logf("multi-process completed dialect=%s supplierCalls=%d", common.MainDatabaseType(), calls.Load())
}
