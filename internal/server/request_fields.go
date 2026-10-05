package server

import (
	"encoding/json"
	"errors"
)

// optionalString distinguishes an omitted field from an explicitly supplied
// null. Callers may safely use value != nil to enforce update/target selection.
type optionalString struct {
	value *string
}

func (field *optionalString) UnmarshalJSON(raw []byte) error {
	var value *string
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	if value == nil {
		return errors.New("field must be a string")
	}
	field.value = value
	return nil
}
