package proxy

import (
	"strings"
	"testing"
)

func TestReadGeminiEventsRejectsUnboundedOrIncompleteStreams(t *testing.T) {
	for _, wire := range []string{"data: {}", "event: test\n\n", strings.Repeat(": padding\n", maxGeminiResponseBytes/10+1)} {
		if err := readGeminiEvents(strings.NewReader(wire), func([]byte) error { return nil }); err == nil {
			t.Fatal("accepted invalid stream")
		}
	}
}

func TestReadGeminiEventsJoinsMultilineDataAndSkipsComments(t *testing.T) {
	var events []string
	wire := ": waiting\r\n\r\ndata: {\"a\":1,\r\ndata: \"b\":2}\r\n\r\ndata: {}\n\n"
	if err := readGeminiEvents(strings.NewReader(wire), func(data []byte) error {
		events = append(events, string(data))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0] != "{\"a\":1,\n\"b\":2}" || events[1] != "{}" {
		t.Fatalf("events = %q", events)
	}
}
