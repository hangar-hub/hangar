package hfapi

import (
	"net/http"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/hangar-hub/hangar/internal/httpx"
)

// cardHelpURL explains card metadata in the warnings validate-yaml returns.
const cardHelpURL = "https://huggingface.co/docs/hub/model-cards#model-card-metadata"

// validateYAML serves POST /api/validate-yaml (contract §7.4). It always
// answers JSON, because the client parses the body before the status.
func (h *Handler) validateYAML(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Content  string `json:"content"`
		RepoType string `json:"repoType"`
	}
	if !h.decodeJSON(w, r, &req) {
		return
	}
	front, found := FrontMatter(req.Content)
	warnings := []map[string]string{}
	if !found || strings.TrimSpace(front) == "" {
		warnings = append(warnings, map[string]string{"message": "empty or missing yaml metadata in repo card", "help": cardHelpURL})
		httpx.WriteJSON(w, r, http.StatusOK, map[string]any{"errors": []any{}, "warnings": warnings})
		return
	}
	if _, err := ParseCardData(front); err != nil {
		httpx.WriteJSON(w, r, http.StatusBadRequest, map[string]any{
			"errors":   []map[string]string{{"message": "Invalid YAML metadata in README.md: " + err.Error()}},
			"warnings": warnings,
		})
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, map[string]any{"errors": []any{}, "warnings": warnings})
}

// FrontMatter returns the YAML between a leading `---` line and the next
// `---` line of a Model card, and whether the card has front matter.
func FrontMatter(card string) (string, bool) {
	s := strings.TrimPrefix(card, "\uFEFF")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if !strings.HasPrefix(s, "---\n") {
		return "", false
	}
	body := s[len("---\n"):]
	if strings.HasPrefix(body, "---\n") || body == "---" {
		return "", true
	}
	end := strings.Index(body, "\n---\n")
	if end < 0 {
		if strings.HasSuffix(body, "\n---") {
			return body[:len(body)-len("\n---")], true
		}
		return "", false
	}
	return body[:end], true
}

// ParseCardData parses card front matter, which must be a YAML mapping.
func ParseCardData(front string) (map[string]any, error) {
	var out map[string]any
	if err := yaml.Unmarshal([]byte(front), &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}
