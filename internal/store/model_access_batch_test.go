package store

import (
	"errors"
	"reflect"
	"testing"
)

func TestNormalizeModelAccessBatch(t *testing.T) {
	for _, models := range [][]string{nil, {}, {"a", "a"}, {" a", "a "}, {"bad/model"}, make([]string, 1001)} {
		if _, err := normalizeModelAccessBatch(models); !errors.Is(err, ErrInvalid) {
			t.Fatalf("models %#v: %v", models, err)
		}
	}
	models, err := normalizeModelAccessBatch([]string{"gpt-b", "gpt-a"})
	if err != nil || !reflect.DeepEqual(models, []string{"gpt-a", "gpt-b"}) {
		t.Fatalf("models %#v: %v", models, err)
	}
}
