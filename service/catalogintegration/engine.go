// Package catalogintegration owns the opt-in DanToken integration lifecycle.
// Adapters must provide an executable cost bound and immutable pricing evidence;
// an unregistered model is discoverable but never silently admitted.
package catalogintegration

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	c "github.com/QuantumNous/new-api/pkg/catalogcontract"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type Scope struct {
	AllowsOperation                   func(string, c.Operation) bool
	PrincipalID, Group, ScopeID       string
	Models                            []string
	SupplyModels                      []string
	AuthorizationHash, BindingVersion string
	AllowsModel                       func(string) bool
}
type Principal struct {
	ID              string
	ResolveScope    func(context.Context) (Scope, error)
	CanSubmit       func(context.Context) error
	AcceptanceScope func(context.Context, *gorm.DB) (Scope, error)
	CatalogScope    func(context.Context, *gorm.DB) (Scope, error)
}
type Bound struct {
	Estimated, Maximum c.Money
	PricingSnapshot    []byte
	RoundingVersion    string
}
type Outcome struct {
	State, ProviderTaskID string
	Result                common.RawMessage
	Artifacts             []c.Artifact
	Receipt               *c.Receipt
	NotCharged            bool // Trusted adapter proof that no provider request was sent.
}

// Adapter is trusted compiled code, never executable content from a catalog.
// Prepare validates the exact native request and enforces its bound. Frozen
// pricing must include effective group/channel/plugin/interpreter revisions.
// Submit must not retry an uncertain external request. Observe never submits.
type Adapter interface {
	Definition(context.Context, Scope, string) (c.OperationDefinition, error)
	Prepare(context.Context, Scope, c.QuoteRequest) (Bound, error)
	Submit(context.Context, model.CatalogStoredQuote, c.Quote, common.RawMessage) (Outcome, error)
	Observe(context.Context, model.CatalogStoredQuote, c.Invocation) (Outcome, error)
}
type AdapterRegistration struct {
	Key       string
	Model     string
	Operation c.Operation
	Adapter   Adapter
}
type Engine struct {
	SnapshotGate       func(context.Context, *gorm.DB, Scope, []c.CatalogModel) error
	DisplayNames       map[string]string
	DB                 *gorm.DB
	SourceID           string
	ArtifactRoot       string
	Adapters           []AdapterRegistration
	AdmissionError     error // Invalid current admission never blocks authorized recovery.
	RecoveryAdapter    func(model.CatalogStoredQuote) (Adapter, error)
	Now                func() time.Time
	CheckDirectory     func(*gorm.DB) error
	ValidateAcceptance func(context.Context, *gorm.DB, Scope, model.CatalogStoredQuote, c.Quote) error
}

// SQLite cannot promote competing deferred readers to writers. Preserve the
// conflict and original-call recovery path; never retry a transaction or send.
func (e *Engine) databaseConflict(err error) error {
	var coded interface{ Code() int }
	if e.DB.Dialector.Name() == "sqlite" && errors.As(err, &coded) && coded.Code()&0xff == 5 {
		return c.Invalid("INVOCATION_CONFLICT", "another invocation holds the database writer", "query the original invocation; do not replay submission")
	}
	return err
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now().UTC()
	}
	return time.Now().UTC()
}
func (e *Engine) registration(name string, op c.Operation) (AdapterRegistration, bool) {
	for _, a := range e.Adapters {
		if a.Model == name && a.Operation == op {
			return a, true
		}
	}
	return AdapterRegistration{}, false
}

func ValidRequestID(id string) bool {
	return len(id) > 0 && len(id) <= 128 && !strings.ContainsAny(id, "/\\?#\r\n\t ")
}

// RequestHash is defined over a canonical JSON tree with raw numeric lexemes,
// never float64. No generic URL stripping is allowed: an adapter may normalize
// temporary signed URLs only after verifying an immutable authorized asset.
func RequestHash(raw common.RawMessage) (string, error) {
	if common.GetJsonType(raw) != "object" {
		return "", c.Invalid("REQUEST_INVALID", "native request must be a JSON object", "send one native input object")
	}
	normalized, err := canonicalJSON(raw)
	if err != nil {
		return "", err
	}
	return c.Hash(struct {
		Version string            `json:"version"`
		Request common.RawMessage `json:"request"`
	}{"native-json-v1", normalized})
}
func canonicalJSON(raw common.RawMessage) (common.RawMessage, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty JSON")
	}
	switch common.GetJsonType(raw) {
	case "object":
		var m map[string]common.RawMessage
		if err := common.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		for k, v := range m {
			n, err := canonicalJSON(v)
			if err != nil {
				return nil, err
			}
			m[k] = n
		}
		return common.Marshal(m)
	case "string":
		var value string
		if err := common.Unmarshal(raw, &value); err != nil {
			return nil, err
		}
		return common.Marshal(value)
	case "array":
		var a []common.RawMessage
		if err := common.Unmarshal(raw, &a); err != nil {
			return nil, err
		}
		for i, v := range a {
			n, err := canonicalJSON(v)
			if err != nil {
				return nil, err
			}
			a[i] = n
		}
		return common.Marshal(a)
	default:
		var v common.RawMessage
		if err := common.Unmarshal(raw, &v); err != nil {
			return nil, err
		}
		return common.Marshal(v)
	}
}
func (e *Engine) Quote(ctx context.Context, p Principal, r c.QuoteRequest) (c.Quote, error) {
	var out c.Quote
	if !ValidRequestID(r.ClientRequestID) || !r.Operation.Valid() || r.Model == "" || r.SnapshotID == "" || r.OperationRevision == "" {
		return out, c.Invalid("REQUEST_INVALID", "invalid quote identity", "supply a model, operation and stable request ID")
	}
	if _, existingErr := model.FindCatalogInvocation(e.DB, p.ID, r.ClientRequestID); existingErr == nil {
		return out, c.Invalid("SUBMISSION_EXISTS", "logical request was already accepted", "recover the existing invocation; do not re-quote")
	} else if !errors.Is(existingErr, gorm.ErrRecordNotFound) {
		return out, existingErr
	}
	scope, err := p.ResolveScope(ctx)
	if err != nil {
		return out, err
	}
	if err = p.CanSubmit(ctx); err != nil {
		return out, err
	}
	snap, err := e.Catalog(ctx, p, r.SnapshotID)
	if err != nil {
		return out, err
	}
	if !snap.ValidUntil.After(e.now()) {
		return out, c.Invalid("CATALOG_SNAPSHOT_INVALID", "snapshot expired", "refresh the catalog")
	}
	if !slices.Contains(scope.Models, r.Model) || (scope.AllowsOperation != nil && !scope.AllowsOperation(r.Model, r.Operation)) {
		return out, c.Invalid("OPERATION_NOT_ADMITTED", "model is outside this scope", "refresh authorized models")
	}
	a, ok := e.registration(r.Model, r.Operation)
	if !ok {
		return out, c.Invalid("OPERATION_NOT_ADMITTED", "no verified execution adapter", "complete operation and billing admission")
	}
	var published *c.OperationDefinition
	for _, m := range snap.Models {
		if m.ID == r.Model {
			for _, def := range m.Operations {
				if def.Operation == r.Operation {
					copy := def
					published = &copy
				}
			}
		}
	}
	def, err := a.Adapter.Definition(ctx, scope, r.Model)
	if err != nil {
		return out, err
	}
	if published == nil || def.Operation != r.Operation || def.Validate() != nil || def.OperationRevision != r.OperationRevision || published.OperationRevision != def.OperationRevision || published.PricingRevision != def.PricingRevision {
		return out, c.Invalid("OPERATION_NOT_ADMITTED", "operation revision is not current", "refresh and validate the operation")
	}
	var body struct {
		Model string `json:"model"`
	}
	if common.Unmarshal(r.Request, &body) != nil || body.Model != r.Model {
		return out, c.Invalid("QUOTE_REQUEST_MISMATCH", "native request model differs", "quote the exact native request")
	}
	hash, err := RequestHash(r.Request)
	if err != nil {
		return out, c.Invalid("REQUEST_INVALID", "invalid native JSON", "supply valid JSON")
	}
	r.Request, err = canonicalJSON(r.Request)
	if err != nil {
		return out, err
	}
	bound, err := a.Adapter.Prepare(ctx, scope, r)
	if err != nil {
		return out, err
	}
	if len(bound.PricingSnapshot) == 0 || bound.RoundingVersion == "" {
		return out, c.Invalid("QUOTE_BOUND_UNAVAILABLE", "pricing evidence is incomplete", "supply a frozen executable pricing snapshot")
	}
	priceHash, err := c.Hash(struct {
		SourceID        string            `json:"sourceId"`
		ScopeID         string            `json:"scopeId"`
		Group           string            `json:"group"`
		AdapterKey      string            `json:"adapterKey"`
		PricingRevision string            `json:"pricingRevision"`
		Effective       common.RawMessage `json:"effective"`
	}{e.SourceID, scope.ScopeID, scope.Group, a.Key, def.PricingRevision, common.RawMessage(bound.PricingSnapshot)})
	if err != nil {
		return out, err
	}
	out = c.Quote{QuoteID: uuid.NewString(), ScopeID: scope.ScopeID, ClientRequestID: r.ClientRequestID, RequestHash: hash, OperationRevision: def.OperationRevision, PricingRevision: def.PricingRevision, PricingSnapshotHash: priceHash, ExpiresAt: e.now().Add(5 * time.Minute), EstimatedCost: bound.Estimated, MaxCost: bound.Maximum, RoundingVersion: bound.RoundingVersion}
	if err = out.Validate(scope.ScopeID, r.ClientRequestID, hash, r.OperationRevision, e.now()); err != nil {
		return out, err
	}
	data, err := common.Marshal(out)
	if err != nil {
		return out, err
	}
	err = e.DB.Create(&model.CatalogStoredQuote{QuoteID: out.QuoteID, PrincipalID: p.ID, ClientRequestID: r.ClientRequestID, ModelName: r.Model, Operation: string(r.Operation), SubmitPath: def.SubmitPath, AdapterKey: a.Key, FixedGroup: scope.Group, ScopeID: scope.ScopeID, Data: data, FrozenPricing: bound.PricingSnapshot}).Error
	return out, err
}
func (e *Engine) readQuote(principal, id string) (model.CatalogStoredQuote, c.Quote, error) {
	var row model.CatalogStoredQuote
	var q c.Quote
	err := e.DB.Where("quote_id = ?", id).Take(&row).Error
	if err != nil {
		return row, q, err
	}
	if row.PrincipalID != principal {
		return row, q, gorm.ErrRecordNotFound
	}
	err = common.Unmarshal(row.Data, &q)
	return row, q, err
}
func decodeInvocation(row model.CatalogInvocationRecord) (c.Invocation, error) {
	var out c.Invocation
	err := common.Unmarshal(row.Data, &out)
	return out, err
}

// Submit is called only after current credential and read authorization. Existing
// IDs are recovered before expiry, balance, model admission or new-send checks.
func (e *Engine) Submit(ctx context.Context, p Principal, path, quoteID, id string, raw common.RawMessage) (c.Invocation, error) {
	var out c.Invocation
	if !ValidRequestID(id) {
		return out, c.Invalid("REQUEST_INVALID", "invalid request ID", "use a stable bounded request ID")
	}
	hash, err := RequestHash(raw)
	if err != nil {
		return out, c.Invalid("REQUEST_INVALID", "invalid native JSON", "send one valid native input object")
	}
	old, err := model.FindCatalogInvocation(e.DB, p.ID, id)
	if err == nil {
		if old.RequestHash != hash {
			return out, c.Invalid("IDEMPOTENCY_CONFLICT", "request ID has different content", "recover the original invocation")
		}
		q, _, er := e.readQuote(p.ID, old.QuoteID)
		if er != nil {
			return out, er
		}
		if path != q.SubmitPath {
			return out, c.Invalid("QUOTE_REQUEST_MISMATCH", "native endpoint differs", "recover through the original endpoint or invocation query")
		}
		return decodeInvocation(old)
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return out, err
	}
	if e.AdmissionError != nil {
		return out, e.AdmissionError
	}
	stored, q, err := e.readQuote(p.ID, quoteID)
	if err != nil {
		return out, err
	}
	scope, err := p.ResolveScope(ctx)
	if err != nil {
		return out, err
	}
	if err = p.CanSubmit(ctx); err != nil {
		return out, err
	}
	if err = q.Validate(scope.ScopeID, id, hash, q.OperationRevision, e.now()); err != nil {
		return out, err
	}
	a, ok := e.registration(stored.ModelName, c.Operation(stored.Operation))
	if !ok || a.Key != stored.AdapterKey || path != stored.SubmitPath || !slices.Contains(scope.Models, stored.ModelName) || (scope.AllowsOperation != nil && !scope.AllowsOperation(stored.ModelName, c.Operation(stored.Operation))) {
		return out, c.Invalid("OPERATION_NOT_ADMITTED", "operation is no longer admitted", "refresh the catalog; do not substitute a model")
	}
	def, err := a.Adapter.Definition(ctx, scope, stored.ModelName)
	if err != nil {
		return out, err
	}
	// Price changes do not invalidate a still-valid frozen quote. The adapter must
	// retain the quoted execution variant; stopping supply still denies new sends.
	if def.Validate() != nil || def.Operation != c.Operation(stored.Operation) || def.SubmitPath != path || def.OperationRevision != q.OperationRevision {
		return out, c.Invalid("OPERATION_NOT_ADMITTED", "execution variant changed", "refresh the operation")
	}
	out = c.Invocation{InvocationID: uuid.NewString(), ClientRequestID: id, RequestHash: hash, ExecutionState: "accepted", BillingState: "pending"}
	data, err := common.Marshal(out)
	if err != nil {
		return out, err
	}
	var row model.CatalogInvocationRecord
	var owner bool
	err = e.DB.Transaction(func(tx *gorm.DB) error {
		if p.AcceptanceScope != nil {
			current, gateErr := p.AcceptanceScope(ctx, tx)
			if gateErr != nil {
				return gateErr
			}
			if current.ScopeID != scope.ScopeID || current.AuthorizationHash != scope.AuthorizationHash || !slices.Contains(current.Models, stored.ModelName) || (current.AllowsOperation != nil && !current.AllowsOperation(stored.ModelName, c.Operation(stored.Operation))) {
				return c.Invalid("OPERATION_NOT_ADMITTED", "authorization changed before acceptance", "refresh authorized operations")
			}
			scope = current
		}
		if e.CheckDirectory != nil {
			if gateErr := e.CheckDirectory(tx); gateErr != nil {
				return gateErr
			}
		}
		if e.ValidateAcceptance != nil {
			if gateErr := e.ValidateAcceptance(ctx, tx, scope, stored, q); gateErr != nil {
				return gateErr
			}
		}
		var claimErr error
		row, owner, claimErr = model.ClaimCatalogInvocation(tx, p.ID, id, hash, quoteID, data)
		return claimErr
	})
	if err != nil {
		return out, e.databaseConflict(err)
	}
	if !owner {
		return decodeInvocation(row)
	}
	// Persist uncertainty BEFORE external I/O. A crash at this point can lose a
	// send, but it cannot cause an automatic duplicate on process restart.
	out.ExecutionState = "unknown"
	data, err = common.Marshal(out)
	if err != nil {
		return out, err
	}
	if err = model.AdvanceCatalogInvocation(e.DB, &row, "unknown", data); err != nil {
		return out, err
	}
	raw, err = canonicalJSON(raw)
	if err != nil {
		return out, err
	}
	result, err := a.Adapter.Submit(ctx, stored, q, raw)
	if err != nil {
		return out, c.Invalid("SUBMISSION_UNKNOWN", "submission needs authoritative observation", "query this invocation; never resubmit with a new ID")
	}
	return e.recordOutcome(&row, stored, q, out, result)
}
func (e *Engine) Invocation(ctx context.Context, p Principal, id string, observe bool) (c.Invocation, error) {
	out, err := e.invocation(ctx, p, id, observe)
	if err != nil {
		return out, e.databaseConflict(err)
	}
	var rows []model.CatalogReceiptRecord
	if err = e.DB.Where("invocation_key = ?", model.CatalogKey(p.ID, id)).Order("revision ASC").Find(&rows).Error; err != nil {
		return out, err
	}
	for _, row := range rows {
		var r c.Receipt
		if err = common.Unmarshal(row.Data, &r); err != nil {
			return out, err
		}
		out.Receipts = append(out.Receipts, r)
	}
	return out, nil
}
func (e *Engine) invocation(ctx context.Context, p Principal, id string, observe bool) (c.Invocation, error) {
	row, err := model.FindCatalogInvocation(e.DB, p.ID, id)
	if err != nil {
		return c.Invocation{}, err
	}
	out, err := decodeInvocation(row)
	if err != nil || !observe {
		return out, err
	}
	if out.BillingState == "not_charged" && out.ExecutionState == "failed" {
		return out, nil
	}
	stored, q, err := e.readQuote(p.ID, row.QuoteID)
	if err != nil {
		return out, err
	}
	// accepted is durable proof that the sender has not crossed its I/O CAS.
	// Expiry can safely close this abandoned claim; unknown can never use this
	// path. Racing with the original sender is resolved by the same CAS.
	if row.State == "accepted" && !q.ExpiresAt.After(e.now()) {
		out.ExecutionState = "failed"
		out.BillingState = "not_charged"
		data, err := common.Marshal(out)
		if err != nil {
			return out, err
		}
		return out, model.AdvanceCatalogInvocation(e.DB, &row, out.ExecutionState, data)
	}
	a, ok := e.registration(stored.ModelName, c.Operation(stored.Operation))
	if !ok || a.Key != stored.AdapterKey {
		if e.RecoveryAdapter == nil {
			return out, nil
		}
		adapter, recoveryErr := e.RecoveryAdapter(stored)
		if recoveryErr != nil {
			return out, recoveryErr
		}
		a.Adapter = adapter
	}
	next, err := a.Adapter.Observe(ctx, stored, out)
	if err != nil {
		return out, err
	}
	return e.recordOutcome(&row, stored, q, out, next)
}

var quotaInteger = regexp.MustCompile(`^(0|[1-9][0-9]{0,39})$`)

func (e *Engine) recordOutcome(row *model.CatalogInvocationRecord, stored model.CatalogStoredQuote, q c.Quote, out c.Invocation, next Outcome) (c.Invocation, error) {
	if !slices.Contains([]string{"accepted", "running", "succeeded", "failed", "cancelled", "unknown"}, next.State) {
		return out, c.Invalid("PROVIDER_STATE_INVALID", "unknown provider state", "query authoritative evidence")
	}
	if slices.Contains([]string{"succeeded", "failed", "cancelled"}, out.ExecutionState) && next.State != out.ExecutionState {
		return out, c.Invalid("PROVIDER_STATE_INVALID", "terminal execution changed", "reconcile provider evidence")
	}
	out.ExecutionState = next.State
	if next.ProviderTaskID != "" {
		if out.ProviderTaskID != "" && out.ProviderTaskID != next.ProviderTaskID {
			return out, c.Invalid("PROVIDER_STATE_INVALID", "provider task identity changed", "reconcile provider evidence")
		}
		out.ProviderTaskID = next.ProviderTaskID
	}
	if len(next.Result) > 0 {
		out.Result = next.Result
	}
	if next.Artifacts != nil {
		out.Artifacts = next.Artifacts
	}
	if next.NotCharged {
		if next.State != "failed" || next.Receipt != nil || out.Receipt != nil {
			return out, fmt.Errorf("invalid not-charged proof")
		}
		out.BillingState = "not_charged"
	}
	if next.Receipt == nil {
		data, err := common.Marshal(out)
		if err != nil {
			return out, err
		}
		return out, model.AdvanceCatalogInvocation(e.DB, row, out.ExecutionState, data)
	}
	r := *next.Receipt
	if r.SourceID != e.SourceID || r.IntegrationPrincipalID != row.PrincipalID || r.QuoteID != row.QuoteID || r.PricingRevision != q.PricingRevision || r.ReceiptID == "" || r.AmountMode != "cumulative" || r.Revision < 1 || r.RawQuota == "" || r.QuotaPerUnit == "" || r.RoundingVersion == "" {
		return out, c.Invalid("RECEIPT_MISMATCH", "receipt evidence or identity is invalid", "reconcile provider evidence")
	}
	if !quotaInteger.MatchString(r.RawQuota) || !quotaInteger.MatchString(r.QuotaPerUnit) || r.QuotaPerUnit == "0" || r.RoundingVersion != q.RoundingVersion || common.GetJsonType(r.RawUsage) != "object" {
		return out, c.Invalid("RECEIPT_MISMATCH", "invalid quota or frozen rounding evidence", "reconcile provider evidence")
	}
	usage, usageErr := canonicalJSON(r.RawUsage)
	if usageErr != nil {
		return out, c.Invalid("RECEIPT_MISMATCH", "invalid usage JSON", "retrieve authoritative evidence")
	}
	r.RawUsage = usage
	// Supplier emits cumulative receipts; the shared consumer also understands
	// explicit delta receipts. No implicit conversion or rounding occurs here.
	old := out.Receipt
	if old == nil {
		if r.Revision != 1 || r.PredecessorRevision != 0 {
			return out, c.Invalid("RECEIPT_MISMATCH", "missing first receipt revision", "retrieve predecessor receipts")
		}
	} else {
		if r.Revision == old.Revision {
			a, _ := c.Hash(r)
			b, _ := c.Hash(old)
			if a != b {
				return out, c.Invalid("RECEIPT_MISMATCH", "immutable receipt changed", "reconcile provider evidence")
			}
			data, er := common.Marshal(out)
			if er != nil {
				return out, er
			}
			return out, model.AdvanceCatalogInvocation(e.DB, row, out.ExecutionState, data)
		}
		if old.IsFinal && !r.IsFinal {
			return out, c.Invalid("RECEIPT_MISMATCH", "final receipt cannot become provisional", "retrieve final evidence")
		}
		if r.ReceiptID != old.ReceiptID || r.Revision != old.Revision+1 || r.PredecessorRevision != old.Revision {
			return out, c.Invalid("RECEIPT_MISMATCH", "receipt sequence has a gap", "retrieve predecessor receipts")
		}
	}
	cost, err := r.Cost.Micros()
	if err != nil {
		return out, err
	}
	max, err := q.MaxCost.Micros()
	if err != nil {
		return out, err
	}
	if r.IsFinal && r.SettledAt.IsZero() {
		return out, c.Invalid("RECEIPT_MISMATCH", "final receipt has no settlement time", "retrieve final evidence")
	}
	out.Receipt = &r
	wasReconciliation := out.BillingState == "reconciliation_required"
	out.BillingState = "pending"
	if r.IsFinal {
		out.BillingState = "settled"
	}
	if cost.Cmp(max) > 0 || wasReconciliation {
		out.BillingState = "reconciliation_required"
	}
	if old != nil && old.IsFinal {
		prev, _ := old.Cost.Micros()
		if cost.Cmp(prev) > 0 {
			out.BillingState = "reconciliation_required"
		}
	}
	return out, model.CommitCatalogReceipt(e.DB, row, r, out)
}
