package config_test

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/config"
)

const testKey = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=" // 32 bytes

func validEnv() []string {
	return []string{
		"HANGAR_SERVER_PUBLIC_URL=http://localhost:8080",
		"HANGAR_SECURITY_INSTANCE_KEY=" + testKey,
	}
}

func TestDefaultsValidateWithRequiredKeys(t *testing.T) {
	cfg, err := config.Load(config.LoadOptions{Env: validEnv()})
	require.NoError(t, err)
	require.NoError(t, cfg.Validate())
	require.Equal(t, ":8080", cfg.Server.Listen)
	require.Equal(t, 64*config.MiB, cfg.Server.MaxRequestBody)
	require.Equal(t, 60*time.Second, cfg.Jobs.Lease)
	require.Equal(t, "https://huggingface.co", cfg.Upstreams["huggingface"].BaseURL)
	require.Equal(t, "http://localhost:8080/xet-cas", cfg.CASURL())
	key, err := cfg.InstanceKeyBytes()
	require.NoError(t, err)
	require.Len(t, key, 32)
}

func TestValidateReportsEveryError(t *testing.T) {
	cfg := config.Default()
	cfg.Server.Role = "web" // with SQLite
	cfg.Storage.Backend = "gcs"
	cfg.Pull.Windows = []string{"25:00-07:00"}
	cfg.GC.Schedule = "every day"
	cfg.Telemetry.LogLevel = "trace"
	err := cfg.Validate()
	require.Error(t, err)
	var errs config.Errors
	require.ErrorAs(t, err, &errs)
	msg := err.Error()
	for _, want := range []string{
		"server.public_url is required",
		"security.instance_key is required",
		"needs database.driver postgres",
		"storage.backend \"gcs\"",
		"pull.windows",
		"gc.schedule",
		"telemetry.log_level",
	} {
		require.Contains(t, msg, want)
	}
	require.GreaterOrEqual(t, len(errs), 7)
}

func TestLoadOrderFileEnvFlags(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "hangar.yaml")
	require.NoError(t, os.WriteFile(file, []byte(`
server:
  public_url: https://file.example.com
  listen: ":9000"
  trusted_proxies: [10.0.0.0/8]
storage:
  backend: s3
  s3:
    bucket: from-file
    use_path_style: true
upstreams:
  huggingface:
    enabled: false
  local:
    base_url: https://other.example.com
    enabled: true
pull:
  bandwidth_limit: 10MiB
oidc:
  providers:
    - id: unifi
      name: UniFi
      issuer: https://sso.example.com
      client_id: hangar
      org_mappings:
        - {group: ml-team, org: ml, role: write}
`), 0o600))

	env := append(validEnv(),
		"HANGAR_SERVER_LISTEN=:9100",
		"HANGAR_STORAGE_S3_BUCKET=from-env",
		"HANGAR_SERVER_TRUSTED_PROXIES=192.168.0.0/16, 127.0.0.1",
		"HANGAR_UPSTREAMS_LOCAL_TOKEN=secret-token",
		"HANGAR_JOBS_LEASE=90s",
		"HANGAR_GIT_MAX_BLOB_SIZE=20MiB",
		"HANGAR_TEST_POSTGRES=1", // ignored prefix
	)
	cfg, err := config.Load(config.LoadOptions{File: file, Env: env, Set: []string{"server.listen=:9200", "jobs.workers=8"}})
	require.NoError(t, err)
	require.Equal(t, ":9200", cfg.Server.Listen, "flags win over env")
	require.Equal(t, "from-env", cfg.Storage.S3.Bucket, "env wins over file")
	require.True(t, cfg.Storage.S3.UsePathStyle)
	require.Equal(t, "http://localhost:8080", cfg.Server.PublicURL, "env wins over file")
	require.Equal(t, []string{"192.168.0.0/16", "127.0.0.1"}, cfg.Server.TrustedProxies)
	require.False(t, cfg.Upstreams["huggingface"].Enabled)
	require.Equal(t, "https://huggingface.co", cfg.Upstreams["huggingface"].BaseURL, "file merges into defaults")
	require.Equal(t, "secret-token", cfg.Upstreams["local"].Token)
	require.Equal(t, 90*time.Second, cfg.Jobs.Lease)
	require.Equal(t, 8, cfg.Jobs.Workers)
	require.Equal(t, 20*config.MiB, cfg.Git.MaxBlobSize)
	require.Equal(t, 10*config.MiB, cfg.Pull.BandwidthLimit)
	require.Len(t, cfg.OIDC.Providers, 1)
	require.Equal(t, "write", cfg.OIDC.Providers[0].OrgMappings[0].Role)
	require.NoError(t, cfg.Validate())
}

func TestLoadReportsUnknownKeysAndBadValues(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "hangar.yaml")
	require.NoError(t, os.WriteFile(file, []byte("server:\n  lisen: \":1\"\njobs:\n  lease: soon\n"), 0o600))
	_, err := config.Load(config.LoadOptions{
		File: file,
		Env:  []string{"HANGAR_NOPE=1"},
		Set:  []string{"nope.key=1", "broken"},
	})
	require.Error(t, err)
	msg := err.Error()
	require.Contains(t, msg, "HANGAR_NOPE")
	require.Contains(t, msg, "nope.key")
	require.Contains(t, msg, "broken")
	require.Contains(t, msg, "lisen")
	require.Contains(t, msg, "lease")
}

func TestConfigFileFromEnv(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "c.yaml")
	require.NoError(t, os.WriteFile(file, []byte("server:\n  listen: \":7000\"\n"), 0o600))
	cfg, err := config.Load(config.LoadOptions{Env: []string{"HANGAR_CONFIG=" + file}})
	require.NoError(t, err)
	require.Equal(t, ":7000", cfg.Server.Listen)
}

func TestValidateBeforeSecretsAllowsAnEmptyInstanceKey(t *testing.T) {
	cfg, err := config.Load(config.LoadOptions{Env: []string{"HANGAR_SERVER_PUBLIC_URL=http://localhost:8080"}})
	require.NoError(t, err)
	require.NoError(t, cfg.ValidateBeforeSecrets(), "EnsureSecrets generates the key")
	require.ErrorContains(t, cfg.Validate(), "security.instance_key is required")

	cfg.Security.InstanceKey = "c2hvcnQ="
	require.ErrorContains(t, cfg.ValidateBeforeSecrets(), "security.instance_key: decodes to 5 bytes")

	cfg.Security.InstanceKey = ""
	cfg.Storage.Backend = "tape"
	require.ErrorContains(t, cfg.ValidateBeforeSecrets(), "storage.backend")
}

func TestOIDCProvidersFromJSONEnv(t *testing.T) {
	env := append(validEnv(), `HANGAR_OIDC_PROVIDERS=[{"id":"kc","name":"Keycloak","issuer":"https://kc.example.com/realms/x","client_id":"hangar","scopes":["openid"]}]`)
	cfg, err := config.Load(config.LoadOptions{Env: env})
	require.NoError(t, err)
	require.Len(t, cfg.OIDC.Providers, 1)
	require.Equal(t, "kc", cfg.OIDC.Providers[0].ID)
	require.NoError(t, cfg.Validate())

	_, err = config.Load(config.LoadOptions{Env: []string{"HANGAR_OIDC_PROVIDERS=not json"}})
	require.ErrorContains(t, err, "JSON array")
}

func TestValidateOIDCAndUpstreams(t *testing.T) {
	cfg, err := config.Load(config.LoadOptions{Env: validEnv()})
	require.NoError(t, err)
	cfg.OIDC.Providers = []config.OIDCProvider{
		{ID: "a", Name: "A", Issuer: "https://a", ClientID: "c"},
		{ID: "a", Issuer: "ftp://x", OrgMappings: []config.OrgMapping{{Org: "-", Role: "owner"}}},
	}
	cfg.Upstreams["Bad_ID"] = config.UpstreamConfig{BaseURL: "nope"}
	cfg.Security.LocalLogin = false
	err = cfg.Validate()
	require.Error(t, err)
	for _, want := range []string{"is used twice", "name is required", "issuer", "client_id is required", "group is required", "org_mappings[0].org", "org_mappings[0].role", "Bad_ID", "base_url"} {
		require.Contains(t, err.Error(), want)
	}
}

func TestByteSize(t *testing.T) {
	for in, want := range map[string]config.ByteSize{
		"1024": 1024, "64MiB": 64 * config.MiB, "10GiB": 10 * config.GiB, "100KiB": 100 * config.KiB,
		"5GB": 5_000_000_000, "1.5KiB": 1536, "2 MiB": 2 * config.MiB,
	} {
		got, err := config.ParseByteSize(in)
		require.NoError(t, err, in)
		require.Equal(t, want, got, in)
	}
	for _, bad := range []string{"", "MiB", "12XB", "1e400"} {
		_, err := config.ParseByteSize(bad)
		require.Error(t, err, bad)
	}
	require.Equal(t, "64MiB", (64 * config.MiB).String())
	require.Equal(t, "1000", config.ByteSize(1000).String())
	b, err := (2 * config.GiB).MarshalText()
	require.NoError(t, err)
	var back config.ByteSize
	require.NoError(t, back.UnmarshalText(b))
	require.Equal(t, 2*config.GiB, back)
}

func TestEnsureSecretsGeneratesOnceThenReads(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Server.DataDir = filepath.Join(dir, "data")
	gen, err := config.EnsureSecrets(&cfg)
	require.NoError(t, err)
	require.True(t, gen.Any())
	require.NotEmpty(t, gen.InstanceKey)
	require.Len(t, gen.AdminPassword, 24)
	require.Equal(t, gen.InstanceKey, cfg.Security.InstanceKey)
	raw, err := base64.StdEncoding.DecodeString(cfg.Security.InstanceKey)
	require.NoError(t, err)
	require.Len(t, raw, 32)

	info, err := os.Stat(gen.Path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	again := config.Default()
	again.Server.DataDir = cfg.Server.DataDir
	gen2, err := config.EnsureSecrets(&again)
	require.NoError(t, err)
	require.False(t, gen2.Any(), "second start reads the file and prints nothing")
	require.Equal(t, cfg.Security.InstanceKey, again.Security.InstanceKey)
	require.Equal(t, cfg.Security.BootstrapAdmin.Password, again.Security.BootstrapAdmin.Password)
}

func TestEnsureSecretsPasswordFileAndExplicit(t *testing.T) {
	dir := t.TempDir()
	pw := filepath.Join(dir, "pw")
	require.NoError(t, os.WriteFile(pw, []byte("correct horse battery\n"), 0o600))
	cfg := config.Default()
	cfg.Server.DataDir = dir
	cfg.Security.InstanceKey = testKey
	cfg.Security.BootstrapAdmin.PasswordFile = pw
	gen, err := config.EnsureSecrets(&cfg)
	require.NoError(t, err)
	require.False(t, gen.Any())
	require.Equal(t, "correct horse battery", cfg.Security.BootstrapAdmin.Password)
	_, err = os.Stat(filepath.Join(dir, config.SecretsFileName))
	require.True(t, os.IsNotExist(err), "nothing generated, nothing written")

	cfg.Security.BootstrapAdmin.PasswordFile = filepath.Join(dir, "missing")
	_, err = config.EnsureSecrets(&cfg)
	require.Error(t, err)
}

func TestReferenceListsEveryKey(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, config.WriteReference(&buf))
	out := buf.String()
	for _, f := range config.Fields() {
		require.Contains(t, out, "`"+f.Key+"`", f.Key)
		require.NotEmpty(t, f.Description, "every key needs a description: %s", f.Key)
	}
	require.Contains(t, out, "HANGAR_STORAGE_S3_BUCKET")
	require.Contains(t, out, "`upstreams.<id>.base_url`")
	require.Contains(t, out, "`oidc.providers[].client_id`")
	require.Contains(t, out, "| `64MiB` |")
	require.NotContains(t, out, "\u2014", "house style: no em dashes")
	// Secrets never show a default value.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "`security.instance_key`") {
			require.Contains(t, line, "`\"\"`")
		}
	}
}

func TestEnvName(t *testing.T) {
	require.Equal(t, "HANGAR_STORAGE_S3_BUCKET", config.EnvName("storage.s3.bucket"))
	require.Equal(t, "HANGAR_PULL_PULL_THROUGH_ENABLED", config.EnvName("pull.pull_through.enabled"))
}

func TestParsePrefix(t *testing.T) {
	p, err := config.ParsePrefix("10.1.2.3/8")
	require.NoError(t, err)
	require.Equal(t, "10.0.0.0/8", p.String())
	p, err = config.ParsePrefix("::1")
	require.NoError(t, err)
	require.Equal(t, 128, p.Bits())
	_, err = config.ParsePrefix("nope")
	require.Error(t, err)
}
