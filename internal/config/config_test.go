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

	"github.com/hangar-hub/hangar/internal/config"
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
	require.Equal(t, "hangar-admin", cfg.Security.BootstrapAdmin.Username, "admin is a reserved Namespace name")
	require.Equal(t, "http://localhost:8080/xet-cas", cfg.CASURL())
	require.Equal(t, config.UploadDirect, cfg.Storage.S3.UploadMode, "clients upload straight to the store by default")
	require.Equal(t, config.ByteSize(300_000_000), cfg.Viewer.MaxRequestBytes, "the Hub's 300 MB Viewer budget")
	require.Equal(t, "https://hangar.page/", cfg.UI.DocsURL)
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
	cfg.Storage.S3.DownloadMode = "stream"
	cfg.Storage.S3.UploadMode = "presigned"
	cfg.Storage.S3.StartupWait = -time.Second
	cfg.Viewer.MaxRequestBytes = 0
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
		`storage.s3.download_mode "stream" must be redirect or proxy`,
		`storage.s3.upload_mode "presigned" must be direct or proxy`,
		"storage.s3.startup_wait must be between 0 and 1h",
		"viewer.max_request_bytes",
	} {
		require.Contains(t, msg, want)
	}
	require.GreaterOrEqual(t, len(errs), 11)
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
		"HANGAR_TEST_POSTGRES=1",                // ignored prefix
		"HANGAR_CLI_URL=https://hangar.example", // the client commands' variables
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

func TestCommitInlineMaxHoldsARegularFile(t *testing.T) {
	cfg, err := config.Load(config.LoadOptions{Env: []string{"HANGAR_SERVER_PUBLIC_URL=http://localhost:8080", "HANGAR_HFAPI_COMMIT_INLINE_MAX=1MiB"}})
	require.NoError(t, err)
	require.ErrorContains(t, cfg.ValidateBeforeSecrets(), "hfapi.commit_inline_max must be at least hfapi.regular_upload_max")
	cfg.HFAPI.CommitInlineMax = 10 * config.MiB
	require.NoError(t, cfg.ValidateBeforeSecrets())
	def, err := config.Load(config.LoadOptions{Env: []string{"HANGAR_SERVER_PUBLIC_URL=http://localhost:8080"}})
	require.NoError(t, err)
	require.Equal(t, config.GiB, def.HFAPI.CommitInlineMax)
}

// TestWebAndWorkerRolesNeedAnExplicitInstanceKey: a web or worker process
// with its own server.data_dir would otherwise generate a key of its own,
// and could not read what the other processes sealed or signed. Processes
// that share a data_dir may use the key an earlier start saved there.
func TestWebAndWorkerRolesNeedAnExplicitInstanceKey(t *testing.T) {
	for _, role := range []string{"web", "worker"} {
		cfg, err := config.Load(config.LoadOptions{Env: []string{"HANGAR_SERVER_PUBLIC_URL=http://localhost:8080",
			"HANGAR_SERVER_ROLE=" + role, "HANGAR_DATABASE_DRIVER=postgres", "HANGAR_DATABASE_DSN=postgres://hangar@db/hangar"}})
		require.NoError(t, err)
		err = cfg.Validate()
		require.ErrorContains(t, err, "security.instance_key is required when server.role is "+role, role)
		require.NotContains(t, err.Error(), "leave it empty on first start", role)

		// Before secrets, the key may still come from a shared data_dir.
		require.NoError(t, cfg.ValidateBeforeSecrets(), role)
		cfg.Server.DataDir = t.TempDir()
		_, err = config.EnsureSecrets(cfg)
		require.ErrorContains(t, err, "security.instance_key is required when server.role is "+role, role)
		require.NoFileExists(t, filepath.Join(cfg.Server.DataDir, config.SecretsFileName), role)

		// Processes that share a data_dir share the key saved there.
		require.NoError(t, os.WriteFile(filepath.Join(cfg.Server.DataDir, config.SecretsFileName),
			[]byte("instance_key: "+testKey+"\nbootstrap_admin_password: stored-password\n"), 0o600))
		_, err = config.EnsureSecrets(cfg)
		require.NoError(t, err, role)
		require.Equal(t, testKey, cfg.Security.InstanceKey, role)
		require.NoError(t, cfg.Validate(), role)

		cfg.Security.InstanceKey = testKey
		require.NoError(t, cfg.ValidateBeforeSecrets(), role)
		require.NoError(t, cfg.Validate(), role)
	}
}

// TestValidateRefusesURLDSNsForSQLite: database.driver defaults to sqlite,
// so a Postgres DSN without HANGAR_DATABASE_DRIVER=postgres used to pass
// validation and become a relative SQLite file path.
func TestValidateRefusesURLDSNsForSQLite(t *testing.T) {
	for _, dsn := range []string{
		"postgres://hangar:s3cret@db.example.com:5432/hangar",
		"postgresql://hangar:s3cret@db.example.com/hangar",
		"Postgres://hangar:s3cret@db.example.com/hangar",
		"mysql://hangar:s3cret@db.example.com/hangar",
		"http://hangar:s3cret@db.example.com/",
	} {
		cfg, err := config.Load(config.LoadOptions{Env: append(validEnv(), "HANGAR_DATABASE_DSN="+dsn)})
		require.NoError(t, err)
		require.Equal(t, config.DriverSQLite, cfg.Database.Driver)
		for _, err := range []error{cfg.Validate(), cfg.ValidateBeforeSecrets()} {
			require.ErrorContains(t, err, "database.driver is sqlite", dsn)
			require.ErrorContains(t, err, "set database.driver to postgres", dsn)
			require.NotContains(t, err.Error(), "s3cret", "the error must not echo the password")
			require.NotContains(t, err.Error(), "db.example.com", "the error names the scheme only")
		}
	}
	for _, dsn := range []string{"/var/lib/hangar/hangar.db", "hangar.db", "file:hangar.db?cache=shared", "FILE:hangar.db", `C:\hangar\hangar.db`, "c:/hangar/hangar.db", ":memory:"} {
		cfg, err := config.Load(config.LoadOptions{Env: append(validEnv(), "HANGAR_DATABASE_DSN="+dsn)})
		require.NoError(t, err)
		require.NoError(t, cfg.Validate(), dsn)
	}
	cfg, err := config.Load(config.LoadOptions{Env: append(validEnv(), "HANGAR_DATABASE_DRIVER=postgres",
		"HANGAR_DATABASE_DSN=postgres://hangar:s3cret@db.example.com/hangar")})
	require.NoError(t, err)
	require.NoError(t, cfg.Validate())
}

func TestValidateRefusesAReservedBootstrapUsername(t *testing.T) {
	cfg, err := config.Load(config.LoadOptions{Env: append(validEnv(), "HANGAR_SECURITY_BOOTSTRAP_ADMIN_USERNAME=admin")})
	require.NoError(t, err)
	require.ErrorContains(t, cfg.Validate(), `security.bootstrap_admin.username: invalid namespace name: "admin" is reserved`)
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

func TestLoadStoredSecretsReadsWithoutWriting(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	cfg := config.Default()
	cfg.Server.DataDir = dir
	require.NoError(t, config.LoadStoredSecrets(&cfg), "no file yet is not an error")
	require.Empty(t, cfg.Security.InstanceKey)
	_, err := os.Stat(dir)
	require.ErrorIs(t, err, os.ErrNotExist, "nothing is created")

	first := config.Default()
	first.Server.DataDir = dir
	_, err = config.EnsureSecrets(&first)
	require.NoError(t, err)
	require.NoError(t, config.LoadStoredSecrets(&cfg))
	require.Equal(t, first.Security.InstanceKey, cfg.Security.InstanceKey)
	require.Equal(t, first.Security.BootstrapAdmin.Password, cfg.Security.BootstrapAdmin.Password)

	set := config.Default()
	set.Server.DataDir = dir
	set.Security.InstanceKey = "explicit"
	require.NoError(t, config.LoadStoredSecrets(&set))
	require.Equal(t, "explicit", set.Security.InstanceKey, "a configured key wins")
}

func TestUIDocsURL(t *testing.T) {
	cfg, err := config.Load(config.LoadOptions{Env: append(validEnv(), "HANGAR_UI_DOCS_URL=https://docs.internal/hangar/")})
	require.NoError(t, err)
	require.NoError(t, cfg.Validate())
	require.Equal(t, "https://docs.internal/hangar/", cfg.UI.DocsURL)

	cfg, err = config.Load(config.LoadOptions{Env: append(validEnv(), "HANGAR_UI_DOCS_URL=")})
	require.NoError(t, err)
	require.NoError(t, cfg.Validate())
	require.Empty(t, cfg.UI.DocsURL, "an empty value turns the links off")

	cfg, err = config.Load(config.LoadOptions{Env: append(validEnv(), "HANGAR_UI_DOCS_URL=docs.internal")})
	require.NoError(t, err)
	require.ErrorContains(t, cfg.Validate(), `ui.docs_url "docs.internal" must be an absolute http or https URL`)
}
