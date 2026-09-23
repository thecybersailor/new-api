package setting

var (
	AccountPaymentBaseURL           string
	AccountPaymentProjectKey        string
	AccountPaymentEnvironment       string = "test"
	AccountPaymentKeyID             string
	AccountPaymentServiceSecret     string
	AccountPaymentDefinitionVersion string
	AccountPaymentItemRef           string = "topup"
	AccountPaymentDescription       string = "New API credits"
	AccountPaymentCurrency          string = "usd"
	AccountPaymentSnapshot          string = "{}"
	AccountPaymentMinTopUp          int    = 1
)
