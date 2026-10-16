package deploy

import (
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jamesagarside/hangar/internal/httpx"
)

// healthcheckWait matches readyzWait in cmd/hangar/healthcheck.go: how long
// hangar healthcheck waits for /readyz. The image HEALTHCHECK must give the
// command longer than that, so the command reports instead of being killed.
const healthcheckWait = httpx.ReadyTimeout + 2*time.Second

// TestImageHealthcheckOutwaitsReadyz checks that the image HEALTHCHECK
// waits longer than /readyz may take to answer. With Docker's --timeout
// shorter than that, a slow but healthy database or Blob store marks the
// container unhealthy.
func TestImageHealthcheckOutwaitsReadyz(t *testing.T) {
	b, err := os.ReadFile("../Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^HEALTHCHECK\s.*--timeout=(\S+)`).FindStringSubmatch(string(b))
	if m == nil {
		t.Fatal("the Dockerfile has no HEALTHCHECK with --timeout")
	}
	d, err := time.ParseDuration(m[1])
	if err != nil {
		t.Fatalf("HEALTHCHECK --timeout=%s: %v", m[1], err)
	}
	if d <= healthcheckWait {
		t.Errorf("HEALTHCHECK --timeout=%s must be longer than hangar healthcheck's wait of %s (/readyz answers within %s)",
			d, healthcheckWait, httpx.ReadyTimeout)
	}
}

// TestTraefikHealthcheckOutwaitsReadyz checks that every Traefik health
// check on /readyz in the Compose examples sets a timeout longer than
// /readyz may take. Traefik's default is 5 seconds.
func TestTraefikHealthcheckOutwaitsReadyz(t *testing.T) {
	label := regexp.MustCompile(`traefik\.http\.services\.([\w-]+)\.loadbalancer\.healthcheck\.(path|timeout):\s*(\S+)`)
	checked := 0
	for _, path := range composeFiles(t) {
		raw, _ := readCompose(t, path)
		paths, timeouts := map[string]string{}, map[string]string{}
		for _, m := range label.FindAllStringSubmatch(stripComments(raw), -1) {
			if m[2] == "path" {
				paths[m[1]] = m[3]
			} else {
				timeouts[m[1]] = strings.Trim(m[3], `"`)
			}
		}
		for svc, p := range paths {
			if p != "/readyz" {
				continue
			}
			checked++
			v, ok := timeouts[svc]
			if !ok {
				t.Errorf("%s: Traefik service %s checks /readyz with the default 5 second timeout; set healthcheck.timeout above %s", path, svc, httpx.ReadyTimeout)
				continue
			}
			d, err := time.ParseDuration(v)
			if err != nil {
				t.Errorf("%s: Traefik service %s healthcheck.timeout %q: %v", path, svc, v, err)
				continue
			}
			if d <= httpx.ReadyTimeout {
				t.Errorf("%s: Traefik service %s healthcheck.timeout %s must be longer than %s", path, svc, d, httpx.ReadyTimeout)
			}
		}
	}
	if checked == 0 {
		t.Fatal("found no Traefik health check on /readyz; the check no longer covers it")
	}
}
