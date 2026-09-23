package service

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
)

const accountSignatureClockSkew = 5 * time.Minute

type AccountPaymentClient struct {
	BaseURL     string
	Environment string
	KeyID       string
	Secret      string
	HTTPClient  *http.Client
	Now         func() time.Time
	Nonce       func() (string, error)
}

type AccountCreateOrderRequest struct {
	ProductPurchaseRef string         `json:"product_purchase_ref"`
	PayerIdentityID    string         `json:"payer_identity_id"`
	BeneficiaryRef     string         `json:"beneficiary_ref"`
	DefinitionVersion  string         `json:"definition_version"`
	ItemRef            string         `json:"item_ref"`
	Description        string         `json:"description"`
	NetAmount          int64          `json:"net_amount"`
	Currency           string         `json:"currency"`
	CollectionKind     string         `json:"collection_kind"`
	Snapshot           map[string]any `json:"snapshot"`
	RequestKey         string         `json:"request_key"`
}

type AccountCreateOrderResponse struct {
	OrderRef          string `json:"order_ref"`
	CheckoutURL       string `json:"checkout_url"`
	CheckoutAvailable bool   `json:"checkout_available"`
}

type accountAPIResponse[T any] struct {
	Data  T                `json:"data"`
	Error *accountAPIError `json:"error,omitempty"`
}

type accountAPIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (c *AccountPaymentClient) CreateOrder(ctx context.Context, request AccountCreateOrderRequest) (*AccountCreateOrderResponse, error) {
	response, err := doAccountJSON[AccountCreateOrderResponse](ctx, c, http.MethodPost, "/internal/v1/payments/orders", request)
	if err != nil {
		return nil, err
	}
	return response, nil
}

func doAccountJSON[T any](ctx context.Context, c *AccountPaymentClient, method string, path string, payload any) (*T, error) {
	if strings.TrimSpace(c.BaseURL) == "" || strings.TrimSpace(c.KeyID) == "" || strings.TrimSpace(c.Secret) == "" {
		return nil, errors.New("account payment is not configured")
	}

	baseURL, err := url.Parse(c.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid account base url: %w", err)
	}
	baseURL.Path = strings.TrimRight(baseURL.Path, "/") + path
	query := baseURL.Query()
	if c.Environment != "" {
		query.Set("environment", c.Environment)
	}
	baseURL.RawQuery = query.Encode()

	body, err := common.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, baseURL.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if err = c.SignRequest(req, body); err != nil {
		return nil, err
	}

	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var envelope accountAPIResponse[T]
	if err = common.Unmarshal(respBody, &envelope); err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if envelope.Error != nil && envelope.Error.Message != "" {
			return nil, errors.New(envelope.Error.Message)
		}
		return nil, fmt.Errorf("account payment returned status %d", resp.StatusCode)
	}
	if envelope.Error != nil {
		return nil, errors.New(envelope.Error.Message)
	}
	return &envelope.Data, nil
}

func (c *AccountPaymentClient) SignRequest(req *http.Request, body []byte) error {
	now := time.Now().UTC()
	if c.Now != nil {
		now = c.Now().UTC()
	}
	nonce, err := accountNonce()
	if c.Nonce != nil {
		nonce, err = c.Nonce()
	}
	if err != nil {
		return err
	}

	timestamp := fmt.Sprintf("%d", now.Unix())
	signature := AccountPaymentSignature(req.Method, accountCanonicalPathQuery(req.URL), timestamp, nonce, body, c.Secret)
	req.Header.Set("X-Account-Key-Id", c.KeyID)
	req.Header.Set("X-Account-Timestamp", timestamp)
	req.Header.Set("X-Account-Nonce", nonce)
	req.Header.Set("X-Account-Signature", signature)
	return nil
}

func VerifyAccountPaymentSignature(method string, requestURL *url.URL, body []byte, keyID string, secret string, now time.Time, headers http.Header) error {
	if keyID == "" || secret == "" {
		return errors.New("account payment is not configured")
	}
	if headers.Get("X-Account-Key-Id") != keyID {
		return errors.New("account key id mismatch")
	}
	timestamp := headers.Get("X-Account-Timestamp")
	nonce := headers.Get("X-Account-Nonce")
	signature := headers.Get("X-Account-Signature")
	if timestamp == "" || nonce == "" || signature == "" {
		return errors.New("missing account signature headers")
	}
	if len(nonce) > 128 || !isVisibleASCII(nonce) {
		return errors.New("invalid account nonce")
	}
	signedAtUnix, err := parseUnixTimestamp(timestamp)
	if err != nil {
		return errors.New("invalid account timestamp")
	}
	signedAt := time.Unix(signedAtUnix, 0)
	if now.IsZero() {
		now = time.Now()
	}
	if signedAt.Before(now.Add(-accountSignatureClockSkew)) || signedAt.After(now.Add(accountSignatureClockSkew)) {
		return errors.New("account signature timestamp outside allowed window")
	}

	expected := AccountPaymentSignature(method, accountCanonicalPathQuery(requestURL), timestamp, nonce, body, secret)
	if !hmac.Equal([]byte(signature), []byte(expected)) {
		return errors.New("invalid account signature")
	}
	return nil
}

func AccountPaymentSignature(method string, canonicalPathQuery string, timestamp string, nonce string, body []byte, secret string) string {
	bodyDigest := sha256.Sum256(body)
	signingInput := strings.Join([]string{
		strings.ToUpper(method),
		canonicalPathQuery,
		timestamp,
		nonce,
		hex.EncodeToString(bodyDigest[:]),
	}, "\n")
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signingInput))
	return hex.EncodeToString(mac.Sum(nil))
}

func accountCanonicalPathQuery(u *url.URL) string {
	if u == nil {
		return ""
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	if u.RawQuery == "" {
		return path
	}
	values, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return path + "?" + u.RawQuery
	}
	return path + "?" + values.Encode()
}

func accountNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func parseUnixTimestamp(value string) (int64, error) {
	var n int64
	if value == "" {
		return 0, errors.New("empty timestamp")
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return 0, errors.New("invalid timestamp")
		}
		n = n*10 + int64(r-'0')
	}
	return n, nil
}

func isVisibleASCII(value string) bool {
	for _, r := range value {
		if r < 0x21 || r > 0x7e {
			return false
		}
	}
	return true
}
