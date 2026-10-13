package ollama

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"sync"
)

// The Ollama chat templates and their matching index are vendored from
// github.com/ollama/ollama v0.31.1, template/ (MIT licence; see
// third_party/ollama/LICENSE).
//
//go:embed third_party/ollama/template/*.json third_party/ollama/template/*.gotmpl third_party/ollama/LICENSE
var templateFS embed.FS

const templateDir = "third_party/ollama/template/"

// maxDistance is Ollama's threshold: a template matches when the
// Levenshtein distance from a known Jinja template is under 100.
const maxDistance = 100

// Template is an Ollama Go template matched from a Jinja chat template.
type Template struct {
	Name string
	// Body is the Go template text.
	Body []byte
	// Params is the template's parameters JSON, such as stop tokens, or nil.
	Params []byte
}

type indexEntry struct {
	Template string `json:"template"`
	Name     string `json:"name"`
}

var loadTemplates = sync.OnceValues(func() ([]indexEntry, error) {
	raw, err := templateFS.ReadFile(templateDir + "index.json")
	if err != nil {
		return nil, err
	}
	var entries []indexEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("ollama template index: %w", err)
	}
	return entries, nil
})

// MatchTemplate finds the Ollama template for a Jinja chat template, as
// Ollama's template.Named does: the index entry at the smallest
// Levenshtein distance, if that distance is under 100. ok is false when
// nothing matches.
func MatchTemplate(chatTemplate string) (Template, bool, error) {
	if chatTemplate == "" {
		return Template{}, false, nil
	}
	entries, err := loadTemplates()
	if err != nil {
		return Template{}, false, err
	}
	best, bestName := maxDistance, ""
	for _, e := range entries {
		if d := boundedLevenshtein(chatTemplate, e.Template, best); d < best {
			best, bestName = d, e.Name
		}
	}
	if bestName == "" {
		return Template{}, false, nil
	}
	return namedTemplate(bestName)
}

func namedTemplate(name string) (Template, bool, error) {
	body, err := templateFS.ReadFile(templateDir + name + ".gotmpl")
	if err != nil {
		return Template{}, false, err
	}
	t := Template{Name: name, Body: bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n"))}
	if params, err := templateFS.ReadFile(templateDir + name + ".json"); err == nil {
		t.Params = params
	}
	return t, true, nil
}

// boundedLevenshtein returns the edit distance between a and b in runes,
// or limit when it is limit or more. It only fills a band of width limit
// around the diagonal, so long templates that are far apart cost little.
func boundedLevenshtein(a, b string, limit int) int {
	ra, rb := []rune(a), []rune(b)
	if len(ra)-len(rb) >= limit || len(rb)-len(ra) >= limit {
		return limit
	}
	if len(ra) == 0 {
		return min(len(rb), limit)
	}
	inf := limit + 1
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		if j <= limit {
			prev[j] = j
		} else {
			prev[j] = inf
		}
	}
	for i := 1; i <= len(ra); i++ {
		lo, hi := max(1, i-limit), min(len(rb), i+limit)
		for j := range cur {
			cur[j] = inf
		}
		if i <= limit {
			cur[0] = i
		}
		rowMin := cur[0]
		for j := lo; j <= hi; j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			v := min(prev[j-1]+cost, prev[j]+1, cur[j-1]+1)
			if v > inf {
				v = inf
			}
			cur[j] = v
			rowMin = min(rowMin, v)
		}
		if rowMin >= limit {
			return limit
		}
		prev, cur = cur, prev
	}
	return min(prev[len(rb)], limit)
}
