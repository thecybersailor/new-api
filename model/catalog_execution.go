package model

import (
	"errors"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

var ErrCatalogReconciliation = errors.New("catalog settlement requires reconciliation")

// CatalogExecution is the durable supplier wallet reservation and recovery
// journal. A relay may settle it once; observations never repeat provider I/O.
type CatalogExecution struct {
	ID             int    `gorm:"primaryKey"`
	QuoteID        string `gorm:"size:64;uniqueIndex;not null"`
	UserID         int
	TokenID        int
	ChannelID      int `gorm:"index:idx_catalog_execution_admission,priority:1"`
	Reserved       int
	Actual         int
	BillingState   string `gorm:"size:32;index:idx_catalog_execution_admission,priority:2"`
	ExecutionState string `gorm:"size:32"`
	Outcome        []byte
	NativeBody     []byte // Bounded native response; binary media lives in the artifact store
	ContentType    string `gorm:"size:128"`
	TaskID         string `gorm:"size:128;index"`
	TaskData       []byte
	TaskVersion    int64
	SettledAt      time.Time `gorm:"default:null"`
}

type CatalogArtifact struct {
	ID          int    `gorm:"primaryKey"`
	LookupKey   string `gorm:"size:64;uniqueIndex;not null"`
	PrincipalID string `gorm:"size:128;not null"`
	RequestID   string `gorm:"size:128;not null"`
	ArtifactID  string `gorm:"size:64;not null"`
	Checksum    string `gorm:"size:64;not null"`
	Size        int64
	MIME        string `gorm:"size:128"`
}

func ReserveCatalogExecution(db *gorm.DB, row *CatalogExecution) error {
	if row.Reserved < 0 || row.Reserved > common.MaxQuota {
		return errors.New("invalid catalog reservation")
	}
	err := db.Transaction(func(tx *gorm.DB) error {
		created := tx.Create(row)
		if created.Error != nil {
			return created.Error
		}
		if created.RowsAffected != 1 {
			return errors.New("catalog reservation already exists; observe without resending")
		}
		var token Token
		if err := lockForUpdate(tx).Where("id = ? AND user_id = ?", row.TokenID, row.UserID).Take(&token).Error; err != nil {
			return err
		}
		if token.Status != common.TokenStatusEnabled || (token.ExpiredTime != -1 && token.ExpiredTime <= time.Now().Unix()) {
			return errors.New("supplier token is not active")
		}
		wallet := tx.Model(&User{}).Where("id = ? AND status = ? AND quota >= ?", row.UserID, common.UserStatusEnabled, row.Reserved).UpdateColumn("quota", gorm.Expr("quota - ?", row.Reserved))
		if wallet.Error != nil {
			return wallet.Error
		}
		if wallet.RowsAffected != 1 {
			return errors.New("insufficient supplier wallet quota")
		}
		q := tx.Model(&Token{}).Where("id = ?", row.TokenID)
		if !token.UnlimitedQuota {
			q = q.Where("remain_quota >= ?", row.Reserved)
		}
		result := q.Updates(map[string]any{"remain_quota": gorm.Expr("remain_quota - ?", row.Reserved), "used_quota": gorm.Expr("used_quota + ?", row.Reserved)})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("insufficient supplier token quota")
		}
		return nil
	})
	if err == nil {
		_ = invalidateUserCache(row.UserID)
		var token Token
		if db.First(&token, row.TokenID).Error == nil {
			_ = invalidateTokenCacheForMutation(token.Key)
		}
	}
	return err
}

func SettleCatalogExecution(db *gorm.DB, quoteID string, actual int) error {
	if actual < 0 || actual > common.MaxQuota {
		return errors.New("invalid catalog settlement")
	}
	var row CatalogExecution
	var exceeded bool
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := lockForUpdate(tx).Where("quote_id = ?", quoteID).Take(&row).Error; err != nil {
			return err
		}
		if row.BillingState == "settled" {
			if row.Actual != actual {
				return errors.New("catalog settlement changed")
			}
			return nil
		}
		if row.BillingState != "reserved" {
			return ErrCatalogReconciliation
		}
		if actual > row.Reserved {
			exceeded = true
			return tx.Model(&row).Updates(map[string]any{"actual": actual, "billing_state": "reconciliation_required"}).Error
		}
		refund := row.Reserved - actual
		// All funding and token changes commit with the terminal journal entry.
		if err := tx.Model(&User{}).Where("id = ?", row.UserID).UpdateColumn("quota", gorm.Expr("quota + ?", refund)).Error; err != nil {
			return err
		}
		if err := tx.Model(&Token{}).Where("id = ?", row.TokenID).Updates(map[string]any{"remain_quota": gorm.Expr("remain_quota + ?", refund), "used_quota": gorm.Expr("used_quota - ?", refund)}).Error; err != nil {
			return err
		}
		return tx.Model(&row).Where("billing_state = ?", "reserved").Updates(map[string]any{"actual": actual, "billing_state": "settled", "settled_at": time.Now().UTC()}).Error
	})
	if err == nil {
		_ = invalidateUserCache(row.UserID)
		var token Token
		if db.First(&token, row.TokenID).Error == nil {
			_ = invalidateTokenCacheForMutation(token.Key)
		}
	}
	if exceeded {
		return ErrCatalogReconciliation
	}
	return err
}
