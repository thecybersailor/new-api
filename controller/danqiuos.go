package controller

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/oauth"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
	"golang.org/x/oauth2"
	"gorm.io/gorm"
)

func danQiuOSHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
func danQiuOSCookieName(state string) string {
	prefix := "danqiuos_flow_"
	if strings.HasPrefix(system_setting.ServerAddress, "https://") {
		prefix = "__Host-" + prefix
	}
	return prefix + danQiuOSHash(state)[:24]
}
func startDanQiuOSBrowserFlow() string { return oauth2.GenerateVerifier() }
func setDanQiuOSCookie(c *gin.Context, state, secret string) {
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie(danQiuOSCookieName(state), secret, 600, "/", "", strings.HasPrefix(system_setting.ServerAddress, "https://"), true)
}
func validateDanQiuOSBrowser(c *gin.Context, flow *model.AuthFlow, state string) error {
	if origin := c.GetHeader("Origin"); origin != "" && origin != strings.TrimRight(system_setting.ServerAddress, "/") {
		return oauth.ErrDanQiuOSAuth
	}
	var payload oauthFlowPayload
	secret, err := c.Cookie(danQiuOSCookieName(state))
	if err != nil || secret == "" || json.Unmarshal([]byte(flow.Payload), &payload) != nil || payload.DanQiuOSBrowserHash == "" || subtle.ConstantTimeCompare([]byte(danQiuOSHash(secret)), []byte(payload.DanQiuOSBrowserHash)) != 1 {
		return oauth.ErrDanQiuOSAuth
	}
	return nil
}

// PrepareDanQiuOSAssertion exchanges a fragment-delivered central access token
// for a short-lived local assertion. Central tokens never enter URLs or storage.
func PrepareDanQiuOSAssertion(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	var request struct {
		State       string `json:"state"`
		AccessToken string `json:"access_token"`
	}
	if c.ShouldBindJSON(&request) != nil {
		danQiuOSFailure(c)
		return
	}
	flow, err := model.GetAuthFlow(request.State, model.AuthFlowMatch{Purpose: model.AuthFlowPurposeOAuth, Provider: "danqiuos"})
	if err != nil || validateDanQiuOSBrowser(c, flow, request.State) != nil {
		danQiuOSFailure(c)
		return
	}
	provider, ok := oauth.GetProvider("danqiuos").(*oauth.DanQiuOSProvider)
	if !ok {
		danQiuOSFailure(c)
		return
	}
	identity, err := provider.VerifySession(c.Request.Context(), request.AccessToken)
	if err != nil {
		danQiuOSFailure(c)
		return
	}
	payload, err := json.Marshal(identity)
	if err != nil {
		danQiuOSFailure(c)
		return
	}
	expiresAt := time.Now().Add(time.Minute)
	if flow.ExpiresAt.Before(expiresAt) {
		expiresAt = flow.ExpiresAt
	}
	if upstreamExpiry := time.Unix(identity.ValidUntil, 0); upstreamExpiry.Before(expiresAt) {
		expiresAt = upstreamExpiry
	}
	code, _, err := model.CreateAuthFlow(model.AuthFlowCreate{Purpose: oauth.DanQiuOSAssertionPurpose, Provider: "danqiuos", SessionId: flow.TokenHash, Payload: string(payload), ExpiresAt: expiresAt})
	if err != nil {
		danQiuOSFailure(c)
		return
	}
	common.ApiSuccess(c, gin.H{"code": code})
}
func danQiuOSFailure(c *gin.Context) {
	c.JSON(http.StatusForbidden, gin.H{"success": false, "message": "DanQiuOS authentication failed. Restart sign-in."})
}

func findOrCreateDanQiuOSUser(provider oauth.Provider, identity *oauth.OAuthUser) (*model.User, error) {
	user := &model.User{}
	err := provider.FillUserByProviderID(user, identity.ProviderUserID)
	if err == nil {
		return user, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	// A retained claim must never be reassigned after soft deletion.
	var count int64
	if err = model.DB.Model(&model.ExternalIdentityClaim{}).Where("provider = ? AND subject = ?", "danqiuos", identity.ProviderUserID).Count(&count).Error; err != nil {
		return nil, err
	}
	if count > 0 {
		return nil, &OAuthUserDeletedError{}
	}
	if !common.RegisterEnabled {
		return nil, &OAuthRegistrationDisabledError{}
	}
	user.Username = "dq_" + danQiuOSHash(identity.ProviderUserID)[:16]
	user.DisplayName = "DanQiuOS User"
	user.Role = common.RoleCommonUser
	user.Status = common.UserStatusEnabled
	err = model.DB.Transaction(func(tx *gorm.DB) error {
		if err := user.InsertWithTx(tx, 0); err != nil {
			return err
		}
		return model.ClaimExternalIdentityWithTx(tx, "danqiuos", identity.ProviderUserID, user.Id)
	})
	if err != nil {
		return nil, err
	}
	user.FinalizeOAuthUserCreation(0)
	return user, nil
}
