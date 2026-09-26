package store

import (
	"context"
	"errors"
	"testing"
)

func TestModelIdentificationRejectsUnsafeTaskMetadataBeforeSQL(t *testing.T) {
	s := &Store{}
	ctx := context.Background()
	id := "00000000-0000-0000-0000-000000000002"
	for _, stage := range []struct {
		name            string
		index, progress int
	}{
		{"response body", 1, 0}, {"preflight", 1, 0}, {"probing", 0, 0},
		{"validating", 2, 2}, {"scoring", 2, 2}, {"saving", 3, 2},
	} {
		if _, err := s.UpdateModelIdentificationRun(ctx, id, stage.name, stage.index, stage.progress); !errors.Is(err, ErrInvalid) {
			t.Fatalf("stage accepted: %+v %v", stage, err)
		}
	}
	for _, detail := range []ModelIdentificationFailure{
		{Source: "raw-upstream-message"}, {Source: "upstream", UpstreamStatus: 600},
		{Source: "sidecar", RetryAfter: 3601}, {Source: "sidecar", RetryAfter: -1},
	} {
		if _, err := s.FailModelIdentificationRun(ctx, id, "model_identification_probe_failed", detail); !errors.Is(err, ErrInvalid) {
			t.Fatalf("failure metadata accepted: %+v %v", detail, err)
		}
	}
	if _, err := s.BeginModelIdentificationRun(ctx, "0123456789abcdef", "gpt-test", "not-an-actor-id"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("actor accepted: %v", err)
	}
}
