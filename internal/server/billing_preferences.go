package server

import (
	"context"
	"net/http"

	"github.com/wsw/codex-gateway/internal/store"
)

type billingSubscriptionRepository interface {
	PutSubscription(context.Context, store.PutSubscriptionParams) (store.BillingSubscriptionState, error)
	DeleteSubscription(context.Context, store.DeleteSubscriptionParams) (store.BillingSubscriptionState, error)
	SetBillingSubscriptionPreferences(context.Context, store.SetBillingSubscriptionPreferencesParams) (store.BillingSubscriptionState, error)
	SetBillingCashPreferences(context.Context, store.SetBillingCashPreferencesParams) (store.BillingCashPreferences, error)
}

func (s *Server) billingSubscriptionStorage() billingSubscriptionRepository {
	if s.billingSubscriptionRepo != nil {
		return s.billingSubscriptionRepo
	}
	return s.store
}

type billingPreferencesInput struct {
	OperationID   string
	Priority      *int
	ChargeEnabled *bool
}

func (s *Server) decodeBillingPreferences(w http.ResponseWriter, r *http.Request) (billingPreferencesInput, bool) {
	var input billingPreferencesInput
	if !s.decodeBillingPlanFields(w, r, map[string]any{"operation_id": &input.OperationID, "priority": &input.Priority, "charge_enabled": &input.ChargeEnabled}) {
		return input, false
	}
	if validateBillingOperation(input.OperationID, "用户调整消费偏好") != nil || input.Priority == nil || *input.Priority < 1 || *input.Priority > 100 || input.ChargeEnabled == nil {
		s.billingInputError(w, r)
		return input, false
	}
	return input, true
}

func (s *Server) setBillingSubscriptionPreferences(w http.ResponseWriter, r *http.Request) {
	subscriptionID := r.PathValue("subscription_id")
	if !validBillingResourceID(subscriptionID) {
		s.billingInputError(w, r)
		return
	}
	input, ok := s.decodeBillingPreferences(w, r)
	if !ok {
		return
	}
	value, err := s.billingSubscriptionStorage().SetBillingSubscriptionPreferences(r.Context(), store.SetBillingSubscriptionPreferencesParams{
		BillingWriteParams: s.billingWriteParams(r, input.OperationID, "用户调整订阅消费偏好"), UserID: userFrom(r.Context()).ID,
		SubscriptionID: subscriptionID, Priority: *input.Priority, ChargeEnabled: *input.ChargeEnabled,
	})
	if err != nil {
		s.billingStoreError(w, r, "set subscription preferences", err)
		return
	}
	writeJSON(w, http.StatusOK, value)
}

func (s *Server) setBillingCashPreferences(w http.ResponseWriter, r *http.Request) {
	input, ok := s.decodeBillingPreferences(w, r)
	if !ok {
		return
	}
	value, err := s.billingSubscriptionStorage().SetBillingCashPreferences(r.Context(), store.SetBillingCashPreferencesParams{
		BillingWriteParams: s.billingWriteParams(r, input.OperationID, "用户调整余额消费偏好"), UserID: userFrom(r.Context()).ID,
		Priority: *input.Priority, ChargeEnabled: *input.ChargeEnabled,
	})
	if err != nil {
		s.billingStoreError(w, r, "set cash preferences", err)
		return
	}
	writeJSON(w, http.StatusOK, value)
}
