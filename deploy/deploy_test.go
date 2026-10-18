// Package deploy holds checks on the Compose examples, the Helm chart's
// example values and the install commands in the docs. They read the files
// only, so they run with go test and need no Docker or Helm.
package deploy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// secretName matches variable and key names that hold a secret.
var secretName = regexp.MustCompile(`(?i)(password|secret|token|_key$|dsn$)`)

// interpolation matches a Compose interpolation such as ${NAME:?message}.
var interpolation = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)([^}]*)\}`)

// generatedWhenEmpty are variables whose empty default is safe: Hangar
// generates the value on first start and prints it once.
var generatedWhenEmpty = map[string]bool{"HANGAR_ADMIN_PASSWORD": true}

// hangarPort is the one published port that may listen on every interface:
// Hangar's own HTTP port, which enforces Hangar's authorisation.
const hangarPort = "${HANGAR_PORT:-8080}:8080"

func composeFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("compose/*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no Compose files in deploy/compose")
	}
	return files
}

type composeFile struct {
	Services map[string]struct {
		Environment map[string]any `yaml:"environment"`
		Ports       []any          `yaml:"ports"`
	} `yaml:"services"`
}

func readCompose(t *testing.T, path string) (string, composeFile) {
	t.Helper()
	b, err := os.ReadFile(path) // #nosec G304 -- test fixture path.
	if err != nil {
		t.Fatal(err)
	}
	var c composeFile
	if err := yaml.Unmarshal(b, &c); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return string(b), c
}

// stripComments drops YAML comment lines, which Compose does not
// interpolate.
func stripComments(s string) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// TestComposeSecretsHaveNoDefaults checks that every secret in the Compose
// examples comes from a required variable, so nobody runs an example with
// a password printed in this repository.
func TestComposeSecretsHaveNoDefaults(t *testing.T) {
	for _, path := range composeFiles(t) {
		raw, c := readCompose(t, path)
		for _, m := range interpolation.FindAllStringSubmatch(stripComments(raw), -1) {
			name, rest := m[1], m[2]
			if !secretName.MatchString(name) {
				continue
			}
			if rest == ":-" && generatedWhenEmpty[name] {
				continue
			}
			if !strings.HasPrefix(rest, ":?") || len(rest) < 3 {
				t.Errorf("%s: %s must be required with ${%s:?message}, not %q", path, name, name, m[0])
			}
		}
		for svc, s := range c.Services {
			for key, v := range s.Environment {
				for _, p := range secretProblems(key, v) {
					t.Errorf("%s: services.%s.environment.%s: %s", path, svc, key, p)
				}
			}
		}
	}
}

// secretProblems reports a secret key with a literal value instead of a
// required variable. JSON values, such as HANGAR_OIDC_PROVIDERS, are
// checked key by key.
func secretProblems(key string, v any) []string {
	s, ok := v.(string)
	if !ok {
		if v != nil && secretName.MatchString(key) {
			return []string{fmt.Sprintf("a secret must come from a required variable, not the literal %v", v)}
		}
		return nil
	}
	if trimmed := strings.TrimSpace(s); strings.HasPrefix(trimmed, "[") || strings.HasPrefix(trimmed, "{") {
		var doc any
		if err := json.Unmarshal([]byte(trimmed), &doc); err == nil {
			return jsonSecretProblems(doc)
		}
	}
	if !secretName.MatchString(key) {
		return nil
	}
	if name := interpolatedName(s); generatedWhenEmpty[name] && strings.TrimSpace(s) == "${"+name+":-}" {
		return nil
	}
	if !requiredOnly(s) {
		return []string{fmt.Sprintf("a secret must come from a required variable such as ${NAME:?message}, not %q", s)}
	}
	return nil
}

func jsonSecretProblems(doc any) []string {
	var out []string
	switch d := doc.(type) {
	case map[string]any:
		for k, v := range d {
			if s, ok := v.(string); ok && secretName.MatchString(k) {
				if !requiredOnly(s) {
					out = append(out, fmt.Sprintf("%s must come from a required variable such as ${NAME:?message}, not %q", k, s))
				}
				continue
			}
			out = append(out, jsonSecretProblems(v)...)
		}
	case []any:
		for _, v := range d {
			out = append(out, jsonSecretProblems(v)...)
		}
	}
	return out
}

// requiredOnly reports whether s has at least one interpolation and every
// one of them is a required variable. A DSN such as
// postgres://hangar:${POSTGRES_PASSWORD:?...}@postgres/hangar passes.
func requiredOnly(s string) bool {
	ms := interpolation.FindAllStringSubmatch(s, -1)
	if len(ms) == 0 {
		return false
	}
	for _, m := range ms {
		if !strings.HasPrefix(m[2], ":?") {
			return false
		}
	}
	return true
}

func interpolatedName(s string) string {
	if m := interpolation.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}

// TestComposeSecretChecksCatchDefaults makes sure the checks above fail on
// the forms they exist to catch.
func TestComposeSecretChecksCatchDefaults(t *testing.T) {
	for _, tc := range []struct {
		key, value string
		ok         bool
	}{
		{"POSTGRES_PASSWORD", "${POSTGRES_PASSWORD:-hangar-example-password}", false},
		{"KC_BOOTSTRAP_ADMIN_PASSWORD", "admin", false},
		{"HANGAR_DATABASE_DSN", "postgres://hangar:hangar@postgres:5432/hangar", false},
		{"HANGAR_DATABASE_DSN", "postgres://hangar:${POSTGRES_PASSWORD:-x}@postgres:5432/hangar", false},
		{"HANGAR_OIDC_PROVIDERS", `[{"id": "k", "client_secret": "hangar-example-client-secret"}]`, false},
		{"MINIO_ROOT_PASSWORD", "${MINIO_ROOT_PASSWORD}", false},
		{"MINIO_ROOT_PASSWORD", "${MINIO_ROOT_PASSWORD:?Set it}", true},
		{"HANGAR_DATABASE_DSN", "postgres://hangar:${POSTGRES_PASSWORD:?Set it}@postgres:5432/hangar", true},
		{"HANGAR_OIDC_PROVIDERS", `[{"id": "k", "client_secret": "${S:?Set it}"}]`, true},
		{"HANGAR_SECURITY_BOOTSTRAP_ADMIN_PASSWORD", "${HANGAR_ADMIN_PASSWORD:-}", true},
		{"HANGAR_SECURITY_BOOTSTRAP_ADMIN_PASSWORD", "${OTHER_PASSWORD:-}", false},
		{"HANGAR_STORAGE_S3_ACCESS_KEY_ID", "hangar", true},
	} {
		problems := secretProblems(tc.key, tc.value)
		if got := len(problems) == 0; got != tc.ok {
			t.Errorf("%s=%q: passed=%v, want %v (%v)", tc.key, tc.value, got, tc.ok, problems)
		}
	}
}

// TestComposeOnlyHangarListensOnEveryInterface checks that database, object
// store and admin ports are published on 127.0.0.1 or not at all. Anyone on
// the network who reaches MinIO's console with its root user can read and
// change every Blob without going through Hangar.
func TestComposeOnlyHangarListensOnEveryInterface(t *testing.T) {
	for _, path := range composeFiles(t) {
		_, c := readCompose(t, path)
		for svc, s := range c.Services {
			for _, p := range s.Ports {
				if err := checkPort(p); err != "" {
					t.Errorf("%s: services.%s.ports: %s", path, svc, err)
				}
			}
		}
	}
}

func checkPort(p any) string {
	switch p := p.(type) {
	case string:
		if p == hangarPort || strings.HasPrefix(p, "127.0.0.1:") {
			return ""
		}
		return "publish " + p + " on 127.0.0.1 (\"127.0.0.1:" + p + "\") or drop it; only " + hangarPort + " may listen on every interface"
	case map[string]any:
		if ip, _ := p["host_ip"].(string); ip == "127.0.0.1" {
			return ""
		}
		return "set host_ip: 127.0.0.1 on every published port except Hangar's"
	default:
		return "unrecognised port entry"
	}
}

func TestComposePortCheckCatchesExposedPorts(t *testing.T) {
	for p, ok := range map[any]bool{
		"9001:9001":               false,
		"8082:8082":               false,
		"0.0.0.0:8081:8081":       false,
		"127.0.0.1:9001:9001":     true,
		hangarPort:                true,
		"5432:5432":               false,
		"127.0.0.1:5432:5432/tcp": true,
	} {
		if got := checkPort(p) == ""; got != ok {
			t.Errorf("checkPort(%q) passed=%v, want %v", p, got, ok)
		}
	}
}

// TestKeycloakRealmClientSecretComesFromTheEnvironment checks that the
// example realm reads each client secret from a variable that the Compose
// file requires, so the realm file holds no secret.
func TestKeycloakRealmClientSecretComesFromTheEnvironment(t *testing.T) {
	b, err := os.ReadFile("compose/keycloak/hangar-realm.json")
	if err != nil {
		t.Fatal(err)
	}
	var realm struct {
		Clients []struct {
			ClientID     string `json:"clientId"`
			PublicClient bool   `json:"publicClient"`
			Secret       string `json:"secret"`
		} `json:"clients"`
	}
	if err := json.Unmarshal(b, &realm); err != nil {
		t.Fatal(err)
	}
	_, c := readCompose(t, "compose/oidc-keycloak.yaml")
	env := c.Services["keycloak"].Environment
	placeholder := regexp.MustCompile(`^\$\{([A-Z_][A-Z0-9_]*)\}$`)
	for _, cl := range realm.Clients {
		if cl.PublicClient {
			continue
		}
		m := placeholder.FindStringSubmatch(cl.Secret)
		if m == nil {
			t.Errorf("client %s: secret must be a ${NAME} placeholder that Keycloak fills from its environment, not %q", cl.ClientID, cl.Secret)
			continue
		}
		v, _ := env[m[1]].(string)
		if !requiredOnly(v) {
			t.Errorf("client %s: the keycloak service must set %s from a required variable, got %q", cl.ClientID, m[1], v)
		}
	}
}

// TestHelmExampleValuesHoldNoCredentials checks that the chart's example
// and CI values keep credentials out of Helm values: people copy these
// files, and Helm stores values in the release.
func TestHelmExampleValuesHoldNoCredentials(t *testing.T) {
	files, err := filepath.Glob("helm/examples/*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	ci, err := filepath.Glob("helm/hangar/ci/*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, ci...)
	if len(files) == 0 {
		t.Fatal("no example values")
	}
	for _, path := range files {
		b, err := os.ReadFile(path) // #nosec G304 -- test fixture path.
		if err != nil {
			t.Fatal(err)
		}
		var v struct {
			Secrets map[string]any `yaml:"secrets"`
		}
		if err := yaml.Unmarshal(b, &v); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if len(v.Secrets) > 0 {
			t.Errorf("%s: put credentials in a Secret named by existingSecret, not in secrets (%d keys)", path, len(v.Secrets))
		}
	}
}

// helmCommand matches a helm install or upgrade command, with its
// continuation lines joined.
var helmCommand = regexp.MustCompile(`helm (?:install|upgrade)[^` + "`" + `\n]*`)

// TestHelmInstallCommandsSetPublicURL checks every helm install and upgrade
// command in the README and the docs: the chart refuses to render without
// publicURL, so a command that sets neither it nor a values file fails.
func TestHelmInstallCommandsSetPublicURL(t *testing.T) {
	files := []string{"../README.md"}
	err := filepath.WalkDir("../docs", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (path == "../docs/spec" || path == "../docs/build") {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(path, ".md") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, path := range files {
		b, err := os.ReadFile(path) // #nosec G304 -- repository docs.
		if err != nil {
			t.Fatal(err)
		}
		text := strings.ReplaceAll(string(b), "\\\n", " ")
		for _, cmd := range helmCommand.FindAllString(text, -1) {
			if !strings.Contains(cmd, "oci://ghcr.io/hangar-hub/charts/hangar") && !strings.Contains(cmd, "deploy/helm/hangar") {
				continue
			}
			found++
			if !setsPublicURL(cmd) {
				t.Errorf("%s: %q does not set publicURL (--set publicURL=... or -f values.yaml)", path, strings.Join(strings.Fields(cmd), " "))
			}
		}
	}
	if found == 0 {
		t.Fatal("found no helm install commands; is the pattern still right?")
	}
}

// helmTemplateCommand matches a helm template command of Hangar's chart in
// a shell block, with its continuation lines joined.
var helmTemplateCommand = regexp.MustCompile(`(?m)^helm template [^\n]*(?:oci://ghcr\.io/hangar-hub/charts/hangar|deploy/helm/hangar)[^\n]*`)

// TestHelmTemplateCommandsSkipTests checks the helm template commands in the
// GitOps docs and the GitOps example values. helm template renders the
// chart's helm test Pod as an ordinary Pod, so a render applied as it is
// runs it before Hangar is up and leaves it in Error; Argo CD skips it, and
// --skip-tests does the same for every other tool.
func TestHelmTemplateCommandsSkipTests(t *testing.T) {
	b, err := os.ReadFile("../docs/get-started/helm.md")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(b), "\\\n", " ")
	cmds := helmTemplateCommand.FindAllString(text, -1)
	if len(cmds) == 0 {
		t.Fatal("found no helm template commands in docs/get-started/helm.md; is the pattern still right?")
	}
	for _, cmd := range cmds {
		if !slices.Contains(strings.Fields(cmd), "--skip-tests") {
			t.Errorf("docs/get-started/helm.md: %q does not pass --skip-tests", cmd)
		}
	}
	if !strings.Contains(text, "skipTests: true") {
		t.Error("docs/get-started/helm.md does not give Kustomize's skipTests for helmCharts")
	}
	values, err := os.ReadFile("helm/examples/gitops-values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(values), "--skip-tests") {
		t.Error("helm/examples/gitops-values.yaml does not say to render with --skip-tests")
	}
}

func setsPublicURL(cmd string) bool {
	f := strings.Fields(cmd)
	for i, w := range f {
		switch {
		case strings.HasPrefix(w, "publicURL=") || strings.HasPrefix(w, "--set=publicURL=") || strings.HasPrefix(w, "'publicURL="):
			return true
		case (w == "--set" || w == "--set-string") && i+1 < len(f) && strings.HasPrefix(strings.Trim(f[i+1], `'"`), "publicURL="):
			return true
		case w == "-f" || w == "--values" || strings.HasPrefix(w, "--values=") || w == "--reuse-values":
			return true
		}
	}
	return false
}

func TestSetsPublicURL(t *testing.T) {
	for cmd, ok := range map[string]bool{
		"helm install hangar oci://ghcr.io/hangar-hub/charts/hangar":                                         false,
		"helm install hangar oci://ghcr.io/hangar-hub/charts/hangar --namespace hangar":                      false,
		"helm install hangar oci://ghcr.io/hangar-hub/charts/hangar --set publicURL=https://h.example.com":   true,
		"helm install hangar oci://ghcr.io/hangar-hub/charts/hangar --set 'publicURL=https://h.example.com'": true,
		"helm install hangar oci://ghcr.io/hangar-hub/charts/hangar -f values.yaml":                          true,
	} {
		if got := setsPublicURL(cmd); got != ok {
			t.Errorf("setsPublicURL(%q) = %v, want %v", cmd, got, ok)
		}
	}
}

// TestKeycloakRealmDefinesRequestedScopes checks that the example realm can
// answer Hangar's authorisation request. A realm file that lists
// clientScopes replaces Keycloak's built-in ones, so every scope Hangar asks
// for (other than openid), and basic for the sub claim, must be defined in
// the file and be a default scope of the client. Otherwise Keycloak refuses
// the request with invalid_scope.
func TestKeycloakRealmDefinesRequestedScopes(t *testing.T) {
	b, err := os.ReadFile("compose/keycloak/hangar-realm.json")
	if err != nil {
		t.Fatal(err)
	}
	var realm struct {
		ClientScopes []struct {
			Name            string `json:"name"`
			ProtocolMappers []struct {
				ProtocolMapper string            `json:"protocolMapper"`
				Config         map[string]string `json:"config"`
			} `json:"protocolMappers"`
		} `json:"clientScopes"`
		Clients []struct {
			ClientID            string   `json:"clientId"`
			RedirectURIs        []string `json:"redirectUris"`
			DefaultClientScopes []string `json:"defaultClientScopes"`
		} `json:"clients"`
	}
	if err := json.Unmarshal(b, &realm); err != nil {
		t.Fatal(err)
	}
	claims := map[string]map[string]bool{}
	mappers := map[string]map[string]bool{}
	for _, s := range realm.ClientScopes {
		claims[s.Name], mappers[s.Name] = map[string]bool{}, map[string]bool{}
		for _, m := range s.ProtocolMappers {
			mappers[s.Name][m.ProtocolMapper] = true
			if c := m.Config["claim.name"]; c != "" {
				claims[s.Name][c] = true
			}
			// Mappers with a fixed claim name.
			switch m.ProtocolMapper {
			case "oidc-full-name-mapper":
				claims[s.Name]["name"] = true
			case "oidc-sub-mapper":
				claims[s.Name]["sub"] = true
			}
		}
	}

	_, c := readCompose(t, "compose/oidc-keycloak.yaml")
	hangarEnv := c.Services["hangar"].Environment
	var providers []struct {
		ID       string            `json:"id"`
		ClientID string            `json:"client_id"`
		Scopes   []string          `json:"scopes"`
		Claims   map[string]string `json:"claims"`
	}
	raw, _ := hangarEnv["HANGAR_OIDC_PROVIDERS"].(string)
	if err := json.Unmarshal([]byte(raw), &providers); err != nil || len(providers) != 1 {
		t.Fatalf("HANGAR_OIDC_PROVIDERS: want one provider, got %d (%v)", len(providers), err)
	}
	p := providers[0]
	var client *struct {
		ClientID            string   `json:"clientId"`
		RedirectURIs        []string `json:"redirectUris"`
		DefaultClientScopes []string `json:"defaultClientScopes"`
	}
	for i := range realm.Clients {
		if realm.Clients[i].ClientID == p.ClientID {
			client = &realm.Clients[i]
		}
	}
	if client == nil {
		t.Fatalf("the realm has no client %q", p.ClientID)
	}
	defaults := map[string]bool{}
	for _, s := range client.DefaultClientScopes {
		defaults[s] = true
		if claims[s] == nil {
			t.Errorf("client %s: default scope %q is not defined in clientScopes", client.ClientID, s)
		}
	}
	for _, s := range append([]string{"basic"}, p.Scopes...) {
		if s == "openid" {
			continue
		}
		if claims[s] == nil {
			t.Errorf("scope %q is not defined in the realm's clientScopes", s)
		}
		if !defaults[s] {
			t.Errorf("scope %q is not a default scope of client %s", s, client.ClientID)
		}
	}
	if !mappers["basic"]["oidc-sub-mapper"] {
		t.Error("the basic scope needs an oidc-sub-mapper, or tokens have no sub claim")
	}
	for _, claim := range p.Claims {
		found := false
		for _, s := range p.Scopes {
			found = found || claims[s][claim]
		}
		if !found {
			t.Errorf("no requested scope maps the %q claim Hangar reads", claim)
		}
	}

	// The redirect URI follows Hangar's public URL, so OIDC works on any
	// HANGAR_PORT.
	want := "${" + "HANGAR_PUBLIC_URL}/auth/oidc/" + p.ID + "/callback"
	if len(client.RedirectURIs) != 1 || client.RedirectURIs[0] != want {
		t.Errorf("client %s: redirectUris = %q, want [%q]", client.ClientID, client.RedirectURIs, want)
	}
	if kc, h := c.Services["keycloak"].Environment["HANGAR_PUBLIC_URL"], hangarEnv["HANGAR_SERVER_PUBLIC_URL"]; kc == nil || kc != h {
		t.Errorf("the keycloak service's HANGAR_PUBLIC_URL (%v) must match Hangar's HANGAR_SERVER_PUBLIC_URL (%v)", kc, h)
	}
}

// gitClone matches a git clone command, with any options before clone, and
// not a quoted message such as "git clone".
var gitClone = regexp.MustCompile(`(?:^|[^"'\w-])git\s+(?:-c\s+\S+\s+)*clone\s`)

// TestSmokeCloneSkipsCredentialHelpers checks that scripts/smoke.sh, which
// CI runs against every Compose example and the Helm chart, clones with the
// host's credential helpers turned off. A GUI helper such as osxkeychain can
// block the clone, and git offers URL credentials to helpers for storage.
func TestSmokeCloneSkipsCredentialHelpers(t *testing.T) {
	b, err := os.ReadFile("../scripts/smoke.sh")
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for n, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") || !gitClone.MatchString(line) {
			continue
		}
		found++
		if !strings.Contains(line, "git -c credential.helper= ") {
			t.Errorf("scripts/smoke.sh:%d: run git clone as git -c credential.helper= clone: %s", n+1, strings.TrimSpace(line))
		}
	}
	if found == 0 {
		t.Fatal("found no git clone in scripts/smoke.sh; is the pattern still right?")
	}
}

// nginxBlock matches an nginx code block in the docs.
var nginxBlock = regexp.MustCompile("(?s)```nginx\n(.*?)```")

// nginxStreaming are the directives every nginx proxy in the docs needs for
// large files, with the value each must have. Without proxy_buffering off and
// proxy_max_temp_file_size 0, nginx spools a response for a slow reader to a
// temporary file of at most 1 GiB, and a large read from the Blob store can
// end early: on a home cluster, completing 1 GiB+ uploads failed with
// "upload part 65/66: unexpected EOF".
var nginxStreaming = map[string]string{
	"proxy_buffering":          "off",
	"proxy_request_buffering":  "off",
	"proxy_max_temp_file_size": "0",
	"client_max_body_size":     "0",
	"proxy_read_timeout":       "1h",
}

// nginxProblems returns the streaming directives a config lacks.
func nginxProblems(conf string) []string {
	got := map[string]string{}
	for _, line := range strings.Split(conf, "\n") {
		f := strings.Fields(strings.TrimSuffix(strings.TrimSpace(line), ";"))
		if len(f) == 2 {
			got[f[0]] = f[1]
		}
	}
	var problems []string
	for k, want := range nginxStreaming {
		if got[k] != want {
			problems = append(problems, fmt.Sprintf("%s %s", k, want))
		}
	}
	return problems
}

// TestProxyDocsNginxStreamsLargeFiles checks every nginx example in the
// proxy docs, for Hangar and for MinIO, sets the directives large files need.
func TestProxyDocsNginxStreamsLargeFiles(t *testing.T) {
	b, err := os.ReadFile("../docs/operate/proxies.md")
	if err != nil {
		t.Fatal(err)
	}
	blocks := nginxBlock.FindAllStringSubmatch(string(b), -1)
	upstreams := map[string]bool{}
	for _, m := range blocks {
		for _, p := range nginxProblems(m[1]) {
			t.Errorf("nginx example without %s:\n%s", p, m[1])
		}
		for _, port := range []string{"8080", "9000"} {
			if strings.Contains(m[1], "proxy_pass http://127.0.0.1:"+port) {
				upstreams[port] = true
			}
		}
	}
	if !upstreams["8080"] || !upstreams["9000"] {
		t.Errorf("want nginx examples for Hangar (:8080) and MinIO (:9000), found %v", upstreams)
	}
	if !strings.Contains(string(b), "nginx.ingress.kubernetes.io/proxy-max-temp-file-size") {
		t.Error("the ingress-nginx example does not turn off the temporary file limit")
	}
}

func TestNginxProblems(t *testing.T) {
	if p := nginxProblems("client_max_body_size 0;\nproxy_buffering off;\n"); len(p) != 3 {
		t.Errorf("nginxProblems found %v, want the three missing directives", p)
	}
	if p := nginxProblems("proxy_buffering on;\nproxy_request_buffering off;\nproxy_max_temp_file_size 0;\nclient_max_body_size 0;\nproxy_read_timeout 1h;"); len(p) != 1 {
		t.Errorf("nginxProblems found %v, want proxy_buffering off", p)
	}
}

// TestStorageDocsMinIORegion checks that the storage docs tell MinIO users to
// use their MinIO's region. A MinIO with a region set rejects requests signed
// for us-east-1, which the docs used to prescribe.
func TestStorageDocsMinIORegion(t *testing.T) {
	b, err := os.ReadFile("../docs/operate/storage.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "| MinIO |") {
			continue
		}
		cells := strings.Split(line, "|")
		if strings.Contains(cells[2], "region: us-east-1") {
			t.Errorf("the MinIO settings prescribe us-east-1: %s", cells[2])
		}
		if !strings.Contains(line, "configured with") {
			t.Errorf("the MinIO row does not say to use MinIO's configured region: %s", line)
		}
		return
	}
	t.Fatal("no MinIO row in docs/operate/storage.md")
}

// pgIsReadyToken matches one argument of a pg_isready command written as a
// YAML list (["CMD", "pg_isready", "-U", "hangar"]) or as a shell string.
var pgIsReadyToken = regexp.MustCompile(`[^\s"',\[\]]+`)

// TestPostgresHealthchecksUseTCP checks that every pg_isready health check
// in the Compose examples and the CI workflows connects over TCP. The
// postgres image's entrypoint runs a temporary init server that listens
// only on the Unix socket, so a check without -h reports healthy before
// Postgres accepts TCP. A service that waits on that check, such as the
// scale example's one-shot migrate, then gets connection refused.
func TestPostgresHealthchecksUseTCP(t *testing.T) {
	workflows, err := filepath.Glob("../.github/workflows/*.yml")
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, path := range append(composeFiles(t), workflows...) {
		b, err := os.ReadFile(path) // #nosec G304 -- test fixture path.
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "#") || !strings.Contains(line, "pg_isready") {
				continue
			}
			found++
			if !pgIsReadyUsesTCP(line) {
				t.Errorf("%s: line %d: %s\n\tpass -h 127.0.0.1 so the check waits for Postgres to accept TCP, not just the init server's Unix socket",
					path, i+1, strings.TrimSpace(line))
			}
		}
	}
	if found == 0 {
		t.Fatal("no pg_isready health check found; the check has nothing to test")
	}
}

// pgIsReadyUsesTCP reports whether the pg_isready command in line names a
// TCP host. A host that starts with / is a socket directory.
func pgIsReadyUsesTCP(line string) bool {
	args := pgIsReadyToken.FindAllString(line, -1)
	start := -1
	for i, a := range args {
		if a == "pg_isready" || strings.HasSuffix(a, "/pg_isready") {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return false
	}
	for i := start; i < len(args); i++ {
		a := args[i]
		var host string
		switch {
		case a == "-h" || a == "--host":
			if i+1 < len(args) {
				host = args[i+1]
			}
		case strings.HasPrefix(a, "--host="):
			host = strings.TrimPrefix(a, "--host=")
		case strings.HasPrefix(a, "-h") && !strings.HasPrefix(a, "--"):
			host = strings.TrimPrefix(a, "-h")
		default:
			continue
		}
		return host != "" && !strings.HasPrefix(host, "/")
	}
	return false
}

func TestPgIsReadyTCPCheck(t *testing.T) {
	for line, ok := range map[string]bool{
		`test: ["CMD", "pg_isready", "-U", "hangar", "-d", "hangar"]`:                    false,
		`test: ["CMD", "pg_isready", "-h", "127.0.0.1", "-U", "hangar", "-d", "hangar"]`: true,
		`--health-cmd "pg_isready -U hangar"`:                                            false,
		`--health-cmd "pg_isready -h 127.0.0.1 -U hangar"`:                               true,
		`test: ["CMD-SHELL", "pg_isready --host=localhost -U hangar"]`:                   true,
		`test: ["CMD-SHELL", "pg_isready -h127.0.0.1 -U hangar"]`:                        true,
		`test: ["CMD-SHELL", "pg_isready -h /var/run/postgresql -U hangar"]`:             false,
		`test: ["CMD-SHELL", "pg_isready -h"]`:                                           false,
	} {
		if got := pgIsReadyUsesTCP(line); got != ok {
			t.Errorf("pgIsReadyUsesTCP(%q) = %v, want %v", line, got, ok)
		}
	}
}
