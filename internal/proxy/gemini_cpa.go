package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"

	"github.com/wsw/codex-gateway/internal/cpaprotocol"
)

type nativeGeminiFrame struct {
	ModelVersion string `json:"modelVersion"`
	ResponseID   string `json:"responseId"`
	Candidates   []struct {
		Index        int    `json:"index"`
		FinishReason string `json:"finishReason"`
		Content      struct {
			Role  string                       `json:"role"`
			Parts []map[string]json.RawMessage `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
	Usage json.RawMessage `json:"usageMetadata"`
}

func nativeGeminiUsage(data []byte) (Usage, error) {
	if err := nativeUsageCounts(data, "promptTokenCount", "candidatesTokenCount", "thoughtsTokenCount", "cachedContentTokenCount", "totalTokenCount"); err != nil {
		return Usage{}, err
	}
	var u struct {
		Prompt     *int64 `json:"promptTokenCount"`
		Candidates *int64 `json:"candidatesTokenCount"`
		Thoughts   int64  `json:"thoughtsTokenCount"`
		Cached     int64  `json:"cachedContentTokenCount"`
		Total      *int64 `json:"totalTokenCount"`
	}
	if json.Unmarshal(data, &u) != nil || u.Prompt == nil || u.Candidates == nil || *u.Prompt < 0 || *u.Candidates < 0 || u.Thoughts < 0 || u.Cached < 0 || u.Cached > *u.Prompt || *u.Candidates > math.MaxInt64-u.Thoughts {
		return Usage{}, errors.New("invalid native Gemini usage")
	}
	out := *u.Candidates + u.Thoughts
	if *u.Prompt > math.MaxInt64-out || *u.Prompt+out == 0 || u.Total != nil && *u.Total != *u.Prompt+out {
		return Usage{}, errors.New("inconsistent native Gemini usage")
	}
	return Usage{InputTokens: *u.Prompt, CachedTokens: u.Cached, OutputTokens: out, ReasoningTokens: u.Thoughts}, nil
}

func nativeUsageCounts(data []byte, keys ...string) error {
	fields, err := cpaprotocol.Object(data)
	if err != nil {
		return errors.New("invalid native usage object")
	}
	for _, key := range keys {
		if raw, ok := fields[key]; ok {
			var count *int64
			if json.Unmarshal(raw, &count) != nil || count == nil || *count < 0 {
				return errors.New("invalid native usage count")
			}
		}
	}
	return nil
}

func parseNativeGemini(data []byte, tools map[string]struct{}) (nativeGeminiFrame, bool, error) {
	var f nativeGeminiFrame
	fields, err := cpaprotocol.Object(data)
	if err != nil || fields["error"] != nil || json.Unmarshal(data, &f) != nil || len(f.Candidates) > 1 {
		return f, false, errors.New("invalid native Gemini frame")
	}
	token := false
	for _, candidate := range f.Candidates {
		if candidate.Index != 0 || candidate.Content.Role != "" && candidate.Content.Role != "model" || candidate.FinishReason != "" && candidate.FinishReason != "STOP" && candidate.FinishReason != "MAX_TOKENS" {
			return f, false, errors.New("invalid native Gemini candidate")
		}
		for _, p := range candidate.Content.Parts {
			for key := range p {
				if key != "text" && key != "functionCall" && key != "thought" && key != "thoughtSignature" {
					return f, false, errors.New("unsupported native Gemini response part")
				}
			}
			if p["text"] != nil && p["functionCall"] != nil || p["text"] == nil && p["functionCall"] == nil && p["thoughtSignature"] == nil {
				return f, false, errors.New("invalid native Gemini part")
			}
			if raw := p["thought"]; raw != nil {
				var thought bool
				if json.Unmarshal(raw, &thought) != nil {
					return f, false, errors.New("invalid thought flag")
				}
			}
			if raw := p["thoughtSignature"]; raw != nil {
				var sig string
				if json.Unmarshal(raw, &sig) != nil || sig == "" || len(sig) > 65536 {
					return f, false, errors.New("invalid thought signature")
				}
			}
			if p["text"] == nil && p["functionCall"] == nil {
				continue
			}
			if raw := p["text"]; raw != nil {
				var s string
				if json.Unmarshal(raw, &s) != nil {
					return f, false, errors.New("invalid native Gemini text")
				}
				token = token || s != ""
				continue
			}
			var call struct {
				Name string                     `json:"name"`
				Args map[string]json.RawMessage `json:"args"`
				ID   string                     `json:"id"`
			}
			if json.Unmarshal(p["functionCall"], &call) != nil || call.Args == nil {
				return f, false, errors.New("invalid native Gemini function call")
			}
			if _, ok := tools[call.Name]; !ok {
				return f, false, errors.New("undeclared native Gemini function")
			}
			token = true
		}
	}
	return f, token, nil
}

// CPA emits ordinary Gemini data frames. Usage snapshots are cumulative; only
// the last validated snapshot is settled, including thought tokens exactly once.
func (c *Client) forwardNativeGemini(ctx context.Context, w http.ResponseWriter, response *http.Response, model string, tools map[string]struct{}, stream bool, initial Result) (result Result, failure *Failure) {
	result = initial
	defer func() {
		result.CompletedAt = time.Now()
		if failure != nil && result.BytesOut > 0 {
			result.AbortStream = true
			failure.Status = 0
		}
	}()
	write := func(data []byte, token bool) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if result.FirstByteAt.IsZero() {
			kind := "application/json"
			if stream {
				kind = "text/event-stream"
				w.Header().Set("X-Accel-Buffering", "no")
			}
			w.Header().Set("Content-Type", kind+"; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusOK)
			result.FirstByteAt = time.Now()
		}
		var n int
		var err error
		if stream {
			n, err = fmt.Fprintf(w, "data: %s\n\n", data)
		} else {
			n, err = w.Write(data)
		}
		result.BytesOut += int64(n)
		if token && result.FirstTokenAt.IsZero() {
			result.FirstTokenAt = time.Now()
		}
		if err == nil && stream {
			err = http.NewResponseController(w).Flush()
		}
		return err
	}
	if !stream {
		data, err := io.ReadAll(io.LimitReader(response.Body, maxGeminiResponseBytes+1))
		if err != nil {
			return result, c.transportFailure(ctx, err)
		}
		if len(data) > maxGeminiResponseBytes {
			return result, geminiProtocolFailure(errors.New("native Gemini response too large"))
		}
		f, token, err := parseNativeGemini(data, tools)
		if err != nil || len(f.Candidates) != 1 || f.Candidates[0].FinishReason == "" {
			return result, geminiProtocolFailure(errors.New("native Gemini completion missing"))
		}
		u, err := nativeGeminiUsage(f.Usage)
		if err != nil {
			return result, geminiProtocolFailure(err)
		}
		if err := write(data, token); err != nil {
			return result, c.transportFailure(ctx, err)
		}
		result.Model, result.ServiceTier, result.Usage = model, "default", u
		return result, nil
	}
	finished, hasUsage, terminalUsage := false, false, false
	responseID, version := "", ""
	var last Usage
	err := readGeminiEvents(response.Body, func(data []byte) error {
		f, token, err := parseNativeGemini(data, tools)
		if err != nil {
			return err
		}
		if f.ResponseID != "" {
			if responseID != "" && responseID != f.ResponseID {
				return errors.New("native Gemini response identity changed")
			}
			responseID = f.ResponseID
		}
		if f.ModelVersion != "" {
			if version != "" && version != f.ModelVersion {
				return errors.New("native Gemini model changed")
			}
			version = f.ModelVersion
		}
		if finished && len(f.Candidates) > 0 {
			return errors.New("candidate after native Gemini completion")
		}
		for _, candidate := range f.Candidates {
			if candidate.FinishReason != "" {
				finished = true
			}
		}
		if f.Usage != nil {
			fields, err := cpaprotocol.Object(f.Usage)
			if err != nil {
				return errors.New("invalid native Gemini usage")
			}
			// Google may send an early usage snapshot containing only prompt
			// tokens. It cannot be used to settle the completed response.
			for _, key := range []string{"promptTokenCount", "candidatesTokenCount", "thoughtsTokenCount", "cachedContentTokenCount", "totalTokenCount"} {
				if raw, ok := fields[key]; ok {
					var count *int64
					if json.Unmarshal(raw, &count) != nil || count == nil || *count < 0 {
						return errors.New("invalid native Gemini usage count")
					}
				}
			}
			if fields["promptTokenCount"] == nil || fields["candidatesTokenCount"] == nil {
				if finished {
					return errors.New("missing terminal native Gemini usage")
				}
				return write(data, token)
			}
			u, err := nativeGeminiUsage(f.Usage)
			if err != nil {
				return err
			}
			if hasUsage && (u.InputTokens < last.InputTokens || u.OutputTokens < last.OutputTokens || u.CachedTokens < last.CachedTokens || u.ReasoningTokens < last.ReasoningTokens) {
				return errors.New("native Gemini cumulative usage regressed")
			}
			last, hasUsage = u, true
			terminalUsage = finished
		}
		return write(data, token)
	})
	if err != nil {
		if ctx.Err() != nil || isTimeout(err) {
			return result, c.transportFailure(ctx, err)
		}
		return result, geminiProtocolFailure(err)
	}
	if !finished || !hasUsage || !terminalUsage {
		return result, geminiProtocolFailure(errors.New("native Gemini stream missing completion or usage"))
	}
	result.Model, result.ServiceTier, result.Usage = model, "default", last
	return result, nil
}
