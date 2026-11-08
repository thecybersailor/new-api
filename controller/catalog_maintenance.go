package controller

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	c "github.com/QuantumNous/new-api/pkg/catalogcontract"
	integration "github.com/QuantumNous/new-api/service/catalogintegration"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type catalogMaintenanceIssue struct {
	Model     string      `json:"model"`
	Operation c.Operation `json:"operation"`
	Field     string      `json:"field"`
	Message   string      `json:"message"`
}
type catalogMaintenanceRequest struct {
	ActivateDatabase  bool                            `json:"activateDatabase"`
	ManifestRevision  string                          `json:"manifestRevision"`
	DraftRevision     int64                           `json:"draftRevision"`
	PublishedRevision int64                           `json:"publishedRevision"`
	Entries           []model.CatalogMaintenanceEntry `json:"entries"`
	Selections        []catalogSelection              `json:"selections"`
	Reason            string                          `json:"reason"`
}
type catalogSelection struct {
	Model     string      `json:"model"`
	Operation c.Operation `json:"operation"`
}

func catalogMaintenanceError(ctx *gin.Context, err error) {
	status := http.StatusBadRequest
	code := "CATALOG_INVALID"
	message := err.Error()
	var known *c.Error
	if errors.As(err, &known) {
		code = known.Code
		if code == "CATALOG_CONFLICT" {
			status = http.StatusConflict
		}
	}
	ctx.JSON(status, gin.H{"success": false, "code": code, "message": message})
}
func catalogMaintenanceBody(ctx *gin.Context) (catalogMaintenanceRequest, bool) {
	var r catalogMaintenanceRequest
	ctx.Request.Body = http.MaxBytesReader(ctx.Writer, ctx.Request.Body, 2<<20)
	if err := ctx.ShouldBindJSON(&r); err != nil {
		catalogMaintenanceError(ctx, fmt.Errorf("invalid bounded catalog request"))
		return r, false
	}
	if len(r.Reason) > 1000 || len(r.Selections) > 500 || r.DraftRevision < 0 || r.PublishedRevision < 0 {
		catalogMaintenanceError(ctx, fmt.Errorf("invalid revision, selection or change reason"))
		return r, false
	}
	return r, true
}
func catalogDirectoryEntries(raw []byte) ([]model.CatalogMaintenanceEntry, error) {
	entries := []model.CatalogMaintenanceEntry{}
	err := common.Unmarshal(raw, &entries)
	return entries, err
}

func GetCatalogMaintenance(ctx *gin.Context) {
	directory, err := model.ReadCatalogDirectory(model.DB)
	if err != nil {
		catalogMaintenanceError(ctx, err)
		return
	}
	entries, err := catalogDirectoryEntries(directory.Draft)
	if err != nil {
		catalogMaintenanceError(ctx, err)
		return
	}
	published, err := catalogDirectoryEntries(directory.Published)
	if err != nil {
		catalogMaintenanceError(ctx, err)
		return
	}
	var audit []model.CatalogDirectoryAudit
	if err = model.DB.Order("id DESC").Limit(50).Find(&audit).Error; err != nil {
		catalogMaintenanceError(ctx, err)
		return
	}
	issues := validateCatalogMaintenance(entries)
	manifest := []model.CatalogMaintenanceEntry{}
	manifestRevision := ""
	var manifestErr error
	if !directory.DatabaseEnabled {
		manifest, manifestRevision, manifestErr = catalogLegacyManifest()
	}
	manifestError := ""
	if manifestErr != nil {
		manifestError = "Server manifest is invalid; fix it before database activation"
	}
	for _, entry := range published {
		for _, op := range entry.Operations {
			if op.State != "published" {
				continue
			}
			var config CatalogRelayOperation
			if common.Unmarshal(op.Config, &config) != nil {
				continue
			}
			fingerprint, checkErr := catalogDependencyFingerprint(model.DB, config)
			if checkErr != nil || fingerprint != op.DependencyHash {
				issues = append(issues, catalogMaintenanceIssue{entry.Model, op.Operation, "dependencies", "Published dependencies changed; review and republish this operation"})
			}
		}
	}
	ctx.JSON(200, gin.H{"success": true, "data": gin.H{"draftRevision": directory.DraftRevision, "publishedRevision": directory.PublishedRevision, "databaseEnabled": directory.DatabaseEnabled, "entries": entries, "published": published, "issues": issues, "audit": audit, "baseline": catalogCandidateEntries(), "manifestEntries": manifest, "manifestRevision": manifestRevision, "manifestError": manifestError}})
}

func SaveCatalogMaintenanceDraft(ctx *gin.Context) {
	r, ok := catalogMaintenanceBody(ctx)
	if !ok {
		return
	}
	if err := normalizeCatalogMaintenance(r.Entries); err != nil {
		catalogMaintenanceError(ctx, err)
		return
	}
	directory, err := model.ReadCatalogDirectory(model.DB)
	if err != nil {
		catalogMaintenanceError(ctx, err)
		return
	}
	if directory.DraftRevision != r.DraftRevision || directory.PublishedRevision != r.PublishedRevision {
		catalogMaintenanceError(ctx, c.Invalid("CATALOG_CONFLICT", "directory changed", "refresh before saving"))
		return
	}
	published, err := catalogDirectoryEntries(directory.Published)
	if err != nil {
		catalogMaintenanceError(ctx, err)
		return
	}
	// Published identities remain represented in the maintenance workflow even
	// after retirement. Only never-published candidates may be deleted.
	for _, old := range published {
		for _, oldOp := range old.Operations {
			found := false
			for _, entry := range r.Entries {
				if entry.Model == old.Model {
					for _, op := range entry.Operations {
						if op.Operation == oldOp.Operation {
							found = true
						}
					}
				}
			}
			if !found {
				catalogMaintenanceError(ctx, fmt.Errorf("published operation %s/%s must be retired, not deleted", old.Model, oldOp.Operation))
				return
			}
		}
	}
	raw, err := common.Marshal(r.Entries)
	if err != nil {
		catalogMaintenanceError(ctx, err)
		return
	}
	err = model.SaveCatalogDirectory(model.DB, directory, raw, directory.Published, "save_draft", r.Reason, ctx.GetInt("id"), false)
	if err != nil {
		catalogMaintenanceError(ctx, err)
		return
	}
	ctx.JSON(200, gin.H{"success": true})
}

func normalizeCatalogMaintenance(entries []model.CatalogMaintenanceEntry) error {
	if entries == nil || len(entries) > 500 {
		return fmt.Errorf("provide an array of at most 500 models")
	}
	seen := map[string]bool{}
	for i := range entries {
		entry := &entries[i]
		if strings.TrimSpace(entry.Model) == "" || len(entry.Model) > 256 || len(entry.DisplayName) > 256 || len(entry.Owner) > 200 || len(entry.Operations) > 8 || seen[entry.Model] {
			return fmt.Errorf("invalid or duplicate model identity")
		}
		seen[entry.Model] = true
		if entry.DisplayName == "" {
			entry.DisplayName = entry.Model
		}
		ops := map[c.Operation]bool{}
		for j := range entry.Operations {
			op := &entry.Operations[j]
			if !op.Operation.Valid() || ops[op.Operation] || len(op.Evidence) > 4000 {
				return fmt.Errorf("invalid or duplicate operation")
			}
			ops[op.Operation] = true
			var config CatalogRelayOperation
			if len(op.Config) == 0 {
				op.Config = []byte("{}")
			}
			if err := common.Unmarshal(op.Config, &config); err != nil {
				return fmt.Errorf("invalid operation configuration")
			}
			config.Model = entry.Model
			config.Definition.Operation = op.Operation
			config.DependencyHash = ""
			raw, err := common.Marshal(config)
			if err != nil {
				return err
			}
			// Only the typed configuration is persisted: no credential/script
			// or arbitrary unknown manifest fields can survive an import.
			op.Config = raw
			op.DependencyHash = ""
			op.State = "draft"
		}
		slices.SortFunc(entry.Operations, func(a, b model.CatalogMaintenanceOperation) int {
			return strings.Compare(string(a.Operation), string(b.Operation))
		})
	}
	slices.SortFunc(entries, func(a, b model.CatalogMaintenanceEntry) int { return strings.Compare(a.Model, b.Model) })
	return nil
}

func validateCatalogMaintenance(entries []model.CatalogMaintenanceEntry) []catalogMaintenanceIssue {
	issues := []catalogMaintenanceIssue{}
	for _, entry := range entries {
		for _, op := range entry.Operations {
			if entry.Owner == "" {
				issues = append(issues, catalogMaintenanceIssue{entry.Model, op.Operation, "owner", "Assign a configuration owner"})
			}
			if strings.TrimSpace(op.Evidence) == "" {
				issues = append(issues, catalogMaintenanceIssue{entry.Model, op.Operation, "evidence", "Provide the capability, limit and pricing evidence reference"})
			}
			var config CatalogRelayOperation
			if err := common.Unmarshal(op.Config, &config); err != nil {
				issues = append(issues, catalogMaintenanceIssue{entry.Model, op.Operation, "config", "Invalid configuration JSON"})
				continue
			}
			engine := &integration.Engine{ArtifactRoot: os.Getenv("DANTOKEN_ARTIFACT_DIR")}
			if err := registerCatalogRelayOperations(engine, &model.Token{}, []CatalogRelayOperation{config}); err != nil {
				issues = append(issues, catalogMaintenanceIssue{entry.Model, op.Operation, "config", err.Error()})
				continue
			}
			if _, err := catalogDependencyFingerprint(model.DB, config); err != nil {
				issues = append(issues, catalogMaintenanceIssue{entry.Model, op.Operation, "channelId", err.Error()})
			}
		}
	}
	return issues
}

// No upstream I/O: this freezes the dependencies of already configured trusted
// adapters. Actual model evidence remains the consumer's separate admission.
func catalogDependencyFingerprint(db *gorm.DB, config CatalogRelayOperation) (string, error) {
	var channel model.Channel
	if err := db.Where("id = ?", config.ChannelID).Take(&channel).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", c.Invalid("OPERATION_NOT_ADMITTED", "configured channel not found", "restore and republish the operation")
		}
		return "", err
	}
	route, err := catalogChannelFingerprint(&channel)
	if err != nil {
		return "", err
	}
	if channel.Status != common.ChannelStatusEnabled {
		return "", c.Invalid("OPERATION_NOT_ADMITTED", "configured channel is disabled", "restore and republish the operation")
	}
	var abilities []model.Ability
	if err = db.Where("channel_id = ? AND model = ? AND enabled = ?", config.ChannelID, config.Model, true).Find(&abilities).Error; err != nil {
		return "", err
	}
	if len(abilities) == 0 {
		return "", c.Invalid("OPERATION_NOT_ADMITTED", "channel does not supply this exact model", "restore and republish the operation")
	}
	prices, err := model.GetModelPricingSnapshotWithDB(db, []string{config.Model})
	if err != nil {
		return "", err
	}
	return c.Hash(struct {
		Route               string
		Prices              []model.ModelPricingEntry
		Groups, GroupGroups string
		Unit                float64
	}{route, prices.Entries, ratio_setting.GroupRatio2JSONString(), ratio_setting.GroupGroupRatio2JSONString(), common.QuotaPerUnit})
}

func PreviewCatalogMaintenance(ctx *gin.Context) {
	r, ok := catalogMaintenanceBody(ctx)
	if !ok {
		return
	}
	directory, err := model.ReadCatalogDirectory(model.DB)
	if err != nil {
		catalogMaintenanceError(ctx, err)
		return
	}
	if directory.DraftRevision != r.DraftRevision || directory.PublishedRevision != r.PublishedRevision {
		catalogMaintenanceError(ctx, c.Invalid("CATALOG_CONFLICT", "directory changed", "refresh before preview"))
		return
	}
	entries, err := catalogDirectoryEntries(directory.Draft)
	if err != nil {
		catalogMaintenanceError(ctx, err)
		return
	}
	published, err := catalogDirectoryEntries(directory.Published)
	if err != nil {
		catalogMaintenanceError(ctx, err)
		return
	}
	ctx.JSON(200, gin.H{"success": true, "data": gin.H{"draftRevision": directory.DraftRevision, "publishedRevision": directory.PublishedRevision, "entries": entries, "published": published, "issues": validateCatalogMaintenance(entries)}})
}

func PublishCatalogMaintenance(ctx *gin.Context) { changeCatalogPublication(ctx, false) }
func RetireCatalogMaintenance(ctx *gin.Context)  { changeCatalogPublication(ctx, true) }

func changeCatalogPublication(ctx *gin.Context, retire bool) {
	r, ok := catalogMaintenanceBody(ctx)
	if !ok {
		return
	}
	if len(r.Selections) == 0 {
		catalogMaintenanceError(ctx, fmt.Errorf("select explicit model operations"))
		return
	}
	directory, err := model.ReadCatalogDirectory(model.DB)
	if err != nil {
		catalogMaintenanceError(ctx, err)
		return
	}
	if directory.DraftRevision != r.DraftRevision || directory.PublishedRevision != r.PublishedRevision {
		catalogMaintenanceError(ctx, c.Invalid("CATALOG_CONFLICT", "directory changed", "refresh the preview"))
		return
	}
	entries, err := catalogDirectoryEntries(directory.Draft)
	if err != nil {
		catalogMaintenanceError(ctx, err)
		return
	}
	published, err := catalogDirectoryEntries(directory.Published)
	if err != nil {
		catalogMaintenanceError(ctx, err)
		return
	}
	if !directory.DatabaseEnabled {
		legacy, revision, er := catalogLegacyManifest()
		if er != nil {
			catalogMaintenanceError(ctx, fmt.Errorf("server manifest must be valid before migration"))
			return
		}
		if !r.ActivateDatabase || revision != r.ManifestRevision {
			catalogMaintenanceError(ctx, c.Invalid("CATALOG_CONFLICT", "review database activation and current server manifest", "refresh the publication preview"))
			return
		}
		for _, entry := range legacy {
			for _, op := range entry.Operations {
				if !slices.ContainsFunc(r.Selections, func(s catalogSelection) bool { return s.Model == entry.Model && s.Operation == op.Operation }) {
					catalogMaintenanceError(ctx, fmt.Errorf("first database publication must explicitly review every legacy operation, including %s/%s", entry.Model, op.Operation))
					return
				}
			}
		}
	}
	selected := map[string]bool{}
	for _, selection := range r.Selections {
		key := selection.Model + "/" + string(selection.Operation)
		if selected[key] || !selection.Operation.Valid() {
			catalogMaintenanceError(ctx, fmt.Errorf("invalid selection"))
			return
		}
		selected[key] = true
	}
	for i := range entries {
		entry := &entries[i]
		for j := range entry.Operations {
			op := &entry.Operations[j]
			key := entry.Model + "/" + string(op.Operation)
			if !selected[key] {
				continue
			}
			delete(selected, key)
			if retire {
				op.State = "retired"
			} else {
				issues := validateCatalogMaintenance([]model.CatalogMaintenanceEntry{{Model: entry.Model, Owner: entry.Owner, Operations: []model.CatalogMaintenanceOperation{*op}}})
				if len(issues) > 0 {
					ctx.JSON(422, gin.H{"success": false, "message": "Resolve selected operation issues before publishing", "issues": issues})
					return
				}
				var config CatalogRelayOperation
				if err = common.Unmarshal(op.Config, &config); err != nil {
					catalogMaintenanceError(ctx, err)
					return
				}
				op.DependencyHash, err = catalogDependencyFingerprint(model.DB, config)
				if err != nil {
					catalogMaintenanceError(ctx, err)
					return
				}
				op.State = "published"
			}
			pi := -1
			for n := range published {
				if published[n].Model == entry.Model {
					pi = n
					break
				}
			}
			if pi < 0 {
				published = append(published, model.CatalogMaintenanceEntry{Model: entry.Model, DisplayName: entry.DisplayName, Owner: entry.Owner, Operations: []model.CatalogMaintenanceOperation{}})
				pi = len(published) - 1
			}
			published[pi].DisplayName = entry.DisplayName
			published[pi].Owner = entry.Owner
			oi := -1
			for n := range published[pi].Operations {
				if published[pi].Operations[n].Operation == op.Operation {
					oi = n
					break
				}
			}
			if oi < 0 {
				published[pi].Operations = append(published[pi].Operations, *op)
			} else {
				published[pi].Operations[oi] = *op
			}
		}
	}
	if len(selected) > 0 {
		catalogMaintenanceError(ctx, fmt.Errorf("selected operation does not exist in this draft"))
		return
	}
	raw, err := common.Marshal(entries)
	if err != nil {
		catalogMaintenanceError(ctx, err)
		return
	}
	live, err := common.Marshal(published)
	if err != nil {
		catalogMaintenanceError(ctx, err)
		return
	}
	action := "publish"
	if retire {
		action = "retire"
	}
	err = model.SaveCatalogDirectory(model.DB, directory, raw, live, action, r.Reason, ctx.GetInt("id"), true)
	if err != nil {
		catalogMaintenanceError(ctx, err)
		return
	}
	ctx.JSON(200, gin.H{"success": true})
}

func GetCatalogBindings(ctx *gin.Context) {
	var rows []model.CatalogIntegrationBinding
	if err := model.DB.Order("id ASC").Find(&rows).Error; err != nil {
		catalogMaintenanceError(ctx, err)
		return
	}
	for i := range rows {
		rows[i].ScopeID, _ = c.Hash(struct {
			Source, Principal, Group string
			Account                  int
		}{"dantoken", rows[i].PrincipalID, rows[i].FixedGroup, rows[i].UserID})
		rows[i].BindingVersion = model.CatalogKey("binding-v2", rows[i].ScopeID)
	}
	ctx.JSON(200, gin.H{"success": true, "data": rows})
}
func SaveCatalogBinding(ctx *gin.Context) {
	var r struct {
		Binding model.CatalogIntegrationBinding `json:"binding"`
		Reason  string                          `json:"reason"`
	}
	ctx.Request.Body = http.MaxBytesReader(ctx.Writer, ctx.Request.Body, 64<<10)
	if ctx.ShouldBindJSON(&r) != nil || r.Binding.TokenID <= 0 || r.Binding.UserID <= 0 || len(r.Binding.PrincipalID) == 0 || len(r.Binding.PrincipalID) > 128 || len(r.Binding.FixedGroup) == 0 || len(r.Binding.FixedGroup) > 128 || strings.TrimSpace(r.Reason) == "" || len(r.Reason) > 1000 {
		catalogMaintenanceError(ctx, fmt.Errorf("invalid binding and change reason"))
		return
	}
	if r.Binding.ModelPolicy != "all_in_group" && r.Binding.ModelPolicy != "allowlist" {
		catalogMaintenanceError(ctx, fmt.Errorf("select an explicit authorization policy"))
		return
	}
	var names, operations []string
	if common.UnmarshalJsonStr(r.Binding.AllowedModels, &names) != nil || common.UnmarshalJsonStr(r.Binding.AllowedOperations, &operations) != nil || names == nil || operations == nil || len(names) > 500 || len(operations) > 8 {
		catalogMaintenanceError(ctx, fmt.Errorf("allowed models and operations must be bounded JSON arrays"))
		return
	}
	for _, name := range names {
		if strings.TrimSpace(name) == "" || len(name) > 256 {
			catalogMaintenanceError(ctx, fmt.Errorf("invalid allowed model"))
			return
		}
	}
	for _, op := range operations {
		if !c.Operation(op).Valid() {
			catalogMaintenanceError(ctx, fmt.Errorf("invalid allowed operation"))
			return
		}
	}
	slices.Sort(names)
	names = slices.Compact(names)
	slices.Sort(operations)
	operations = slices.Compact(operations)
	raw, _ := common.Marshal(names)
	r.Binding.AllowedModels = string(raw)
	raw, _ = common.Marshal(operations)
	r.Binding.AllowedOperations = string(raw)
	if err := model.SaveCatalogBinding(model.DB, r.Binding, r.Binding.Revision, ctx.GetInt("id"), r.Reason); err != nil {
		catalogMaintenanceError(ctx, err)
		return
	}
	ctx.JSON(200, gin.H{"success": true})
}

func catalogLegacyManifest() ([]model.CatalogMaintenanceEntry, string, error) {
	entries := []model.CatalogMaintenanceEntry{}
	path := os.Getenv("DANTOKEN_CATALOG_OPERATIONS_FILE")
	if path == "" {
		return entries, "", nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	if len(raw) > 2<<20 {
		return nil, "", fmt.Errorf("manifest too large")
	}
	var configs []CatalogRelayOperation
	if err = common.Unmarshal(raw, &configs); err != nil {
		return nil, "", err
	}
	revision, err := c.Hash(configs)
	if err != nil {
		return nil, "", err
	}
	for _, config := range configs {
		raw, err = common.Marshal(config)
		if err != nil {
			return nil, "", err
		}
		index := -1
		for i := range entries {
			if entries[i].Model == config.Model {
				index = i
				break
			}
		}
		if index < 0 {
			entries = append(entries, model.CatalogMaintenanceEntry{Model: config.Model, DisplayName: config.Model, Operations: []model.CatalogMaintenanceOperation{}})
			index = len(entries) - 1
		}
		entries[index].Operations = append(entries[index].Operations, model.CatalogMaintenanceOperation{Operation: config.Definition.Operation, Config: raw, State: "draft"})
	}
	return entries, revision, nil
}
