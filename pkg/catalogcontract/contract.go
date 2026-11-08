// Package catalogcontract defines the versioned DanToken supplier contract. No
// provider prices, inferred capabilities, or deployment configuration live here.
package catalogcontract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/QuantumNous/new-api/common"
	"math/big"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
)

type Operation string

const (
	TextGeneration       Operation = "text_generation"
	ImageGeneration      Operation = "image_generation"
	ImageEdit            Operation = "image_edit"
	VideoGeneration      Operation = "video_generation"
	AudioTranscription   Operation = "audio_transcription"
	SpeechSynthesis      Operation = "speech_synthesis"
	Embedding            Operation = "embedding"
	MultimodalGeneration Operation = "multimodal_generation"
)

func (o Operation) Valid() bool {
	switch o {
	case TextGeneration, ImageGeneration, ImageEdit, VideoGeneration, AudioTranscription, SpeechSynthesis, Embedding, MultimodalGeneration:
		return true
	}
	return false
}

type Error struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	RequestID  string `json:"requestId,omitempty"`
	NextAction string `json:"nextAction"`
	Retryable  bool   `json:"retryable"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message + "; " + e.NextAction }
func Invalid(code, message, action string) error {
	return &Error{Code: code, Message: message, NextAction: action}
}

type Money struct {
	Currency     string `json:"currency"`
	AmountMicros string `json:"amountMicros"`
}

var unsignedDecimal = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)
var signedDecimal = regexp.MustCompile(`^(0|-?[1-9][0-9]*)$`)

func (m Money) Micros() (*big.Int, error) {
	if m.Currency != "USD" || !unsignedDecimal.MatchString(m.AmountMicros) || len(m.AmountMicros) > 40 {
		return nil, Invalid("PRICE_INVALID", "expected nonnegative USD integer micros", "refresh the authoritative price contract")
	}
	v, ok := new(big.Int).SetString(m.AmountMicros, 10)
	if !ok {
		return nil, fmt.Errorf("invalid micros")
	}
	return v, nil
}
func Hash(value any) (string, error) {
	b, e := common.Marshal(value)
	if e != nil {
		return "", e
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}
func ValidateNativePath(p string) error {
	u, e := url.Parse(p)
	if e != nil || !strings.HasPrefix(p, "/v1/") || u.IsAbs() || u.Host != "" || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(p, "%\\\r\n") || path.Clean(p) != p {
		return Invalid("PROTOCOL_INVALID", "invalid native operation path", "use a registered /v1/ adapter path without query or traversal")
	}
	return nil
}

type OperationDefinition struct {
	Operation         Operation       `json:"operation"`
	OperationRevision string          `json:"operationRevision"`
	InputSchema       json.RawMessage `json:"inputSchema"`
	OutputSchema      json.RawMessage `json:"outputSchema"`
	Limits            json.RawMessage `json:"limits"`
	InputModalities   []string        `json:"inputModalities"`
	OutputModalities  []string        `json:"outputModalities"`
	ExecutionMode     []string        `json:"executionMode"`
	Protocol          string          `json:"protocol"`
	SubmitPath        string          `json:"submitPath"`
	QueryPathTemplate string          `json:"queryPathTemplate,omitempty"`
	PricingRevision   string          `json:"pricingRevision"`
	UsageSchema       json.RawMessage `json:"usageSchema"`
	Capabilities      json.RawMessage `json:"capabilities,omitempty"`
}

func (o OperationDefinition) Validate() error {
	if !o.Operation.Valid() || o.OperationRevision == "" || o.PricingRevision == "" || o.Protocol == "" || len(o.InputModalities) == 0 || len(o.OutputModalities) == 0 || len(o.ExecutionMode) == 0 {
		return Invalid("OPERATION_INVALID", "operation definition is incomplete", "supply operation, immutable revisions, modalities and execution mode")
	}
	for _, mode := range o.ExecutionMode {
		if mode != "sync" && mode != "stream" && mode != "async" {
			return Invalid("OPERATION_INVALID", "unknown execution mode", "publish sync, stream or async explicitly")
		}
	}
	for _, raw := range []json.RawMessage{o.InputSchema, o.OutputSchema, o.Limits, o.UsageSchema} {
		var m map[string]any
		if common.Unmarshal(raw, &m) != nil || m == nil {
			return Invalid("OPERATION_INVALID", "missing object schema or limits", "publish complete validated metadata")
		}
	}
	if err := ValidateNativePath(o.SubmitPath); err != nil {
		return err
	}
	if o.QueryPathTemplate != "" {
		return ValidateNativePath(o.QueryPathTemplate)
	}
	return nil
}

type CatalogModel struct {
	ID          string                `json:"id"`
	DisplayName string                `json:"displayName"`
	SupplyState string                `json:"supplyState"`
	Operations  []OperationDefinition `json:"operations"`
}
type CatalogRemoval struct {
	ModelID   string    `json:"modelId"`
	Operation Operation `json:"operation,omitempty"`
	Reasons   []string  `json:"reasons"`
}

type CatalogSnapshot struct {
	BindingVersion        string           `json:"bindingVersion"`
	AuthorizationRevision int64            `json:"authorizationRevision"`
	CatalogRevision       int64            `json:"catalogRevision"`
	TotalCount            int              `json:"totalCount"`
	ContentDigest         string           `json:"contentDigest"`
	EmptyReason           string           `json:"emptyReason,omitempty"`
	Removals              []CatalogRemoval `json:"removals,omitempty"`
	SchemaVersion         int              `json:"schemaVersion"`
	SourceID              string           `json:"sourceId"`
	ScopeID               string           `json:"scopeId"`
	SnapshotID            string           `json:"snapshotId"`
	GeneratedAt           time.Time        `json:"generatedAt"`
	ValidUntil            time.Time        `json:"validUntil"`
	NextCursor            string           `json:"nextCursor,omitempty"`
	Complete              bool             `json:"complete"`
	Models                []CatalogModel   `json:"models"`
}

func (s CatalogSnapshot) Validate(now time.Time) error {
	if s.SchemaVersion != 2 {
		return Invalid("CATALOG_SCHEMA_UNSUPPORTED", "unknown catalog schema version", "upgrade the consumer before activating this source")
	}
	if s.SourceID == "" || s.ScopeID == "" || s.SnapshotID == "" || s.GeneratedAt.IsZero() || !s.ValidUntil.After(s.GeneratedAt) || !s.ValidUntil.After(now) {
		return Invalid("CATALOG_SNAPSHOT_INVALID", "catalog identity or lifetime is invalid", "fetch a complete fresh snapshot")
	}
	if !s.Complete || s.NextCursor != "" {
		return Invalid("CATALOG_SNAPSHOT_INCOMPLETE", "catalog pages are incomplete", "retrieve all pages from the same snapshot before activation")
	}
	if err := s.validateV2(); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, m := range s.Models {
		if m.ID == "" || m.DisplayName == "" || seen[m.ID] {
			return Invalid("CATALOG_MODEL_INVALID", "missing or duplicate model identity", "correct the source snapshot")
		}
		seen[m.ID] = true
		if m.SupplyState != "active" && m.SupplyState != "retired" && m.SupplyState != "disabled" {
			return Invalid("CATALOG_MODEL_INVALID", "unknown supply state", "declare active, retired or disabled")
		}
		ops := map[Operation]bool{}
		for _, o := range m.Operations {
			if ops[o.Operation] {
				return Invalid("CATALOG_MODEL_INVALID", "duplicate operation", "publish one definition per model operation")
			}
			ops[o.Operation] = true
			if err := o.Validate(); err != nil {
				return err
			}
		}
	}
	return nil
}

type QuoteRequest struct {
	ClientRequestID   string          `json:"clientRequestId"`
	SnapshotID        string          `json:"snapshotId"`
	Model             string          `json:"model"`
	Operation         Operation       `json:"operation"`
	OperationRevision string          `json:"operationRevision"`
	Request           json.RawMessage `json:"request"`
}
type Quote struct {
	QuoteID             string    `json:"quoteId"`
	ScopeID             string    `json:"scopeId"`
	ClientRequestID     string    `json:"clientRequestId"`
	RequestHash         string    `json:"requestHash"`
	OperationRevision   string    `json:"operationRevision"`
	PricingRevision     string    `json:"pricingRevision"`
	PricingSnapshotHash string    `json:"pricingSnapshotHash"`
	ExpiresAt           time.Time `json:"expiresAt"`
	EstimatedCost       Money     `json:"estimatedCost"`
	MaxCost             Money     `json:"maxCost"`
	RoundingVersion     string    `json:"roundingVersion,omitempty"`
}

func (q Quote) Validate(scope, id, hash, revision string, now time.Time) error {
	if q.QuoteID == "" || q.ScopeID != scope || q.ClientRequestID != id || q.RequestHash != hash || q.OperationRevision != revision || q.PricingRevision == "" || q.PricingSnapshotHash == "" {
		return Invalid("QUOTE_REQUEST_MISMATCH", "quote identity or immutable request differs", "obtain a quote for the exact authorized request")
	}
	if !q.ExpiresAt.After(now) {
		return Invalid("QUOTE_EXPIRED", "quote expired before submission", "reconcile the original invocation before obtaining another quote")
	}
	max, e := q.MaxCost.Micros()
	if e != nil {
		return e
	}
	estimate, e := q.EstimatedCost.Micros()
	if e != nil {
		return e
	}
	if estimate.Cmp(max) > 0 {
		return Invalid("QUOTE_BOUND_UNAVAILABLE", "estimate exceeds maximum cost", "ask the source to provide an enforceable bound")
	}
	return nil
}

type Artifact struct {
	ArtifactID string `json:"artifactId"`
	Type       string `json:"type"`
	MIME       string `json:"mime"`
	Size       int64  `json:"size"`
	Checksum   string `json:"checksum"`
	ReadPath   string `json:"readPath"`
}
type Invocation struct {
	InvocationID    string          `json:"invocationId"`
	ClientRequestID string          `json:"clientRequestId"`
	RequestHash     string          `json:"requestHash"`
	ExecutionState  string          `json:"executionState"`
	BillingState    string          `json:"billingState"`
	ProviderTaskID  string          `json:"providerTaskId,omitempty"`
	Result          json.RawMessage `json:"result,omitempty"`
	Artifacts       []Artifact      `json:"artifacts,omitempty"`
	Receipt         *Receipt        `json:"receipt,omitempty"`
	Receipts        []Receipt       `json:"receipts,omitempty"`
}
type Receipt struct {
	SourceID               string          `json:"sourceId"`
	IntegrationPrincipalID string          `json:"integrationPrincipalId"`
	ReceiptID              string          `json:"receiptId"`
	Revision               int64           `json:"revision"`
	PredecessorRevision    int64           `json:"predecessorRevision"`
	AmountMode             string          `json:"amountMode"`
	IsFinal                bool            `json:"isFinal"`
	QuoteID                string          `json:"quoteId"`
	PricingRevision        string          `json:"pricingRevision"`
	RawUsage               json.RawMessage `json:"rawUsage"`
	Cost                   Money           `json:"cost"`
	SettledAt              time.Time       `json:"settledAt"`
	RawQuota               string          `json:"rawQuota,omitempty"`
	QuotaPerUnit           string          `json:"quotaPerUnit,omitempty"`
	RoundingVersion        string          `json:"roundingVersion,omitempty"`
}
