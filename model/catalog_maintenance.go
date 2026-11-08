package model

import (
	"errors"
	"time"

	"github.com/QuantumNous/new-api/common"
	c "github.com/QuantumNous/new-api/pkg/catalogcontract"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Drafts never drive the relay. Published JSON and its audit are committed with
// the pointer in one transaction; no file fallback is permitted once enabled.
type CatalogDirectory struct {
	ID                int    `gorm:"primaryKey" json:"-"`
	Name              string `gorm:"size:64;uniqueIndex;not null" json:"-"`
	DraftRevision     int64  `json:"draftRevision"`
	PublishedRevision int64  `json:"publishedRevision"`
	DatabaseEnabled   bool   `json:"databaseEnabled"`
	Draft             []byte `json:"-"`
	Published         []byte `json:"-"`
}
type CatalogDirectoryAudit struct {
	ID                int       `gorm:"primaryKey" json:"id"`
	DraftRevision     int64     `json:"draftRevision"`
	PublishedRevision int64     `json:"publishedRevision"`
	ActorID           int       `json:"actorId"`
	Action            string    `gorm:"size:32;not null" json:"action"`
	Reason            string    `gorm:"type:text" json:"reason"`
	Data              []byte    `json:"-"`
	CreatedAt         time.Time `json:"createdAt"`
}
type CatalogScopeVersion struct {
	ID                    int    `gorm:"primaryKey"`
	ScopeID               string `gorm:"size:64;uniqueIndex;not null"`
	AuthorizationHash     string `gorm:"size:64"`
	AuthorizationRevision int64
	CatalogHash           string `gorm:"size:64"`
	CatalogRevision       int64
	Version               int64
	Snapshot              []byte
}

type CatalogMaintenanceOperation struct {
	Operation      c.Operation       `json:"operation"`
	Config         common.RawMessage `json:"config"`
	Evidence       string            `json:"evidence"`
	State          string            `json:"state"`
	DependencyHash string            `json:"dependencyHash,omitempty"`
}
type CatalogMaintenanceEntry struct {
	Model       string                        `json:"model"`
	DisplayName string                        `json:"displayName"`
	Owner       string                        `json:"owner"`
	Operations  []CatalogMaintenanceOperation `json:"operations"`
}

func ReadCatalogDirectory(db *gorm.DB) (CatalogDirectory, error) {
	var out CatalogDirectory
	err := db.Where("name = ?", "dantoken").Take(&out).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return CatalogDirectory{Draft: []byte("[]"), Published: []byte("[]")}, nil
	}
	return out, err
}

// SaveCatalogDirectory uses compare-and-swap for SQLite as well as row locks on
// MySQL/PostgreSQL. The supplied expected revisions cover both draft and live.
func SaveCatalogDirectory(db *gorm.DB, expected CatalogDirectory, draft, published []byte, action, reason string, actor int, publish bool) error {
	if actor <= 0 || reason == "" {
		return c.Invalid("REQUEST_INVALID", "actor and change reason are required", "record an accountable change")
	}
	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&CatalogDirectory{Name: "dantoken", Draft: []byte("[]"), Published: []byte("[]")}).Error; err != nil {
			return err
		}
		var current CatalogDirectory
		if err := lockForUpdate(tx).Where("name = ?", "dantoken").Take(&current).Error; err != nil {
			return err
		}
		if current.DraftRevision != expected.DraftRevision || current.PublishedRevision != expected.PublishedRevision {
			return c.Invalid("CATALOG_CONFLICT", "directory changed since this preview", "refresh and review the new differences")
		}
		updates := map[string]any{"draft": draft, "draft_revision": current.DraftRevision + 1}
		revision := current.PublishedRevision
		if publish {
			revision++
			updates["published"] = published
			updates["published_revision"] = revision
			updates["database_enabled"] = true
		}
		result := tx.Model(&current).Where("draft_revision = ? AND published_revision = ?", expected.DraftRevision, expected.PublishedRevision).Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return c.Invalid("CATALOG_CONFLICT", "directory was concurrently changed", "refresh before saving")
		}
		evidence, err := common.Marshal(struct{ Draft, Published common.RawMessage }{draft, published})
		if err != nil {
			return err
		}
		return tx.Create(&CatalogDirectoryAudit{DraftRevision: expected.DraftRevision + 1, PublishedRevision: revision, ActorID: actor, Action: action, Reason: reason, Data: evidence, CreatedAt: time.Now().UTC()}).Error
	})
}

func LockCatalogScopeVersion(tx *gorm.DB, scope string) (CatalogScopeVersion, error) {
	row := CatalogScopeVersion{ScopeID: scope}
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
		return row, err
	}
	err := lockForUpdate(tx).Where("scope_id = ?", scope).Take(&row).Error
	return row, err
}

// Locks only this integration's credential and owner, never other principals.
// Directory publication and credential edits serialize with first acceptance.
func LockCatalogAcceptance(tx *gorm.DB, tokenID int) error {
	var token Token
	if err := lockForUpdate(tx).Where("id = ?", tokenID).Take(&token).Error; err != nil {
		return err
	}
	var user User
	if err := lockForUpdate(tx).Where("id = ?", token.UserId).Take(&user).Error; err != nil {
		return err
	}
	var binding CatalogIntegrationBinding
	if err := lockForUpdate(tx).Where("token_id = ?", tokenID).Take(&binding).Error; err != nil {
		return err
	}
	var directory CatalogDirectory
	err := lockForUpdate(tx).Where("name = ?", "dantoken").Take(&directory).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	return err
}

func LockCatalogChannel(tx *gorm.DB, id int) error {
	var row Channel
	if err := lockForUpdate(tx).Where("id = ?", id).Take(&row).Error; err != nil {
		return err
	}
	var abilities []Ability
	if err := lockForUpdate(tx).Where("channel_id = ?", id).Find(&abilities).Error; err != nil {
		return err
	}
	var options []Option
	// Pricing editors use these same persisted option rows. Locks end at
	// acceptance, before any provider I/O, and never alter prices.
	return lockForUpdate(tx).Where(commonKeyCol+" IN ?", modelPricingOptionKeys).Order(commonKeyCol).Find(&options).Error
}

func SaveCatalogBinding(db *gorm.DB, next CatalogIntegrationBinding, expected int64, actor int, reason string) error {
	return db.Transaction(func(tx *gorm.DB) error {
		var token Token
		if err := lockForUpdate(tx).Where("id = ? AND user_id = ?", next.TokenID, next.UserID).Take(&token).Error; err != nil {
			return err
		}
		group := token.Group
		var user User
		if err := lockForUpdate(tx).Where("id = ?", next.UserID).Take(&user).Error; err != nil {
			return err
		}
		if group == "" {
			group = user.Group
		}
		if group == "" || group == "auto" || group != next.FixedGroup {
			return c.Invalid("SCOPE_CHANGED", "token does not use this fixed group", "select a matching dedicated token")
		}
		var count int64
		if err := tx.Model(&CatalogIntegrationBinding{}).Where("principal_id = ? AND user_id <> ?", next.PrincipalID, next.UserID).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			return c.Invalid("ACCESS_DENIED", "principal belongs to another account", "use an account-owned principal")
		}
		var old CatalogIntegrationBinding
		err := lockForUpdate(tx).Where("token_id = ?", next.TokenID).Take(&old).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			if expected != 0 {
				return c.Invalid("CATALOG_CONFLICT", "binding no longer matches", "refresh bindings")
			}
			next.Revision = 1
			if err = tx.Create(&next).Error; err != nil {
				return err
			}
		} else {
			if err != nil {
				return err
			}
			if old.Revision != expected {
				return c.Invalid("CATALOG_CONFLICT", "binding changed", "refresh bindings")
			}
			if old.UserID != next.UserID || old.PrincipalID != next.PrincipalID || old.FixedGroup != next.FixedGroup {
				return c.Invalid("SCOPE_CHANGED", "binding identity is immutable", "create an explicit new binding and migration plan")
			}
			next.ID = old.ID
			next.Revision = expected + 1
			result := tx.Model(&old).Where("revision = ?", expected).Updates(map[string]any{"revision": next.Revision, "enabled": next.Enabled, "model_policy": next.ModelPolicy, "allowed_models": next.AllowedModels, "allowed_operations": next.AllowedOperations})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return c.Invalid("CATALOG_CONFLICT", "binding changed", "refresh bindings")
			}
		}
		// The explicit policy belongs to a logical binding, not a credential.
		if err = tx.Model(&CatalogIntegrationBinding{}).Where("principal_id = ? AND user_id = ? AND fixed_group = ? AND id <> ?", next.PrincipalID, next.UserID, next.FixedGroup, next.ID).Updates(map[string]any{"model_policy": next.ModelPolicy, "allowed_models": next.AllowedModels, "allowed_operations": next.AllowedOperations, "revision": gorm.Expr("revision + 1")}).Error; err != nil {
			return err
		}
		raw, err := common.Marshal(next)
		if err != nil {
			return err
		}
		return tx.Create(&CatalogDirectoryAudit{ActorID: actor, Action: "authorize", Reason: reason, Data: raw, CreatedAt: time.Now().UTC()}).Error
	})
}
