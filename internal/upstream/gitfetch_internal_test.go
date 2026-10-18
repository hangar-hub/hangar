package upstream

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/model"
)

func pkt(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }

// TestFetchPackBoundsAdvertisedRefs checks that an Upstream cannot make
// Hangar read refs without end. Before MaxUpstreamRefs, every advertised
// ref was kept and became a ref update of the Mirror.
func TestFetchPackBoundsAdvertisedRefs(t *testing.T) {
	old := MaxUpstreamRefs
	MaxUpstreamRefs = 10
	t.Cleanup(func() { MaxUpstreamRefs = old })
	refs := func(n int) string {
		var b strings.Builder
		for i := range n {
			b.WriteString(pkt(fmt.Sprintf("%s refs/heads/b%d\n", strings.Repeat("a", 40), i)))
		}
		return b.String() + "0000"
	}
	for _, v2 := range []bool{false, true} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && v2:
				_, _ = w.Write([]byte(pkt("# service=git-upload-pack\n") + "0000" + pkt("version 2\n") + pkt("ls-refs\n") + "0000"))
			case r.Method == http.MethodGet:
				_, _ = w.Write([]byte(pkt("# service=git-upload-pack\n") + "0000" + refs(11)))
			default:
				_, _ = w.Write([]byte(refs(11)))
			}
		}))
		c := NewHTTPClient(HTTPOptions{Version: "test", MaxAttempts: 1})
		id := model.RepoID{Type: model.RepoTypeModel, Namespace: "a", Name: "b"}
		_, err := c.FetchPack(context.Background(), Upstream{Name: "x", BaseURL: srv.URL}, Credentials{}, id, nil)
		require.ErrorContains(t, err, "the upstream advertises more than 10 refs", "v2=%v", v2)
		srv.Close()
	}
}
