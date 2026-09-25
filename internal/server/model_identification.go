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
	modelIdentificationProbeTimeout = 180 * time.Second
	modelIdentificationListTimeout  = 10 * time.Second
)

type modelIdentificationRepository interface {
	BeginModelIdentificationRun(context.Context, string, string) (store.ModelIdentification, error)
	AdvanceModelIdentificationRun(context.Context, string, int) (store.ModelIdentification, error)
	CompleteModelIdentificationRun(context.Context, string, store.ModelIdentificationResult) (store.ModelIdentification, error)
	FailModelIdentificationRun(context.Context, string, string) (store.ModelIdentification, error)
	ListModelIdentifications(context.Context) ([]store.ModelIdentification, error)
	RecoverInterruptedModelIdentificationRuns(context.Context) (int64, error)
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
	if _, err := repository.RecoverInterruptedModelIdentificationRuns(r.Context()); err != nil {
		internalError(s, w, r, "recover interrupted model identifications", err)
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
	result := modelIdentificationOptionsDTO{
		Accounts: []modelIdentificationAccountOption{},
		Models:   []string{}, ReferenceVersion: modelid.ReferenceVersion,
	}
	if s.upstream == nil {
		return result, errors.New("upstream client unavailable")
	}
	accounts, err := s.upstream.ListUpstreamAccounts(ctx)
	if err != nil {
		return result, err
	}
	accountAvailable := false
	for _, account := range accounts {
		status := account.Status
		if !upstreamAccountSourceStatusKnown(account) {
			status = "unavailable"
		}
		result.Accounts = append(result.Accounts, modelIdentificationAccountOption{
			ID: account.ID, MaskedEmail: account.MaskedEmail, Status: status,
		})
		if account.ID == accountID && status == "available" {
			accountAvailable = true
		}
	}
	if accountID == "" {
		return result, nil
	}
	if !accountAvailable {
		return result, errModelIdentificationAccountUnavailable
	}
	models, err := s.upstream.ListUpstreamAccountModels(ctx, accountID)
	if err != nil {
		return result, err
	}
	result.AccountID = accountID
	for _, model := range models {
		if s.config.UsagePricing.IsManageableModel(model) {
			if _, antigravity := s.config.AntigravityModelRoutes[model]; !antigravity {
				result.Models = append(result.Models, model)
			}
		}
	}
	sort.Strings(result.Models)
	return result, nil
}

var (
	errModelIdentificationAccountUnavailable = errors.New("model identification account unavailable")
	errModelIdentificationModelUnavailable   = errors.New("model identification model unavailable")
)

func (s *Server) modelIdentificationOptionsError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, errModelIdentificationAccountUnavailable) {
		httpx.WriteError(w, r, http.StatusConflict, "invalid_request_error", "upstream_account_unavailable", "所选上游账号目前不可用")
		return
	}
	if errors.Is(err, errModelIdentificationModelUnavailable) {
		httpx.WriteError(w, r, http.StatusConflict, "invalid_request_error", "model_unavailable", "所选模型不再可用")
		return
	}
	var upstreamErr *gatewayproxy.InternalAPIError
	if errors.As(err, &upstreamErr) {
		httpx.WriteError(w, r, http.StatusBadGateway, "upstream_error", upstreamErr.SafeCode(), "上游账号模型信息暂不可用")
		return
	}
	internalError(s, w, r, "load model identification options", err)
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
	if !validUpstreamAccountID(input.AccountID) || !validModel(input.Model) ||
		!s.config.UsagePricing.IsManageableModel(input.Model) {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_model_identification_selection", "请选择有效的上游账号与已配置模型")
		return
	}
	if _, antigravity := s.config.AntigravityModelRoutes[input.Model]; antigravity {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "model_identification_bridge_unsupported", "独立桥接模型无法使用此鉴别方式")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), modelIdentificationListTimeout)
	options, err := s.resolveModelIdentificationOptions(ctx, input.AccountID)
	cancel()
	if err != nil {
		s.modelIdentificationOptionsError(w, r, err)
		return
	}
	if !modelIdentificationHasModel(options.Models, input.Model) {
		s.modelIdentificationOptionsError(w, r, errModelIdentificationModelUnavailable)
		return
	}
	syncCtx, syncCancel := context.WithTimeout(r.Context(), modelIdentificationListTimeout)
	err = s.syncModelIdentificationAccounts(syncCtx)
	syncCancel()
	if err != nil {
		s.modelIdentificationOptionsError(w, r, err)
		return
	}
	repository := s.modelIdentificationStorage()
	if repository == nil {
		internalError(s, w, r, "begin model identification", errors.New("model identification repository unavailable"))
		return
	}
	run, err := repository.BeginModelIdentificationRun(r.Context(), input.AccountID, input.Model)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.modelIdentificationOptionsError(w, r, errModelIdentificationAccountUnavailable)
			return
		}
		if errors.Is(err, store.ErrConflict) {
			httpx.WriteError(w, r, http.StatusConflict, "invalid_request_error", "model_identification_running", "已有模型鉴别正在运行，请等待完成")
			return
		}
		internalError(s, w, r, "begin model identification", err)
		return
	}
	s.identificationWG.Add(1)
	go func() {
		defer s.identificationWG.Done()
		s.runModelIdentification(run.RunID, input.AccountID, input.Model)
	}()
	writeJSON(w, http.StatusAccepted, map[string]any{"run_id": run.RunID})
}

// The run reservation has a foreign key to the gateway's account snapshot.
// Refresh it from the authoritative sidecar immediately before reserving.
func (s *Server) syncModelIdentificationAccounts(ctx context.Context) error {
	if s.store == nil {
		return nil
	}
	s.upstreamAccountSyncMu.Lock()
	defer s.upstreamAccountSyncMu.Unlock()
	accounts, err := s.upstream.ListUpstreamAccounts(ctx)
	if err != nil {
		return err
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

func (s *Server) runModelIdentification(runID, accountID, model string) {
	parent := s.identificationContext
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, modelIdentificationRunTimeout)
	defer cancel()
	code := "model_identification_interrupted"
	completed := false
	defer func() {
		if recover() != nil {
			code = "model_identification_interrupted"
		}
		if completed {
			return
		}
		failureCtx, failureCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer failureCancel()
		if _, err := s.modelIdentificationStorage().FailModelIdentificationRun(failureCtx, runID, code); err != nil && s.logger != nil {
			s.logger.Error("record model identification failure", "run_id", runID, "error", err)
		}
	}()

	challenges := modelid.Challenges()
	if len(challenges) != 3 {
		code = "model_identification_reference_unavailable"
		return
	}
	replies := make([]string, 0, len(challenges))
	for index, challenge := range challenges {
		if ctx.Err() != nil {
			code = modelIdentificationContextCode(ctx.Err())
			return
		}
		checkCtx, checkCancel := context.WithTimeout(ctx, modelIdentificationListTimeout)
		options, err := s.resolveModelIdentificationOptions(checkCtx, accountID)
		checkCancel()
		if err != nil {
			code = modelIdentificationFailureCode(err)
			if ctx.Err() != nil {
				code = modelIdentificationContextCode(ctx.Err())
			}
			return
		}
		if !modelIdentificationHasModel(options.Models, model) {
			code = "model_identification_model_unavailable"
			return
		}
		probeCtx, probeCancel := context.WithTimeout(ctx, modelIdentificationProbeTimeout)
		reply, err := s.upstream.ProbeUpstreamAccount(probeCtx, accountID, model, challenge.Prompt)
		probeCancel()
		if err != nil {
			code = modelIdentificationFailureCode(err)
			if ctx.Err() != nil {
				code = modelIdentificationContextCode(ctx.Err())
			}
			return
		}
		replies = append(replies, reply)
		if _, err := s.modelIdentificationStorage().AdvanceModelIdentificationRun(ctx, runID, index+1); err != nil {
			code = "model_identification_storage_failed"
			return
		}
	}
	result, err := modelid.Score(replies)
	if err != nil {
		code = "model_identification_invalid_answer"
		return
	}
	conclusion := result.ClosestModel
	if result.Level == "family_only" {
		conclusion = result.Family
	} else if result.Level == "insufficient" {
		conclusion = "无法可靠判定"
	}
	finishCtx, finishCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer finishCancel()
	if _, err := s.modelIdentificationStorage().CompleteModelIdentificationRun(finishCtx, runID, store.ModelIdentificationResult{
		Conclusion: conclusion, ClosestModel: result.ClosestModel, MatchLevel: result.Level,
		Fit: result.Fit, Margin: result.Margin, ReferenceVersion: result.BankVersion,
	}); err != nil {
		code = "model_identification_storage_failed"
		return
	}
	completed = true
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
	if errors.Is(err, context.DeadlineExceeded) {
		return "model_identification_timeout"
	}
	if errors.Is(err, errModelIdentificationAccountUnavailable) {
		return "model_identification_account_unavailable"
	}
	if errors.Is(err, errModelIdentificationModelUnavailable) {
		return "model_identification_model_unavailable"
	}
	var upstreamErr *gatewayproxy.InternalAPIError
	if errors.As(err, &upstreamErr) {
		switch upstreamErr.SafeCode() {
		case "probe_account_mismatch":
			return "model_identification_account_mismatch"
		case "sidecar_timeout", "upstream_quota_timeout":
			return "model_identification_timeout"
		case "upstream_account_disabled", "upstream_account_unavailable", "invalid_upstream_account":
			return "model_identification_account_unavailable"
		case "model_unavailable":
			return "model_identification_model_unavailable"
		default:
			return "model_identification_probe_failed"
		}
	}
	return "model_identification_probe_failed"
}
