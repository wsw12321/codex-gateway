package server

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
)

const (
	maxModelPrefix             = 256 << 10
	maxRoutingJSONNesting      = 10_000
	maxConcurrentRequestSpools = 4
)

var errModelNotFound = errors.New("valid top-level request routing was not found")

var errRequestBodySpool = errors.New("spool request body")

var errRequestBodySpoolBusy = errors.New("request body spool capacity exhausted")

type requestRouting struct {
	Model       string
	ServiceTier string
}

type countingBody struct {
	reader io.Reader
	closer io.Closer
	bytes  int64
}

type temporaryRequestFile struct {
	file    *os.File
	path    string
	release func()
	once    sync.Once
	err     error
}

func (f *temporaryRequestFile) Read(p []byte) (int, error) { return f.file.Read(p) }

func (f *temporaryRequestFile) Close() error {
	f.once.Do(func() {
		closeErr := f.file.Close()
		removeErr := os.Remove(f.path)
		if errors.Is(removeErr, os.ErrNotExist) {
			removeErr = nil
		}
		f.err = errors.Join(closeErr, removeErr)
		f.release()
	})
	return f.err
}

func (r *countingBody) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.bytes += int64(n)
	return n, err
}

func (r *countingBody) Close() error { return r.closer.Close() }

func (s *Server) prepareModelBody(w http.ResponseWriter, r *http.Request, limit int64) (requestRouting, *countingBody, error) {
	if r.ContentLength > limit {
		return requestRouting{}, nil, &http.MaxBytesError{Limit: limit}
	}
	release, err := s.acquireRequestSpool()
	if err != nil {
		return requestRouting{}, nil, err
	}
	releaseOnReturn := true
	defer func() {
		if releaseOnReturn {
			release()
		}
	}()
	original := http.MaxBytesReader(w, r.Body, limit)
	file, err := os.CreateTemp("", "codex-gateway-request-*")
	if err != nil {
		_ = original.Close()
		return requestRouting{}, nil, fmt.Errorf("%w: create temporary file: %v", errRequestBodySpool, err)
	}
	path := file.Name()
	cleanup := func() {
		_ = file.Close()
		_ = os.Remove(path)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = original.Close()
		cleanup()
		return requestRouting{}, nil, fmt.Errorf("%w: secure temporary file: %v", errRequestBodySpool, err)
	}
	if _, err := io.Copy(file, original); err != nil {
		_ = original.Close()
		cleanup()
		var maxBytes *http.MaxBytesError
		if errors.As(err, &maxBytes) {
			return requestRouting{}, nil, err
		}
		return requestRouting{}, nil, fmt.Errorf("%w: copy request body: %v", errRequestBodySpool, err)
	}
	if err := original.Close(); err != nil {
		cleanup()
		return requestRouting{}, nil, fmt.Errorf("%w: close incoming request body: %v", errRequestBodySpool, err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return requestRouting{}, nil, fmt.Errorf("%w: rewind temporary file: %v", errRequestBodySpool, err)
	}
	routing, err := scanTopLevelRouting(file)
	if err != nil {
		cleanup()
		return requestRouting{}, nil, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return requestRouting{}, nil, fmt.Errorf("%w: rewind parsed request body: %v", errRequestBodySpool, err)
	}
	temporary := &temporaryRequestFile{file: file, path: path, release: release}
	body := &countingBody{
		reader: temporary, closer: temporary,
	}
	r.Body = body
	releaseOnReturn = false
	return routing, body, nil
}

func (s *Server) acquireRequestSpool() (func(), error) {
	s.spoolOnce.Do(func() {
		if s.spoolSlots == nil {
			s.spoolSlots = make(chan struct{}, maxConcurrentRequestSpools)
		}
	})
	select {
	case s.spoolSlots <- struct{}{}:
		var once sync.Once
		return func() {
			once.Do(func() { <-s.spoolSlots })
		}, nil
	default:
		return nil, errRequestBodySpoolBusy
	}
}

// extractTopLevelModel is a streaming JSON lexical scanner. It recognizes only
// a string-valued "model" member at object depth one, so prompt text cannot
// impersonate the routing field. Full JSON validity remains the upstream's
// responsibility.
func extractTopLevelModel(raw []byte) (string, error) {
	routing, err := extractTopLevelRouting(raw)
	return routing.Model, err
}

// extractTopLevelRouting scans the complete JSON object and extracts only
// string-valued model and service_tier members at object depth one, so prompt
// text cannot impersonate routing fields.
func extractTopLevelRouting(raw []byte) (requestRouting, error) {
	return scanTopLevelRouting(bytes.NewReader(raw))
}

func scanTopLevelRouting(source io.Reader) (requestRouting, error) {
	var routing requestRouting
	seenModel, seenServiceTier := false, false
	reader := bufio.NewReaderSize(source, 64<<10)
	character, err := nextNonSpaceByte(reader)
	if err != nil || character != '{' {
		return routing, errModelNotFound
	}
	for {
		character, err = nextNonSpaceByte(reader)
		if err != nil {
			return routing, errModelNotFound
		}
		if character == '}' {
			break
		}
		if character != '"' {
			return routing, errModelNotFound
		}
		key, escaped, truncated, err := readStreamingJSONString(reader, len("service_tier")+1)
		if err != nil || escaped {
			return routing, errModelNotFound
		}
		if !truncated && key != "model" && key != "service_tier" &&
			(strings.EqualFold(key, "model") || strings.EqualFold(key, "service_tier")) {
			return routing, errModelNotFound
		}
		character, err = nextNonSpaceByte(reader)
		if err != nil || character != ':' {
			return routing, errModelNotFound
		}
		character, err = nextNonSpaceByte(reader)
		if err != nil {
			return routing, errModelNotFound
		}
		if key == "model" {
			if character != '"' {
				return routing, errModelNotFound
			}
			model, escaped, truncated, err := readStreamingJSONString(reader, 129)
			if err != nil || escaped || truncated || !validModel(model) || seenModel {
				return routing, errModelNotFound
			}
			routing.Model, seenModel = model, true
		} else if key == "service_tier" {
			if character != '"' {
				return routing, errModelNotFound
			}
			serviceTier, escaped, truncated, err := readStreamingJSONString(reader, 33)
			if err != nil || escaped || truncated || !validServiceTier(serviceTier) || seenServiceTier {
				return routing, errModelNotFound
			}
			routing.ServiceTier, seenServiceTier = serviceTier, true
		} else {
			if err := skipStreamingJSONValue(reader, character); err != nil {
				return routing, errModelNotFound
			}
		}
		character, err = nextNonSpaceByte(reader)
		if err != nil {
			return routing, errModelNotFound
		}
		if character == ',' {
			continue
		}
		if character == '}' {
			break
		}
		return routing, errModelNotFound
	}
	if !seenModel {
		return routing, errModelNotFound
	}
	for {
		character, err = reader.ReadByte()
		if errors.Is(err, io.EOF) {
			return routing, nil
		}
		if err != nil || !isSpace(character) {
			return routing, errModelNotFound
		}
	}
}

func nextNonSpaceByte(reader *bufio.Reader) (byte, error) {
	for {
		character, err := reader.ReadByte()
		if err != nil || !isSpace(character) {
			return character, err
		}
	}
}

func readStreamingJSONString(reader *bufio.Reader, captureLimit int) (string, bool, bool, error) {
	var value strings.Builder
	escaped := false
	truncated := false
	for {
		character, err := reader.ReadByte()
		if err != nil {
			return "", escaped, truncated, err
		}
		switch character {
		case '"':
			return value.String(), escaped, truncated, nil
		case '\\':
			escaped = true
			escape, err := reader.ReadByte()
			if err != nil {
				return "", escaped, truncated, err
			}
			switch escape {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			case 'u':
				for index := 0; index < 4; index++ {
					hex, err := reader.ReadByte()
					if err != nil || !isHexDigit(hex) {
						return "", escaped, truncated, errModelNotFound
					}
				}
			default:
				return "", escaped, truncated, errModelNotFound
			}
		default:
			if character < 0x20 {
				return "", escaped, truncated, errModelNotFound
			}
			if value.Len() < captureLimit {
				value.WriteByte(character)
			} else {
				truncated = true
			}
		}
	}
}

func skipStreamingJSONValue(reader *bufio.Reader, first byte) error {
	if first == '"' {
		_, _, _, err := readStreamingJSONString(reader, 0)
		return err
	}
	if first != '{' && first != '[' {
		for {
			character, err := reader.ReadByte()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			if character == ',' || character == '}' || character == ']' {
				return reader.UnreadByte()
			}
			if isSpace(character) {
				return nil
			}
		}
	}

	stack := []byte{first}
	for len(stack) > 0 {
		character, err := reader.ReadByte()
		if err != nil {
			return err
		}
		switch character {
		case '"':
			if _, _, _, err := readStreamingJSONString(reader, 0); err != nil {
				return err
			}
		case '{', '[':
			if len(stack) >= maxRoutingJSONNesting {
				return errModelNotFound
			}
			stack = append(stack, character)
		case '}', ']':
			if (character == '}' && stack[len(stack)-1] != '{') ||
				(character == ']' && stack[len(stack)-1] != '[') {
				return errModelNotFound
			}
			stack = stack[:len(stack)-1]
		}
	}
	return nil
}

func isHexDigit(value byte) bool {
	return (value >= '0' && value <= '9') || (value >= 'a' && value <= 'f') || (value >= 'A' && value <= 'F')
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

func validServiceTier(value string) bool {
	if value == "" || len(value) > 32 {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') &&
			r != '-' && r != '_' && r != '.' && r != ':' {
			return false
		}
	}
	return true
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

// recordedUpstreamModel accepts only the same bounded model syntax allowed on
// requests. A malformed or missing upstream value falls back to the already
// validated requested model in CompleteUsageRequest instead of leaving the
// usage row stuck in progress on a database constraint failure.
func recordedUpstreamModel(model string) string {
	if !validModel(model) {
		return ""
	}
	return model
}

func recordedUpstreamServiceTier(serviceTier string) string {
	if !validServiceTier(serviceTier) {
		return ""
	}
	return serviceTier
}

func modelError(model string) error { return fmt.Errorf("model %q is not allowed", model) }
