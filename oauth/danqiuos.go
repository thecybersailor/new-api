package oauth

// DanQiuOS uses its existing Account session-transfer protocol, not OIDC code
// exchange. Only a verified personal subject crosses into the local auth flow.
import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/gin-gonic/gin"
)

const DanQiuOSIssuer = "https://account.danqiuos.cn"
const DanQiuOSAPI = "https://account-api.danqiuos.cn"
const DanQiuOSAssertionPurpose = "danqiuos_assertion"

var ErrDanQiuOSAuth = errors.New("DanQiuOS authentication failed. Restart sign-in.")

func DanQiuOSEnabled() bool { return os.Getenv("DANTOKEN_DANQIUOS_LOGIN_ENABLED") == "true" }
func DanQiuOSCallbackURL() (string, error) {
	u, err := url.Parse(system_setting.ServerAddress)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", ErrDanQiuOSAuth
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1")) {
		return "", ErrDanQiuOSAuth
	}
	u.Path = "/auth/callback"
	return u.String(), nil
}
func DanQiuOSAuthorizationURL(state string) (string, error) {
	callback, err := DanQiuOSCallbackURL()
	if err != nil || !DanQiuOSEnabled() {
		return "", ErrDanQiuOSAuth
	}
	u, _ := url.Parse(callback)
	q := u.Query()
	q.Set("state", state)
	u.RawQuery = q.Encode()
	return DanQiuOSIssuer + "/?" + url.Values{"redirect_to": {u.String()}}.Encode(), nil
}

type DanQiuOSProvider struct {
	client *http.Client
	keys   oidc.KeySet
}

func NewDanQiuOSProvider(client *http.Client) *DanQiuOSProvider {
	// Never forward an Account bearer credential to a redirect target.
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &DanQiuOSProvider{client: &copyClient, keys: oidc.NewRemoteKeySet(oidc.ClientContext(context.Background(), &copyClient), DanQiuOSAPI+"/auth/v1/.well-known/jwks.json")}
}
func init()                               { Register("danqiuos", NewDanQiuOSProvider(&http.Client{Timeout: 5 * time.Second})) }
func (*DanQiuOSProvider) GetName() string { return "DanQiuOS" }
func (*DanQiuOSProvider) IsEnabled() bool {
	return DanQiuOSEnabled() && !HasCustomProviderConflict("danqiuos")
}
func (*DanQiuOSProvider) GetProviderPrefix() string             { return "dq_" }
func (*DanQiuOSProvider) ProviderUserIDColumn() string          { return "" }
func (*DanQiuOSProvider) SetProviderUserID(*model.User, string) {} // persisted in external_identity_claims
func (*DanQiuOSProvider) IsUserIDTaken(subject string) bool {
	var n int64
	return model.DB.Model(&model.ExternalIdentityClaim{}).Where("provider = ? AND subject = ?", "danqiuos", subject).Count(&n).Error == nil && n > 0
}
func (*DanQiuOSProvider) FillUserByProviderID(user *model.User, subject string) error {
	var claim model.ExternalIdentityClaim
	if err := model.DB.Where("provider = ? AND subject = ?", "danqiuos", subject).First(&claim).Error; err != nil {
		return err
	}
	return model.DB.First(user, claim.UserId).Error
}
func (p *DanQiuOSProvider) VerifySession(ctx context.Context, accessToken string) (*OAuthUser, error) {
	if !p.IsEnabled() || accessToken == "" || len(accessToken) > 32768 {
		return nil, ErrDanQiuOSAuth
	}
	verifier := oidc.NewVerifier(DanQiuOSIssuer, p.keys, &oidc.Config{ClientID: "web_user", SupportedSigningAlgs: []string{oidc.ES256}})
	verified, err := verifier.Verify(ctx, accessToken)
	if err != nil {
		return nil, ErrDanQiuOSAuth
	}
	var claims struct {
		UserID     string `json:"user_id"`
		InstanceID string `json:"instance_id"`
		SessionID  uint64 `json:"session_id"`
		NBF        int64  `json:"nbf"`
	}
	if verified.Claims(&claims) != nil || verified.Subject == "" || len(verified.Subject) > 128 || strings.TrimSpace(verified.Subject) != verified.Subject || claims.UserID != verified.Subject || claims.InstanceID != "web_user" || claims.SessionID == 0 || claims.NBF > time.Now().Unix() || verified.IssuedAt.After(time.Now()) || len(verified.Audience) != 1 {
		return nil, ErrDanQiuOSAuth
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, DanQiuOSAPI+"/account/v1/me", nil)
	if err != nil {
		return nil, ErrDanQiuOSAuth
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	res, err := p.client.Do(req)
	if err != nil {
		return nil, ErrDanQiuOSAuth
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, ErrDanQiuOSAuth
	}
	var profile struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
		HasError bool            `json:"has_error"`
		Error    json.RawMessage `json:"error"`
	}
	if json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&profile) != nil || profile.HasError || (len(profile.Error) > 0 && string(profile.Error) != "null") || profile.Data.ID != verified.Subject {
		return nil, ErrDanQiuOSAuth
	}
	// No email linking, imported privileges, or unverified display metadata.
	return &OAuthUser{ProviderUserID: verified.Subject, DisplayName: "DanQiuOS User", ValidUntil: verified.Expiry.Unix()}, nil
}
func (*DanQiuOSProvider) ExchangeToken(_ context.Context, code string, c *gin.Context) (*OAuthToken, error) {
	if c.GetString("danqiuos_state_hash") == "" {
		return nil, ErrDanQiuOSAuth
	}
	flow, err := model.ConsumeAuthFlow(code, model.AuthFlowMatch{Purpose: DanQiuOSAssertionPurpose, Provider: "danqiuos", SessionId: c.GetString("danqiuos_state_hash")})
	if err != nil {
		return nil, ErrDanQiuOSAuth
	}
	var identity OAuthUser
	if json.Unmarshal([]byte(flow.Payload), &identity) != nil || identity.ProviderUserID == "" {
		return nil, ErrDanQiuOSAuth
	}
	return &OAuthToken{VerifiedIdentity: &identity}, nil
}
func (*DanQiuOSProvider) GetUserInfo(_ context.Context, token *OAuthToken) (*OAuthUser, error) {
	if token == nil || token.VerifiedIdentity == nil || token.VerifiedIdentity.ValidUntil <= time.Now().Unix() {
		return nil, ErrDanQiuOSAuth
	}
	return token.VerifiedIdentity, nil
}
