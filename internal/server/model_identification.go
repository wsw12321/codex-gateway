package server

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/wsw/codex-gateway/internal/httpx"
	"github.com/wsw/codex-gateway/internal/modelid"
	gatewayproxy "github.com/wsw/codex-gateway/internal/proxy"
	"github.com/wsw/codex-gateway/internal/store"
)

const (
	modelIdentificationRunTimeout   = 10 * time.Minute
	modelIdentificationProbeTimeout = gatewayproxy.ModelIdentificationProbeTimeout
	modelIdentificationListTimeout  = 10 * time.Second
)

type modelIdentificationRepository interface {
	BeginModelIdentificationRun(context.Context, string, string, ...string) (store.ModelIdentification, error)
	UpdateModelIdentificationRun(context.Context, string, string, int, int) (store.ModelIdentification, error)
	GetModelIdentificationRun(context.Context, string) (store.ModelIdentification, error)
	CompleteModelIdentificationRun(context.Context, string, store.ModelIdentificationResult) (store.ModelIdentification, error)
	FailModelIdentificationRun(context.Context, string, string, ...store.ModelIdentificationFailure) (store.ModelIdentification, error)
	ListModelIdentifications(context.Context) ([]store.ModelIdentification, error)
}

func (s *Server) modelIdentificationStorage() modelIdentificationRepository {
	if s.modelIdentificationRepo != nil {
		return s.modelIdentificationRepo
	}
	return s.store
}

type modelIdentificationAccountOption struct {
	ID          string `json:"id"`
	MaskedEmail string `json:"masked_email"`
	Status      string `json:"status"`
}

type modelIdentificationOptionsDTO struct {
	Accounts         []modelIdentificationAccountOption `json:"accounts"`
	AccountID        string                             `json:"account_id,omitempty"`
	Models           []string                           `json:"models"`
	ReferenceVersion string                             `json:"reference_version"`
}

func (s *Server) modelIdentificationsJSON(w http.ResponseWriter, r *http.Request) {
	repository := s.modelIdentificationStorage()
	if repository == nil {
		internalError(s, w, r, "list model identifications", errors.New("model identification repository unavailable"))
		return
	}
	items, err := repository.ListModelIdentifications(r.Context())
	if err != nil {
		internalError(s, w, r, "list model identifications", err)
		return
	}
	if items == nil {
		items = []store.ModelIdentification{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"identifications":   items,
		"reference_version": modelid.ReferenceVersion,
	})
}

func (s *Server) modelIdentificationOptions(w http.ResponseWriter, r *http.Request) {
	accountID := r.URL.Query().Get("account_id")
	if accountID != "" && !validUpstreamAccountID(accountID) {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_upstream_account", "上游账号 ID 无效")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), modelIdentificationListTimeout)
	defer cancel()
	options, err := s.resolveModelIdentificationOptions(ctx, accountID)
	if err != nil {
		s.modelIdentificationOptionsError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, options)
}

func (s *Server) resolveModelIdentificationOptions(ctx context.Context, accountID string) (modelIdentificationOptionsDTO, error) {
	result := modelIdentificationOptionsDTO{Accounts: []modelIdentificationAccountOption{}, Models: []string{}, ReferenceVersion: modelid.ReferenceVersion}
	if s.upstream == nil {
		return result, errors.New("upstream client unavailable")
	}
	if err := s.upstream.RequireModelIdentificationCapability(ctx); err != nil {
		return result, err
	}
	accounts, err := s.upstream.ListUpstreamAccounts(ctx)
	if err != nil {
		return result, err
	}
	found := false
	for _, account := range accounts {
		result.Accounts = append(result.Accounts, modelIdentificationAccountOption{ID: account.ID, MaskedEmail: account.MaskedEmail, Status: account.Status})
		found = found || account.ID == accountID
	}
	if accountID == "" {
		return result, nil
	}
	if !found {
		return result, errModelIdentificationAccountUnavailable
	}
	models, err := s.upstream.ListModelIdentificationModels(ctx, accountID)
	if err != nil {
		return result, err
	}
	result.AccountID = accountID
	result.Models = models
	sort.Strings(result.Models)
	return result, nil
}

var (
	errModelIdentificationAccountUnavailable = errors.New("model identification account unavailable")
	errModelIdentificationModelUnavailable   = errors.New("model identification model unavailable")
)

func (s *Server) modelIdentificationOptionsError(w http.ResponseWriter, r *http.Request, err error) {
	code := modelIdentificationFailureCode(err)
	status := http.StatusBadGateway
	if code == "model_identification_protocol_unsupported" {
		status = http.StatusServiceUnavailable
	}
	if errors.Is(err, errModelIdentificationAccountUnavailable) || errors.Is(err, errModelIdentificationModelUnavailable) {
		status = http.StatusConflict
	}
	httpx.WriteError(w, r, status, "upstream_error", code, "模型鉴定准备失败，请刷新后重试")
}

func (s *Server) modelIdentificationRunJSON(w http.ResponseWriter, r *http.Request) {
	repository := s.modelIdentificationStorage()
	if repository == nil {
		internalError(s, w, r, "get model identification", errors.New("repository unavailable"))
		return
	}
	run, err := repository.GetModelIdentificationRun(r.Context(), r.PathValue("run_id"))
	if errors.Is(err, store.ErrNotFound) {
		httpx.WriteError(w, r, http.StatusNotFound, "invalid_request_error", "model_identification_run_not_found", "该鉴定任务不存在或已被新任务替代")
		return
	}
	if err != nil {
		internalError(s, w, r, "get model identification", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": run})
}

type modelIdentificationRunInput struct {
	AccountID string `json:"account_id"`
	Model     string `json:"model"`
}

func (s *Server) createModelIdentificationRun(w http.ResponseWriter, r *http.Request) {
	var input modelIdentificationRunInput
	if err := decodeJSON(w, r, &input, 4096); err != nil {
		badJSON(w, r, err)
		return
	}
	if !validUpstreamAccountID(input.AccountID) || !gatewayproxy.ValidModelIdentificationModel(input.Model) {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_model_identification_selection", "请选择有效的上游账号与模型")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), modelIdentificationListTimeout)
	err := s.syncModelIdentificationAccounts(ctx, input.AccountID)
	cancel()
	if err != nil {
		s.modelIdentificationOptionsError(w, r, err)
		return
	}
	repository := s.modelIdentificationStorage()
	if repository == nil {
		internalError(s, w, r, "begin model identification", errors.New("repository unavailable"))
		return
	}
	actorID := userFrom(r.Context()).ID
	run, err := repository.BeginModelIdentificationRun(r.Context(), input.AccountID, input.Model, actorID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.modelIdentificationOptionsError(w, r, errModelIdentificationAccountUnavailable)
			return
		}
		if errors.Is(err, store.ErrConflict) {
			httpx.WriteError(w, r, http.StatusConflict, "invalid_request_error", "model_identification_running", "已有模型鉴定正在运行，请等待完成")
			return
		}
		internalError(s, w, r, "begin model identification", err)
		return
	}
	s.identificationWG.Add(1)
	go func() { defer s.identificationWG.Done(); s.runModelIdentification(run) }()
	writeJSON(w, http.StatusAccepted, map[string]any{"run_id": run.RunID, "run": run})
}

// The run reservation has a foreign key to the gateway's account snapshot.
// Refresh it from the authoritative sidecar immediately before reserving.
func (s *Server) syncModelIdentificationAccounts(ctx context.Context, accountID string) error {
	if s.upstream == nil {
		return errors.New("upstream client unavailable")
	}
	s.upstreamAccountSyncMu.Lock()
	defer s.upstreamAccountSyncMu.Unlock()
	accounts, err := s.upstream.ListUpstreamAccounts(ctx)
	if err != nil {
		return err
	}
	found := false
	for _, account := range accounts {
		found = found || account.ID == accountID
	}
	if !found {
		return errModelIdentificationAccountUnavailable
	}
	if s.store == nil {
		return nil
	}
	snapshots := make([]store.UpstreamAccountSnapshot, 0, len(accounts))
	for _, account := range accounts {
		status := store.UpstreamAccountStatusUnavailable
		if account.Status == "available" && upstreamAccountSourceStatusKnown(account) {
			status = store.UpstreamAccountStatusAvailable
		}
		snapshots = append(snapshots, store.UpstreamAccountSnapshot{
			ID: account.ID, MaskedEmail: account.MaskedEmail, Plan: account.Plan,
			Status: status, LastSyncedAt: account.LastSyncedAt,
		})
	}
	return s.store.SyncUpstreamAccounts(ctx, snapshots, time.Now().UTC())
}

func modelIdentificationHasModel(models []string, model string) bool {
	for _, candidate := range models {
		if candidate == model {
			return true
		}
	}
	return false
}

func (s *Server) runModelIdentification(run store.ModelIdentification) {
	parent := s.identificationContext
	if parent == nil {
		parent = context.Background()
	}
	deadline := time.Now().Add(modelIdentificationRunTimeout)
	if run.RunStartedAt != nil {
		deadline = run.RunStartedAt.Add(modelIdentificationRunTimeout)
	}
	ctx, cancel := context.WithDeadline(parent, deadline)
	defer cancel()
	code := "model_identification_interrupted"
	detail := store.ModelIdentificationFailure{Source: "gateway"}
	completed := false
	stage, probeIndex := "preflight", 0
	sidecarStage := ""
	elapsed := func() int64 {
		if run.RunStartedAt == nil {
			return 0
		}
		return max(0, time.Since(*run.RunStartedAt).Milliseconds())
	}
	defer func() {
		if recover() != nil {
			code = "model_identification_interrupted"
			detail = store.ModelIdentificationFailure{Source: "gateway"}
		}
		if completed {
			return
		}
		failureCtx, failureCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer failureCancel()
		_, err := s.modelIdentificationStorage().FailModelIdentificationRun(failureCtx, run.RunID, code, detail)
		if s.logger != nil {
			s.logger.Warn("model identification failed", "run_id", run.RunID, "actor_id", run.RunActorID, "account_id", run.AccountID, "stage", stage, "sidecar_stage", sidecarStage, "probe_index", probeIndex, "code", code, "source", detail.Source, "upstream_status", detail.UpstreamStatus, "retry_after", detail.RetryAfter, "elapsed_ms", elapsed(), "failure_recorded", err == nil)
		}
	}()
	fail := func(err error) {
		code, detail = modelIdentificationFailure(err)
		var diagnostic *gatewayproxy.ModelIdentificationError
		if errors.As(err, &diagnostic) {
			switch diagnostic.Stage {
			case "preflight", "probing", "validating":
				sidecarStage = diagnostic.Stage
			}
		}
		if ctx.Err() != nil {
			code = modelIdentificationContextCode(ctx.Err())
			detail = store.ModelIdentificationFailure{Source: "gateway"}
		}
	}
	update := func(next string, index, progress int) bool {
		if ctx.Err() != nil {
			fail(ctx.Err())
			return false
		}
		if _, err := s.modelIdentificationStorage().UpdateModelIdentificationRun(ctx, run.RunID, next, index, progress); err != nil {
			code = "model_identification_storage_failed"
			detail = store.ModelIdentificationFailure{Source: "storage"}
			return false
		}
		stage, probeIndex = next, index
		return true
	}
	if !update("preflight", 0, 0) {
		return
	}
	if s.upstream == nil {
		code = "model_identification_unavailable"
		return
	}
	checkCtx, checkCancel := context.WithTimeout(ctx, modelIdentificationListTimeout)
	err := s.upstream.RequireModelIdentificationCapability(checkCtx)
	var models []string
	if err == nil {
		models, err = s.upstream.ListModelIdentificationModels(checkCtx, run.AccountID)
	}
	checkCancel()
	if err != nil {
		fail(err)
		return
	}
	if !modelIdentificationHasModel(models, run.RequestedModel) {
		fail(errModelIdentificationModelUnavailable)
		return
	}
	challenges := modelid.Challenges()
	if len(challenges) != 3 {
		code = "model_identification_reference_unavailable"
		return
	}
	replies := make([]string, 0, len(challenges))
	for index, challenge := range challenges {
		if !update("probing", index+1, index) {
			return
		}
		probeCtx, probeCancel := context.WithTimeout(ctx, modelIdentificationProbeTimeout)
		reply, err := s.upstream.ProbeModelIdentification(probeCtx, gatewayproxy.ModelIdentificationProbe{
			RunID: run.RunID, UserID: run.RunActorID, AccountID: run.AccountID, Model: run.RequestedModel, Prompt: challenge.Prompt, ProbeIndex: index + 1,
		})
		probeCancel()
		if err != nil {
			fail(err)
			return
		}
		if !update("validating", index+1, index) {
			return
		}
		replies = append(replies, reply)
		if err := modelid.ValidateRepliesPrefix(replies); err != nil {
			code = "model_identification_invalid_answer"
			detail = store.ModelIdentificationFailure{Source: "validation"}
			return
		}
	}
	if !update("scoring", 3, 3) {
		return
	}
	result, err := modelid.Score(replies)
	if err != nil {
		code = "model_identification_invalid_answer"
		detail = store.ModelIdentificationFailure{Source: "validation"}
		return
	}
	conclusion := result.ClosestModel
	if result.Level == "family_only" {
		conclusion = result.Family
	} else if result.Level == "insufficient" {
		conclusion = "无法可靠判定"
	}
	if !update("saving", 3, 3) {
		return
	}
	finishCtx, finishCancel := context.WithTimeout(ctx, 5*time.Second)
	defer finishCancel()
	if _, err := s.modelIdentificationStorage().CompleteModelIdentificationRun(finishCtx, run.RunID, store.ModelIdentificationResult{
		Conclusion: conclusion, ClosestModel: result.ClosestModel, MatchLevel: result.Level, Fit: result.Fit, Margin: result.Margin, ReferenceVersion: result.BankVersion,
	}); err != nil {
		code = "model_identification_storage_failed"
		detail = store.ModelIdentificationFailure{Source: "storage"}
		if ctx.Err() != nil {
			fail(ctx.Err())
		}
		return
	}
	completed = true
	if s.logger != nil {
		s.logger.Info("model identification succeeded", "run_id", run.RunID, "actor_id", run.RunActorID, "account_id", run.AccountID, "stage", "saving", "probe_index", 3, "elapsed_ms", elapsed())
	}
}

func modelIdentificationContextCode(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "model_identification_timeout"
	}
	return "model_identification_interrupted"
}

// StopModelIdentifications cancels in-memory runs during a graceful shutdown.
// The worker records a terminal failure with a fresh short database context.
func (s *Server) StopModelIdentifications(ctx context.Context) error {
	if s.identificationCancel != nil {
		s.identificationCancel()
	}
	completed := make(chan struct{})
	go func() {
		s.identificationWG.Wait()
		close(completed)
	}()
	select {
	case <-completed:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func modelIdentificationFailureCode(err error) string {
	code, _ := modelIdentificationFailure(err)
	return code
}

func modelIdentificationFailure(err error) (string, store.ModelIdentificationFailure) {
	detail := store.ModelIdentificationFailure{Source: "gateway"}
	// Account discovery still uses the ordinary metadata client. Normalize its
	// local error codes too, without exposing the sidecar status as an upstream
	// status or retaining a potentially sensitive wrapped parsing error.
	var metadataErr *gatewayproxy.InternalAPIError
	if errors.As(err, &metadataErr) {
		detail.Source = "sidecar"
		suffix := "probe_failed"
		switch metadataErr.SafeCode() {
		case "sidecar_timeout", "upstream_quota_timeout":
			suffix = "timeout"
		case "sidecar_unavailable":
			suffix = "network_failed"
		case "sidecar_invalid_response":
			suffix = "invalid_response"
		case "sidecar_auth_failed", "sidecar_auth_unavailable", "sidecar_account_registry_unavailable":
			suffix = "unavailable"
		case "invalid_upstream_account":
			suffix = "account_not_found"
		}
		return "model_identification_" + suffix, detail
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "model_identification_timeout", detail
	}
	if errors.Is(err, context.Canceled) {
		return "model_identification_interrupted", detail
	}
	if errors.Is(err, errModelIdentificationAccountUnavailable) {
		return "model_identification_account_not_found", detail
	}
	if errors.Is(err, errModelIdentificationModelUnavailable) {
		return "model_identification_model_unavailable", detail
	}
	var diagnostic *gatewayproxy.ModelIdentificationError
	if errors.As(err, &diagnostic) {
		detail.Source = "sidecar"
		if diagnostic.UpstreamStatus >= 100 && diagnostic.UpstreamStatus <= 599 {
			detail.UpstreamStatus = diagnostic.UpstreamStatus
			detail.Source = "upstream"
		}
		if diagnostic.RetryAfter > 0 && diagnostic.RetryAfter <= 3600 {
			detail.RetryAfter = diagnostic.RetryAfter
		}
		suffix := "probe_failed"
		switch diagnostic.SafeCode() {
		case "protocol_unsupported":
			suffix = "protocol_unsupported"
		case "probe_request_invalid", "probe_actor_invalid", "probe_context_invalid":
			suffix = "request_invalid"
		case "probe_account_not_found":
			suffix = "account_not_found"
		case "probe_account_mismatch":
			suffix = "account_mismatch"
		case "probe_credential_unavailable":
			suffix = "credential_missing"
		case "probe_model_unavailable", "probe_model_unsupported":
			suffix = "model_unavailable"
		case "probe_unavailable":
			suffix = "unavailable"
		case "probe_timeout":
			suffix = "timeout"
		case "probe_canceled":
			suffix = "interrupted"
		case "probe_authentication_failed":
			suffix = "auth_failed"
		case "probe_rate_limited":
			suffix = "rate_limited"
		case "probe_upstream_unavailable":
			suffix = "upstream_failed"
		case "probe_upstream_rejected":
			suffix = "upstream_rejected"
			if diagnostic.UpstreamStatus == 403 {
				suffix = "forbidden"
			} else if diagnostic.UpstreamStatus >= 300 && diagnostic.UpstreamStatus <= 399 {
				suffix = "redirect"
			}
		case "probe_network_failed":
			suffix = "network_failed"
		case "probe_response_incomplete":
			suffix = "incomplete"
		case "probe_invalid_response":
			suffix = "invalid_response"
		case "probe_response_too_large", "probe_output_too_large", "probe_output_tokens_exceeded":
			suffix = "response_too_large"
		case "probe_unexpected_tool":
			suffix = "unexpected_tool"
		case "probe_empty_output":
			suffix = "empty_output"
		}
		return "model_identification_" + suffix, detail
	}
	return "model_identification_probe_failed", detail
}
