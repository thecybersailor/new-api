package model

import (
	"errors"
	"strconv"
	"time"

	"github.com/QuantumNous/new-api/common"
	contract "github.com/QuantumNous/new-api/pkg/catalogcontract"
	"gorm.io/gorm"
)

// Binding is explicit and server-owned. Rotating a token preserves PrincipalID;
// sharing an account never implicitly grants access to another integration.
type CatalogIntegrationBinding struct {
	ScopeID           string `gorm:"-" json:"scopeId"`
	BindingVersion    string `gorm:"-" json:"bindingVersion"`
	Revision          int64  `json:"revision"`
	ModelPolicy       string `gorm:"size:32" json:"modelPolicy"`
	AllowedModels     string `gorm:"type:text" json:"allowedModels"`
	AllowedOperations string `gorm:"type:text" json:"allowedOperations"`
	ID                int    `gorm:"primaryKey"`
	TokenID           int    `gorm:"uniqueIndex;not null" json:"tokenId"`
	UserID            int    `gorm:"index;not null" json:"userId"`
	PrincipalID       string `gorm:"size:128;index;not null" json:"principalId"`
	FixedGroup        string `gorm:"size:128;not null" json:"fixedGroup"`
	Enabled           bool   `json:"enabled"`
}
type CatalogStoredSnapshot struct {
	AuthorizationHash string `gorm:"size:64"`
	ID                int    `gorm:"primaryKey"`
	LookupKey         string `gorm:"size:64;uniqueIndex;not null"`
	PrincipalID       string `gorm:"size:128;not null"`
	ScopeID           string `gorm:"size:64;not null"`
	SnapshotID        string `gorm:"size:64;not null"`
	ValidUntil        time.Time
	Data              []byte
}
type CatalogStoredQuote struct {
	ID              int    `gorm:"primaryKey"`
	QuoteID         string `gorm:"size:64;uniqueIndex;not null"`
	PrincipalID     string `gorm:"size:128;not null"`
	ClientRequestID string `gorm:"size:128;not null"`
	ModelName       string `gorm:"size:256;not null"`
	Operation       string `gorm:"size:64;not null"`
	SubmitPath      string `gorm:"size:256;not null"`
	AdapterKey      string `gorm:"size:128;not null"`
	FixedGroup      string `gorm:"size:128;not null"`
	ScopeID         string `gorm:"size:64;not null"`
	Data            []byte
	FrozenPricing   []byte
}
type CatalogInvocationRecord struct {
	ID              int    `gorm:"primaryKey"`
	LookupKey       string `gorm:"size:64;uniqueIndex;not null"`
	PrincipalID     string `gorm:"size:128;not null"`
	ClientRequestID string `gorm:"size:128;not null"`
	RequestHash     string `gorm:"size:64;not null"`
	QuoteID         string `gorm:"size:64;not null"`
	State           string `gorm:"size:32;not null"`
	Version         int64
	Data            []byte
	UpdatedAt       time.Time
}
type CatalogReceiptRecord struct {
	ID            int    `gorm:"primaryKey"`
	LookupKey     string `gorm:"size:64;uniqueIndex;not null"`
	InvocationKey string `gorm:"size:64;index;not null"`
	Revision      int64
	Data          []byte
}

func CatalogIntegrationModels() []any {
	return []any{&CatalogIntegrationBinding{}, &CatalogStoredSnapshot{}, &CatalogStoredQuote{}, &CatalogInvocationRecord{}, &CatalogReceiptRecord{}, &CatalogExecution{}, &CatalogArtifact{}, &CatalogDirectory{}, &CatalogDirectoryAudit{}, &CatalogScopeVersion{}}
}

// CatalogKey hashes a tuple, not a concatenation. ASCII hashes keep key identity
// case-sensitive even on MySQL's default case-insensitive text collation.
func CatalogKey(parts ...string) string { h, _ := contract.Hash(parts); return h }

func FindCatalogInvocation(db *gorm.DB, principal, requestID string) (CatalogInvocationRecord, error) {
	var row CatalogInvocationRecord
	err := db.Where("lookup_key = ?", CatalogKey(principal, requestID)).Take(&row).Error
	if err == nil && (row.PrincipalID != principal || row.ClientRequestID != requestID) {
		return row, errors.New("invocation identity mismatch")
	}
	return row, err
}

// ClaimCatalogInvocation must precede all provider I/O. The unique constraint,
// rather than an in-process mutex, elects one sender across nodes and restarts.
func ClaimCatalogInvocation(db *gorm.DB, principal, requestID, hash, quoteID string, data []byte) (CatalogInvocationRecord, bool, error) {
	row := CatalogInvocationRecord{LookupKey: CatalogKey(principal, requestID), PrincipalID: principal, ClientRequestID: requestID, RequestHash: hash, QuoteID: quoteID, State: "accepted", Data: data, UpdatedAt: time.Now().UTC()}
	// Plain INSERT distinguishes ownership even with MySQL clientFoundRows:
	// an ON DUPLICATE KEY no-op can otherwise report one affected row.
	var inserted bool
	// Acceptance now owns an outer transaction. A nested savepoint keeps a
	// PostgreSQL unique-key conflict from poisoning that transaction before
	// reading the winner. Plain INSERT still avoids MySQL clientFoundRows ambiguity.
	insertErr := db.Transaction(func(tx *gorm.DB) error {
		r := tx.Create(&row)
		inserted = r.Error == nil && r.RowsAffected == 1
		return r.Error
	})
	if insertErr == nil && inserted {
		return row, true, nil
	}
	old, err := FindCatalogInvocation(db, principal, requestID)
	if err != nil {
		if insertErr != nil {
			return row, false, insertErr
		}
		return old, false, err
	}

	if old.RequestHash != hash {
		return old, false, contract.Invalid("IDEMPOTENCY_CONFLICT", "request ID has different content", "use the original request or a new logical request ID")
	}
	return old, false, nil
}

// Compare-and-swap works on all three databases, including SQLite (no FOR UPDATE).
func AdvanceCatalogInvocation(db *gorm.DB, row *CatalogInvocationRecord, state string, data []byte) error {
	r := db.Model(&CatalogInvocationRecord{}).Where("id = ? AND version = ? AND state = ?", row.ID, row.Version, row.State).Updates(map[string]any{"state": state, "data": data, "version": row.Version + 1, "updated_at": time.Now().UTC()})
	if r.Error != nil {
		return r.Error
	}
	if r.RowsAffected != 1 {
		return contract.Invalid("INVOCATION_CONFLICT", "invocation was concurrently updated", "read the invocation; never resend")
	}
	row.State = state
	row.Data = data
	row.Version++
	return nil
}

// A receipt and its observable invocation state commit together. A failure
// rolls back both writes so observation cannot leave half a settlement.
func CommitCatalogReceipt(db *gorm.DB, row *CatalogInvocationRecord, receipt contract.Receipt, updated contract.Invocation) error {
	raw, err := common.Marshal(receipt)
	if err != nil {
		return err
	}
	payload, err := common.Marshal(updated)
	if err != nil {
		return err
	}
	key := CatalogKey(receipt.SourceID, receipt.IntegrationPrincipalID, updated.InvocationID, receipt.ReceiptID, strconv.FormatInt(receipt.Revision, 10))
	next := *row
	err = db.Transaction(func(tx *gorm.DB) error {
		var old CatalogReceiptRecord
		e := tx.Where("lookup_key = ?", key).Take(&old).Error
		if e == nil {
			if string(old.Data) != string(raw) {
				return contract.Invalid("RECEIPT_MISMATCH", "receipt revision changed", "reconcile immutable provider evidence")
			}
			return nil
		}
		if !errors.Is(e, gorm.ErrRecordNotFound) {
			return e
		}
		// Elect this revision's writer before inserting its receipt. Concurrent
		// observers lose the existing CAS, instead of racing a unique-key INSERT.
		// Both writes still commit or roll back in this one transaction.
		if e = AdvanceCatalogInvocation(tx, &next, updated.ExecutionState, payload); e != nil {
			return e
		}
		return tx.Create(&CatalogReceiptRecord{LookupKey: key, InvocationKey: row.LookupKey, Revision: receipt.Revision, Data: raw}).Error
	})
	if err == nil {
		*row = next
	}
	return err
}
