//go:build helm

package deploy

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/jamesagarside/hangar/internal/httpx"
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

// TestChartReadinessProbeOutwaitsReadyz checks that the readiness probe of
// every Hangar container waits longer than /readyz may take to answer.
// Kubernetes' default timeoutSeconds of 1 marks the pod unready whenever a
// healthy database or Blob store answers slowly.
func TestChartReadinessProbeOutwaitsReadyz(t *testing.T) {
	probes := 0
	for _, values := range []string{"helm/hangar/ci/default-values.yaml", "helm/examples/scale-values.yaml"} {
		for _, doc := range renderChart(t, "-f", values) {
			spec := podSpec(doc)
			if spec == nil {
				continue
			}
			md, _ := doc["metadata"].(map[string]any)
			containers, _ := spec["containers"].([]any)
			for _, c := range containers {
				cm, _ := c.(map[string]any)
				probe, ok := cm["readinessProbe"].(map[string]any)
				if !ok {
					continue
				}
				httpGet, _ := probe["httpGet"].(map[string]any)
				if httpGet["path"] != "/readyz" {
					continue
				}
				probes++
				secs, _ := probe["timeoutSeconds"].(int)
				if time.Duration(secs)*time.Second <= httpx.ReadyTimeout {
					t.Errorf("%s: %s %v container %v: readinessProbe.timeoutSeconds is %d; /readyz may take up to %s",
						values, doc["kind"], md["name"], cm["name"], secs, httpx.ReadyTimeout)
				}
				if period, _ := probe["periodSeconds"].(int); period == 0 {
					t.Errorf("%s: %s %v: readinessProbe has no periodSeconds", values, doc["kind"], md["name"])
				}
			}
		}
	}
	if probes == 0 {
		t.Fatal("rendered no readiness probe on /readyz; the check no longer covers it")
	}
}

// renderNotes runs a client-side dry-run install with the documented release
// name and returns the NOTES it prints. It reads no kubeconfig, so it needs
// no cluster.
func renderNotes(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("helm", append([]string{"install", releaseName, "helm/hangar",
		"--dry-run=client", "--namespace", releaseName}, args...)...)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+filepath.Join(t.TempDir(), "none"))
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("helm install --dry-run=client %v: %v\n%s", args, err, errb.String())
	}
	_, notes, ok := strings.Cut(out.String(), "\nNOTES:\n")
	if !ok {
		t.Fatalf("helm install --dry-run=client %v printed no NOTES", args)
	}
	return notes
}

// secretKeys returns the keys of each rendered Secret, by name.
func secretKeys(docs []map[string]any) map[string]map[string]bool {
	secrets := map[string]map[string]bool{}
	for _, doc := range docs {
		if doc["kind"] != "Secret" {
			continue
		}
		md, _ := doc["metadata"].(map[string]any)
		keys := map[string]bool{}
		for _, field := range []string{"data", "stringData"} {
			m, _ := doc[field].(map[string]any)
			for k := range m {
				keys[k] = true
			}
		}
		secrets[md["name"].(string)] = keys
	}
	return secrets
}

// notesSecretRead matches a kubectl command in NOTES that reads one key of a
// Secret.
var notesSecretRead = regexp.MustCompile(`kubectl get secret --namespace \S+ (\S+) \\\s+-o jsonpath='\{\.data\.(\w+)\}'`)

// TestChartNotesReadOnlyRenderedSecretKeys checks that every Secret key the
// install NOTES tell the user to read exists in the Secret the chart renders
// with the same values, and that the NOTES name the generated Secret exactly
// when the chart renders it. When `secrets` sets the bootstrap admin
// password but not the Instance key, the generated Secret holds only the
// Instance key, so pointing the user at a generated password there prints an
// empty string.
func TestChartNotesReadOnlyRenderedSecretKeys(t *testing.T) {
	const generated = releaseName + "-generated"
	cases := []struct {
		name string
		args []string
	}{
		{"generated", nil},
		{"password set", []string{"--set-string", "secrets.HANGAR_SECURITY_BOOTSTRAP_ADMIN_PASSWORD=set"}},
		{"instance key set", []string{"--set-string", "secrets.HANGAR_SECURITY_INSTANCE_KEY=set"}},
		{"both set", []string{"--set-string", "secrets.HANGAR_SECURITY_INSTANCE_KEY=set",
			"--set-string", "secrets.HANGAR_SECURITY_BOOTSTRAP_ADMIN_PASSWORD=set"}},
		{"existing secret", []string{"--set", "existingSecret=hangar-env"}},
		{"generation off", []string{"--set", "generateSecrets=false"}},
	}
	reads := 0
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"-f", "helm/hangar/ci/default-values.yaml"}, tc.args...)
			secrets := secretKeys(renderChart(t, args...))
			notes := renderNotes(t, args...)
			for _, m := range notesSecretRead.FindAllStringSubmatch(notes, -1) {
				reads++
				keys, ok := secrets[m[1]]
				if !ok {
					t.Errorf("NOTES read Secret %s, which the chart does not render\n%s", m[1], notes)
					continue
				}
				if !keys[m[2]] {
					t.Errorf("NOTES read %s from Secret %s, which holds only %v\n%s", m[2], m[1], keys, notes)
				}
			}
			_, rendered := secrets[generated]
			if mentioned := strings.Contains(notes, generated); mentioned != rendered {
				t.Errorf("NOTES mention %s: %v; chart renders it: %v\n%s", generated, mentioned, rendered, notes)
			}
		})
	}
	if reads == 0 {
		t.Fatal("NOTES read no Secret key in any case; the check no longer covers the password command")
	}
}
