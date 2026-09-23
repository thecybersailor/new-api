package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestAccountWebhookCreditsTopupAndReturnsDurableAck(t *testing.T) {
	oldDB := model.DB
	oldLogDB := model.LOG_DB
	oldRedisEnabled := common.RedisEnabled
	oldQuotaPerUnit := common.QuotaPerUnit
	oldCompliance := operation_setting.GetPaymentSetting().ComplianceConfirmed
	oldComplianceVersion := operation_setting.GetPaymentSetting().ComplianceTermsVersion
	oldProjectKey := setting.AccountPaymentProjectKey
	oldEnvironment := setting.AccountPaymentEnvironment
	oldBaseURL := setting.AccountPaymentBaseURL
	oldDefinitionVersion := setting.AccountPaymentDefinitionVersion
	oldItemRef := setting.AccountPaymentItemRef
	oldCurrency := setting.AccountPaymentCurrency
	oldKeyID := setting.AccountPaymentKeyID
	oldSecret := setting.AccountPaymentServiceSecret
	t.Cleanup(func() {
		model.DB = oldDB
		model.LOG_DB = oldLogDB
		common.RedisEnabled = oldRedisEnabled
		common.QuotaPerUnit = oldQuotaPerUnit
		operation_setting.GetPaymentSetting().ComplianceConfirmed = oldCompliance
		operation_setting.GetPaymentSetting().ComplianceTermsVersion = oldComplianceVersion
		setting.AccountPaymentProjectKey = oldProjectKey
		setting.AccountPaymentEnvironment = oldEnvironment
		setting.AccountPaymentBaseURL = oldBaseURL
		setting.AccountPaymentDefinitionVersion = oldDefinitionVersion
		setting.AccountPaymentItemRef = oldItemRef
		setting.AccountPaymentCurrency = oldCurrency
		setting.AccountPaymentKeyID = oldKeyID
		setting.AccountPaymentServiceSecret = oldSecret
	})

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.TopUp{}, &model.Log{}))
	model.DB = db
	model.LOG_DB = db
	common.RedisEnabled = false
	common.QuotaPerUnit = 500000
	operation_setting.GetPaymentSetting().ComplianceConfirmed = true
	operation_setting.GetPaymentSetting().ComplianceTermsVersion = operation_setting.CurrentComplianceTermsVersion
	setting.AccountPaymentProjectKey = "apfuna"
	setting.AccountPaymentEnvironment = "test"
	setting.AccountPaymentBaseURL = "https://account.example"
	setting.AccountPaymentDefinitionVersion = "new-api-topup-v1"
	setting.AccountPaymentItemRef = "topup"
	setting.AccountPaymentCurrency = "usd"
	setting.AccountPaymentKeyID = "acct-key"
	setting.AccountPaymentServiceSecret = "acct-secret"

	require.NoError(t, model.DB.Create(&model.User{Id: 42, Username: "account-user", Status: common.UserStatusEnabled}).Error)
	require.NoError(t, model.DB.Create(&model.TopUp{
		UserId:          42,
		Amount:          2,
		Money:           12.34,
		TradeNo:         "account-ref-42",
		PaymentMethod:   model.PaymentMethodAccount,
		PaymentProvider: model.PaymentProviderAccount,
		CreateTime:      common.GetTimestamp(),
		Status:          common.TopUpStatusPending,
	}).Error)

	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	body := `{"event_ref":"evt_1","project_key":"apfuna","environment":"test","effect_key":"payment:pi_1","aggregate_ref":"acct_order_1","revision":1,"kind":"payment.confirmed","occurred_at":"2026-09-23T09:00:00Z","data":{"receipt_ref":"receipt_1","order_ref":"acct_order_1","product_purchase_ref":"account-ref-42","payer_identity_id":"42","beneficiary_ref":"42","provider_payment_ref":"pi_1","net_amount":1234,"tax_amount":0,"total_amount":1234,"currency":"usd","paid_at":"2026-09-23T09:00:00Z"}}`
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/account/webhook", strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Request.Header.Set("X-Account-Key-Id", "acct-key")
	ctx.Request.Header.Set("X-Account-Timestamp", timestamp)
	ctx.Request.Header.Set("X-Account-Nonce", "nonce-1")
	ctx.Request.Header.Set("X-Account-Signature", service.AccountPaymentSignature(
		http.MethodPost,
		"/api/account/webhook",
		timestamp,
		"nonce-1",
		[]byte(body),
		"acct-secret",
	))

	AccountWebhook(ctx)

	assert.Equal(t, http.StatusOK, recorder.Code)
	digest := sha256.Sum256([]byte(body))
	assert.JSONEq(t, `{"data":{"event_ref":"evt_1","effect_key":"payment:pi_1","payload_digest":"`+hex.EncodeToString(digest[:])+`","durable":true}}`, recorder.Body.String())

	topUp := model.GetTopUpByTradeNo("account-ref-42")
	require.NotNil(t, topUp)
	assert.Equal(t, common.TopUpStatusSuccess, topUp.Status)

	var user model.User
	require.NoError(t, model.DB.Select("quota").Where("id = ?", 42).First(&user).Error)
	assert.Equal(t, 1000000, user.Quota)
}
