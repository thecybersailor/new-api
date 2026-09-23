package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"github.com/thanhpk/randstr"
)

type AccountPayRequest struct {
	Amount        int64  `json:"amount"`
	PaymentMethod string `json:"payment_method"`
}

type AccountPaymentEvent struct {
	EventRef     string `json:"event_ref"`
	ProjectKey   string `json:"project_key"`
	Environment  string `json:"environment"`
	EffectKey    string `json:"effect_key"`
	AggregateRef string `json:"aggregate_ref"`
	Kind         string `json:"kind"`
	Data         struct {
		ReceiptRef         string `json:"receipt_ref"`
		OrderRef           string `json:"order_ref"`
		ProductPurchaseRef string `json:"product_purchase_ref"`
		PayerIdentityID    string `json:"payer_identity_id"`
		BeneficiaryRef     string `json:"beneficiary_ref"`
		ProviderPaymentRef string `json:"provider_payment_ref"`
		NetAmount          int64  `json:"net_amount"`
		TotalAmount        int64  `json:"total_amount"`
		Currency           string `json:"currency"`
		PaidAt             string `json:"paid_at"`
	} `json:"data"`
}

func getAccountMinTopup() int64 {
	if setting.AccountPaymentMinTopUp <= 0 {
		return getMinTopup()
	}
	minTopup := int64(setting.AccountPaymentMinTopUp)
	if operation_setting.GetQuotaDisplayType() == operation_setting.QuotaDisplayTypeTokens {
		dMinTopup := decimal.NewFromInt(minTopup)
		dQuotaPerUnit := decimal.NewFromFloat(common.QuotaPerUnit)
		quota, err := common.WalletQuotaFromDecimalStrict(dMinTopup.Mul(dQuotaPerUnit))
		if err != nil {
			return common.MaxWalletQuota
		}
		return int64(quota)
	}
	return minTopup
}

func RequestAccountAmount(c *gin.Context) {
	var req AccountPayRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusOK, gin.H{"message": "error", "data": "参数错误"})
		return
	}
	if req.Amount < getAccountMinTopup() {
		c.JSON(http.StatusOK, gin.H{"message": "error", "data": fmt.Sprintf("充值数量不能小于 %d", getAccountMinTopup())})
		return
	}
	id := c.GetInt("id")
	if rejectInvalidTopUpQuota(c, id, req.Amount) {
		return
	}
	group, err := model.GetUserGroup(id, true)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"message": "error", "data": "获取用户分组失败"})
		return
	}
	payMoney := getPayMoney(req.Amount, group)
	if payMoney <= 0.01 {
		c.JSON(http.StatusOK, gin.H{"message": "error", "data": "充值金额过低"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "success", "data": strconv.FormatFloat(payMoney, 'f', 2, 64)})
}

func RequestAccountPay(c *gin.Context) {
	var req AccountPayRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusOK, gin.H{"message": "error", "data": "参数错误"})
		return
	}
	if req.PaymentMethod != model.PaymentMethodAccount {
		c.JSON(http.StatusOK, gin.H{"message": "error", "data": "不支持的支付渠道"})
		return
	}
	if req.Amount < getAccountMinTopup() {
		c.JSON(http.StatusOK, gin.H{"message": "error", "data": fmt.Sprintf("充值数量不能小于 %d", getAccountMinTopup())})
		return
	}

	id := c.GetInt("id")
	if rejectInvalidTopUpQuota(c, id, req.Amount) {
		return
	}
	user, err := model.GetUserById(id, false)
	if err != nil || user == nil {
		c.JSON(http.StatusOK, gin.H{"message": "error", "data": "用户不存在"})
		return
	}
	group, err := model.GetUserGroup(id, true)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"message": "error", "data": "获取用户分组失败"})
		return
	}
	payMoney := getPayMoney(req.Amount, group)
	if payMoney <= 0.01 {
		c.JSON(http.StatusOK, gin.H{"message": "error", "data": "充值金额过低"})
		return
	}
	netAmount, err := accountNetAmountFromMoney(payMoney)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"message": "error", "data": "充值金额无效"})
		return
	}

	reference := fmt.Sprintf("account-api-ref-%d-%d-%s", user.Id, time.Now().UnixMilli(), randstr.String(4))
	referenceId := "ref_" + common.Sha1([]byte(reference))
	amount := req.Amount
	if operation_setting.GetQuotaDisplayType() == operation_setting.QuotaDisplayTypeTokens {
		dAmount := decimal.NewFromInt(amount)
		dQuotaPerUnit := decimal.NewFromFloat(common.QuotaPerUnit)
		amount = dAmount.Div(dQuotaPerUnit).IntPart()
	}
	topUp := &model.TopUp{
		UserId:          id,
		Amount:          amount,
		Money:           payMoney,
		TradeNo:         referenceId,
		PaymentMethod:   model.PaymentMethodAccount,
		PaymentProvider: model.PaymentProviderAccount,
		CreateTime:      time.Now().Unix(),
		Status:          common.TopUpStatusPending,
	}
	if err = topUp.Insert(); err != nil {
		logger.LogError(c.Request.Context(), fmt.Sprintf("Account 创建充值订单失败 user_id=%d trade_no=%s amount=%d error=%q", id, referenceId, req.Amount, err.Error()))
		c.JSON(http.StatusOK, gin.H{"message": "error", "data": "创建订单失败"})
		return
	}

	checkoutURL, err := createAccountCheckout(c.Request.Context(), referenceId, id, netAmount)
	if err != nil {
		logger.LogError(c.Request.Context(), fmt.Sprintf("Account 创建 Checkout 失败 user_id=%d trade_no=%s amount=%d error=%q", id, referenceId, req.Amount, err.Error()))
		c.JSON(http.StatusOK, gin.H{"message": "error", "data": "拉起支付失败"})
		return
	}

	logger.LogInfo(c.Request.Context(), fmt.Sprintf("Account 充值订单创建成功 user_id=%d trade_no=%s amount=%d money=%.2f", id, referenceId, req.Amount, payMoney))
	c.JSON(http.StatusOK, gin.H{
		"message": "success",
		"data": gin.H{
			"checkout_url": checkoutURL,
			"order_id":     referenceId,
		},
	})
}

func createAccountCheckout(ctx context.Context, referenceId string, userID int, netAmount int64) (string, error) {
	snapshot := map[string]any{}
	snapshotRaw := strings.TrimSpace(setting.AccountPaymentSnapshot)
	if snapshotRaw != "" {
		if err := common.UnmarshalJsonStr(snapshotRaw, &snapshot); err != nil {
			return "", err
		}
	}
	client := &service.AccountPaymentClient{
		BaseURL:     setting.AccountPaymentBaseURL,
		Environment: setting.AccountPaymentEnvironment,
		KeyID:       setting.AccountPaymentKeyID,
		Secret:      setting.AccountPaymentServiceSecret,
	}
	userRef := strconv.Itoa(userID)
	resp, err := client.CreateOrder(ctx, service.AccountCreateOrderRequest{
		ProductPurchaseRef: referenceId,
		PayerIdentityID:    userRef,
		BeneficiaryRef:     userRef,
		DefinitionVersion:  setting.AccountPaymentDefinitionVersion,
		ItemRef:            setting.AccountPaymentItemRef,
		Description:        setting.AccountPaymentDescription,
		NetAmount:          netAmount,
		Currency:           strings.ToLower(setting.AccountPaymentCurrency),
		CollectionKind:     "one_time",
		Snapshot:           snapshot,
		RequestKey:         referenceId,
	})
	if err != nil {
		return "", err
	}
	if resp.CheckoutURL == "" || !resp.CheckoutAvailable {
		return "", errors.New("account checkout is not available")
	}
	return resp.CheckoutURL, nil
}

func AccountWebhook(c *gin.Context) {
	if !isAccountWebhookEnabled() {
		logger.LogWarn(c.Request.Context(), fmt.Sprintf("Account webhook 被拒绝 reason=webhook_disabled path=%q client_ip=%s", c.Request.RequestURI, c.ClientIP()))
		c.AbortWithStatus(http.StatusForbidden)
		return
	}

	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		logger.LogError(c.Request.Context(), fmt.Sprintf("Account webhook 读取请求体失败 path=%q client_ip=%s error=%q", c.Request.RequestURI, c.ClientIP(), err.Error()))
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	if err = service.VerifyAccountPaymentSignature(c.Request.Method, c.Request.URL, body, setting.AccountPaymentKeyID, setting.AccountPaymentServiceSecret, time.Now(), c.Request.Header); err != nil {
		logger.LogWarn(c.Request.Context(), fmt.Sprintf("Account webhook 验签失败 path=%q client_ip=%s error=%q", c.Request.RequestURI, c.ClientIP(), err.Error()))
		c.AbortWithStatus(http.StatusForbidden)
		return
	}

	var event AccountPaymentEvent
	if err = common.Unmarshal(body, &event); err != nil {
		logger.LogWarn(c.Request.Context(), fmt.Sprintf("Account webhook 解析失败 path=%q client_ip=%s error=%q", c.Request.RequestURI, c.ClientIP(), err.Error()))
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	if err = validateAccountPaymentEvent(&event); err != nil {
		logger.LogWarn(c.Request.Context(), fmt.Sprintf("Account webhook 事件校验失败 event_ref=%s trade_no=%s client_ip=%s error=%q", event.EventRef, event.Data.ProductPurchaseRef, c.ClientIP(), err.Error()))
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	LockOrder(event.Data.ProductPurchaseRef)
	defer UnlockOrder(event.Data.ProductPurchaseRef)
	alreadyDone, err := model.RechargeAccount(event.Data.ProductPurchaseRef, c.ClientIP())
	if err != nil {
		logger.LogError(c.Request.Context(), fmt.Sprintf("Account 充值处理失败 event_ref=%s trade_no=%s client_ip=%s error=%q", event.EventRef, event.Data.ProductPurchaseRef, c.ClientIP(), err.Error()))
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	if alreadyDone {
		logger.LogInfo(c.Request.Context(), fmt.Sprintf("Account 重复通知幂等忽略 event_ref=%s trade_no=%s client_ip=%s", event.EventRef, event.Data.ProductPurchaseRef, c.ClientIP()))
	} else {
		logger.LogInfo(c.Request.Context(), fmt.Sprintf("Account 充值成功 event_ref=%s trade_no=%s client_ip=%s", event.EventRef, event.Data.ProductPurchaseRef, c.ClientIP()))
	}

	digest := sha256.Sum256(body)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"event_ref":      event.EventRef,
		"effect_key":     event.EffectKey,
		"payload_digest": hex.EncodeToString(digest[:]),
		"durable":        true,
	}})
}

func validateAccountPaymentEvent(event *AccountPaymentEvent) error {
	if event == nil {
		return errors.New("missing event")
	}
	if event.Kind != "payment.confirmed" {
		return errors.New("unsupported account event kind")
	}
	if setting.AccountPaymentProjectKey != "" && event.ProjectKey != setting.AccountPaymentProjectKey {
		return errors.New("account project mismatch")
	}
	if setting.AccountPaymentEnvironment != "" && event.Environment != setting.AccountPaymentEnvironment {
		return errors.New("account environment mismatch")
	}
	if event.AggregateRef == "" || event.AggregateRef != event.Data.OrderRef {
		return errors.New("account order reference mismatch")
	}
	if event.Data.ProviderPaymentRef != "" && event.EffectKey != "payment:"+event.Data.ProviderPaymentRef {
		return errors.New("account effect key mismatch")
	}
	topUp := model.GetTopUpByTradeNo(event.Data.ProductPurchaseRef)
	if topUp == nil {
		return model.ErrTopUpNotFound
	}
	if topUp.PaymentProvider != model.PaymentProviderAccount {
		return model.ErrPaymentMethodMismatch
	}
	userRef := strconv.Itoa(topUp.UserId)
	if event.Data.PayerIdentityID != userRef || event.Data.BeneficiaryRef != userRef {
		return errors.New("account user reference mismatch")
	}
	expectedNetAmount, err := accountNetAmountFromMoney(topUp.Money)
	if err != nil {
		return err
	}
	if event.Data.NetAmount != expectedNetAmount {
		return errors.New("account net amount mismatch")
	}
	if !strings.EqualFold(event.Data.Currency, setting.AccountPaymentCurrency) {
		return errors.New("account currency mismatch")
	}
	return nil
}

func accountNetAmountFromMoney(money float64) (int64, error) {
	amount := decimal.NewFromFloat(money).Mul(decimal.NewFromInt(100)).Round(0)
	if amount.LessThanOrEqual(decimal.Zero) {
		return 0, errors.New("invalid account amount")
	}
	return amount.IntPart(), nil
}
