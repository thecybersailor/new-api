package catalogintegration

import (
	"bytes"
	"context"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	c "github.com/QuantumNous/new-api/pkg/catalogcontract"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type testAdapter struct {
	operation     c.Operation
	calls         atomic.Int32
	loseResponse  bool
	definitionErr error
	now           time.Time
}

func (a *testAdapter) Definition(context.Context, Scope, string) (c.OperationDefinition, error) {
	if a.definitionErr != nil {
		return c.OperationDefinition{}, a.definitionErr
	}
	return c.OperationDefinition{Operation: a.operation, OperationRevision: "op-v1", PricingRevision: "price-v1", InputSchema: common.RawMessage(`{"type":"object"}`), OutputSchema: common.RawMessage(`{"type":"object"}`), Limits: common.RawMessage(`{"count":1}`), UsageSchema: common.RawMessage(`{"type":"object"}`), InputModalities: []string{"text"}, OutputModalities: []string{"text"}, ExecutionMode: []string{"sync", "async"}, Protocol: "fixture.native", SubmitPath: "/v1/fixture"}, nil
}
func (a *testAdapter) Prepare(context.Context, Scope, c.QuoteRequest) (Bound, error) {
	return Bound{c.Money{"USD", "5"}, c.Money{"USD", "10"}, []byte(`{"effective":"fixture-only","groupRatio":1,"pluginVersion":"v1"}`), "fixture-v1"}, nil
}
func (a *testAdapter) Submit(_ context.Context, row model.CatalogStoredQuote, q c.Quote, _ common.RawMessage) (Outcome, error) {
	a.calls.Add(1)
	if a.loseResponse {
		return Outcome{}, errors.New("response lost")
	}
	return Outcome{State: "succeeded", Result: common.RawMessage(`{"result":"fixture"}`), Receipt: &c.Receipt{SourceID: "fixture-dantoken", IntegrationPrincipalID: row.PrincipalID, ReceiptID: q.QuoteID, Revision: 1, AmountMode: "cumulative", IsFinal: true, QuoteID: q.QuoteID, PricingRevision: q.PricingRevision, RawUsage: common.RawMessage(`{"count":1}`), Cost: c.Money{"USD", "5"}, SettledAt: a.now, RawQuota: "5", QuotaPerUnit: "1000000", RoundingVersion: "fixture-v1"}}, nil
}
func (a *testAdapter) Observe(context.Context, model.CatalogStoredQuote, c.Invocation) (Outcome, error) {
	return Outcome{State: "unknown"}, nil
}

func integrationDB(t *testing.T, dialect string) *gorm.DB {
	t.Helper()
	var d gorm.Dialector
	switch dialect {
	case "sqlite":
		d = sqlite.Open(filepath.Join(t.TempDir(), "catalog.db") + "?_pragma=busy_timeout(5000)")
	case "mysql":
		dsn := os.Getenv("TEST_MYSQL_DSN")
		if dsn == "" {
			t.Skip("TEST_MYSQL_DSN not configured")
		}
		d = mysql.Open(dsn)
	case "postgres":
		dsn := os.Getenv("TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("TEST_POSTGRES_DSN not configured")
		}
		d = postgres.Open(dsn)
	}
	oldType := common.MainDatabaseType()
	common.SetMainDatabaseType(common.DatabaseType(dialect))
	t.Cleanup(func() { common.SetMainDatabaseType(oldType) })
	db, err := gorm.Open(d, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	sql, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sql.Close() })
	// Dedicated disposable database only; all test tables have integration names.
	for _, m := range model.CatalogIntegrationModels() {
		require.NoError(t, db.Migrator().DropTable(m))
	}
	require.NoError(t, db.AutoMigrate(model.CatalogIntegrationModels()...))
	return db
}
func fixture(t *testing.T, db *gorm.DB, op c.Operation) (*Engine, Principal, *testAdapter, c.QuoteRequest, c.Quote) {
	t.Helper()
	now := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	name := "fixture/" + string(op)
	a := &testAdapter{operation: op, now: now}
	e := &Engine{DB: db, SourceID: "fixture-dantoken", Now: func() time.Time { return now }, Adapters: []AdapterRegistration{{Key: "fixture-v1", Model: name, Operation: op, Adapter: a}}}
	p := Principal{ID: "principal", ResolveScope: func(context.Context) (Scope, error) {
		return Scope{PrincipalID: "principal", Group: "fixed", ScopeID: "scope", Models: []string{name, "unverified-model"}}, nil
	}, CanSubmit: func(context.Context) error { return nil }}
	snap, err := e.Catalog(context.Background(), p, "")
	require.NoError(t, err)
	// v2 executable catalog omits unconfigured candidates; maintenance retains them.
	require.Len(t, snap.Models, 1)
	assert.Equal(t, name, snap.Models[0].ID)
	raw, err := common.Marshal(map[string]any{"model": name, "prompt": "hello"})
	require.NoError(t, err)
	r := c.QuoteRequest{ClientRequestID: "request-" + string(op), SnapshotID: snap.SnapshotID, Model: name, Operation: op, OperationRevision: "op-v1", Request: raw}
	q, err := e.Quote(context.Background(), p, r)
	require.NoError(t, err)
	return e, p, a, r, q
}
func TestCatalogLifecycleDatabaseMatrix(t *testing.T) {
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db := integrationDB(t, dialect)
			var version string
			versionQuery := "SELECT version()"
			if dialect == "sqlite" {
				versionQuery = "SELECT sqlite_version()"
			}
			require.NoError(t, db.Raw(versionQuery).Scan(&version).Error)
			t.Logf("database version: %s", version)

			for _, m := range model.CatalogIntegrationModels() {
				require.NoError(t, db.Migrator().DropTable(m))
			}
			// Representative pre-integration tables use the unchanged released
			// User/Token definitions. Additive integration migration must preserve
			// existing balances, credentials and authorization limits.
			require.NoError(t, db.Migrator().DropTable(&model.Token{}, &model.User{}))
			require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}))
			legacyUser := model.User{Username: "legacy-fixture", Status: common.UserStatusEnabled, Quota: 1234}
			require.NoError(t, db.Create(&legacyUser).Error)
			legacyToken := model.Token{UserId: legacyUser.Id, Key: "legacy-fixture-key", Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 567, ModelLimitsEnabled: true, ModelLimits: "preserved-model"}
			require.NoError(t, db.Create(&legacyToken).Error)
			// Fresh migration and restart, then populated-schema repeat: no row loss or
			// unique-identity weakening on an already used database.
			require.NoError(t, db.AutoMigrate(model.CatalogIntegrationModels()...))
			e, p, a, r, q := fixture(t, db, c.TextGeneration)
			ctx := context.Background()
			out, err := e.Submit(ctx, p, "/v1/fixture", q.QuoteID, r.ClientRequestID, r.Request)
			require.NoError(t, err)
			assert.Equal(t, "settled", out.BillingState)
			assert.Equal(t, "succeeded", out.ExecutionState)
			require.NoError(t, db.AutoMigrate(model.CatalogIntegrationModels()...))
			require.NoError(t, db.AutoMigrate(model.CatalogIntegrationModels()...))
			restarted := *e
			restarted.Now = func() time.Time { return a.now.Add(time.Hour) }
			p.CanSubmit = func(context.Context) error { return errors.New("balance exhausted") }
			p.ResolveScope = func(context.Context) (Scope, error) { return Scope{}, errors.New("model retired") }
			again, err := restarted.Submit(ctx, p, "/v1/fixture", q.QuoteID, r.ClientRequestID, r.Request)
			require.NoError(t, err)
			assert.Equal(t, out.InvocationID, again.InvocationID)
			assert.EqualValues(t, 1, a.calls.Load())
			_, err = restarted.Submit(ctx, p, "/v1/fixture", q.QuoteID, r.ClientRequestID, common.RawMessage(`{"model":"changed"}`))
			require.ErrorContains(t, err, "IDEMPOTENCY_CONFLICT")
			_, err = restarted.Invocation(ctx, Principal{ID: "other"}, r.ClientRequestID, false)
			require.ErrorIs(t, err, gorm.ErrRecordNotFound)

			var preserved model.Token
			require.NoError(t, db.First(&preserved, legacyToken.Id).Error)
			assert.Equal(t, legacyToken.Key, preserved.Key)
			assert.Equal(t, 567, preserved.RemainQuota)
			assert.Equal(t, "preserved-model", preserved.ModelLimits)
			var preservedUser model.User
			require.NoError(t, db.First(&preservedUser, legacyUser.Id).Error)
			assert.Equal(t, 1234, preservedUser.Quota)
			var rows int64
			require.NoError(t, db.Model(&model.CatalogInvocationRecord{}).Count(&rows).Error)
			assert.EqualValues(t, 1, rows)
			require.NoError(t, db.Model(&model.CatalogReceiptRecord{}).Count(&rows).Error)
			assert.EqualValues(t, 1, rows)
		})
	}
}
func TestCatalogConcurrentSubmissionAndUnknown(t *testing.T) {
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db := integrationDB(t, dialect)
			e, p, a, r, q := fixture(t, db, c.VideoGeneration)
			a.loseResponse = true
			// A barrier starts two independent callers for the same logical invocation.
			var wg sync.WaitGroup
			start := make(chan struct{})
			errs := make(chan error, 2)
			for range 2 {
				wg.Go(func() {
					<-start
					_, err := e.Submit(context.Background(), p, "/v1/fixture", q.QuoteID, r.ClientRequestID, r.Request)
					errs <- err
				})
			}
			close(start)
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					require.ErrorContains(t, err, "SUBMISSION_UNKNOWN")
				}
			}
			assert.EqualValues(t, 1, a.calls.Load())
			out, err := e.Invocation(context.Background(), p, r.ClientRequestID, false)
			require.NoError(t, err)
			assert.Equal(t, "unknown", out.ExecutionState)
			_, err = e.Submit(context.Background(), p, "/v1/fixture", q.QuoteID, r.ClientRequestID, r.Request)
			require.NoError(t, err)
			assert.EqualValues(t, 1, a.calls.Load())
		})
	}
}

func TestCatalogDuplicateClaimInsideAcceptanceTransaction(t *testing.T) {
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db := integrationDB(t, dialect)
			first, owned, err := model.ClaimCatalogInvocation(db, "p", "r", "hash", "q", []byte(`{}`))
			require.NoError(t, err)
			require.True(t, owned)
			require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
				winner, secondOwned, claimErr := model.ClaimCatalogInvocation(tx, "p", "r", "hash", "q", []byte(`{}`))
				require.NoError(t, claimErr)
				assert.False(t, secondOwned)
				assert.Equal(t, first.ID, winner.ID)
				return tx.Model(&model.CatalogInvocationRecord{}).Where("id = ?", first.ID).Update("version", 2).Error
			}))
		})
	}
}
func TestCatalogAllOperationsAndWireFixtures(t *testing.T) {
	for _, op := range []c.Operation{c.TextGeneration, c.ImageGeneration, c.ImageEdit, c.VideoGeneration, c.AudioTranscription, c.SpeechSynthesis, c.Embedding, c.MultimodalGeneration} {
		t.Run(string(op), func(t *testing.T) {
			db := integrationDB(t, "sqlite")
			e, p, _, r, q := fixture(t, db, op)
			out, err := e.Submit(context.Background(), p, "/v1/fixture", q.QuoteID, r.ClientRequestID, r.Request)
			require.NoError(t, err)
			assert.Equal(t, "succeeded", out.ExecutionState)
		})
	}
	files, err := filepath.Glob("../../pkg/catalogcontract/testdata/*.json")
	require.NoError(t, err)
	require.Len(t, files, 5)
	for _, f := range files {
		raw, err := os.ReadFile(f)
		require.NoError(t, err)
		switch filepath.Base(f) {
		case "catalog-v1.json":
			var v c.CatalogSnapshot
			require.NoError(t, common.Unmarshal(raw, &v))
			require.ErrorContains(t, v.Validate(time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)), "CATALOG_SCHEMA_UNSUPPORTED")
			require.Len(t, v.Models, 8)
		case "catalog-v2.json":
			var v c.CatalogSnapshot
			require.NoError(t, common.Unmarshal(raw, &v))
			require.NoError(t, v.Validate(v.GeneratedAt))
		case "quote-v1.json":
			var v c.Quote
			require.NoError(t, common.Unmarshal(raw, &v))
			require.NoError(t, v.Validate(v.ScopeID, v.ClientRequestID, v.RequestHash, v.OperationRevision, time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)))
		case "invocation-v1.json":
			var v c.Invocation
			require.NoError(t, common.Unmarshal(raw, &v))
			require.NotNil(t, v.Receipt)
			assert.Equal(t, "succeeded", v.ExecutionState)
			require.NotEmpty(t, v.Receipts)
		}
	}
}
func TestCatalogHashAndExpiredFirstSubmission(t *testing.T) {
	a, err := RequestHash(common.RawMessage(`{"n":9007199254740993,"model":"x"}`))
	require.NoError(t, err)
	b, err := RequestHash(common.RawMessage(`{ "model":"x", "n":9007199254740993 }`))
	require.NoError(t, err)
	assert.Equal(t, a, b)
	b, err = RequestHash(common.RawMessage(`{"n":9007199254740992,"model":"x"}`))
	require.NoError(t, err)
	assert.NotEqual(t, a, b)
	db := integrationDB(t, "sqlite")
	e, p, adapter, r, q := fixture(t, db, c.Embedding)
	e.Now = func() time.Time { return adapter.now.Add(time.Hour) }
	_, err = e.Submit(context.Background(), p, "/v1/fixture", q.QuoteID, r.ClientRequestID, r.Request)
	require.ErrorContains(t, err, "QUOTE_EXPIRED")
	assert.Zero(t, adapter.calls.Load())
}
func TestCatalogReceiptsAndHTTPRecovery(t *testing.T) {
	db := integrationDB(t, "sqlite")
	e, p, a, r, q := fixture(t, db, c.ImageGeneration)
	out, err := e.Submit(context.Background(), p, "/v1/fixture", q.QuoteID, r.ClientRequestID, r.Request)
	require.NoError(t, err)
	row, err := model.FindCatalogInvocation(db, p.ID, r.ClientRequestID)
	require.NoError(t, err)
	stored, _, err := e.readQuote(p.ID, q.QuoteID)
	require.NoError(t, err)
	correction := *out.Receipt
	correction.Revision = 3
	correction.PredecessorRevision = 2
	_, err = e.recordOutcome(&row, stored, q, out, Outcome{State: "succeeded", Receipt: &correction})
	require.ErrorContains(t, err, "RECEIPT_MISMATCH")
	correction.Revision = 2
	correction.PredecessorRevision = 1
	correction.Cost.AmountMicros = "3"
	out, err = e.recordOutcome(&row, stored, q, out, Outcome{State: "succeeded", Receipt: &correction})
	require.NoError(t, err)
	assert.Equal(t, "settled", out.BillingState)
	correction.Revision = 3
	correction.PredecessorRevision = 2
	correction.Cost.AmountMicros = "4"
	out, err = e.recordOutcome(&row, stored, q, out, Outcome{State: "succeeded", Receipt: &correction})
	require.NoError(t, err)
	assert.Equal(t, "reconciliation_required", out.BillingState)
	var count int64
	require.NoError(t, db.Model(&model.CatalogReceiptRecord{}).Count(&count).Error)
	assert.EqualValues(t, 3, count)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/v1/invocations/:id", func(ctx *gin.Context) { e.Handle(ctx, p) })
	// Disable observation adapter to exercise a pure persisted read after restart.
	e.Adapters = nil
	req := httptest.NewRequest(http.MethodGet, "/v1/invocations/"+r.ClientRequestID, nil)
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	require.Equal(t, 200, resp.Code)
	assert.Contains(t, resp.Body.String(), "reconciliation_required")
	var restored c.Invocation
	require.NoError(t, common.Unmarshal(resp.Body.Bytes(), &restored))
	require.Len(t, restored.Receipts, 3)
	assert.EqualValues(t, 1, restored.Receipts[0].Revision)
	assert.EqualValues(t, 3, restored.Receipts[2].Revision)
	assert.EqualValues(t, 1, a.calls.Load())
	assert.Equal(t, "no-store", resp.Header().Get("Cache-Control"))
}

func TestCatalogSharedRequestHashVectors(t *testing.T) {
	raw, err := os.ReadFile("../../pkg/catalogcontract/testdata/request-hash-v1.json")
	require.NoError(t, err)
	var fixture struct {
		Cases []struct{ Request, Normalized, Hash string }
	}
	require.NoError(t, common.Unmarshal(raw, &fixture))
	require.Len(t, fixture.Cases, 2)
	for _, tc := range fixture.Cases {
		hash, err := RequestHash(common.RawMessage(tc.Request))
		require.NoError(t, err)
		assert.Equal(t, tc.Hash, hash)
		normalized, err := canonicalJSON(common.RawMessage(tc.Request))
		require.NoError(t, err)
		assert.Equal(t, tc.Normalized, string(normalized))
	}
	for _, bad := range []string{`null`, `[]`, `{} {}`, `{"x":NaN}`} {
		_, err := RequestHash(common.RawMessage(bad))
		require.Error(t, err)
	}
}

func TestCatalogQuoteScopeAndReceiptSafety(t *testing.T) {
	db := integrationDB(t, "sqlite")
	e, p, a, r, q := fixture(t, db, c.AudioTranscription)
	ctx := context.Background()
	unauthorized := p
	unauthorized.ID = "other"
	_, err := e.Submit(ctx, unauthorized, "/v1/fixture", q.QuoteID, r.ClientRequestID, r.Request)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
	assert.Zero(t, a.calls.Load())
	changedScope := p
	changedScope.ResolveScope = func(context.Context) (Scope, error) {
		return Scope{PrincipalID: p.ID, ScopeID: "new-scope", Group: "other", Models: []string{r.Model}}, nil
	}
	_, err = e.Catalog(ctx, changedScope, r.SnapshotID)
	require.ErrorContains(t, err, "SCOPE_CHANGED")
	_, err = e.Submit(ctx, changedScope, "/v1/fixture", q.QuoteID, r.ClientRequestID, r.Request)
	require.ErrorContains(t, err, "QUOTE_REQUEST_MISMATCH")
	assert.Zero(t, a.calls.Load())
	_, err = e.Submit(ctx, p, "/v1/different", q.QuoteID, r.ClientRequestID, r.Request)
	require.ErrorContains(t, err, "OPERATION_NOT_ADMITTED")
	out, err := e.Submit(ctx, p, "/v1/fixture", q.QuoteID, r.ClientRequestID, r.Request)
	require.NoError(t, err)
	_, err = e.Quote(ctx, p, r)
	require.ErrorContains(t, err, "SUBMISSION_EXISTS")
	row, err := model.FindCatalogInvocation(db, p.ID, r.ClientRequestID)
	require.NoError(t, err)
	stored, _, err := e.readQuote(p.ID, q.QuoteID)
	require.NoError(t, err)
	receipt := *out.Receipt
	receipt.Revision = 2
	receipt.PredecessorRevision = 1
	receipt.RawQuota = "-1"
	_, err = e.recordOutcome(&row, stored, q, out, Outcome{State: "succeeded", Receipt: &receipt})
	require.ErrorContains(t, err, "RECEIPT_MISMATCH")
	receipt.RawQuota = "11"
	receipt.Cost.AmountMicros = "11"
	out, err = e.recordOutcome(&row, stored, q, out, Outcome{State: "succeeded", Receipt: &receipt})
	require.NoError(t, err)
	assert.Equal(t, "reconciliation_required", out.BillingState)
	assert.EqualValues(t, 1, a.calls.Load())
}

func TestCatalogAbandonedUnsentClaimCanBeReleased(t *testing.T) {
	db := integrationDB(t, "sqlite")
	e, p, a, r, q := fixture(t, db, c.TextGeneration)
	inv := c.Invocation{InvocationID: "unsent", ClientRequestID: r.ClientRequestID, RequestHash: q.RequestHash, ExecutionState: "accepted", BillingState: "pending"}
	data, err := common.Marshal(inv)
	require.NoError(t, err)
	_, owned, err := model.ClaimCatalogInvocation(db, p.ID, r.ClientRequestID, q.RequestHash, q.QuoteID, data)
	require.NoError(t, err)
	require.True(t, owned)
	e.Now = func() time.Time { return a.now.Add(time.Hour) }
	restored, err := e.Invocation(context.Background(), p, r.ClientRequestID, true)
	require.NoError(t, err)
	assert.Equal(t, "failed", restored.ExecutionState)
	assert.Equal(t, "not_charged", restored.BillingState)
	assert.Zero(t, a.calls.Load())
	repeated, err := e.Invocation(context.Background(), p, r.ClientRequestID, true)
	require.NoError(t, err)
	assert.Equal(t, "not_charged", repeated.BillingState)
}

func TestCatalogTransientFailureCannotPublishPartialSnapshot(t *testing.T) {
	db := integrationDB(t, "sqlite")
	e, p, a, _, _ := fixture(t, db, c.TextGeneration)
	var before int64
	require.NoError(t, db.Model(&model.CatalogStoredSnapshot{}).Count(&before).Error)
	a.definitionErr = errors.New("configuration storage unavailable")
	_, err := e.Catalog(context.Background(), p, "")
	require.ErrorContains(t, err, "storage unavailable")
	var after int64
	require.NoError(t, db.Model(&model.CatalogStoredSnapshot{}).Count(&after).Error)
	assert.Equal(t, before, after)
	a.definitionErr = c.Invalid("OPERATION_NOT_ADMITTED", "adapter withdrawn", "await admission")
	snapshot, err := e.Catalog(context.Background(), p, "")
	require.NoError(t, err)
	require.True(t, snapshot.Complete)
	assert.Empty(t, snapshot.Models)
	assert.Equal(t, "supply_empty", snapshot.EmptyReason)
	require.Len(t, snapshot.Removals, 1)
	assert.Equal(t, "supply_retired", snapshot.Removals[0].Reasons[0])
}

func TestCatalogWalletSettlementDatabaseMatrix(t *testing.T) {
	oldRedis := common.RedisEnabled
	common.RedisEnabled = false
	t.Cleanup(func() { common.RedisEnabled = oldRedis })
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db := integrationDB(t, dialect)
			require.NoError(t, db.Migrator().DropTable(&model.Token{}, &model.User{}))
			require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}))
			user := model.User{Username: "wallet-fixture", Status: common.UserStatusEnabled, Quota: 10000}
			require.NoError(t, db.Create(&user).Error)
			token := model.Token{UserId: user.Id, Key: "wallet-fixture-token", Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 10000}
			require.NoError(t, db.Create(&token).Error)
			var winners atomic.Int32
			var wg sync.WaitGroup
			for range 8 {
				wg.Go(func() {
					row := model.CatalogExecution{QuoteID: "one-reservation", UserID: user.Id, TokenID: token.Id, Reserved: 1000, BillingState: "reserved"}
					if err := model.ReserveCatalogExecution(db, &row); err == nil {
						winners.Add(1)
					} else {
						t.Logf("reserve attempt: %v", err)
					}
				})
			}
			wg.Wait()
			require.EqualValues(t, 1, winners.Load())
			require.NoError(t, db.First(&user, user.Id).Error)
			assert.Equal(t, 9000, user.Quota)
			// Native result committed before settlement survives a process boundary.
			native := make([]byte, 100000) // Exceeds MySQL BLOB's 64KiB; migration must retain bounded media.
			require.NoError(t, db.Model(&model.CatalogExecution{}).Where("quote_id = ?", "one-reservation").Updates(map[string]any{"native_body": native, "outcome": []byte(`{"State":"succeeded"}`)}).Error)
			require.NoError(t, db.AutoMigrate(model.CatalogIntegrationModels()...))
			for range 8 {
				wg.Go(func() { _ = model.SettleCatalogExecution(db, "one-reservation", 300) })
			}
			wg.Wait()
			require.NoError(t, model.SettleCatalogExecution(db, "one-reservation", 300))
			require.Error(t, model.SettleCatalogExecution(db, "one-reservation", 301))
			require.NoError(t, db.First(&user, user.Id).Error)
			assert.Equal(t, 9700, user.Quota)
			require.NoError(t, db.First(&token, token.Id).Error)
			assert.Equal(t, 9700, token.RemainQuota)
			assert.Equal(t, 300, token.UsedQuota)
			var recovered model.CatalogExecution
			require.NoError(t, db.Where("quote_id = ?", "one-reservation").Take(&recovered).Error)
			assert.Equal(t, native, recovered.NativeBody)
			assert.False(t, recovered.SettledAt.IsZero())
			over := model.CatalogExecution{QuoteID: "overrun", UserID: user.Id, TokenID: token.Id, Reserved: 100, BillingState: "reserved"}
			require.NoError(t, model.ReserveCatalogExecution(db, &over))
			require.Error(t, model.SettleCatalogExecution(db, "overrun", 101))
			require.NoError(t, db.First(&user, user.Id).Error)
			assert.Equal(t, 9600, user.Quota)
			require.NoError(t, db.Where("quote_id = ?", "overrun").Take(&over).Error)
			assert.Equal(t, "reconciliation_required", over.BillingState)
			assert.Equal(t, 101, over.Actual)
			insufficient := model.CatalogExecution{QuoteID: "insufficient", UserID: user.Id, TokenID: token.Id, Reserved: 10001, BillingState: "reserved"}
			require.Error(t, model.ReserveCatalogExecution(db, &insufficient))
			var count int64
			require.NoError(t, db.Model(&model.CatalogExecution{}).Where("quote_id = ?", "insufficient").Count(&count).Error)
			assert.Zero(t, count)
		})
	}
}

func TestCatalogMultipartRejectsAmbiguousScalars(t *testing.T) {
	for _, tc := range []struct{ name, first, second string }{
		{"duplicate", `"fixture/model"`, `"other/model"`},
		{"not-json", `fixture/model`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			writer := multipart.NewWriter(&b)
			require.NoError(t, writer.WriteField("model", tc.first))
			if tc.second != "" {
				require.NoError(t, writer.WriteField("model", tc.second))
			}
			require.NoError(t, writer.Close())
			request := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &b)
			request.Header.Set("Content-Type", writer.FormDataContentType())
			_, _, err := DecodeNativeInput(request)
			require.Error(t, err)
		})
	}
}

func TestCatalogV2ChangesAuthorizationAndAuthoritativeEmpty(t *testing.T) {
	db := integrationDB(t, "sqlite")
	e, p, a, _, _ := fixture(t, db, c.TextGeneration)
	names := []string{"fixture/text_generation"}
	rule := "rule-all"
	allowed := true
	p.ResolveScope = func(context.Context) (Scope, error) {
		return Scope{PrincipalID: p.ID, ScopeID: "stable", Group: "fixed", Models: names, SupplyModels: []string{"fixture/text_generation", "second"}, AuthorizationHash: rule, AllowsModel: func(string) bool { return allowed }}, nil
	}
	first, err := e.Catalog(context.Background(), p, "")
	require.NoError(t, err)
	e.Adapters = append(e.Adapters, AdapterRegistration{Key: "second", Model: "second", Operation: c.TextGeneration, Adapter: a})
	names = append(names, "second")
	added, err := e.Catalog(context.Background(), p, "")
	require.NoError(t, err)
	assert.Equal(t, first.ScopeID, added.ScopeID)
	assert.Equal(t, first.AuthorizationRevision, added.AuthorizationRevision)
	assert.Greater(t, added.CatalogRevision, first.CatalogRevision)
	rule = "rule-none"
	names = []string{}
	allowed = false
	revoked, err := e.Catalog(context.Background(), p, "")
	require.NoError(t, err)
	assert.Equal(t, "authorization_empty", revoked.EmptyReason)
	assert.Empty(t, revoked.Models)
	assert.Equal(t, added.CatalogRevision, revoked.CatalogRevision)
	assert.Greater(t, revoked.AuthorizationRevision, added.AuthorizationRevision)
	require.Len(t, revoked.Removals, 2)
	assert.Equal(t, []string{"authorization_removed"}, revoked.Removals[0].Reasons)
	_, err = e.Catalog(context.Background(), p, added.SnapshotID)
	require.ErrorContains(t, err, "CATALOG_AUTHORIZATION_CHANGED")
	same, err := e.Catalog(context.Background(), p, "")
	require.NoError(t, err)
	assert.Equal(t, revoked.AuthorizationRevision, same.AuthorizationRevision)
	assert.Equal(t, revoked.CatalogRevision, same.CatalogRevision)
	broken := same
	broken.TotalCount = 1
	require.Error(t, broken.Validate(e.now()))
	broken = same
	broken.ContentDigest = "tampered"
	require.Error(t, broken.Validate(e.now()))
	broken = same
	broken.EmptyReason = ""
	require.Error(t, broken.Validate(e.now()))
}

func TestCatalogAcceptanceRejectsPolicyRaceBeforeClaim(t *testing.T) {
	db := integrationDB(t, "sqlite")
	e, p, a, r, q := fixture(t, db, c.TextGeneration)
	p.AcceptanceScope = func(context.Context, *gorm.DB) (Scope, error) {
		return Scope{}, c.Invalid("ACCESS_DENIED", "revoked after quote", "refresh")
	}
	_, err := e.Submit(context.Background(), p, "/v1/fixture", q.QuoteID, r.ClientRequestID, r.Request)
	require.ErrorContains(t, err, "ACCESS_DENIED")
	assert.Zero(t, a.calls.Load())
	var count int64
	require.NoError(t, db.Model(&model.CatalogInvocationRecord{}).Count(&count).Error)
	assert.Zero(t, count)
}

func TestCatalogMaintenanceDatabaseMatrix(t *testing.T) {
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db := integrationDB(t, dialect)
			// Upgrade the actual pre-maintenance binding shape without losing ownership.
			require.NoError(t, db.Migrator().DropTable(&model.CatalogIntegrationBinding{}))
			require.NoError(t, db.Table("catalog_integration_bindings").AutoMigrate(&legacyCatalogBinding{}))
			require.NoError(t, db.Table("catalog_integration_bindings").Create(&legacyCatalogBinding{ID: 9, TokenID: 12, UserID: 8, PrincipalID: "legacy-principal", FixedGroup: "fixed", Enabled: true}).Error)
			for range 2 {
				require.NoError(t, db.AutoMigrate(model.CatalogIntegrationModels()...))
			}
			var migrated model.CatalogIntegrationBinding
			require.NoError(t, db.First(&migrated, 9).Error)
			assert.Equal(t, "legacy-principal", migrated.PrincipalID)
			assert.Equal(t, 12, migrated.TokenID)
			assert.Equal(t, 8, migrated.UserID)
			assert.True(t, migrated.Enabled)
			assert.Empty(t, migrated.ModelPolicy)
			assert.Empty(t, migrated.AllowedOperations)
			// Upgrade an existing directory with a real immutable audit, twice.
			empty, err := model.ReadCatalogDirectory(db)
			require.NoError(t, err)
			require.NoError(t, model.SaveCatalogDirectory(db, empty, []byte(`[{"model":"A"}]`), []byte(`[]`), "save_draft", "fixture", 1, false))
			require.ErrorContains(t, model.SaveCatalogDirectory(db, empty, []byte(`[]`), []byte(`[]`), "save_draft", "stale", 1, false), "CATALOG_CONFLICT")
			before, err := model.ReadCatalogDirectory(db)
			require.NoError(t, err)
			assert.EqualValues(t, 1, before.DraftRevision)
			assert.False(t, before.DatabaseEnabled)
			require.NoError(t, model.SaveCatalogDirectory(db, before, before.Draft, []byte(`[{"model":"A"}]`), "publish", "reviewed", 1, true))
			for range 2 {
				require.NoError(t, db.AutoMigrate(model.CatalogIntegrationModels()...))
			}
			after, err := model.ReadCatalogDirectory(db)
			require.NoError(t, err)
			assert.True(t, after.DatabaseEnabled)
			assert.EqualValues(t, 1, after.PublishedRevision)
			assert.JSONEq(t, `[{"model":"A"}]`, string(after.Published))
			var audits []model.CatalogDirectoryAudit
			require.NoError(t, db.Order("id").Find(&audits).Error)
			require.Len(t, audits, 2)
			assert.EqualValues(t, 2, audits[1].DraftRevision)
			require.NoError(t, model.SaveCatalogDirectory(db, after, after.Draft, []byte(`[]`), "retire", "last operation", 1, true))
			final, err := model.ReadCatalogDirectory(db)
			require.NoError(t, err)
			assert.EqualValues(t, 2, final.PublishedRevision)
			assert.JSONEq(t, `[]`, string(final.Published))
		})
	}
}

type legacyCatalogBinding struct {
	ID          int    `gorm:"primaryKey"`
	TokenID     int    `gorm:"uniqueIndex;not null"`
	UserID      int    `gorm:"index;not null"`
	PrincipalID string `gorm:"size:128;index;not null"`
	FixedGroup  string `gorm:"size:128;not null"`
	Enabled     bool
}

func TestCatalogConcurrentScopeChangeDoesNotPublishSnapshot(t *testing.T) {
	db := integrationDB(t, "sqlite")
	e, p, _, _, _ := fixture(t, db, c.TextGeneration)
	var before int64
	require.NoError(t, db.Model(&model.CatalogStoredSnapshot{}).Count(&before).Error)
	p.CatalogScope = func(ctx context.Context, _ *gorm.DB) (Scope, error) {
		scope, err := p.ResolveScope(ctx)
		scope.Models = []string{}
		return scope, err
	}
	_, err := e.Catalog(context.Background(), p, "")
	require.ErrorContains(t, err, "CATALOG_AUTHORIZATION_CHANGED")
	var after int64
	require.NoError(t, db.Model(&model.CatalogStoredSnapshot{}).Count(&after).Error)
	assert.Equal(t, before, after)
}

// A receipt INSERT failure after a successful invocation CAS must roll back
// both writes, on every supported engine. Immutable receipts remain unchanged.
func TestCatalogReceiptCommitRollbackDatabaseMatrix(t *testing.T) {
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db := integrationDB(t, dialect)
			before := c.Invocation{InvocationID: "receipt-rollback", ClientRequestID: "receipt-rollback", RequestHash: "hash", ExecutionState: "unknown", BillingState: "pending"}
			raw, err := common.Marshal(before)
			require.NoError(t, err)
			row, owned, err := model.ClaimCatalogInvocation(db, "principal", before.ClientRequestID, "hash", "quote", raw)
			require.NoError(t, err)
			require.True(t, owned)
			original := row
			receipt := c.Receipt{SourceID: "source", IntegrationPrincipalID: "principal", ReceiptID: "receipt", Revision: 1, QuoteID: "quote", Cost: c.Money{Currency: "USD", AmountMicros: "5"}}
			after := before
			after.ExecutionState = "succeeded"
			after.BillingState = "settled"
			after.Receipt = &receipt
			injected := errors.New("test receipt insert denied after CAS")
			require.NoError(t, db.Callback().Create().Before("gorm:create").Register("test:receipt_insert_failure", func(tx *gorm.DB) {
				if tx.Statement.Table == "catalog_receipt_records" {
					tx.AddError(injected)
				}
			}))
			require.ErrorIs(t, model.CommitCatalogReceipt(db, &row, receipt, after), injected)
			unchanged, err := model.FindCatalogInvocation(db, "principal", before.ClientRequestID)
			require.NoError(t, err)
			require.Equal(t, original.Version, unchanged.Version)
			require.Equal(t, original.Data, unchanged.Data)
			require.Equal(t, original.State, unchanged.State)
			var count int64
			require.NoError(t, db.Model(&model.CatalogReceiptRecord{}).Count(&count).Error)
			require.Zero(t, count)
			require.NoError(t, db.Callback().Create().Remove("test:receipt_insert_failure"))
			require.NoError(t, model.CommitCatalogReceipt(db, &row, receipt, after))
			saved, err := model.FindCatalogInvocation(db, "principal", before.ClientRequestID)
			require.NoError(t, err)
			require.Equal(t, original.Version+1, saved.Version)
			require.NoError(t, model.CommitCatalogReceipt(db, &row, receipt, after))
			receipt.Cost.AmountMicros = "6"
			require.ErrorContains(t, model.CommitCatalogReceipt(db, &row, receipt, after), "RECEIPT_MISMATCH")
			final, err := model.FindCatalogInvocation(db, "principal", before.ClientRequestID)
			require.NoError(t, err)
			require.Equal(t, saved.Data, final.Data)
			require.Equal(t, saved.Version, final.Version)
			require.NoError(t, db.Model(&model.CatalogReceiptRecord{}).Count(&count).Error)
			require.EqualValues(t, 1, count)
		})
	}
}
