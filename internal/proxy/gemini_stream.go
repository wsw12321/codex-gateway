package proxy

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
)

// readGeminiEvents calls event with the data of each complete SSE event.
func readGeminiEvents(reader io.Reader, event func([]byte) error) error {
	limited := &io.LimitedReader{R: reader, N: maxGeminiResponseBytes + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 64<<10), maxGeminiResponseBytes)
	var data bytes.Buffer
	pending := false
	for scanner.Scan() {
		line := scanner.Text()
		if limited.N == 0 {
			return errors.New("Gemini stream exceeds limit")
		}
		if line == "" {
			if pending {
				if err := event(data.Bytes()); err != nil {
					return err
				}
				data.Reset()
				pending = false
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			return errors.New("unexpected Gemini SSE field")
		}
		if pending {
			data.WriteByte('\n')
		}
		data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		pending = true
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if pending || limited.N == 0 {
		return errors.New("incomplete or oversized Gemini SSE event")
	}
	return nil
}
