//go:build helm

package deploy

import (
	"bytes"
	"errors"
	"io"
	"os/exec"
	"testing"

	"go.yaml.in/yaml/v3"
)

// releaseName is the release name the README and docs/get-started/helm.md
// use. It names the Service hangar, which is what makes Kubernetes
// service-link variables start with HANGAR_.
const releaseName = "hangar"

// renderChart runs helm template with the documented release name.
func renderChart(t *testing.T, args ...string) []map[string]any {
	t.Helper()
	cmd := exec.Command("helm", append([]string{"template", releaseName, "helm/hangar"}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("helm template %v: %v\n%s", args, err, errb.String())
	}
	var docs []map[string]any
	dec := yaml.NewDecoder(&out)
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("helm template %v: %v", args, err)
		}
		if doc != nil {
			docs = append(docs, doc)
		}
	}
	return docs
}

// podSpec returns the Pod spec of a workload or Pod, or nil.
func podSpec(doc map[string]any) map[string]any {
	spec, _ := doc["spec"].(map[string]any)
	switch doc["kind"] {
	case "Pod":
		return spec
	case "Deployment", "StatefulSet", "DaemonSet", "Job":
		tmpl, _ := spec["template"].(map[string]any)
		s, _ := tmpl["spec"].(map[string]any)
		return s
	case "CronJob":
		job, _ := spec["jobTemplate"].(map[string]any)
		js, _ := job["spec"].(map[string]any)
		tmpl, _ := js["template"].(map[string]any)
		s, _ := tmpl["spec"].(map[string]any)
		return s
	}
	return nil
}

// TestChartPodsHaveNoServiceLinks renders the chart with the documented
// release name and checks that every Pod spec, including the migrate hook
// Job and the helm test Pod, sets enableServiceLinks: false. Otherwise
// Kubernetes injects HANGAR_SERVICE_HOST, HANGAR_PORT and the like for the
// Service named hangar, and Hangar refuses to start on unknown HANGAR_*
// variables.
func TestChartPodsHaveNoServiceLinks(t *testing.T) {
	kinds := map[string]bool{}
	for _, values := range []string{"helm/hangar/ci/default-values.yaml", "helm/examples/scale-values.yaml"} {
		for _, doc := range renderChart(t, "-f", values) {
			spec := podSpec(doc)
			if spec == nil {
				continue
			}
			md, _ := doc["metadata"].(map[string]any)
			kinds[doc["kind"].(string)] = true
			if v, ok := spec["enableServiceLinks"].(bool); !ok || v {
				t.Errorf("%s: %s %v: set enableServiceLinks: false on the Pod spec", values, doc["kind"], md["name"])
			}
		}
	}
	for _, k := range []string{"Deployment", "Job", "Pod"} {
		if !kinds[k] {
			t.Errorf("rendered no %s; the check no longer covers it", k)
		}
	}
}
