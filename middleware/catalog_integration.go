package middleware

import (
	"context"
	"errors"
	"net"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	contract "github.com/QuantumNous/new-api/pkg/catalogcontract"
	"github.com/QuantumNous/new-api/service"
	integration "github.com/QuantumNous/new-api/service/catalogintegration"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// CatalogIntegration is opt-in while rollout admission is incomplete. It runs
// before ordinary relay auth so exhausted balances cannot prevent recovery.
// No production adapter is registered until its bound and native settlement
// have passed admission; account models then remain visible with no operations.
func CatalogIntegration(factories ...func(*gin.Context, *model.Token) (*integration.Engine, error)) gin.HandlerFunc {
	return func(ctx *gin.Context) {
		path := ctx.Request.URL.Path
		isControl := path == "/v1/catalog" || path == "/v1/quotes" || strings.HasPrefix(path, "/v1/invocations/")
		key := relayTokenParts(ctx)[0]
		if key == "" || len(key) > 128 {
			if isControl {
				integration.WriteError(ctx, contract.Invalid("UNAUTHORIZED", "invalid integration credential", "provide a current API token"))
			}
			return
		}
		token := &model.Token{}
		err := model.DB.Where(&model.Token{Key: key}).Take(token).Error
		if err == nil && token.Key != key {
			err = gorm.ErrRecordNotFound
		}
		if err != nil {
			if isControl || !errors.Is(err, gorm.ErrRecordNotFound) {
				integration.WriteError(ctx, contract.Invalid("UNAUTHORIZED", "invalid integration credential", "provide a current API token"))
			}
			return
		}
		var binding model.CatalogIntegrationBinding
		err = model.DB.Where("token_id = ?", token.Id).Take(&binding).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			if isControl {
				integration.WriteError(ctx, contract.Invalid("ACCESS_DENIED", "token is not registered for this integration", "configure a server-owned integration binding"))
			}
			return
		}
		if err != nil {
			integration.WriteError(ctx, err)
			return
		}
		principal, err := catalogPrincipal(ctx, token, binding)
		if err != nil {
			integration.WriteError(ctx, err)
			return
		}
		engine := &integration.Engine{DB: model.DB, SourceID: "dantoken"}
		if len(factories) > 0 {
			engine, err = factories[0](ctx, token)
			if err != nil {
				integration.WriteError(ctx, err)
				return
			}
		}
		engine.Handle(ctx, principal)
	}
}

func catalogPrincipal(ctx *gin.Context, t *model.Token, b model.CatalogIntegrationBinding) (integration.Principal, error) {
	deny := func() (integration.Principal, error) {
		return integration.Principal{}, contract.Invalid("ACCESS_DENIED", "integration access is denied", "check the current credential and integration authorization")
	}
	if !b.Enabled || b.UserID != t.UserId || b.PrincipalID == "" || len(b.PrincipalID) > 128 || (t.Status != common.TokenStatusEnabled && t.Status != common.TokenStatusExhausted) || (t.ExpiredTime != -1 && t.ExpiredTime <= time.Now().Unix()) {
		return deny()
	}
	// A rotated credential may share a principal only within the same account.
	// Fail closed on conflicting administrative bindings instead of allowing a
	// misconfiguration to turn invocation IDs into cross-account read access.
	var conflictingBindings int64
	if err := model.DB.Model(&model.CatalogIntegrationBinding{}).Where("principal_id = ? AND user_id <> ?", b.PrincipalID, b.UserID).Count(&conflictingBindings).Error; err != nil {
		return integration.Principal{}, err
	}
	if conflictingBindings != 0 {
		return deny()
	}
	ips := t.GetIpLimits()
	if len(ips) > 0 {
		ip := net.ParseIP(ctx.ClientIP())
		if ip == nil || !common.IsIpInCIDRList(ip, ips) {
			return deny()
		}
	}
	user, err := model.GetUserById(t.UserId, false)
	if err != nil {
		return integration.Principal{}, err
	}
	if user.Status != common.UserStatusEnabled {
		return deny()
	}
	p := integration.Principal{ID: b.PrincipalID}
	p.CanSubmit = func(context.Context) error {
		if os.Getenv("DANTOKEN_CATALOG_ENABLED") != "true" {
			return contract.Invalid("INTEGRATION_DISABLED", "new integration submissions are disabled", "recover existing invocations; activate only after admission")
		}
		if t.Status != common.TokenStatusEnabled || (!t.UnlimitedQuota && t.RemainQuota <= 0) || user.Quota <= 0 {
			return contract.Invalid("INSUFFICIENT_POINTS", "new submissions require available supplier quota", "restore quota; existing invocations remain readable")
		}
		return nil
	}
	p.ResolveScope = func(callCtx context.Context) (integration.Scope, error) {
		return resolveCatalogScope(ctx, model.DB, t.Id)
	}
	p.CatalogScope = func(callCtx context.Context, tx *gorm.DB) (integration.Scope, error) {
		if err := model.LockCatalogAcceptance(tx, t.Id); err != nil {
			return integration.Scope{}, err
		}
		return resolveCatalogScope(ctx, tx, t.Id)
	}
	p.AcceptanceScope = func(callCtx context.Context, tx *gorm.DB) (integration.Scope, error) {
		if err := model.LockCatalogAcceptance(tx, t.Id); err != nil {
			return integration.Scope{}, err
		}
		scope, err := resolveCatalogScope(ctx, tx, t.Id)
		if err != nil {
			return scope, err
		}
		var fresh model.Token
		if err = tx.Where("id = ?", t.Id).Take(&fresh).Error; err != nil {
			return scope, err
		}
		var owner model.User
		if err = tx.Where("id = ?", fresh.UserId).Take(&owner).Error; err != nil {
			return scope, err
		}
		if os.Getenv("DANTOKEN_CATALOG_ENABLED") != "true" {
			return scope, contract.Invalid("INTEGRATION_DISABLED", "new submissions disabled", "recover existing invocations")
		}
		if fresh.Status != common.TokenStatusEnabled || (!fresh.UnlimitedQuota && fresh.RemainQuota <= 0) || owner.Quota <= 0 {
			return scope, contract.Invalid("INSUFFICIENT_POINTS", "supplier quota unavailable", "restore quota")
		}
		return scope, nil
	}
	return p, nil
}

func resolveCatalogScope(ctx *gin.Context, db *gorm.DB, tokenID int) (integration.Scope, error) {
	var scope integration.Scope
	var t model.Token
	var b model.CatalogIntegrationBinding
	if err := db.Where("id = ?", tokenID).Take(&t).Error; err != nil {
		return scope, err
	}
	if err := db.Where("token_id = ?", tokenID).Take(&b).Error; err != nil {
		return scope, err
	}
	var user model.User
	if err := db.Where("id = ?", t.UserId).Take(&user).Error; err != nil {
		return scope, err
	}
	if !b.Enabled || b.UserID != t.UserId || b.PrincipalID == "" || user.Status != common.UserStatusEnabled || (t.Status != common.TokenStatusEnabled && t.Status != common.TokenStatusExhausted) || (t.ExpiredTime != -1 && t.ExpiredTime <= time.Now().Unix()) {
		return scope, contract.Invalid("ACCESS_DENIED", "integration access denied", "restore a current authorized credential")
	}
	if ips := t.GetIpLimits(); len(ips) > 0 && !common.IsIpInCIDRList(net.ParseIP(ctx.ClientIP()), ips) {
		return scope, contract.Invalid("ACCESS_DENIED", "integration access denied", "check current credential policy")
	}
	group := t.Group
	if group == "" {
		group = user.Group
	}
	if group == "" || group == "auto" || group != b.FixedGroup {
		return scope, contract.Invalid("SCOPE_CHANGED", "fixed integration group changed", "explicitly rebind this integration")
	}
	if _, ok := service.GetUserUsableGroups(user.Group)[group]; !ok {
		return scope, contract.Invalid("ACCESS_DENIED", "group no longer authorized", "restore authorization")
	}
	if !ratio_setting.ContainsGroupRatio(group) {
		return scope, contract.Invalid("SCOPE_CHANGED", "group no longer exists", "review the binding")
	}
	var bindings []model.CatalogIntegrationBinding
	if err := db.Where("principal_id = ?", b.PrincipalID).Find(&bindings).Error; err != nil {
		return scope, err
	}
	for _, other := range bindings {
		if other.UserID != b.UserID {
			return scope, contract.Invalid("ACCESS_DENIED", "principal belongs to another account", "correct administrative binding")
		}
	}
	limits := []string{}
	allowed := t.GetModelLimitsMap()
	if t.ModelLimitsEnabled {
		for name := range allowed {
			limits = append(limits, name)
		}
		slices.Sort(limits)
	}
	policy := b.ModelPolicy
	if policy == "" {
		policy = "all_in_group"
	}
	if policy != "all_in_group" && policy != "allowlist" {
		return scope, contract.Invalid("ACCESS_DENIED", "invalid explicit policy", "correct the integration policy")
	}
	policyModels := []string{}
	var policyOps []string
	if b.AllowedModels != "" && common.UnmarshalJsonStr(b.AllowedModels, &policyModels) != nil {
		return scope, contract.Invalid("ACCESS_DENIED", "invalid model policy", "correct the integration policy")
	}
	if b.AllowedOperations != "" && common.UnmarshalJsonStr(b.AllowedOperations, &policyOps) != nil {
		return scope, contract.Invalid("ACCESS_DENIED", "invalid operation policy", "correct the integration policy")
	}
	slices.Sort(policyModels)
	policyModels = slices.Compact(policyModels)
	slices.Sort(policyOps)
	policyOps = slices.Compact(policyOps)
	allowsModel := func(name string) bool {
		return (policy == "all_in_group" || slices.Contains(policyModels, name)) && (!t.ModelLimitsEnabled || allowed[name] || allowed[ratio_setting.RoutingMatchModelName(name)])
	}
	allowsOperation := func(name string, op contract.Operation) bool {
		return allowsModel(name) && (policyOps == nil || slices.Contains(policyOps, string(op)))
	}
	// A rotated credential cannot quietly apply a different model rule under
	// the same logical binding. Disable the old credential or align the rules.
	for _, other := range bindings {
		if other.TokenID == t.Id || !other.Enabled || other.FixedGroup != group {
			continue
		}
		var peer model.Token
		if err := db.Where("id = ?", other.TokenID).Take(&peer).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				continue
			}
			return scope, err
		}
		if (peer.Status != common.TokenStatusEnabled && peer.Status != common.TokenStatusExhausted) || (peer.ExpiredTime != -1 && peer.ExpiredTime <= time.Now().Unix()) {
			continue
		}
		peerLimits := []string{}
		if peer.ModelLimitsEnabled {
			for name := range peer.GetModelLimitsMap() {
				peerLimits = append(peerLimits, name)
			}
			slices.Sort(peerLimits)
		}
		if peer.ModelLimitsEnabled != t.ModelLimitsEnabled || !slices.Equal(peerLimits, limits) || (other.ModelPolicy != "" && other.ModelPolicy != policy) || other.AllowedModels != b.AllowedModels || other.AllowedOperations != b.AllowedOperations {
			return scope, contract.Invalid("ACCESS_DENIED", "rotated credentials have inconsistent policies", "align the logical binding policy before rotation")
		}
	}
	// Explicit rules, never the currently supplied intersection.
	ruleHash, err := contract.Hash(struct {
		Limited                                     bool
		Models                                      []string
		UserGroup, FixedGroup, RoutingRules, Policy string
		PolicyModels, Operations                    []string
	}{t.ModelLimitsEnabled, limits, user.Group, group, "routing-match-v1", policy, policyModels, policyOps})
	if err != nil {
		return scope, err
	}
	var abilities []model.Ability
	if err = db.Where(&model.Ability{Group: group}).Where("enabled = ?", true).Find(&abilities).Error; err != nil {
		return scope, err
	}
	names, supply := []string{}, []string{}
	for _, a := range abilities {
		supply = append(supply, a.Model)
		if allowsModel(a.Model) {
			names = append(names, a.Model)
		}
	}
	slices.Sort(names)
	names = slices.Compact(names)
	slices.Sort(supply)
	supply = slices.Compact(supply)
	scopeID, err := contract.Hash(struct {
		Source, Principal, Group string
		Account                  int
	}{"dantoken", b.PrincipalID, group, b.UserID})
	if err != nil {
		return scope, err
	}
	return integration.Scope{PrincipalID: b.PrincipalID, Group: group, ScopeID: scopeID, Models: names, SupplyModels: supply, AuthorizationHash: ruleHash, BindingVersion: model.CatalogKey("binding-v2", scopeID), AllowsModel: allowsModel, AllowsOperation: allowsOperation}, nil
}
