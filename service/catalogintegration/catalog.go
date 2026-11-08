package catalogintegration

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	c "github.com/QuantumNous/new-api/pkg/catalogcontract"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func (e *Engine) Catalog(ctx context.Context, p Principal, snapshotID string) (c.CatalogSnapshot, error) {
	var out c.CatalogSnapshot
	if e.AdmissionError != nil {
		return out, e.AdmissionError
	}
	scope, err := p.ResolveScope(ctx)
	if err != nil {
		return out, err
	}
	if snapshotID != "" {
		var row model.CatalogStoredSnapshot
		if err = e.DB.Where("lookup_key = ?", model.CatalogKey(p.ID, snapshotID)).Take(&row).Error; err != nil {
			return out, err
		}
		if row.PrincipalID != p.ID || row.ScopeID != scope.ScopeID {
			return out, c.Invalid("SCOPE_CHANGED", "snapshot binding differs", "start a complete synchronization")
		}
		if err = common.Unmarshal(row.Data, &out); err != nil {
			return out, err
		}
		if out.SchemaVersion != 2 || row.AuthorizationHash != scope.AuthorizationHash {
			return out, c.Invalid("CATALOG_AUTHORIZATION_CHANGED", "snapshot authorization is no longer current", "restart synchronization with current permissions")
		}
		return out, out.Validate(e.now())
	}
	now := e.now()
	out = c.CatalogSnapshot{SchemaVersion: 2, SourceID: e.SourceID, ScopeID: scope.ScopeID, BindingVersion: scope.BindingVersion, GeneratedAt: now, ValidUntil: now.Add(24 * time.Hour), Complete: true, Models: []c.CatalogModel{}}
	if out.BindingVersion == "" {
		out.BindingVersion = model.CatalogKey("binding-v2", scope.ScopeID)
	}
	supply := scope.SupplyModels
	if supply == nil {
		supply = scope.Models
	}
	names := slices.Clone(supply)
	slices.Sort(names)
	names = slices.Compact(names)
	supplied := []c.CatalogModel{}
	for _, name := range names {
		m := c.CatalogModel{ID: name, DisplayName: name, SupplyState: "active", Operations: []c.OperationDefinition{}}
		if display := e.DisplayNames[name]; display != "" {
			m.DisplayName = display
		}
		for _, a := range e.Adapters {
			if a.Model != name {
				continue
			}
			def, er := a.Adapter.Definition(ctx, scope, name)
			if er != nil {
				var blocked *c.Error
				if errors.As(er, &blocked) && (blocked.Code == "OPERATION_NOT_ADMITTED" || blocked.Code == "QUOTE_BOUND_UNAVAILABLE") {
					continue
				}
				return out, er
			}
			if def.Operation != a.Operation {
				return out, c.Invalid("OPERATION_INVALID", "adapter operation differs", "correct the published configuration")
			}
			if er = def.Validate(); er != nil {
				return out, er
			}
			m.Operations = append(m.Operations, def)
		}
		slices.SortFunc(m.Operations, func(a, b c.OperationDefinition) int {
			if a.Operation < b.Operation {
				return -1
			}
			if a.Operation > b.Operation {
				return 1
			}
			return 0
		})
		if len(m.Operations) == 0 {
			continue
		}
		supplied = append(supplied, m)
		if slices.Contains(scope.Models, name) {
			visible := m
			visible.Operations = []c.OperationDefinition{}
			for _, op := range m.Operations {
				if scope.AllowsOperation == nil || scope.AllowsOperation(name, op.Operation) {
					visible.Operations = append(visible.Operations, op)
				}
			}
			if len(visible.Operations) > 0 {
				out.Models = append(out.Models, visible)
			}
		}
	}
	content, err := c.CatalogContentDigest(supplied)
	if err != nil {
		return out, err
	}
	out.TotalCount = len(out.Models)
	out.ContentDigest, err = c.CatalogContentDigest(out.Models)
	if err != nil {
		return out, err
	}
	if len(out.Models) == 0 {
		out.EmptyReason = "supply_empty"
		if len(supplied) > 0 {
			out.EmptyReason = "authorization_empty"
		}
	}
	err = e.DB.Transaction(func(tx *gorm.DB) error {
		var fresh Scope
		var er error
		if p.CatalogScope != nil {
			fresh, er = p.CatalogScope(ctx, tx)
		} else {
			fresh, er = p.ResolveScope(ctx)
		}
		if er != nil {
			return er
		}
		head, er := model.LockCatalogScopeVersion(tx, scope.ScopeID)
		if er != nil {
			return er
		}
		if fresh.ScopeID != scope.ScopeID || fresh.AuthorizationHash != scope.AuthorizationHash || !slices.Equal(fresh.Models, scope.Models) || !slices.Equal(fresh.SupplyModels, scope.SupplyModels) {
			return c.Invalid("CATALOG_AUTHORIZATION_CHANGED", "policy changed while reading", "restart synchronization")
		}
		if e.CheckDirectory != nil {
			if er = e.CheckDirectory(tx); er != nil {
				return er
			}
		}
		if e.SnapshotGate != nil {
			if er = e.SnapshotGate(ctx, tx, fresh, supplied); er != nil {
				return er
			}
		}
		out.AuthorizationRevision = head.AuthorizationRevision
		if out.AuthorizationRevision == 0 || head.AuthorizationHash != scope.AuthorizationHash {
			out.AuthorizationRevision++
		}
		out.CatalogRevision = head.CatalogRevision
		if out.CatalogRevision == 0 || head.CatalogHash != content {
			out.CatalogRevision++
		}
		var previous c.CatalogSnapshot
		if len(head.Snapshot) > 0 {
			if er = common.Unmarshal(head.Snapshot, &previous); er != nil {
				return er
			}
		}
		known := map[string]c.CatalogRemoval{}
		for _, r := range previous.Removals {
			known[r.ModelID+"/"+string(r.Operation)] = r
		}
		for _, m := range previous.Models {
			for _, op := range m.Operations {
				known[m.ID+"/"+string(op.Operation)] = c.CatalogRemoval{ModelID: m.ID, Operation: op.Operation}
			}
		}
		for key, r := range known {
			active := false
			for _, m := range out.Models {
				if m.ID == r.ModelID {
					for _, op := range m.Operations {
						if op.Operation == r.Operation {
							active = true
						}
					}
				}
			}
			if active {
				delete(known, key)
				continue
			}
			r.Reasons = []string{"supply_retired"}
			if (scope.AllowsModel != nil && !scope.AllowsModel(r.ModelID)) || (scope.AllowsOperation != nil && !scope.AllowsOperation(r.ModelID, r.Operation)) {
				r.Reasons = []string{"authorization_removed"}
			}
			known[key] = r
		}
		keys := make([]string, 0, len(known))
		for k := range known {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			out.Removals = append(out.Removals, known[k])
		}
		out.SnapshotID, er = c.Hash(out)
		if er != nil {
			return er
		}
		if er = out.Validate(now); er != nil {
			return er
		}
		raw, er := common.Marshal(out)
		if er != nil {
			return er
		}
		result := tx.Model(&head).Where("version = ?", head.Version).Updates(map[string]any{"authorization_hash": scope.AuthorizationHash, "authorization_revision": out.AuthorizationRevision, "catalog_hash": content, "catalog_revision": out.CatalogRevision, "snapshot": raw, "version": head.Version + 1})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return c.Invalid("CATALOG_CONFLICT", "concurrent synchronization", "retry the full snapshot")
		}
		row := model.CatalogStoredSnapshot{LookupKey: model.CatalogKey(p.ID, out.SnapshotID), PrincipalID: p.ID, ScopeID: scope.ScopeID, SnapshotID: out.SnapshotID, AuthorizationHash: scope.AuthorizationHash, ValidUntil: out.ValidUntil, Data: raw}
		return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error
	})
	return out, err
}
