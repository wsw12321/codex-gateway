package server

import (
    "net/http"
    "strings"

    "github.com/wsw/codex-gateway/internal/store"
)

type groupPriorityInput struct { OperationID string `json:"operation_id"`; Reason string `json:"reason"`; Priority int `json:"priority"` }
type groupUpstreamInput struct { OperationID string `json:"operation_id"`; Reason string `json:"reason"`; Strategy string `json:"strategy"`; Mode string `json:"mode"`; AccountIDs []string `json:"account_ids"` }

func (s *Server) setMyGroupPreference(w http.ResponseWriter, r *http.Request) {
    var in groupPriorityInput
    if err:=decodeJSON(w,r,&in,16<<10);err!=nil{badJSON(w,r,err);return}
    if validateBillingOperation(in.OperationID,in.Reason)!=nil || in.Priority<1 || in.Priority>100 { s.billingInputError(w,r);return }
    value,err:=s.store.SetGroupMembershipPriority(r.Context(),store.SetGroupMembershipPreferenceParams{BillingWriteParams:s.billingWriteParams(r,in.OperationID,strings.TrimSpace(in.Reason)),GroupID:r.PathValue("group_id"),UserID:userFrom(r.Context()).ID,Priority:in.Priority})
    if err!=nil{s.billingStoreError(w,r,"set group preference",err);return};writeJSON(w,http.StatusOK,value)
}

func (s *Server) setGroupUpstreamPolicy(w http.ResponseWriter,r *http.Request){
    var in groupUpstreamInput
    if err:=decodeJSON(w,r,&in,64<<10);err!=nil{badJSON(w,r,err);return}
    if validateBillingOperation(in.OperationID,in.Reason)!=nil {s.billingInputError(w,r);return}
    strategy := in.Strategy; if strategy == "" { strategy = in.Mode }
    value,err:=s.store.SetGroupUpstreamAccountPolicy(r.Context(),store.SetGroupUpstreamAccountPolicyParams{BillingWriteParams:s.billingWriteParams(r,in.OperationID,strings.TrimSpace(in.Reason)),GroupID:r.PathValue("id"),Provider:r.PathValue("provider"),Strategy:strategy,AccountIDs:in.AccountIDs})
    if err!=nil{s.groupStoreError(w,r,"set group upstream policy",err);return};writeJSON(w,http.StatusOK,value)
}

func (s *Server) groupUpstreamPolicy(w http.ResponseWriter,r *http.Request){
    value,err:=s.store.GetGroupUpstreamAccounts(r.Context(),r.PathValue("id"),r.PathValue("provider"));if err!=nil{s.groupStoreError(w,r,"get group upstream policy",err);return};writeJSON(w,http.StatusOK,value)
}

func (s *Server) groupPeriods(w http.ResponseWriter,r *http.Request){
    values,err:=s.store.ListGroupUsagePeriods(r.Context(),r.PathValue("id"),50);if err!=nil{s.groupStoreError(w,r,"list group periods",err);return};writeJSON(w,http.StatusOK,map[string]any{"periods":values})
}

func (s *Server) groupAccountStats(w http.ResponseWriter,r *http.Request){
    ids:=append([]string(nil),r.URL.Query()["period_id"]...);if len(ids)==0 {if raw:=r.URL.Query().Get("period_ids");raw!=""{ids=strings.Split(raw,",")}}
    values,err:=s.store.GroupAccountStats(r.Context(),r.PathValue("id"),ids);if err!=nil{s.groupStoreError(w,r,"group account statistics",err);return}; complete:=true; for _,v:=range values { if !v.DataComplete { complete=false } }; writeJSON(w,http.StatusOK,map[string]any{"group_id":r.PathValue("id"),"stats":values,"accounts":values,"data_complete":complete})
}
