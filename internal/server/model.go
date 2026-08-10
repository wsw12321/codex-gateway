package server

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const maxModelPrefix = 256 << 10

var errModelNotFound = errors.New("top-level model field was not found near the start of the request")

type countingBody struct {
	reader io.Reader
	closer io.Closer
	bytes  int64
}

func (r *countingBody) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.bytes += int64(n)
	return n, err
}

func (r *countingBody) Close() error { return r.closer.Close() }

func prepareModelBody(w http.ResponseWriter, r *http.Request, limit int64) (string, *countingBody, error) {
	if r.ContentLength > limit {
		return "", nil, &http.MaxBytesError{Limit: limit}
	}
	original := http.MaxBytesReader(w, r.Body, limit)
	prefix, err := io.ReadAll(io.LimitReader(original, maxModelPrefix+1))
	if err != nil {
		_ = original.Close()
		return "", nil, err
	}
	parsePrefix := prefix
	if len(parsePrefix) > maxModelPrefix {
		parsePrefix = parsePrefix[:maxModelPrefix]
	}
	model, err := extractTopLevelModel(parsePrefix)
	if err != nil {
		_ = original.Close()
		return "", nil, err
	}
	body := &countingBody{
		reader: io.MultiReader(bytes.NewReader(prefix), original), closer: original,
	}
	r.Body = body
	return model, body, nil
}

// extractTopLevelModel is a bounded JSON lexical scanner. It recognizes only
// a string-valued "model" member at object depth one, so prompt text cannot
// impersonate the routing field. Full JSON validity remains the upstream's
// responsibility.
func extractTopLevelModel(raw []byte) (string, error) {
	i := skipSpace(raw, 0)
	if i >= len(raw) || raw[i] != '{' {
		return "", errModelNotFound
	}
	i++
	for i < len(raw) {
		i = skipSpace(raw, i)
		if i >= len(raw) || raw[i] == '}' {
			break
		}
		if raw[i] != '"' {
			return "", errModelNotFound
		}
		key, next, ok := scanJSONString(raw, i)
		if !ok {
			return "", errModelNotFound
		}
		i = skipSpace(raw, next)
		if i >= len(raw) || raw[i] != ':' {
			return "", errModelNotFound
		}
		i = skipSpace(raw, i+1)
		if key == "model" {
			model, _, ok := scanJSONString(raw, i)
			if !ok || !validModel(model) {
				return "", errModelNotFound
			}
			return model, nil
		}
		next, ok = skipJSONValue(raw, i)
		if !ok {
			return "", errModelNotFound
		}
		i = skipSpace(raw, next)
		if i < len(raw) && raw[i] == ',' {
			i++
			continue
		}
		if i < len(raw) && raw[i] == '}' {
			break
		}
		return "", errModelNotFound
	}
	return "", errModelNotFound
}

func skipJSONValue(raw []byte, start int) (int, bool) {
	start = skipSpace(raw, start)
	if start >= len(raw) {
		return start, false
	}
	if raw[start] == '"' {
		_, end, ok := scanJSONString(raw, start)
		return end, ok
	}
	if raw[start] != '{' && raw[start] != '[' {
		for i := start; i < len(raw); i++ {
			if raw[i] == ',' || raw[i] == '}' || raw[i] == ']' || isSpace(raw[i]) {
				return i, i > start
			}
		}
		return len(raw), true
	}
	stack := []byte{raw[start]}
	inString := false
	escaped := false
	for i := start + 1; i < len(raw); i++ {
		c := raw[i]
		if inString {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{', '[':
			stack = append(stack, c)
		case '}', ']':
			if len(stack) == 0 || (c == '}' && stack[len(stack)-1] != '{') || (c == ']' && stack[len(stack)-1] != '[') {
				return i, false
			}
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				return i + 1, true
			}
		}
	}
	return len(raw), false
}

func scanJSONString(raw []byte, start int) (string, int, bool) {
	if start >= len(raw) || raw[start] != '"' {
		return "", start, false
	}
	var value strings.Builder
	for i := start + 1; i < len(raw); i++ {
		switch raw[i] {
		case '"':
			return value.String(), i + 1, true
		case '\\':
			// Routing identifiers are intentionally ASCII and never need JSON
			// escapes. Rejecting escapes also prevents alternate spellings.
			return "", i, false
		default:
			if raw[i] < 0x20 {
				return "", i, false
			}
			value.WriteByte(raw[i])
		}
	}
	return "", len(raw), false
}

func validModel(model string) bool {
	if model == "" || len(model) > 128 {
		return false
	}
	for _, r := range model {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') &&
			r != '-' && r != '_' && r != '.' && r != ':' {
			return false
		}
	}
	return true
}

func skipSpace(raw []byte, index int) int {
	for index < len(raw) && isSpace(raw[index]) {
		index++
	}
	return index
}

func isSpace(value byte) bool { return value == ' ' || value == '\n' || value == '\r' || value == '\t' }

func modelAllowed(model string, allowlist []string) bool {
	if len(allowlist) == 0 {
		return true
	}
	for _, allowed := range allowlist {
		if model == allowed {
			return true
		}
	}
	return false
}

func modelError(model string) error { return fmt.Errorf("model %q is not allowed", model) }
