package catalogcontract

import "strings"

// CatalogContentDigest hashes the ordered complete model collection, not a page.
// Empty collections must be encoded as [] rather than null on both peers.
func CatalogContentDigest(models []CatalogModel) (string, error) {
	if models == nil {
		models = []CatalogModel{}
	}
	return Hash(models)
}

func (s CatalogSnapshot) validateV2() error {
	bad := func(message string) error {
		return Invalid("CATALOG_SNAPSHOT_INVALID", message, "fetch a complete v2 snapshot with matching count, digest and published revisions")
	}
	if strings.TrimSpace(s.BindingVersion) == "" || s.AuthorizationRevision <= 0 || s.CatalogRevision <= 0 {
		return bad("binding and positive authorization/catalog revisions required")
	}
	if s.TotalCount != len(s.Models) {
		return bad("complete catalog count differs")
	}
	digest, err := CatalogContentDigest(s.Models)
	if err != nil {
		return err
	}
	if s.ContentDigest != digest {
		return bad("complete catalog digest differs")
	}
	if len(s.Models) == 0 {
		if s.EmptyReason != "supply_empty" && s.EmptyReason != "authorization_empty" {
			return bad("authoritative empty catalog requires a reason")
		}
	} else if s.EmptyReason != "" {
		return bad("nonempty catalog cannot declare an empty reason")
	}
	seen := map[string]bool{}
	for _, removal := range s.Removals {
		key := removal.ModelID + "/" + string(removal.Operation)
		if removal.ModelID == "" || (removal.Operation != "" && !removal.Operation.Valid()) || seen[key] || len(removal.Reasons) == 0 {
			return bad("invalid or duplicate removal")
		}
		seen[key] = true
		reasons := map[string]bool{}
		for _, reason := range removal.Reasons {
			if (reason != "supply_retired" && reason != "operation_replaced" && reason != "authorization_removed") || reasons[reason] {
				return bad("invalid or duplicate removal reason")
			}
			reasons[reason] = true
		}
		for _, m := range s.Models {
			if m.ID == removal.ModelID && m.SupplyState == "active" {
				for _, op := range m.Operations {
					if (removal.Operation == "" || removal.Operation == op.Operation) && !(removal.Operation != "" && len(removal.Reasons) == 1 && removal.Reasons[0] == "operation_replaced") {
						return bad("removed operation remains executable in snapshot")
					}
				}
			}
		}
	}
	return nil
}

// SameCatalogPageIdentity prevents combining independently authorized snapshots.
func SameCatalogPageIdentity(a, b CatalogSnapshot) bool {
	x, _ := Hash(a.Removals)
	y, _ := Hash(b.Removals)
	return a.SourceID == b.SourceID && a.ScopeID == b.ScopeID && a.BindingVersion == b.BindingVersion && a.AuthorizationRevision == b.AuthorizationRevision && a.CatalogRevision == b.CatalogRevision && a.TotalCount == b.TotalCount && a.ContentDigest == b.ContentDigest && a.EmptyReason == b.EmptyReason && x == y && a.SnapshotID == b.SnapshotID && a.GeneratedAt.Equal(b.GeneratedAt) && a.ValidUntil.Equal(b.ValidUntil) && a.SchemaVersion == b.SchemaVersion
}
