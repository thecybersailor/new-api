package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	c "github.com/QuantumNous/new-api/pkg/catalogcontract"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/relay"
	relaychannel "github.com/QuantumNous/new-api/relay/channel"
	taskplugin "github.com/QuantumNous/new-api/relay/channel/task/jsplugin"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	integration "github.com/QuantumNous/new-api/service/catalogintegration"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// Managed task records deliberately live in the catalog execution journal. The
// legacy poller has timeout refunds; those are not proof of not-charged under
// this contract. Observation uses the same real plugin query/usage/content hooks.
type catalogTaskJournal struct {
	Task         model.Task            `json:"task"`
	Private      model.TaskPrivateData `json:"private"`
	PluginDigest string                `json:"pluginDigest"`
	Usage        map[string]any        `json:"usage,omitempty"`
}

func (a *catalogRelayAdapter) prepareTaskBound(gc *gin.Context, info *relaycommon.RelayInfo, scope integration.Scope, r c.QuoteRequest) (integration.Bound, error) {
	info.CatalogQuoteOnly = true
	if _, taskErr := relay.RelayTaskSubmit(gc, info); taskErr != nil {
		return integration.Bound{}, fmt.Errorf("task quote rejected: %s", taskErr.Message)
	}
	snap := info.TieredBillingSnapshot
	if snap == nil {
		return integration.Bound{}, c.Invalid("QUOTE_BOUND_UNAVAILABLE", "task operation requires a configured usage expression", "configure the existing plugin pricing expression")
	}
	for name, interval := range a.config.UsageBounds {
		if value, exists := snap.UsageFacts[name]; exists {
			raw, err := common.Marshal(value)
			if err != nil {
				return integration.Bound{}, err
			}
			number, err := strconv.ParseFloat(string(raw), 64)
			if err != nil || number < interval.Min || number > interval.Max {
				return integration.Bound{}, c.Invalid("REQUEST_INVALID", "task usage exceeds admitted bounds", "use the operation limits")
			}
		}
	}
	requestInput := billingexpr.RequestInput{Usage: snap.UsageFacts}
	upper, err := billingexpr.CostUpperBound(snap.ExprString, nil, a.config.UsageBounds, requestInput)
	if err != nil {
		return integration.Bound{}, c.Invalid("QUOTE_BOUND_UNAVAILABLE", err.Error(), "supply complete finite usage limits")
	}
	maximum, err := common.QuotaRoundStrict(upper * snap.QuotaPerUnit * snap.GroupRatio)
	if err != nil {
		return integration.Bound{}, err
	}
	maximum = max(maximum, info.PriceData.Quota)
	_, routeHash, err := a.channel(scope)
	if err != nil {
		return integration.Bound{}, err
	}
	frozen := catalogFrozenRelay{Operation: a.config, ChannelID: a.config.ChannelID, RouteHash: routeHash, Price: info.PriceData, OtherRatios: info.PriceData.OtherRatios(), Tiered: snap, MaximumQuota: maximum, QuotaPerUnit: snap.QuotaPerUnit}
	raw, err := common.Marshal(frozen)
	if err != nil {
		return integration.Bound{}, err
	}
	return integration.Bound{Estimated: catalogQuotaMoney(info.PriceData.Quota, snap.QuotaPerUnit), Maximum: catalogQuotaMoney(maximum, snap.QuotaPerUnit), PricingSnapshot: raw, RoundingVersion: "supplier-quota-v1-usd-micros-ceil"}, nil
}

func (a *catalogRelayAdapter) submitTask(ctx context.Context, gc *gin.Context, info *relaycommon.RelayInfo, row model.CatalogStoredQuote, q c.Quote, frozen catalogFrozenRelay) (integration.Outcome, error) {
	result, taskErr := relay.RelayTaskSubmit(gc, info)
	if taskErr != nil {
		return integration.Outcome{}, fmt.Errorf("task submission uncertain: %s", taskErr.Message)
	}
	task := model.InitTask(result.Platform, info)
	task.PrivateData.Key = "" // Credentials are resolved from the authorized channel, never journalled.
	task.PrivateData.UpstreamTaskID = result.UpstreamTaskID
	task.PrivateData.Execution = service.TaskExecutionSnapshotFromContext(gc)
	task.PrivateData.PluginState = result.PluginState
	task.PrivateData.TokenId = a.token.Id
	task.PrivateData.BillingSource = service.BillingSourceWallet
	task.PrivateData.BillingContext = &model.TaskBillingContext{OriginModelName: row.ModelName, TieredSnapshot: frozen.Tiered, GroupRatio: frozen.Price.GroupRatioInfo.GroupRatio}
	task.Data = result.TaskData
	task.Quota = frozen.MaximumQuota
	task.Status = model.TaskStatusSubmitted
	journal := catalogTaskJournal{Task: *task, Private: task.PrivateData}
	pinned, _ := gc.Get(pluginruntime.ContextKeyPinnedPlugin)
	pluginPin, ok := pinned.(pluginruntime.PinnedPlugin)
	if !ok || pluginPin.Plugin == nil {
		return integration.Outcome{}, fmt.Errorf("task plugin identity unavailable")
	}
	journal.PluginDigest = pluginPin.Plugin.Engine.SourceDigest()
	if result.Immediate != nil {
		journal.Task.Status = model.TaskStatus(result.Immediate.Status)
		journal.Task.Progress = result.Immediate.Progress
		journal.Task.FailReason = result.Immediate.Reason
		journal.Private.ResultURL = result.Immediate.Url
		journal.Usage = result.Immediate.UsageFacts
	}
	raw, err := common.Marshal(journal)
	if err != nil {
		return integration.Outcome{}, err
	}
	if err = a.db.Model(&model.CatalogExecution{}).Where("quote_id = ?", q.QuoteID).Updates(map[string]any{"task_id": task.TaskID, "task_data": raw, "execution_state": "running"}).Error; err != nil {
		return integration.Outcome{}, err
	}
	return a.Observe(ctx, row, c.Invocation{ExecutionState: "running"})
}

func (a *catalogRelayAdapter) observeTask(ctx context.Context, row model.CatalogStoredQuote, execution model.CatalogExecution) (integration.Outcome, error) {
	var journal catalogTaskJournal
	if err := common.Unmarshal(execution.TaskData, &journal); err != nil {
		return integration.Outcome{}, err
	}
	task := &journal.Task
	task.PrivateData = journal.Private
	plugin, ok := pluginruntime.DefaultRegistry.Generation().Get(string(task.Platform))
	if !ok || plugin.Engine.SourceDigest() != journal.PluginDigest {
		return integration.Outcome{}, fmt.Errorf("original task plugin content is unavailable")
	}
	var channel model.Channel
	if err := a.db.First(&channel, task.ChannelId).Error; err != nil {
		return integration.Outcome{}, err
	}
	var frozen catalogFrozenRelay
	if err := common.Unmarshal(row.FrozenPricing, &frozen); err != nil {
		return integration.Outcome{}, err
	}
	fingerprint, err := catalogChannelFingerprint(&channel)
	if err != nil || fingerprint != frozen.RouteHash {
		return integration.Outcome{}, fmt.Errorf("original task route changed; restore it before observation")
	}
	adapter := taskplugin.New(plugin)
	adapter.Init(&relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelId: channel.Id, ChannelType: channel.Type, ChannelBaseUrl: channel.GetBaseURL(), ApiKey: channel.Key, ChannelSetting: channel.GetSetting()}})
	if task.Status != model.TaskStatusSuccess && task.Status != model.TaskStatusFailure {
		resp, err := adapter.FetchTask(channel.GetBaseURL(), channel.Key, task, channel.GetSetting().Proxy)
		if err != nil {
			return integration.Outcome{State: "running", ProviderTaskID: task.TaskID}, nil
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return integration.Outcome{State: "running", ProviderTaskID: task.TaskID}, nil
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
		if err != nil || len(body) > 2<<20 {
			return integration.Outcome{}, fmt.Errorf("task observation exceeds limit")
		}
		result, err := adapter.ParseTaskResult(task, resp, body)
		if err != nil {
			return integration.Outcome{State: "running", ProviderTaskID: task.TaskID}, nil
		}
		switch model.TaskStatus(result.Status) {
		case model.TaskStatusSubmitted, model.TaskStatusQueued, model.TaskStatusInProgress, model.TaskStatusSuccess, model.TaskStatusFailure:
		default:
			return integration.Outcome{State: "running", ProviderTaskID: task.TaskID}, nil
		}
		task.Status = model.TaskStatus(result.Status)
		task.Data = body
		task.Progress = result.Progress
		task.FailReason = result.Reason
		if result.Url != "" {
			task.PrivateData.ResultURL = result.Url
		}
		if len(result.PluginState) > 0 {
			task.PrivateData.PluginState = result.PluginState
		}
		journal.Private = task.PrivateData
		journal.Usage = result.UsageFacts
		raw, err := common.Marshal(journal)
		if err != nil {
			return integration.Outcome{}, err
		}
		changed := a.db.Model(&model.CatalogExecution{}).Where("quote_id = ? AND task_version = ?", row.QuoteID, execution.TaskVersion).Updates(map[string]any{"task_data": raw, "task_version": gorm.Expr("task_version + 1")})
		if changed.Error != nil {
			return integration.Outcome{}, changed.Error
		}
		if changed.RowsAffected != 1 {
			return integration.Outcome{}, c.Invalid("INVOCATION_CONFLICT", "task observation advanced concurrently", "read the latest invocation")
		}
	}
	out := integration.Outcome{State: "running", ProviderTaskID: task.TaskID}
	if task.Status != model.TaskStatusSuccess && task.Status != model.TaskStatusFailure {
		return out, nil
	}
	out.State = "succeeded"
	quota := 0
	if task.Status == model.TaskStatusFailure {
		out.State = "failed"
	} else {
		settlement, facts, err := service.EvaluateTaskCompletionUsage(frozen.Tiered, journal.Usage)
		if err != nil || settlement.Clamp != nil {
			return out, fmt.Errorf("task terminal usage needs reconciliation")
		}
		quota = settlement.ActualQuotaAfterGroup
		journal.Usage = facts
	}
	settled := true
	if err := model.SettleCatalogExecution(a.db, row.QuoteID, quota); err != nil {
		if !errors.Is(err, model.ErrCatalogReconciliation) {
			return out, err
		}
		settled = false
	}
	if err := a.db.Where("quote_id = ?", row.QuoteID).Take(&execution).Error; err != nil {
		return out, err
	}
	var q c.Quote
	if err := common.Unmarshal(row.Data, &q); err != nil {
		return out, err
	}
	rawUsage, err := common.Marshal(map[string]any{"status": string(task.Status), "usage": journal.Usage})
	if err != nil {
		return out, err
	}
	out.Receipt = &c.Receipt{SourceID: "dantoken", IntegrationPrincipalID: row.PrincipalID, ReceiptID: q.QuoteID, Revision: 1, AmountMode: "cumulative", IsFinal: settled, QuoteID: q.QuoteID, PricingRevision: q.PricingRevision, RawUsage: rawUsage, Cost: catalogQuotaMoney(quota, frozen.QuotaPerUnit), RawQuota: strconv.Itoa(quota), QuotaPerUnit: strconv.FormatFloat(frozen.QuotaPerUnit, 'f', 0, 64), RoundingVersion: q.RoundingVersion, SettledAt: execution.SettledAt}
	if !execution.SettledAt.IsZero() {
		task.FinishTime = execution.SettledAt.Unix()
	}
	out.Result, err = adapter.ConvertToOpenAIVideo(task)
	if err != nil {
		return out, err
	}
	if task.Status == model.TaskStatusSuccess {
		// Billing evidence is available even when a content fetch temporarily
		// fails. The next observation retries only this read and archive stage.
		artifacts, archiveErr := a.archiveTaskArtifacts(ctx, row, task, adapter)
		if archiveErr == nil {
			out.Artifacts = artifacts
		}
	}

	saved, err := common.Marshal(out)
	if err != nil {
		return out, err
	}
	if err = a.db.Model(&model.CatalogExecution{}).Where("quote_id = ?", row.QuoteID).Updates(map[string]any{"outcome": saved, "native_body": out.Result, "content_type": "application/json", "execution_state": out.State}).Error; err != nil {
		return out, err
	}
	return out, nil
}

func (a *catalogRelayAdapter) archiveTaskArtifacts(ctx context.Context, row model.CatalogStoredQuote, task *model.Task, adapter *taskplugin.TaskAdaptor) ([]c.Artifact, error) {
	var storedArtifacts []c.Artifact

	artifacts, err := adapter.ListArtifacts(task)
	if err != nil {
		return nil, err
	}
	if len(artifacts) == 0 {
		return nil, fmt.Errorf("successful task has no declared artifacts")
	}
	for _, artifact := range artifacts {
		descriptor, err := adapter.BuildContentRequest(task, artifact.Key, relaychannel.TaskArtifactClientRequest{Method: http.MethodGet})
		if err != nil {
			return nil, err
		}
		capture := &catalogResponse{header: make(http.Header), status: 200}
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "/v1/invocations/"+row.ClientRequestID, nil)
		gc := &gin.Context{Request: request, Writer: capture}
		if err = proxyTaskMedia(gc, task, descriptor); err != nil {
			return nil, err
		}
		if capture.overflow {
			return nil, fmt.Errorf("task artifact exceeds limit")
		}
		stored, err := integration.PersistArtifact(a.db, a.root, row.PrincipalID, row.ClientRequestID, artifact.Type, capture.Header().Get("Content-Type"), capture.body.Bytes())
		if err != nil {
			return nil, err
		}
		storedArtifacts = append(storedArtifacts, stored)
	}
	return storedArtifacts, nil
}
