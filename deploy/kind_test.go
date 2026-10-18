//go:build helm && kind

package deploy

// These tests install the chart on a Kubernetes cluster, normally kind (see
// scripts/kind-chart-test.sh and the "Helm chart on kind" CI job). They use
// the current kubeconfig context and need:
//
//   - HANGAR_KIND_IMAGE: the Hangar image, as repository:tag, loadable by the
//     cluster without a pull (kind load docker-image).
//   - The postgres and MinIO images named below, pulled or loaded.

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const (
	kindPostgresImage = "postgres:18-alpine"
	kindMinIOImage    = "pgsty/minio:RELEASE.2026-08-04T00-00-00Z"
)

// kindImage returns the Hangar image as --set arguments, or skips the test.
func kindImage(t *testing.T) []string {
	t.Helper()
	img := os.Getenv("HANGAR_KIND_IMAGE")
	if img == "" {
		t.Skip("HANGAR_KIND_IMAGE is not set")
	}
	repo, tag, ok := strings.Cut(img, ":")
	if !ok {
		t.Fatalf("HANGAR_KIND_IMAGE %q has no tag", img)
	}
	return []string{"--set", "image.repository=" + repo, "--set", "image.tag=" + tag, "--set", "image.pullPolicy=Never"}
}

// cluster runs a command against the cluster in the current context. Its
// output is returned, not logged, because some of it holds secrets.
func cluster(t *testing.T, stdin string, name string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("%s %s: %w: %s", name, args[0], err, errb.String())
	}
	return out.String(), nil
}

// mustCluster is cluster that fails the test on error.
func mustCluster(t *testing.T, stdin string, name string, args ...string) string {
	t.Helper()
	out, err := cluster(t, stdin, name, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// namespace creates a namespace that the test deletes afterwards.
func namespace(t *testing.T, name string) {
	t.Helper()
	_, _ = cluster(t, "", "kubectl", "delete", "namespace", name, "--wait", "--ignore-not-found")
	mustCluster(t, "", "kubectl", "create", "namespace", name)
	t.Cleanup(func() {
		_, _ = cluster(t, "", "kubectl", "delete", "namespace", name, "--wait=false")
	})
}

// dump logs the state of a namespace when a test fails, without Secrets.
func dump(t *testing.T, ns string) {
	t.Helper()
	if !t.Failed() {
		return
	}
	out, _ := cluster(t, "", "kubectl", "get", "pods,jobs,deployments", "--namespace", ns, "-o", "wide")
	t.Logf("namespace %s:\n%s", ns, out)
	for _, c := range []string{"migrate", "hangar"} {
		logs, _ := cluster(t, "", "kubectl", "logs", "--namespace", ns, "deployment/hangar-web", "-c", c, "--tail", "40")
		t.Logf("hangar-web %s logs:\n%s", c, logs)
	}
}

// generatedKey reads the generated Instance key from the cluster.
func generatedKey(t *testing.T, ns string) string {
	t.Helper()
	out := mustCluster(t, "", "kubectl", "get", "secret", "--namespace", ns, releaseName+"-generated",
		"-o", "jsonpath={.data.HANGAR_SECURITY_INSTANCE_KEY}")
	key, err := base64.StdEncoding.DecodeString(out)
	if err != nil || len(key) == 0 {
		t.Fatalf("generated Secret has no Instance key: %v", err)
	}
	return string(key)
}

// renderedKey returns the Instance key in the generated Secret that a
// server-side render (lookup works) produces.
func renderedKey(t *testing.T, ns string, args ...string) string {
	t.Helper()
	out := mustCluster(t, "", "helm", append([]string{"template", releaseName, "helm/hangar",
		"--namespace", ns, "--dry-run=server"}, args...)...)
	for _, doc := range decodeDocs(t, out) {
		md, _ := doc["metadata"].(map[string]any)
		if doc["kind"] == "Secret" && md["name"] == releaseName+"-generated" {
			sd, _ := doc["stringData"].(map[string]any)
			k, _ := sd["HANGAR_SECURITY_INSTANCE_KEY"].(string)
			return k
		}
	}
	t.Fatal("server-side render has no generated Secret")
	return ""
}

// TestKindGeneratedSecretsSurviveRenders installs the chart with generated
// secrets and checks that renders and upgrades with cluster access keep the
// Instance key the first install generated. The offline renders that cannot
// keep it fail instead (TestChartGenerateSecretsNeedsCluster).
func TestKindGeneratedSecretsSurviveRenders(t *testing.T) {
	img := kindImage(t)
	const ns = "chart-generated"
	namespace(t, ns)
	defer dump(t, ns)
	args := append([]string{"-f", "helm/hangar/ci/default-values.yaml"}, img...)
	mustCluster(t, "", "helm", append([]string{"install", releaseName, "helm/hangar", "--namespace", ns,
		"--wait", "--timeout", "5m"}, args...)...)
	key := generatedKey(t, ns)
	if got := renderedKey(t, ns, args...); got != key {
		t.Error("a server-side render made a new Instance key instead of keeping the installed one")
	}
	mustCluster(t, "", "helm", append([]string{"upgrade", releaseName, "helm/hangar", "--namespace", ns,
		"--wait", "--timeout", "5m"}, args...)...)
	if got := generatedKey(t, ns); got != key {
		t.Error("helm upgrade replaced the generated Instance key")
	}
}

// TestKindNotesReadOnlyRenderedSecretKeys runs checkNotes on the value sets
// where the chart generates secrets, which need a cluster to render. The
// namespace does not exist, as on a first install with --create-namespace.
func TestKindNotesReadOnlyRenderedSecretKeys(t *testing.T) {
	kindImage(t)
	const ns = "chart-notes-absent"
	_, _ = cluster(t, "", "kubectl", "delete", "namespace", ns, "--ignore-not-found")
	reads := 0
	for _, tc := range []notesCase{
		{"generated", nil},
		{"password set", []string{"--set-string", "secrets.HANGAR_SECURITY_BOOTSTRAP_ADMIN_PASSWORD=set"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"-f", "helm/hangar/ci/default-values.yaml", "--namespace", ns, "--dry-run=server"}, tc.args...)
			rendered := mustCluster(t, "", "helm", append([]string{"template", releaseName, "helm/hangar"}, args...)...)
			installed := mustCluster(t, "", "helm", append([]string{"install", releaseName, "helm/hangar"}, args...)...)
			reads += checkNotes(t, secretKeys(decodeDocs(t, rendered)), notesOf(t, installed, args))
		})
	}
	if reads == 0 {
		t.Fatal("NOTES read no Secret key in any case; the check no longer covers the password command")
	}
}

// kindBackends is Postgres and MinIO with a bucket, as plain manifests. The
// Postgres image takes a while to start, so Hangar's Pods start first.
const kindBackends = kindPostgres + "\n---\n" + kindMinIO

// kindPostgres is the Postgres half of kindBackends.
const kindPostgres = `
apiVersion: apps/v1
kind: Deployment
metadata: {name: postgres}
spec:
  selector: {matchLabels: {app: postgres}}
  template:
    metadata: {labels: {app: postgres}}
    spec:
      containers:
        - name: postgres
          image: ` + kindPostgresImage + `
          imagePullPolicy: IfNotPresent
          env:
            - {name: POSTGRES_USER, value: hangar}
            - {name: POSTGRES_DB, value: hangar}
            - name: POSTGRES_PASSWORD
              valueFrom: {secretKeyRef: {name: backends, key: POSTGRES_PASSWORD}}
          ports: [{containerPort: 5432}]
---
apiVersion: v1
kind: Service
metadata: {name: postgres}
spec:
  selector: {app: postgres}
  ports: [{port: 5432}]
`

// kindMinIO is the MinIO half of kindBackends, with a Job that creates the
// bucket.
const kindMinIO = `
apiVersion: apps/v1
kind: Deployment
metadata: {name: minio}
spec:
  selector: {matchLabels: {app: minio}}
  template:
    metadata: {labels: {app: minio}}
    spec:
      containers:
        - name: minio
          image: ` + kindMinIOImage + `
          imagePullPolicy: IfNotPresent
          args: [server, /data]
          env:
            - {name: MINIO_ROOT_USER, value: hangar}
            - name: MINIO_ROOT_PASSWORD
              valueFrom: {secretKeyRef: {name: backends, key: MINIO_ROOT_PASSWORD}}
          ports: [{containerPort: 9000}]
---
apiVersion: v1
kind: Service
metadata: {name: minio}
spec:
  selector: {app: minio}
  ports: [{port: 9000}]
---
apiVersion: batch/v1
kind: Job
metadata: {name: minio-bucket}
spec:
  backoffLimit: 30
  template:
    spec:
      restartPolicy: OnFailure
      containers:
        - name: mc
          image: ` + kindMinIOImage + `
          imagePullPolicy: IfNotPresent
          command: [sh, -c, 'mc alias set local http://minio:9000 hangar "$MINIO_ROOT_PASSWORD" && mc mb --ignore-existing local/hangar']
          env:
            - name: MINIO_ROOT_PASSWORD
              valueFrom: {secretKeyRef: {name: backends, key: MINIO_ROOT_PASSWORD}}
`

// randomHex returns n random bytes in hex.
func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// TestKindGitOpsInstall installs the chart the way Argo CD does: rendered
// with helm template and no cluster access, from the GitOps example values,
// and applied in one step together with the Postgres and MinIO it needs.
// Nothing runs first, so Hangar's Pods start before Postgres answers; the
// migrate init containers must wait for it, migrate once between them, and
// let web and worker Pods become ready. The render passes --skip-tests, as
// the GitOps docs say: applied as it is, the helm test Pod would run before
// Hangar is up and stay in Error. No Pod may end up failed.
func TestKindGitOpsInstall(t *testing.T) {
	kindImage(t)
	const ns = "chart-gitops"
	namespace(t, ns)
	defer dump(t, ns)

	// Several migrate init containers race, as in a scaled install.
	rendered := gitopsRender(t, ns, "--set", "web.replicas=3", "--set", "worker.replicas=3")
	docs := decodeDocs(t, rendered)
	if got := hooked(docs); len(got) > 0 {
		t.Fatalf("GitOps render has Helm hooks %v, which Argo CD would run before Postgres", got)
	}
	for _, doc := range docs {
		if doc["kind"] == "Pod" {
			md, _ := doc["metadata"].(map[string]any)
			t.Fatalf("GitOps render has a bare Pod %v, which kubectl apply would run as it is", md["name"])
		}
	}
	// helm template --namespace does not set metadata.namespace, as with
	// Argo CD, which applies into the Application's destination namespace.
	mustCluster(t, kindBackends+"\n---\n"+rendered, "kubectl", "apply", "--namespace", ns, "-f", "-")

	for _, d := range []string{"deployment/hangar-web", "deployment/hangar-worker"} {
		if _, err := cluster(t, "", "kubectl", "rollout", "status", "--namespace", ns, d, "--timeout", "6m"); err != nil {
			t.Fatalf("%s did not become ready: %v", d, err)
		}
	}
	// Ready means /readyz passed, which includes the schema check; ask the
	// migrator too, from inside a web Pod.
	out, err := cluster(t, "", "kubectl", "exec", "--namespace", ns, "deployment/hangar-web", "-c", "hangar", "--",
		"hangar", "admin", "migrate")
	if err != nil {
		t.Fatalf("hangar admin migrate in the web Pod after install: %v\n%s", err, out)
	}
	// Migrators wait for one another on an advisory lock. Without it, the
	// losers failed on bun's CREATE TABLE IF NOT EXISTS or its lock row, and
	// each failure cost the Pod a kubelet back-off (10 s, 20 s, and so on).
	pgLog := mustCluster(t, "", "kubectl", "logs", "--namespace", ns, "deployment/postgres")
	for _, race := range []string{"pg_type_typname_nsp_index", "pg_class_relname_nsp_index", "bun_migration_locks"} {
		if strings.Contains(pgLog, race) {
			t.Errorf("concurrent migrations collided in Postgres (%s):\n%s", race, pgLog)
		}
	}
	phases := mustCluster(t, "", "kubectl", "get", "pods", "--namespace", ns,
		"-o", `jsonpath={range .items[*]}{.metadata.name}{" "}{.status.phase}{"\n"}{end}`)
	for _, line := range strings.Split(strings.TrimSpace(phases), "\n") {
		if name, phase, _ := strings.Cut(line, " "); phase == "Failed" {
			t.Errorf("Pod %s failed after a GitOps install", name)
		}
	}
}

// gitopsRender renders the chart from the GitOps example values for ns,
// with one web and one worker Pod unless extra says otherwise, after
// creating the Secrets those values name.
func gitopsRender(t *testing.T, ns string, extra ...string) string {
	t.Helper()
	img := kindImage(t)
	pgPassword, minioPassword := randomHex(t, 16), randomHex(t, 16)
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	mustCluster(t, "", "kubectl", "create", "secret", "generic", "backends", "--namespace", ns,
		"--from-literal=POSTGRES_PASSWORD="+pgPassword, "--from-literal=MINIO_ROOT_PASSWORD="+minioPassword)
	mustCluster(t, "", "kubectl", "create", "secret", "generic", "hangar-secrets", "--namespace", ns,
		"--from-literal=HANGAR_DATABASE_DSN=postgres://hangar:"+pgPassword+"@postgres:5432/hangar?sslmode=disable",
		"--from-literal=HANGAR_STORAGE_S3_ACCESS_KEY_ID=hangar",
		"--from-literal=HANGAR_STORAGE_S3_SECRET_ACCESS_KEY="+minioPassword,
		"--from-literal=HANGAR_SECURITY_INSTANCE_KEY="+base64.StdEncoding.EncodeToString(key),
		"--from-literal=HANGAR_SECURITY_BOOTSTRAP_ADMIN_PASSWORD="+randomHex(t, 12))
	args := append([]string{"template", releaseName, "helm/hangar", "--namespace", ns,
		"-f", "helm/examples/gitops-values.yaml",
		"--set", "publicURL=http://hangar." + ns + ".svc:8080",
		"--set", "config.storage.s3.endpoint=http://minio:9000",
		"--set", "config.storage.s3.download_mode=proxy",
		"--set", "config.storage.s3.upload_mode=proxy",
		"--skip-tests"}, extra...)
	rendered, errOut, err := helmCmd(t, append(args, img...)...)
	if err != nil {
		t.Fatalf("helm template without cluster access: %v\n%s", err, errOut)
	}
	return rendered
}

// podProxy GETs path on port 8080 of a Pod through the API server, and
// returns the error kubectl reports for an answer other than 2xx.
func podProxy(t *testing.T, ns, pod, path string) (string, error) {
	t.Helper()
	return cluster(t, "", "kubectl", "get", "--raw", "/api/v1/namespaces/"+ns+"/pods/"+pod+":8080/proxy"+path)
}

// TestKindBlobStoreStartsLate installs Hangar with its S3 Blob store, MinIO,
// not running yet, as when an object store Pod starts after Hangar or is
// briefly down during a node drain. Hangar must wait for it for up to
// storage.s3.startup_wait: the web Pod starts, answers /healthz and stays
// not ready on /readyz, and once MinIO and its bucket exist, the web and
// worker Pods become ready with no container restart.
func TestKindBlobStoreStartsLate(t *testing.T) {
	const ns = "chart-late-blobs"
	kindImage(t)
	namespace(t, ns)
	defer dump(t, ns)
	rendered := gitopsRender(t, ns, "--set", "config.storage.s3.startup_wait=10m")
	mustCluster(t, kindPostgres+"\n---\n"+rendered, "kubectl", "apply", "--namespace", ns, "-f", "-")

	// Wait until the web Pod's hangar container has started: its startup
	// probe on /healthz passed, after the migrate init container ran.
	webPod := func() string {
		return strings.TrimSpace(mustCluster(t, "", "kubectl", "get", "pods", "--namespace", ns,
			"-l", "app.kubernetes.io/component=web", "-o", "jsonpath={.items[0].metadata.name}"))
	}
	deadline := time.Now().Add(6 * time.Minute)
	var pod string
	for {
		pod = webPod()
		started, _ := cluster(t, "", "kubectl", "get", "pod", pod, "--namespace", ns,
			"-o", `jsonpath={.status.containerStatuses[?(@.name=="hangar")].started}`)
		if strings.TrimSpace(started) == "true" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the web Pod's hangar container did not start while the Blob store was missing")
		}
		time.Sleep(2 * time.Second)
	}

	// It is waiting for the Blob store: alive, not ready, and saying why.
	if out, err := podProxy(t, ns, pod, "/healthz"); err != nil {
		t.Fatalf("/healthz while the Blob store is missing: %v\n%s", err, out)
	}
	if out, err := podProxy(t, ns, pod, "/readyz"); err == nil {
		t.Fatalf("/readyz answered 2xx while the Blob store is missing:\n%s", out)
	}
	ready := mustCluster(t, "", "kubectl", "get", "pod", pod, "--namespace", ns,
		"-o", `jsonpath={.status.conditions[?(@.type=="Ready")].status}`)
	if strings.TrimSpace(ready) == "True" {
		t.Fatalf("the web Pod is Ready while the Blob store is missing")
	}
	// Hangar's own retry message, not the readiness probe's warning, which
	// also says it is waiting: the store must not appear before Hangar has
	// failed to reach it at least once.
	retried := `"msg":"waiting for the Blob store"`
	var logs string
	for logDeadline := time.Now().Add(time.Minute); ; {
		logs = mustCluster(t, "", "kubectl", "logs", "--namespace", ns, pod, "-c", "hangar")
		if strings.Contains(logs, retried) {
			break
		}
		if time.Now().After(logDeadline) {
			t.Fatalf("the web Pod does not log that it waits for the Blob store:\n%s", logs)
		}
		time.Sleep(2 * time.Second)
	}

	// Start MinIO and make the bucket; Hangar must find it without a restart.
	mustCluster(t, kindMinIO, "kubectl", "apply", "--namespace", ns, "-f", "-")
	if _, err := cluster(t, "", "kubectl", "wait", "--namespace", ns, "job/minio-bucket",
		"--for=condition=complete", "--timeout", "5m"); err != nil {
		t.Fatalf("the MinIO bucket was not created: %v", err)
	}
	for _, d := range []string{"deployment/hangar-web", "deployment/hangar-worker"} {
		if _, err := cluster(t, "", "kubectl", "rollout", "status", "--namespace", ns, d, "--timeout", "4m"); err != nil {
			t.Fatalf("%s did not become ready once the Blob store answered: %v", d, err)
		}
	}
	if got := webPod(); got != pod {
		t.Errorf("the web Pod was replaced (%s, then %s)", pod, got)
	}
	if out, err := podProxy(t, ns, pod, "/readyz"); err != nil {
		t.Fatalf("/readyz once the Blob store answered: %v\n%s", err, out)
	}
	restarts := mustCluster(t, "", "kubectl", "get", "pods", "--namespace", ns, "-l", "app.kubernetes.io/component in (web,worker)",
		"-o", `jsonpath={range .items[*]}{.metadata.name}{" "}{.status.containerStatuses[?(@.name=="hangar")].restartCount}{"\n"}{end}`)
	for _, line := range strings.Split(strings.TrimSpace(restarts), "\n") {
		if name, n, _ := strings.Cut(line, " "); n != "0" {
			t.Errorf("Pod %s restarted its hangar container %s times while it waited for the Blob store", name, n)
		}
	}
	logs = mustCluster(t, "", "kubectl", "logs", "--namespace", ns, pod, "-c", "hangar")
	if !strings.Contains(logs, "the Blob store answered") {
		t.Errorf("the web Pod does not log that the Blob store answered:\n%s", logs)
	}
}
