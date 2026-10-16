package deploy

import (
	"os"
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
