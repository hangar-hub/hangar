package deploy

import (
	"maps"
	"os"
	"regexp"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// TestGoReleaserSignsWithACosign3Bundle checks the signs block against how
// cosign 3 signs blobs: it writes one Sigstore bundle, ignores
// --output-certificate and fails with "create bundle file" when --bundle is
// missing. make release-rehearsal runs the block for real.
func TestGoReleaserSignsWithACosign3Bundle(t *testing.T) {
	b, err := os.ReadFile("../.goreleaser.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Signs []struct {
			Cmd         string   `yaml:"cmd"`
			Signature   string   `yaml:"signature"`
			Certificate string   `yaml:"certificate"`
			Args        []string `yaml:"args"`
		} `yaml:"signs"`
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Signs) == 0 {
		t.Fatal(".goreleaser.yaml signs nothing")
	}
	for i, s := range cfg.Signs {
		if s.Cmd != "cosign" {
			continue
		}
		if !strings.HasSuffix(s.Signature, ".sigstore.json") {
			t.Errorf("signs[%d].signature %q: cosign 3 writes a Sigstore bundle; end the name in .sigstore.json", i, s.Signature)
		}
		if s.Certificate != "" {
			t.Errorf("signs[%d].certificate %q: cosign 3 puts the certificate in the bundle and writes no .pem", i, s.Certificate)
		}
		args := strings.Join(s.Args, " ")
		if !strings.Contains(args, "--bundle=${signature}") {
			t.Errorf("signs[%d].args %q: cosign 3 sign-blob needs --bundle=${signature}", i, args)
		}
		for _, old := range []string{"--output-signature", "--output-certificate"} {
			if strings.Contains(args, old) {
				t.Errorf("signs[%d].args %q: cosign 3 ignores %s with bundles", i, args, old)
			}
		}
	}
}

// TestGoReleaserFooterReachesTheRelease checks that the release notes
// footer is published. release-please creates the GitHub release before
// GoReleaser runs, and with the default mode, keep-existing, GoReleaser
// keeps that release's notes and drops its own, footer included. It also
// checks the footer's helm install commands set publicURL, as
// TestHelmInstallCommandsSetPublicURL does for the docs.
func TestGoReleaserFooterReachesTheRelease(t *testing.T) {
	b, err := os.ReadFile("../.goreleaser.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Changelog struct {
			Disable bool `yaml:"disable"`
		} `yaml:"changelog"`
		Release struct {
			Mode   string `yaml:"mode"`
			Footer string `yaml:"footer"`
		} `yaml:"release"`
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Release.Footer == "" {
		t.Fatal(".goreleaser.yaml has no release.footer; the check no longer covers it")
	}
	if cfg.Release.Mode != "append" {
		t.Errorf("release.mode is %q; use append, so the footer follows the notes release-please writes", cfg.Release.Mode)
	}
	if !cfg.Changelog.Disable {
		t.Error("changelog is on: with mode append, GoReleaser's changelog would repeat release-please's")
	}
	footer := strings.ReplaceAll(cfg.Release.Footer, "\\\n", " ")
	cmds := helmCommand.FindAllString(footer, -1)
	if len(cmds) == 0 {
		t.Error("release.footer has no helm install command")
	}
	for _, cmd := range cmds {
		if !setsPublicURL(cmd) {
			t.Errorf("release.footer: %q does not set publicURL", strings.Join(strings.Fields(cmd), " "))
		}
	}
}

// TestImageMetadataMatchesDockerfile checks that the release workflow
// publishes the Dockerfile's OCI labels. docker/metadata-action generates
// labels and annotations from the repository (its description, URL and the
// licence GitHub detects, which is NOASSERTION for the Elastic License 2.0),
// and they override the Dockerfile's unless the workflow sets them. GHCR
// shows the description of a multi-arch image from the index annotations.
func TestImageMetadataMatchesDockerfile(t *testing.T) {
	df, err := os.ReadFile("../Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	label := regexp.MustCompile(`(org\.opencontainers\.image\.[a-z]+)="([^"$]*)"`)
	want := map[string]string{}
	for _, m := range label.FindAllStringSubmatch(string(df), -1) {
		want[m[1]] = m[2]
	}
	for _, key := range []string{"title", "description", "url", "documentation", "source", "vendor", "licenses"} {
		if want["org.opencontainers.image."+key] == "" {
			t.Errorf("the Dockerfile sets no org.opencontainers.image.%s label", key)
		}
	}

	b, err := os.ReadFile("../.github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		Jobs map[string]struct {
			Steps []struct {
				ID   string            `yaml:"id"`
				Uses string            `yaml:"uses"`
				Env  map[string]string `yaml:"env"`
				With map[string]string `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(b, &wf); err != nil {
		t.Fatal(err)
	}
	parse := func(list string) map[string]string {
		got := map[string]string{}
		for line := range strings.Lines(list) {
			if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
				got[k] = v
			}
		}
		return got
	}
	var metaSteps, buildSteps int
	for _, step := range wf.Jobs["image"].Steps {
		switch {
		case strings.HasPrefix(step.Uses, "docker/metadata-action@"):
			metaSteps++
			for _, input := range []string{"labels", "annotations"} {
				if got := parse(step.With[input]); !maps.Equal(got, want) {
					t.Errorf("release.yml metadata-action %s:\n%v\nthe Dockerfile's labels:\n%v", input, got, want)
				}
			}
			if levels := step.Env["DOCKER_METADATA_ANNOTATIONS_LEVELS"]; !strings.Contains(levels, "index") {
				t.Errorf("release.yml metadata-action annotation levels %q leave out index, where GHCR reads a multi-arch image's description", levels)
			}
		case strings.HasPrefix(step.Uses, "docker/build-push-action@"):
			buildSteps++
			if step.With["annotations"] != "${{ steps.meta.outputs.annotations }}" {
				t.Error("release.yml build-push-action does not pass the metadata-action annotations")
			}
		}
	}
	if metaSteps != 1 || buildSteps != 1 {
		t.Fatalf("release.yml image job has %d metadata-action and %d build-push-action steps; want one of each", metaSteps, buildSteps)
	}
}

// TestChartPackageMetadata checks what the published chart tells Helm and
// artifacthub.io: a README in the package (helm show readme), the licence
// under the annotation key artifacthub.io reads, and an image entry whose
// 0.0.0 tag the release workflow replaces with the release version.
func TestChartPackageMetadata(t *testing.T) {
	if _, err := os.Stat("helm/hangar/README.md"); err != nil {
		t.Fatalf("the chart has no README.md: %v", err)
	}
	ignore, err := os.ReadFile("helm/hangar/.helmignore")
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.Lines(string(ignore)) {
		if p := strings.TrimSpace(line); p == "README.md" || p == "*.md" {
			t.Errorf(".helmignore leaves %s out of the package", p)
		}
	}

	b, err := os.ReadFile("helm/hangar/Chart.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var chart struct {
		Annotations map[string]string `yaml:"annotations"`
	}
	if err := yaml.Unmarshal(b, &chart); err != nil {
		t.Fatal(err)
	}
	if got := chart.Annotations["artifacthub.io/license"]; got != "Elastic-2.0" {
		t.Errorf("artifacthub.io/license is %q; want Elastic-2.0", got)
	}
	var images []struct {
		Name  string `yaml:"name"`
		Image string `yaml:"image"`
	}
	if err := yaml.Unmarshal([]byte(chart.Annotations["artifacthub.io/images"]), &images); err != nil {
		t.Fatalf("artifacthub.io/images: %v", err)
	}
	if len(images) != 1 || images[0].Image != "ghcr.io/hangar-hub/hangar:0.0.0" {
		t.Errorf("artifacthub.io/images is %+v; want one entry, ghcr.io/hangar-hub/hangar:0.0.0, for the release workflow to set", images)
	}
	for _, key := range []string{"artifacthub.io/links", "artifacthub.io/category"} {
		if chart.Annotations[key] == "" {
			t.Errorf("Chart.yaml has no %s annotation", key)
		}
	}

	wf, err := os.ReadFile("../.github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wf), `sed -i "s|image: $IMAGE:0.0.0|image: $IMAGE:$version|" deploy/helm/hangar/Chart.yaml`) {
		t.Error("release.yml no longer sets the image tag in Chart.yaml's artifacthub.io/images before helm package")
	}
}

// TestReleaseWorkflowPinsCosign3 checks that every cosign-installer step in
// the release workflow pins a cosign 3 release, the version the signs block
// and make release-rehearsal are written for.
func TestReleaseWorkflowPinsCosign3(t *testing.T) {
	b, err := os.ReadFile("../.github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		Jobs map[string]struct {
			Steps []struct {
				Uses string            `yaml:"uses"`
				With map[string]string `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(b, &wf); err != nil {
		t.Fatal(err)
	}
	installers := 0
	for name, job := range wf.Jobs {
		for _, step := range job.Steps {
			if !strings.HasPrefix(step.Uses, "sigstore/cosign-installer@") {
				continue
			}
			installers++
			if v := step.With["cosign-release"]; !strings.HasPrefix(v, "v3.") {
				t.Errorf("job %s: cosign-installer cosign-release is %q; pin a cosign 3 release", name, v)
			}
		}
	}
	if installers == 0 {
		t.Fatal("release.yml installs cosign nowhere; the check no longer covers it")
	}
}
