package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/wsw/codex-gateway/internal/httpx"
	"github.com/wsw/codex-gateway/internal/store"
)

const modelRoutesRequestBytes = 16 << 10

type modelRouteRepository interface {
	ListUserModelRoutes(context.Context, string) ([]store.ModelRoute, error)
	ResolveUserModelRoute(context.Context, string, string) (string, bool, error)
	ReplaceUserModelRoutes(context.Context, store.ReplaceUserModelRoutesParams) ([]store.ModelRoute, error)
}

func (s *Server) modelRouteStorage() modelRouteRepository {
	if s.modelRouteRepo != nil {
		return s.modelRouteRepo
	}
	if s.store != nil {
		return s.store
	}
	return nil
}

// applyModelRoute replaces the requested model with the user's routed target
// before any permission, pricing or provider decision, so every later check
// applies to the model that is actually used and billed. CPA performs any
// protocol translation; the gateway only rewrites the top-level model value.
// A failed lookup rejects the request instead of forwarding the unrouted model.
func (s *Server) applyModelRoute(w http.ResponseWriter, r *http.Request, routing *requestRouting, body *countingBody, prepared *preparedAPIRequest) bool {
	key := apiKeyFrom(r.Context())
	repository := s.modelRouteStorage()
	if repository == nil || key.UserID == "" {
		return true
	}
	target, routed, err := repository.ResolveUserModelRoute(r.Context(), key.UserID, routing.Model)
	if err != nil {
		s.logger.Error("resolve model route", "request_id", httpx.RequestID(r.Context()), "error", fmt.Sprintf("%T", err))
		httpx.WriteError(w, r, http.StatusServiceUnavailable, "server_error", "model_route_unavailable", "模型路由暂不可用，请稍后重试")
		return false
	}
	if !routed {
		return true
	}
	size, err := rewriteTopLevelModel(body, *routing, target)
	if err != nil {
		internalError(s, w, r, "rewrite routed model", err)
		return false
	}
	r.ContentLength = size
	prepared.requestedModel = routing.Model
	routing.Model = target
	return true
}

func writeModelRouteIncompatible(w http.ResponseWriter, r *http.Request, source, target, reason string) {
	httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "model_route_incompatible",
		fmt.Sprintf("模型路由 %s → %s 不适用于此接口：%s", source, target, reason))
}

type modelRouteView struct {
	SourceModel string    `json:"source_model"`
	TargetModel string    `json:"target_model"`
	Available   bool      `json:"available"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type modelRoutesResponse struct {
	Routes    []modelRouteView `json:"routes"`
	MaxRoutes int              `json:"max_routes"`
}

func (s *Server) modelRoutesResponse(ctx context.Context, userID string, routes []store.ModelRoute) (modelRoutesResponse, error) {
	allowed, err := s.allowedModelsForAPIKey(ctx, store.APIKey{UserID: userID})
	if err != nil {
		return modelRoutesResponse{}, err
	}
	response := modelRoutesResponse{Routes: make([]modelRouteView, 0, len(routes)), MaxRoutes: store.MaxUserModelRoutes}
	for _, route := range routes {
		_, available := allowed[route.TargetModel]
		response.Routes = append(response.Routes, modelRouteView{
			SourceModel: route.SourceModel, TargetModel: route.TargetModel, Available: available,
			CreatedAt: route.CreatedAt, UpdatedAt: route.UpdatedAt,
		})
	}
	return response, nil
}

func (s *Server) myModelRoutes(w http.ResponseWriter, r *http.Request) {
	repository := s.modelRouteStorage()
	if repository == nil {
		internalError(s, w, r, "list model routes", errors.New("model route storage unavailable"))
		return
	}
	user := userFrom(r.Context())
	routes, err := repository.ListUserModelRoutes(r.Context(), user.ID)
	if err != nil {
		internalError(s, w, r, "list model routes", err)
		return
	}
	response, err := s.modelRoutesResponse(r.Context(), user.ID, routes)
	if err != nil {
		internalError(s, w, r, "resolve model route availability", err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

type modelRouteInput struct {
	SourceModel string `json:"source_model"`
	TargetModel string `json:"target_model"`
}

func (s *Server) replaceMyModelRoutes(w http.ResponseWriter, r *http.Request) {
	repository := s.modelRouteStorage()
	if repository == nil {
		internalError(s, w, r, "replace model routes", errors.New("model route storage unavailable"))
		return
	}
	if !strictJSONRequest(r) {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_model_routes_request", "模型路由请求必须为 JSON，且不能包含查询参数")
		return
	}
	var input *[]modelRouteInput
	if err := decodeSingleJSONField(r, modelRoutesRequestBytes, "routes", &input); err != nil {
		badJSON(w, r, err)
		return
	}
	if input == nil {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_model_routes", "模型路由列表不能为空值")
		return
	}
	user := userFrom(r.Context())
	routes := make([]store.ModelRoute, 0, len(*input))
	for _, item := range *input {
		routes = append(routes, store.ModelRoute{SourceModel: item.SourceModel, TargetModel: item.TargetModel})
	}
	routes, err := store.NormalizeModelRoutes(routes)
	if err != nil {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_model_routes",
			fmt.Sprintf("模型路由无效：最多 %d 条，原模型不能重复、不能与目标相同，目标不能再作为原模型", store.MaxUserModelRoutes))
		return
	}
	allowed, err := s.allowedModelsForAPIKey(r.Context(), store.APIKey{UserID: user.ID})
	if err != nil {
		internalError(s, w, r, "resolve model route targets", err)
		return
	}
	for _, route := range routes {
		if _, ok := allowed[route.TargetModel]; !ok {
			httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "model_route_target_not_allowed",
				fmt.Sprintf("目标模型 %s 不在当前账号可用的模型中", route.TargetModel))
			return
		}
	}
	saved, err := repository.ReplaceUserModelRoutes(r.Context(), store.ReplaceUserModelRoutesParams{
		UserID: user.ID, Routes: routes, At: time.Now().UTC(),
		ActorUserID: user.ID, ActorSessionID: sessionFrom(r.Context()).ID,
		RequestID: httpx.RequestID(r.Context()), SourceIP: safeIP(r.Context()),
	})
	if err != nil {
		s.storeWriteError(w, r, "replace model routes", err)
		return
	}
	response, err := s.modelRoutesResponse(r.Context(), user.ID, saved)
	if err != nil {
		internalError(s, w, r, "resolve model route availability", err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}
