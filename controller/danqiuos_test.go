package controller

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"github.com/QuantumNous/new-api/middleware"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/oauth"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
	"github.com/go-jose/go-jose/v4"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type danQiuOSTransport func(*http.Request) (*http.Response, error)

func (f danQiuOSTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func danQiuOSFixture(t *testing.T) (*oauth.DanQiuOSProvider, func(jwt.MapClaims) string, *int, *string) {
	t.Helper()
	t.Setenv("DANTOKEN_DANQIUOS_LOGIN_ENABLED", "true")
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	jwks, err := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "account-web-user", Algorithm: "ES256", Use: "sig"}}})
	require.NoError(t, err)
	status := 200
	subject := "personal-user-1"
	transport := danQiuOSTransport(func(r *http.Request) (*http.Response, error) {
		require.Equal(t, "account-api.danqiuos.cn", r.URL.Host)
		body := string(jwks)
		code := 200
		if r.URL.Path == "/account/v1/me" {
			require.True(t, strings.HasPrefix(r.Header.Get("Authorization"), "Bearer "))
			code = status
			body = `{"data":{"id":"` + subject + `"}}`
		} else {
			require.Equal(t, "/auth/v1/.well-known/jwks.json", r.URL.Path)
		}
		return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
	})
	provider := oauth.NewDanQiuOSProvider(&http.Client{Transport: transport, Timeout: time.Second})
	signer := func(overrides jwt.MapClaims) string {
		claims := jwt.MapClaims{"iss": oauth.DanQiuOSIssuer, "aud": []string{"web_user"}, "sub": "personal-user-1", "user_id": "personal-user-1", "instance_id": "web_user", "session_id": 123, "nbf": time.Now().Unix() - 1, "iat": time.Now().Unix() - 1, "exp": time.Now().Unix() + 300}
		for k, v := range overrides {
			claims[k] = v
		}
		token := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
		token.Header["kid"] = "account-web-user"
		raw, err := token.SignedString(key)
		require.NoError(t, err)
		return raw
	}
	return provider, signer, &status, &subject
}
func TestDanQiuOSStrictPersonalSession(t *testing.T) {
	for _, scenario := range []string{"valid", "issuer", "audience", "extra audience", "expired", "future", "subject mismatch", "missing session", "instance", "revoked", "profile mismatch", "signature", "disabled"} {
		t.Run(scenario, func(t *testing.T) {
			provider, sign, status, subject := danQiuOSFixture(t)
			claims := jwt.MapClaims{}
			switch scenario {
			case "issuer":
				claims["iss"] = "https://account.pr.autostaff.cn"
			case "audience":
				claims["aud"] = []string{"tenant"}
			case "extra audience":
				claims["aud"] = []string{"web_user", "other"}
			case "expired":
				claims["exp"] = time.Now().Unix() - 1
			case "future":
				claims["nbf"] = time.Now().Unix() + 600
			case "subject mismatch":
				claims["user_id"] = "team-1"
			case "missing session":
				claims["session_id"] = 0
			case "instance":
				claims["instance_id"] = "other"
			case "revoked":
				*status = 401
			case "profile mismatch":
				*subject = "other-user"
			case "disabled":
				t.Setenv("DANTOKEN_DANQIUOS_LOGIN_ENABLED", "false")
			}
			token := sign(claims)
			if scenario == "signature" {
				parts := strings.Split(token, ".")
				parts[2] = "AAAAAAAA"
				token = strings.Join(parts, ".")
			}
			identity, err := provider.VerifySession(context.Background(), token)
			if scenario == "valid" {
				require.NoError(t, err)
				require.Equal(t, "personal-user-1", identity.ProviderUserID)
				require.Empty(t, identity.Email)
				require.Empty(t, identity.Extra)
			} else {
				require.ErrorIs(t, err, oauth.ErrDanQiuOSAuth)
				require.Nil(t, identity)
			}
		})
	}
}
func setupDanQiuOS(t *testing.T) (*model.User, service.AuthIdentity, *oauth.DanQiuOSProvider, func(jwt.MapClaims) string) {
	user, identity := setupSecurityEnrollmentTest(t)
	require.NoError(t, model.DB.AutoMigrate(&model.ExternalIdentityClaim{}))
	provider, sign, _, _ := danQiuOSFixture(t)
	oldProvider := oauth.GetProvider("danqiuos")
	oauth.Register("danqiuos", provider)
	oldAddress := system_setting.ServerAddress
	system_setting.ServerAddress = "https://dantoken.cn"
	oldRegister := common.RegisterEnabled
	common.RegisterEnabled = true
	t.Cleanup(func() {
		oauth.Register("danqiuos", oldProvider)
		system_setting.ServerAddress = oldAddress
		common.RegisterEnabled = oldRegister
	})
	return user, identity, provider, sign
}
func danQiuOSRequest(method, path, body string, cookie *http.Cookie, identity service.AuthIdentity, handler gin.HandlerFunc) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, path, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		c.Request.AddCookie(cookie)
	}
	c.Params = gin.Params{{Key: "provider", Value: "danqiuos"}}
	c.Set("id", identity.UserID)
	c.Set("role", common.RoleCommonUser)
	c.Set("session_id", identity.SessionID)
	c.Set("auth_version", identity.UserAuthVersion)
	c.Set("session_version", identity.SessionVersion)
	handler(c)
	return w
}
func danQiuOSState(t *testing.T, intent, proof string, identity service.AuthIdentity) (string, *http.Cookie) {
	w := securityEnrollmentRequest("POST", "/api/oauth/state", `{"provider":"danqiuos","intent":"`+intent+`"}`, proof, identity, GenerateOAuthCode)
	require.Equal(t, 200, w.Code, w.Body.String())
	var result struct {
		Success bool `json:"success"`
		Data    struct {
			State string `json:"flow_token"`
			URL   string `json:"authorization_url"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
	require.True(t, result.Success, w.Body.String())
	require.Contains(t, result.Data.URL, "https://account.danqiuos.cn/")
	cookies := w.Result().Cookies()
	require.Len(t, cookies, 1)
	require.True(t, cookies[0].HttpOnly)
	require.True(t, cookies[0].Secure)
	return result.Data.State, cookies[0]
}
func danQiuOSAssertion(t *testing.T, state, token string, cookie *http.Cookie) string {
	body, _ := json.Marshal(map[string]string{"state": state, "access_token": token})
	w := danQiuOSRequest("POST", "/api/danqiuos/assertion", string(body), cookie, service.AuthIdentity{}, PrepareDanQiuOSAssertion)
	require.Equal(t, 200, w.Code, w.Body.String())
	var result struct {
		Data struct {
			Code string `json:"code"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
	require.NotEmpty(t, result.Data.Code)
	var flows []model.AuthFlow
	require.NoError(t, model.DB.Find(&flows).Error)
	for _, flow := range flows {
		require.NotContains(t, flow.Payload, token)
	}
	return result.Data.Code
}
func TestDanQiuOSLoginAndSingleUseBrowserBinding(t *testing.T) {
	_, _, provider, sign := setupDanQiuOS(t)
	state, cookie := danQiuOSState(t, "login", "", service.AuthIdentity{})
	body, _ := json.Marshal(map[string]string{"state": state, "access_token": sign(nil)})
	require.Equal(t, 403, danQiuOSRequest("POST", "/api/danqiuos/assertion", string(body), nil, service.AuthIdentity{}, PrepareDanQiuOSAssertion).Code)
	code := danQiuOSAssertion(t, state, sign(nil), cookie)
	url := "/api/oauth/danqiuos?state=" + state + "&code=" + code
	w := danQiuOSRequest("GET", url, "", cookie, service.AuthIdentity{}, HandleOAuth)
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), `"success":true`)
	var user model.User
	require.NoError(t, provider.FillUserByProviderID(&user, "personal-user-1"))
	require.Equal(t, common.RoleCommonUser, user.Role)
	require.Empty(t, user.Email)
	require.Empty(t, user.Password)
	require.Equal(t, 403, danQiuOSRequest("GET", url, "", cookie, service.AuthIdentity{}, HandleOAuth).Code)
	state2, cookie2 := danQiuOSState(t, "login", "", service.AuthIdentity{})
	code2 := danQiuOSAssertion(t, state2, sign(nil), cookie2)
	require.NoError(t, model.DB.Model(&user).Update("status", common.UserStatusDisabled).Error)
	blocked := danQiuOSRequest("GET", "/api/oauth/danqiuos?state="+state2+"&code="+code2, "", cookie2, service.AuthIdentity{}, HandleOAuth)
	require.NotContains(t, blocked.Body.String(), `"success":true`)
}
func TestDanQiuOSBindingPreservesLocalOwnershipAndProof(t *testing.T) {
	user, identity, _, sign := setupDanQiuOS(t)
	require.NoError(t, model.DB.Model(user).Updates(map[string]any{"quota": 4567, "used_quota": 123, "group": "default"}).Error)
	missing := securityEnrollmentRequest("POST", "/api/oauth/state", `{"provider":"danqiuos","intent":"bind"}`, "", identity, GenerateOAuthCode)
	require.Equal(t, 403, missing.Code)
	proof := issueSecurityEnrollmentProof(t, identity, service.VerificationOperation{Scope: service.VerificationScopeAccountBind, Context: json.RawMessage(`{"provider":"danqiuos"}`)}, service.VerificationMethodPassword)
	state, cookie := danQiuOSState(t, "bind", proof, identity)
	code := danQiuOSAssertion(t, state, sign(nil), cookie)
	url := "/api/oauth/danqiuos?state=" + state + "&code=" + code
	require.Equal(t, 403, danQiuOSRequest("GET", url, "", cookie, service.AuthIdentity{}, HandleOAuth).Code)
	w := danQiuOSRequest("GET", url, "", cookie, identity, HandleOAuth)
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), `"success":true`)
	var stored model.User
	require.NoError(t, model.DB.First(&stored, user.Id).Error)
	require.Equal(t, 4567, stored.Quota)
	require.Equal(t, 123, stored.UsedQuota)
	require.Equal(t, user.Role, stored.Role)
	require.Equal(t, user.Password, stored.Password)
	var claim model.ExternalIdentityClaim
	require.NoError(t, model.DB.Where("provider = ?", "danqiuos").First(&claim).Error)
	require.Equal(t, user.Id, claim.UserId)
	// Competing ownership and rebinding cannot alter the existing claim.
	require.ErrorIs(t, model.DB.Transaction(func(tx *gorm.DB) error {
		return model.ClaimExternalIdentityWithTx(tx, "danqiuos", "personal-user-1", user.Id+1)
	}), model.ErrExternalIdentityAlreadyClaimed)
	require.ErrorIs(t, model.DB.Transaction(func(tx *gorm.DB) error {
		return model.ClaimExternalIdentityWithTx(tx, "danqiuos", "other-subject", user.Id)
	}), model.ErrExternalIdentityAlreadyClaimed)
}

// Explicitly opted-in local fixture. All central Account traffic uses the signed
// transport above; no production Account/model endpoints are contacted.
func TestDanQiuOSBrowserFixture(t *testing.T) {
	if os.Getenv("DANTOKEN_SSO_BROWSER_FIXTURE") != "true" {
		t.Skip("local browser fixture is opt-in")
	}
	_, _, _, sign := setupDanQiuOS(t)
	system_setting.ServerAddress = "http://localhost:18796"
	r := gin.New()
	r.GET("/api/setup", func(c *gin.Context) { common.ApiSuccess(c, gin.H{"status": true}) })
	r.GET("/api/status", GetStatus)
	r.POST("/api/oauth/state", middleware.TryUserAuth(), GenerateOAuthCode)
	r.POST("/api/danqiuos/assertion", PrepareDanQiuOSAssertion)
	r.GET("/api/oauth/:provider", middleware.TryUserAuth(), HandleOAuth)
	r.GET("/api/user/self", middleware.UserAuth(), GetSelf)
	r.POST("/api/user/auth/refresh", RefreshAuth)
	r.GET("/__test/session", func(c *gin.Context) { common.ApiSuccess(c, gin.H{"access_token": sign(nil)}) })
	dist, err := filepath.Abs("../web/dist")
	require.NoError(t, err)
	r.NoRoute(func(c *gin.Context) {
		if strings.HasPrefix(c.Request.URL.Path, "/api/") {
			c.JSON(404, gin.H{"success": false})
			return
		}
		path := filepath.Join(dist, filepath.Clean("/"+c.Request.URL.Path))
		if stat, err := os.Stat(path); err == nil && !stat.IsDir() {
			c.File(path)
			return
		}
		c.File(filepath.Join(dist, "index.html"))
	})
	listener, err := net.Listen("tcp", "127.0.0.1:18796")
	require.NoError(t, err)
	server := &http.Server{Handler: r}
	t.Cleanup(func() { server.Close() })
	go server.Serve(listener)
	t.Log("fixture ready on http://localhost:18796")
	<-time.After(3 * time.Minute)
}

func TestDanQiuOSAssertionCannotCrossFlowsOrBrowsers(t *testing.T) {
	_, _, _, sign := setupDanQiuOS(t)
	state, cookie := danQiuOSState(t, "login", "", service.AuthIdentity{})
	code := danQiuOSAssertion(t, state, sign(nil), cookie)
	other, otherCookie := danQiuOSState(t, "login", "", service.AuthIdentity{})
	request := func(state, code string, cookie *http.Cookie) *httptest.ResponseRecorder {
		return danQiuOSRequest("GET", "/api/oauth/danqiuos?state="+state+"&code="+code, "", cookie, service.AuthIdentity{}, HandleOAuth)
	}
	require.Equal(t, 403, request(other, code, otherCookie).Code)
	require.Equal(t, 403, request(state, code, otherCookie).Code)
	// Rejected attempts must not consume the rightful browser's assertion.
	require.Contains(t, request(state, code, cookie).Body.String(), `"success":true`)
}
func TestDanQiuOSExpiredOrRevokedFlowFails(t *testing.T) {
	for _, scenario := range []string{"assertion expired", "parent expired", "session revoked"} {
		t.Run(scenario, func(t *testing.T) {
			_, identity, _, sign := setupDanQiuOS(t)
			proof := issueSecurityEnrollmentProof(t, identity, service.VerificationOperation{Scope: service.VerificationScopeAccountBind, Context: json.RawMessage(`{"provider":"danqiuos"}`)}, service.VerificationMethodPassword)
			state, cookie := danQiuOSState(t, "bind", proof, identity)
			code := danQiuOSAssertion(t, state, sign(nil), cookie)
			switch scenario {
			case "assertion expired":
				require.NoError(t, model.DB.Model(&model.AuthFlow{}).Where("purpose = ?", oauth.DanQiuOSAssertionPurpose).Update("expires_at", time.Now().Add(-time.Second)).Error)
			case "parent expired":
				require.NoError(t, model.DB.Model(&model.AuthFlow{}).Where("purpose = ?", model.AuthFlowPurposeOAuth).Update("expires_at", time.Now().Add(-time.Second)).Error)
			case "session revoked":
				require.NoError(t, model.DB.Model(&model.UserSession{}).Where("sid = ?", identity.SessionID).Update("revoked_at", time.Now().Unix()).Error)
			}
			w := danQiuOSRequest("GET", "/api/oauth/danqiuos?state="+state+"&code="+code, "", cookie, identity, HandleOAuth)
			require.NotContains(t, w.Body.String(), `"success":true`)
			var count int64
			require.NoError(t, model.DB.Model(&model.ExternalIdentityClaim{}).Count(&count).Error)
			require.Zero(t, count)
		})
	}
}
func TestDanQiuOSConcurrentCallbackHasOneWinner(t *testing.T) {
	_, _, _, sign := setupDanQiuOS(t)
	state, cookie := danQiuOSState(t, "login", "", service.AuthIdentity{})
	code := danQiuOSAssertion(t, state, sign(nil), cookie)
	start := make(chan struct{})
	out := make(chan bool, 2)
	for range 2 {
		go func() {
			<-start
			w := danQiuOSRequest("GET", "/api/oauth/danqiuos?state="+state+"&code="+code, "", cookie, service.AuthIdentity{}, HandleOAuth)
			out <- strings.Contains(w.Body.String(), `"success":true`)
		}()
	}
	close(start)
	winners := 0
	for range 2 {
		if <-out {
			winners++
		}
	}
	require.Equal(t, 1, winners)
	var n int64
	require.NoError(t, model.DB.Model(&model.ExternalIdentityClaim{}).Where("provider = ?", "danqiuos").Count(&n).Error)
	require.EqualValues(t, 1, n)
}
