// Package cpaprotocol validates the public text and client-function subset of
// CPA's native protocols. Validation never rewrites signatures or tool payloads.
package cpaprotocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	ErrUnsupported = errors.New("unsupported CPA request field or value")
	ErrNewSession  = errors.New("start a new session; legacy tool history has no usable provider signature")
)

type object = map[string]json.RawMessage

// Object rejects duplicate keys at every depth, trailing values and invalid
// Unicode, before any projection or provider translation can disagree.
func Object(data []byte) (map[string]json.RawMessage, error) {
	if !utf8.Valid(data) {
		return nil, ErrUnsupported
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var value func(int) error
	value = func(depth int) error {
		if depth > 64 {
			return ErrUnsupported
		}
		token, err := d.Token()
		if err != nil {
			return ErrUnsupported
		}
		delim, compound := token.(json.Delim)
		if !compound {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return ErrUnsupported
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return ErrUnsupported
				}
				seen[name] = true
				if err := value(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err := value(depth + 1); err != nil {
					return err
				}
			}
		default:
			return ErrUnsupported
		}
		_, err = d.Token()
		return err
	}
	if value(0) != nil {
		return nil, ErrUnsupported
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return nil, ErrUnsupported
	}
	var out object
	if json.Unmarshal(data, &out) != nil || out == nil {
		return nil, ErrUnsupported
	}
	return out, nil
}

func obj(raw []byte) (object, bool) {
	var o object
	err := json.Unmarshal(raw, &o)
	return o, err == nil && o != nil
}
func text(raw []byte) (string, bool) {
	var s string
	ok := len(raw) > 0 && raw[0] == '"' && json.Unmarshal(raw, &s) == nil
	return s, ok
}
func keys(o object, names ...string) bool {
	for key := range o {
		found := false
		for _, name := range names {
			if key == name {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
func boolean(raw []byte) bool {
	return bytes.Equal(raw, []byte("true")) || bytes.Equal(raw, []byte("false"))
}
func integer(raw []byte, low, high int64) bool {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false
	}
	var n int64
	return json.Unmarshal(raw, &n) == nil && n >= low && n <= high
}
func number(raw []byte, low, high float64) bool {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false
	}
	var n float64
	return json.Unmarshal(raw, &n) == nil && !math.IsNaN(n) && !math.IsInf(n, 0) && n >= low && n <= high
}
func identifier(s string) bool {
	return s != "" && len(s) <= 256 && strings.TrimSpace(s) == s && strings.IndexFunc(s, unicode.IsControl) < 0
}
func stringsArray(raw []byte) bool { var a []string; return json.Unmarshal(raw, &a) == nil && a != nil }

func declaration(o object, names map[string]bool, responses bool) bool {
	if !keys(o, "type", "name", "description", "parameters", "parametersJsonSchema", "response", "responseJsonSchema", "strict", "behavior") {
		return false
	}
	name, ok := text(o["name"])
	if !ok || !identifier(name) || len(name) > 64 || names[name] {
		return false
	}
	names[name] = true
	if raw, ok := o["description"]; ok {
		if _, ok := text(raw); !ok {
			return false
		}
	}
	if _, a := o["parameters"]; a && o["parametersJsonSchema"] != nil {
		return false
	}
	for _, key := range []string{"parameters", "parametersJsonSchema", "response", "responseJsonSchema"} {
		if raw, ok := o[key]; ok {
			if _, ok := obj(raw); !ok {
				return false
			}
		}
	}
	if raw, ok := o["strict"]; ok && (!responses || !boolean(raw)) {
		return false
	}
	if raw, ok := o["behavior"]; ok && !bytes.Equal(raw, []byte(`"BLOCKING"`)) {
		return false
	}
	return true
}

func GeminiRequest(data []byte) error {
	o, err := Object(data)
	if err != nil {
		return err
	}
	if !keys(o, "contents", "systemInstruction", "tools", "toolConfig", "generationConfig", "safetySettings", "labels") {
		return ErrUnsupported
	}
	names := map[string]bool{}
	if raw, ok := o["tools"]; ok {
		var tools []object
		if json.Unmarshal(raw, &tools) != nil || tools == nil {
			return ErrUnsupported
		}
		for _, tool := range tools {
			if !keys(tool, "functionDeclarations") {
				return ErrUnsupported
			}
			var funcs []object
			if json.Unmarshal(tool["functionDeclarations"], &funcs) != nil || len(funcs) == 0 {
				return ErrUnsupported
			}
			for _, f := range funcs {
				if !declaration(f, names, false) {
					return ErrUnsupported
				}
			}
		}
	}
	if raw, ok := o["toolConfig"]; ok {
		cfg, ok := obj(raw)
		if !ok || !keys(cfg, "functionCallingConfig") {
			return ErrUnsupported
		}
		calling, ok := obj(cfg["functionCallingConfig"])
		if !ok || !keys(calling, "mode", "allowedFunctionNames") {
			return ErrUnsupported
		}
		if raw, ok := calling["mode"]; ok {
			mode, ok := text(raw)
			if !ok || mode != "AUTO" && mode != "ANY" && mode != "NONE" && mode != "VALIDATED" {
				return ErrUnsupported
			}
		}
		if raw, ok := calling["allowedFunctionNames"]; ok {
			var a []string
			if json.Unmarshal(raw, &a) != nil || a == nil {
				return ErrUnsupported
			}
			for _, name := range a {
				if !names[name] {
					return ErrUnsupported
				}
			}
		}
	}
	if raw, ok := o["generationConfig"]; ok {
		if err := generation(raw); err != nil {
			return err
		}
	}
	if raw, ok := o["systemInstruction"]; ok {
		system, ok := obj(raw)
		if !ok || !keys(system, "role", "parts") {
			return ErrUnsupported
		}
		var parts []object
		if json.Unmarshal(system["parts"], &parts) != nil || len(parts) == 0 {
			return ErrUnsupported
		}
		for _, p := range parts {
			if !keys(p, "text") {
				return ErrUnsupported
			}
			if _, ok := text(p["text"]); !ok {
				return ErrUnsupported
			}
		}
	}
	var contents []object
	if json.Unmarshal(o["contents"], &contents) != nil || len(contents) == 0 {
		return ErrUnsupported
	}
	pending := map[string]string{}
	for _, content := range contents {
		if !keys(content, "role", "parts") {
			return ErrUnsupported
		}
		role := "user"
		if raw, present := content["role"]; present {
			var valid bool
			role, valid = text(raw)
			if !valid {
				return ErrUnsupported
			}
		}
		if role != "user" && role != "model" {
			return ErrUnsupported
		}
		var parts []object
		if json.Unmarshal(content["parts"], &parts) != nil || len(parts) == 0 {
			return ErrUnsupported
		}
		firstCall := true
		for _, p := range parts {
			if !keys(p, "text", "functionCall", "functionResponse", "thoughtSignature", "thought") {
				return ErrUnsupported
			}
			count := 0
			for _, k := range []string{"text", "functionCall", "functionResponse"} {
				if p[k] != nil {
					count++
				}
			}
			if count != 1 && !(count == 0 && p["thoughtSignature"] != nil && role == "model") {
				return ErrUnsupported
			}
			if raw, ok := p["thought"]; ok && (role != "model" || !boolean(raw)) {
				return ErrUnsupported
			}
			if raw, ok := p["thoughtSignature"]; ok {
				v, ok := text(raw)
				if !ok || v == "" || len(v) > 65536 || strings.TrimSpace(v) != v || strings.IndexFunc(v, unicode.IsControl) >= 0 || role != "model" {
					return ErrUnsupported
				}
			}
			if count == 0 {
				continue
			}
			if raw, ok := p["text"]; ok {
				if _, ok := text(raw); !ok {
					return ErrUnsupported
				}
				continue
			}
			call := p["functionCall"] != nil
			raw := p["functionResponse"]
			payload := "response"
			if call {
				raw = p["functionCall"]
				payload = "args"
			}
			f, ok := obj(raw)
			if !ok || !keys(f, "name", "id", payload) {
				return ErrUnsupported
			}
			name, ok := text(f["name"])
			if !ok || !identifier(name) {
				return ErrUnsupported
			}
			if _, ok := obj(f[payload]); !ok {
				return ErrUnsupported
			}
			id := name
			if raw, ok := f["id"]; ok {
				id, ok = text(raw)
				if !ok || !identifier(id) {
					return ErrUnsupported
				}
			}
			if call {
				if role != "model" || !names[name] || pending[id] != "" {
					return ErrUnsupported
				}
				// Parallel function calls share one model turn. Gemini may attach
				// the turn signature only to its first functionCall part.
				if sig, ok := text(p["thoughtSignature"]); firstCall && (!ok || sig == "") {
					return ErrNewSession
				}
				firstCall = false
				pending[id] = name
			} else {
				if role != "user" || pending[id] != name {
					return ErrUnsupported
				}
				delete(pending, id)
			}
		}
	}
	if len(pending) > 0 {
		return ErrUnsupported
	}
	return nil
}

func generation(raw []byte) error {
	g, ok := obj(raw)
	if !ok {
		return ErrUnsupported
	}
	for k, v := range g {
		valid := false
		switch k {
		case "candidateCount":
			valid = integer(v, 1, 1)
		case "maxOutputTokens":
			valid = integer(v, 1, 1<<20)
		case "temperature":
			valid = number(v, 0, 2)
		case "topP":
			valid = number(v, 0, 1)
		case "topK":
			valid = integer(v, 1, 1000)
		case "seed":
			valid = integer(v, math.MinInt32, math.MaxInt32)
		case "stopSequences":
			valid = stringsArray(v)
		case "responseMimeType":
			s, ok := text(v)
			valid = ok && (s == "text/plain" || s == "application/json")
		case "responseSchema", "responseJsonSchema":
			_, valid = obj(v)
		case "responseModalities":
			var a []string
			valid = json.Unmarshal(v, &a) == nil && len(a) == 1 && a[0] == "TEXT"
		case "thinkingConfig":
			t, ok := obj(v)
			if !ok || !keys(t, "includeThoughts", "thinkingBudget", "thinkingLevel") {
				return ErrUnsupported
			}
			valid = true
			for key, value := range t {
				switch key {
				case "includeThoughts":
					valid = valid && boolean(value)
				case "thinkingBudget":
					valid = valid && integer(value, -1, 1<<20)
				case "thinkingLevel":
					s, ok := text(value)
					valid = valid && ok && (s == "MINIMAL" || s == "LOW" || s == "MEDIUM" || s == "HIGH")
				}
			}
		}
		if !valid {
			return ErrUnsupported
		}
	}
	return nil
}

func ResponsesRequest(data []byte) error {
	o, err := Object(data)
	if err != nil {
		return err
	}
	if !keys(o, "model", "input", "instructions", "stream", "store", "service_tier", "tools", "tool_choice", "parallel_tool_calls", "reasoning", "max_output_tokens", "temperature", "top_p", "metadata", "user", "prompt_cache_key", "session_id", "conversation_id", "previous_response_id", "truncation", "background", "text", "include") {
		return ErrUnsupported
	}
	if raw := o["previous_response_id"]; len(raw) > 0 && !bytes.Equal(raw, []byte("null")) && !bytes.Equal(raw, []byte(`""`)) {
		return ErrNewSession
	}
	if raw := o["background"]; len(raw) > 0 && !bytes.Equal(raw, []byte("false")) && !bytes.Equal(raw, []byte("null")) {
		return ErrUnsupported
	}
	if raw := o["service_tier"]; len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
		s, ok := text(raw)
		if !ok || s != "auto" && s != "default" {
			return ErrUnsupported
		}
	}
	if err := responseOptions(o); err != nil {
		return err
	}
	names := map[string]bool{}
	if raw, ok := o["tools"]; ok {
		var tools []object
		if json.Unmarshal(raw, &tools) != nil {
			return ErrUnsupported
		}
		for _, t := range tools {
			typ, _ := text(t["type"])
			if typ != "function" || !declaration(t, names, true) {
				return ErrUnsupported
			}
		}
	}
	if raw, ok := o["tool_choice"]; ok && !bytes.Equal(raw, []byte("null")) {
		if s, ok := text(raw); ok {
			if s != "auto" && s != "none" && s != "required" {
				return ErrUnsupported
			}
		} else {
			t, ok := obj(raw)
			typ, _ := text(t["type"])
			name, _ := text(t["name"])
			if !ok || !keys(t, "type", "name") || typ != "function" || !names[name] {
				return ErrUnsupported
			}
		}
	}
	if _, ok := text(o["input"]); ok {
		return nil
	}
	var items []object
	if json.Unmarshal(o["input"], &items) != nil || len(items) == 0 {
		return ErrUnsupported
	}
	for _, item := range items {
		typ, _ := text(item["type"])
		switch typ {
		case "", "message":
			if !keys(item, "type", "role", "content", "id", "status") {
				return ErrUnsupported
			}
			role, ok := text(item["role"])
			if !ok || role != "user" && role != "assistant" && role != "system" && role != "developer" {
				return ErrUnsupported
			}
			if _, ok := text(item["content"]); ok {
				continue
			}
			var parts []object
			if json.Unmarshal(item["content"], &parts) != nil || len(parts) == 0 {
				return ErrUnsupported
			}
			for _, p := range parts {
				kind, _ := text(p["type"])
				if kind != "input_text" && kind != "output_text" {
					return ErrUnsupported
				}
				if _, ok := text(p["text"]); !ok || !keys(p, "type", "text", "annotations") {
					return ErrUnsupported
				}
			}
		case "function_call":
			if !keys(item, "type", "id", "call_id", "name", "arguments", "status") {
				return ErrUnsupported
			}
			id, ok := text(item["call_id"])
			name, nok := text(item["name"])
			args, aok := text(item["arguments"])
			if !ok || !nok || !aok || !identifier(id) || !names[name] {
				return ErrUnsupported
			}
			if _, err := Object([]byte(args)); err != nil {
				return ErrUnsupported
			}
		case "function_call_output":
			if !keys(item, "type", "id", "call_id", "output", "status") {
				return ErrUnsupported
			}
			id, ok := text(item["call_id"])
			if !ok || !identifier(id) {
				return ErrUnsupported
			}
			if _, ok := text(item["output"]); !ok && !responseTextParts(item["output"], "input_text") {
				return ErrUnsupported
			}
		case "reasoning":
			if !keys(item, "type", "id", "summary", "encrypted_content", "status") {
				return ErrUnsupported
			}
			if raw, ok := item["encrypted_content"]; ok {
				if _, ok := text(raw); !ok {
					return ErrUnsupported
				}
			}
			if raw, ok := item["summary"]; ok && !responseTextParts(raw, "summary_text") {
				return ErrUnsupported
			}
		default:
			return ErrUnsupported
		}
	}
	return nil
}

func responseTextParts(raw []byte, kind string) bool {
	var parts []object
	if json.Unmarshal(raw, &parts) != nil || parts == nil {
		return false
	}
	for _, part := range parts {
		typ, ok := text(part["type"])
		if !ok || typ != kind || !keys(part, "type", "text") {
			return false
		}
		if _, ok := text(part["text"]); !ok {
			return false
		}
	}
	return true
}

// Validate controls before the string-input fast path. Client functions and
// text formats must not provide alternate routes to media or server tools.
func responseOptions(o object) error {
	for _, key := range []string{"stream", "store", "parallel_tool_calls"} {
		if raw, ok := o[key]; ok && !bytes.Equal(raw, []byte("null")) && !boolean(raw) {
			return ErrUnsupported
		}
	}
	for _, key := range []string{"instructions", "user", "prompt_cache_key", "session_id", "conversation_id"} {
		if raw, ok := o[key]; ok && !bytes.Equal(raw, []byte("null")) {
			if _, ok := text(raw); !ok {
				return ErrUnsupported
			}
		}
	}
	if raw, ok := o["max_output_tokens"]; ok && !bytes.Equal(raw, []byte("null")) && !integer(raw, 1, 1<<20) {
		return ErrUnsupported
	}
	if raw, ok := o["temperature"]; ok && !bytes.Equal(raw, []byte("null")) && !number(raw, 0, 2) {
		return ErrUnsupported
	}
	if raw, ok := o["top_p"]; ok && !bytes.Equal(raw, []byte("null")) && !number(raw, 0, 1) {
		return ErrUnsupported
	}
	if raw, ok := o["truncation"]; ok && !bytes.Equal(raw, []byte("null")) {
		v, ok := text(raw)
		if !ok || v != "auto" && v != "disabled" {
			return ErrUnsupported
		}
	}
	if raw, ok := o["reasoning"]; ok && !bytes.Equal(raw, []byte("null")) {
		r, ok := obj(raw)
		if !ok || !keys(r, "effort", "summary") {
			return ErrUnsupported
		}
		if raw, ok := r["effort"]; ok && !bytes.Equal(raw, []byte("null")) {
			v, ok := text(raw)
			if !ok || v != "none" && v != "minimal" && v != "low" && v != "medium" && v != "high" && v != "xhigh" {
				return ErrUnsupported
			}
		}
		if raw, ok := r["summary"]; ok && !bytes.Equal(raw, []byte("null")) {
			v, ok := text(raw)
			if !ok || v != "auto" && v != "concise" && v != "detailed" {
				return ErrUnsupported
			}
		}
	}
	if raw, ok := o["metadata"]; ok && !bytes.Equal(raw, []byte("null")) {
		m, ok := obj(raw)
		if !ok || len(m) > 16 {
			return ErrUnsupported
		}
		for key, value := range m {
			v, ok := text(value)
			if !ok || len(key) > 64 || len(v) > 512 {
				return ErrUnsupported
			}
		}
	}
	if raw, ok := o["include"]; ok && !bytes.Equal(raw, []byte("null")) {
		var includes []string
		if json.Unmarshal(raw, &includes) != nil || includes == nil {
			return ErrUnsupported
		}
		for _, v := range includes {
			if v != "reasoning.encrypted_content" && v != "message.output_text.logprobs" {
				return ErrUnsupported
			}
		}
	}
	if raw, ok := o["text"]; ok && !bytes.Equal(raw, []byte("null")) {
		t, ok := obj(raw)
		if !ok || !keys(t, "format", "verbosity") {
			return ErrUnsupported
		}
		if raw, ok := t["verbosity"]; ok {
			v, ok := text(raw)
			if !ok || v != "low" && v != "medium" && v != "high" {
				return ErrUnsupported
			}
		}
		if raw, ok := t["format"]; ok {
			f, ok := obj(raw)
			if !ok || !keys(f, "type", "name", "description", "schema", "strict") {
				return ErrUnsupported
			}
			typ, ok := text(f["type"])
			if !ok || typ != "text" && typ != "json_object" && typ != "json_schema" {
				return ErrUnsupported
			}
			if typ == "json_schema" {
				if name, ok := text(f["name"]); !ok || !identifier(name) {
					return ErrUnsupported
				}
				if _, ok := obj(f["schema"]); !ok {
					return ErrUnsupported
				}
			}
			if raw, ok := f["strict"]; ok && !bytes.Equal(raw, []byte("null")) && !boolean(raw) {
				return ErrUnsupported
			}
			if raw, ok := f["description"]; ok {
				if _, ok := text(raw); !ok {
					return ErrUnsupported
				}
			}
		}
	}
	return nil
}
