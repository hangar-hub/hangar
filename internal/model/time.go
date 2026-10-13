package model

import (
	"fmt"
	"strings"
	"time"
)

// HubTimeLayout is the only date format huggingface_hub parses:
// UTC with milliseconds and a literal Z.
const HubTimeLayout = "2006-01-02T15:04:05.000Z"

// FormatHubTime formats t in UTC with HubTimeLayout.
func FormatHubTime(t time.Time) string { return t.UTC().Format(HubTimeLayout) }

// HubTime is a time that marshals to JSON in HubTimeLayout. Use it in every
// Hub API response.
type HubTime struct{ time.Time }

// NewHubTime wraps t.
func NewHubTime(t time.Time) HubTime { return HubTime{t} }

// MarshalJSON implements json.Marshaler.
func (t HubTime) MarshalJSON() ([]byte, error) {
	return []byte(`"` + FormatHubTime(t.Time) + `"`), nil
}

// UnmarshalJSON implements json.Unmarshaler. It accepts RFC 3339 times.
func (t *HubTime) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	parsed, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return fmt.Errorf("parse hub time: %w", err)
	}
	t.Time = parsed.UTC()
	return nil
}
