//go:build helm

package deploy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/jamesagarside/hangar/internal/config"
	"github.com/jamesagarside/hangar/internal/httpx"
)

// releaseName is the release name the README and docs/get-started/helm.md
// use. It names the Service hangar, which is what makes Kubernetes
// service-link variables start with HANGAR_.
const releaseName = "hangar"

// offlineValues are the value sets the offline tests render. helm template
// cannot read the cluster, so the chart refuses to generate secrets there;
// the CI values turn generation off, and deploy/kind_test.go covers it
// against a cluster.
var offlineValues = [][]string{
	{"-f", "helm/hangar/ci/default-values.yaml", "--set", "generateSecrets=false"},
	{"-f", "helm/examples/scale-values.yaml"},
	{"-f", "helm/examples/gitops-values.yaml"},
}

// helmCmd runs helm with no kubeconfig, as Argo CD and CI renders do.
func helmCmd(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := exec.Command("helm", args...)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+filepath.Join(t.TempDir(), "none"))
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	return out.String(), errb.String(), err
}

// renderChart runs helm template with the documented release name.
func renderChart(t *testing.T, args ...string) []map[string]any {
	t.Helper()
	out, errOut, err := helmCmd(t, append([]string{"template", releaseName, "helm/hangar"}, args...)...)
	if err != nil {
		t.Fatalf("helm template %v: %v\n%s", args, err, errOut)
	}
	return decodeDocs(t, out)
}

// renderChartError runs helm template and returns its error output, failing
// the test if the render succeeds.
func renderChartError(t *testing.T, args ...string) string {
	t.Helper()
	_, errOut, err := helmCmd(t, append([]string{"template", releaseName, "helm/hangar"}, args...)...)
	if err == nil {
		t.Fatalf("helm template %v rendered; want an error", args)
	}
	return errOut
}

// decodeDocs splits a multi-document YAML stream.
func decodeDocs(t *testing.T, out string) []map[string]any {
	t.Helper()
	var docs []map[string]any
	dec := yaml.NewDecoder(strings.NewReader(out))
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decode rendered chart: %v", err)
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
	for _, values := range offlineValues {
		for _, doc := range renderChart(t, values...) {
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
	for _, values := range offlineValues {
		for _, doc := range renderChart(t, values...) {
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
	out, errOut, err := helmCmd(t, append([]string{"install", releaseName, "helm/hangar",
		"--dry-run=client", "--namespace", releaseName}, args...)...)
	if err != nil {
		t.Fatalf("helm install --dry-run=client %v: %v\n%s", args, err, errOut)
	}
	return notesOf(t, out, args)
}

// notesOf returns the NOTES part of helm install output.
func notesOf(t *testing.T, out string, args []string) string {
	t.Helper()
	_, notes, ok := strings.Cut(out, "\nNOTES:\n")
	if !ok {
		t.Fatalf("helm install %v printed no NOTES", args)
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

// notesCase is a set of values for the NOTES check.
type notesCase struct {
	name string
	args []string
}

// checkNotes checks that every Secret key the install NOTES tell the user
// to read exists in the Secret the chart renders with the same values, and
// that the NOTES name the generated Secret exactly when the chart renders
// it. It returns how many Secret reads the NOTES hold.
func checkNotes(t *testing.T, secrets map[string]map[string]bool, notes string) int {
	t.Helper()
	const generated = releaseName + "-generated"
	reads := 0
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
	return reads
}

// TestChartNotesReadOnlyRenderedSecretKeys runs checkNotes on the value
// sets that render without a cluster. When `secrets` sets the bootstrap
// admin password but not the Instance key, the generated Secret holds only
// the Instance key, so pointing the user at a generated password there
// prints an empty string. The cases where the chart generates secrets need a
// cluster and are in deploy/kind_test.go.
func TestChartNotesReadOnlyRenderedSecretKeys(t *testing.T) {
	cases := []notesCase{
		{"instance key set", []string{"--set-string", "secrets.HANGAR_SECURITY_INSTANCE_KEY=set"}},
		{"both set", []string{"--set-string", "secrets.HANGAR_SECURITY_INSTANCE_KEY=set",
			"--set-string", "secrets.HANGAR_SECURITY_BOOTSTRAP_ADMIN_PASSWORD=set"}},
		{"existing secret", []string{"--set", "existingSecret=hangar-env"}},
		{"generation off", []string{"--set", "generateSecrets=false"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"-f", "helm/hangar/ci/default-values.yaml"}, tc.args...)
			checkNotes(t, secretKeys(renderChart(t, args...)), renderNotes(t, args...))
		})
	}
}

// TestChartGenerateSecretsNeedsCluster checks that the chart refuses to
// generate the Instance key and admin password when it cannot read them back
// from the cluster. Under helm template and Argo CD, lookup returns nothing,
// so every render would make a new Instance key, and applying it would lock
// Hangar out of the upstream tokens and webhook secrets it has encrypted.
func TestChartGenerateSecretsNeedsCluster(t *testing.T) {
	for _, tc := range []notesCase{
		{"generated", nil},
		{"password set", []string{"--set-string", "secrets.HANGAR_SECURITY_BOOTSTRAP_ADMIN_PASSWORD=set"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"-f", "helm/hangar/ci/default-values.yaml"}, tc.args...)
			msg := renderChartError(t, args...)
			for _, want := range []string{"generateSecrets", "existingSecret", "Argo CD"} {
				if !strings.Contains(msg, want) {
					t.Errorf("helm template error does not mention %s:\n%s", want, msg)
				}
			}
			_, errOut, err := helmCmd(t, append([]string{"install", releaseName, "helm/hangar", "--dry-run=client"}, args...)...)
			if err == nil || !strings.Contains(errOut, "generateSecrets") {
				t.Errorf("helm install --dry-run=client: err %v, want the generateSecrets error\n%s", err, errOut)
			}
		})
	}
}

// hooked returns the kind and name of every rendered resource with a Helm
// hook other than the helm test Pod.
func hooked(docs []map[string]any) []string {
	var out []string
	for _, doc := range docs {
		md, _ := doc["metadata"].(map[string]any)
		ann, _ := md["annotations"].(map[string]any)
		if h, ok := ann["helm.sh/hook"].(string); ok && h != "test" {
			out = append(out, fmt.Sprintf("%s/%v", doc["kind"], md["name"]))
		}
	}
	return out
}

// containerByName returns the named container from a list.
func containerByName(list any, name string) map[string]any {
	items, _ := list.([]any)
	for _, c := range items {
		if cm, _ := c.(map[string]any); cm["name"] == name {
			return cm
		}
	}
	return nil
}

// TestChartMigrationModes checks both ways the chart runs migrations with
// persistence off. Argo CD turns Helm's pre-install hooks into PreSync hooks
// and runs them before anything else in the Application, so a database in
// the same Application never starts and the migrate Job never finishes. In
// initContainer mode the chart renders no hooks other than the helm test Pod,
// and every web and worker Pod migrates in an init container with the same
// configuration and secrets as Hangar itself.
func TestChartMigrationModes(t *testing.T) {
	scale := []string{"-f", "helm/examples/scale-values.yaml"}

	t.Run("hook", func(t *testing.T) {
		docs := renderChart(t, scale...)
		if got := hooked(docs); !slices.Contains(got, "Job/hangar-migrate") {
			t.Errorf("hook mode renders hooks %v; want Job/hangar-migrate", got)
		}
		for _, doc := range docs {
			if spec := podSpec(doc); spec != nil && doc["kind"] == "Deployment" && spec["initContainers"] != nil {
				t.Errorf("hook mode: %v has init containers", doc["metadata"].(map[string]any)["name"])
			}
		}
	})

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"initContainer", append(slices.Clone(scale), "--set", "migrations.mode=initContainer")},
		{"gitops example", []string{"-f", "helm/examples/gitops-values.yaml"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			docs := renderChart(t, tc.args...)
			if got := hooked(docs); len(got) > 0 {
				t.Errorf("initContainer mode renders Helm hooks %v, which Argo CD runs before the database", got)
			}
			deployments := 0
			for _, doc := range docs {
				if doc["kind"] != "Deployment" {
					continue
				}
				deployments++
				md, _ := doc["metadata"].(map[string]any)
				spec := podSpec(doc)
				migrate := containerByName(spec["initContainers"], "migrate")
				if migrate == nil {
					t.Errorf("%v: no migrate init container", md["name"])
					continue
				}
				if args, _ := migrate["args"].([]any); fmt.Sprint(args) != "[admin migrate]" {
					t.Errorf("%v: migrate init container args %v, want [admin migrate]", md["name"], args)
				}
				hangar := containerByName(spec["containers"], "hangar")
				for _, field := range []string{"env", "envFrom"} {
					if fmt.Sprint(migrate[field]) != fmt.Sprint(hangar[field]) {
						t.Errorf("%v: migrate %s %v differs from Hangar's %v", md["name"], field, migrate[field], hangar[field])
					}
				}
			}
			if deployments != 2 {
				t.Errorf("rendered %d Deployments, want web and worker", deployments)
			}
		})
	}

	t.Run("persistence", func(t *testing.T) {
		docs := renderChart(t, "-f", "helm/hangar/ci/default-values.yaml", "--set", "generateSecrets=false",
			"--set", "migrations.mode=initContainer")
		for _, doc := range docs {
			if spec := podSpec(doc); spec != nil && doc["kind"] == "Deployment" && spec["initContainers"] != nil {
				t.Error("with persistence on, the all Pod migrates at start and needs no init container")
			}
		}
		if got := hooked(docs); len(got) > 0 {
			t.Errorf("with persistence on, the chart renders hooks %v", got)
		}
	})

	t.Run("unknown mode", func(t *testing.T) {
		if msg := renderChartError(t, append(slices.Clone(scale), "--set", "migrations.mode=job")...); !strings.Contains(msg, "migrations.mode") {
			t.Errorf("error does not name migrations.mode:\n%s", msg)
		}
	})
}

// podChecksums returns each Deployment's checksum/secret annotation.
func podChecksums(t *testing.T, docs []map[string]any) map[string]string {
	t.Helper()
	sums := map[string]string{}
	for _, doc := range docs {
		if doc["kind"] != "Deployment" {
			continue
		}
		md, _ := doc["metadata"].(map[string]any)
		spec, _ := doc["spec"].(map[string]any)
		tmpl, _ := spec["template"].(map[string]any)
		tmd, _ := tmpl["metadata"].(map[string]any)
		ann, _ := tmd["annotations"].(map[string]any)
		sum, _ := ann["checksum/secret"].(string)
		if sum == "" {
			t.Fatalf("%v has no checksum/secret annotation", md["name"])
		}
		sums[md["name"].(string)] = sum
	}
	return sums
}

// TestChartSecretChecksumFollowsExistingSecret checks that pointing
// existingSecret at a new Secret changes the Pods' checksum/secret
// annotation, so the Pods restart and read it. A GitOps rotation creates a
// Secret with a new name and changes existingSecret.
func TestChartSecretChecksumFollowsExistingSecret(t *testing.T) {
	a := podChecksums(t, renderChart(t, "-f", "helm/examples/gitops-values.yaml", "--set", "existingSecret=hangar-secrets-1"))
	b := podChecksums(t, renderChart(t, "-f", "helm/examples/gitops-values.yaml", "--set", "existingSecret=hangar-secrets-2"))
	if len(a) == 0 {
		t.Fatal("rendered no Deployments")
	}
	for name, sum := range a {
		if b[name] == sum {
			t.Errorf("%s: checksum/secret is %s for both existingSecret names", name, sum)
		}
	}
}

// emptyDirLimit returns the sizeLimit of the named emptyDir volume.
func emptyDirLimit(spec map[string]any, name string) string {
	vols, _ := spec["volumes"].([]any)
	for _, v := range vols {
		vm, _ := v.(map[string]any)
		if vm["name"] != name {
			continue
		}
		ed, _ := vm["emptyDir"].(map[string]any)
		s, _ := ed["sizeLimit"].(string)
		return s
	}
	return ""
}

// gibibytes parses a Kubernetes quantity in Gi.
func gibibytes(t *testing.T, q string) float64 {
	t.Helper()
	n, ok := strings.CutSuffix(q, "Gi")
	if !ok {
		t.Fatalf("quantity %q is not in Gi", q)
	}
	f, err := strconv.ParseFloat(n, 64)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// TestChartCacheVolumeOutgrowsGitCache checks that the default cache volume
// is larger than the default git.cache_size with room to spare. The pack
// cache trims itself only after Repos are released and keeps views and
// staging next to it, and the kubelet evicts a Pod whose emptyDir passes its
// sizeLimit, so a volume the same size as the cache gets Pods evicted. It
// also checks that the chart refuses values where the cache would not fit.
func TestChartCacheVolumeOutgrowsGitCache(t *testing.T) {
	cache := float64(config.Default().Git.CacheSize) / float64(config.GiB)
	for _, values := range offlineValues {
		for _, doc := range renderChart(t, values...) {
			if doc["kind"] != "Deployment" {
				continue
			}
			limit := gibibytes(t, emptyDirLimit(podSpec(doc), "cache"))
			if limit < cache*1.25 {
				t.Errorf("%v: cache volume %vGi; the default git.cache_size is %vGiB and needs a quarter more room", values, limit, cache)
			}
		}
	}
	gitops := []string{"-f", "helm/examples/gitops-values.yaml"}
	if msg := renderChartError(t, append(slices.Clone(gitops), "--set", "config.git.cache_size=14GiB")...); !strings.Contains(msg, "cacheSizeLimit") {
		t.Errorf("error does not name cacheSizeLimit:\n%s", msg)
	}
	renderChartError(t, append(slices.Clone(gitops), "--set", "cacheSizeLimit=10Gi")...)
	// Fits, or lives outside the chart's cache volume.
	renderChart(t, append(slices.Clone(gitops), "--set", "config.git.cache_size=12GiB")...)
	renderChart(t, append(slices.Clone(gitops), "--set", "cacheSizeLimit=40G", "--set", "config.git.cache_size=30000MiB")...)
	renderChart(t, append(slices.Clone(gitops), "--set", "config.git.cache_size=40GiB", "--set", "config.git.cache_dir=/mnt/git")...)
}
