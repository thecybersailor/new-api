package service

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAccountPaymentClientCreateOrderSignsCanonicalRequest(t *testing.T) {
	const (
		keyID  = "acct-key"
		secret = "acct-secret"
		body   = `{"beneficiary_ref":"42","collection_kind":"one_time","currency":"usd","definition_version":"new-api-topup-v1","description":"New API credits","item_ref":"topup","net_amount":1234,"payer_identity_id":"42","product_purchase_ref":"new-api-ref-42","request_key":"new-api-ref-42","snapshot":{"kind":"topup"}}`
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawBody, err := ioReadAll(r.Body)
		require.NoError(t, err)
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/internal/v1/payments/orders", r.URL.Path)
		assert.Equal(t, "test", r.URL.Query().Get("environment"))
		assert.Equal(t, keyID, r.Header.Get("X-Account-Key-Id"))
		assert.Equal(t, "1700000000", r.Header.Get("X-Account-Timestamp"))
		assert.Equal(t, "nonce-1", r.Header.Get("X-Account-Nonce"))
		assert.JSONEq(t, body, string(rawBody))

		bodyDigest := sha256.Sum256(rawBody)
		signingInput := "POST\n/internal/v1/payments/orders?environment=test\n1700000000\nnonce-1\n" + hex.EncodeToString(bodyDigest[:])
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(signingInput))
		assert.Equal(t, hex.EncodeToString(mac.Sum(nil)), r.Header.Get("X-Account-Signature"))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"order_ref":"acct_order_1","checkout_url":"https://account.example/checkout/acct_order_1","checkout_available":true}}`))
	}))
	defer server.Close()

	client := &AccountPaymentClient{
		BaseURL:     server.URL,
		Environment: "test",
		KeyID:       keyID,
		Secret:      secret,
		HTTPClient:  server.Client(),
		Now: func() time.Time {
			return time.Unix(1700000000, 0).UTC()
		},
		Nonce: func() (string, error) {
			return "nonce-1", nil
		},
	}

	response, err := client.CreateOrder(t.Context(), AccountCreateOrderRequest{
		ProductPurchaseRef: "new-api-ref-42",
		PayerIdentityID:    "42",
		BeneficiaryRef:     "42",
		DefinitionVersion:  "new-api-topup-v1",
		ItemRef:            "topup",
		Description:        "New API credits",
		NetAmount:          1234,
		Currency:           "usd",
		CollectionKind:     "one_time",
		Snapshot:           map[string]any{"kind": "topup"},
		RequestKey:         "new-api-ref-42",
	})
	require.NoError(t, err)
	assert.Equal(t, "acct_order_1", response.OrderRef)
	assert.Equal(t, "https://account.example/checkout/acct_order_1", response.CheckoutURL)
	assert.True(t, response.CheckoutAvailable)
}

func ioReadAll(r io.Reader) ([]byte, error) {
	return io.ReadAll(r)
}
