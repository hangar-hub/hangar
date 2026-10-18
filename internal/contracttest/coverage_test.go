// Package contracttest checks that the phase 0 interfaces have every method
// the Hub API, git and Xet contracts need (docs/spec/03-hf-api.md,
// docs/spec/04-git.md and the research contracts in docs/research), and that
// docs/build/interface-coverage.md lists every requirement checked here.
package contracttest

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/hangar-hub/hangar/internal/audit"
	"github.com/hangar-hub/hangar/internal/auth"
	"github.com/hangar-hub/hangar/internal/blob"
	"github.com/hangar-hub/hangar/internal/collections"
	"github.com/hangar-hub/hangar/internal/community"
	"github.com/hangar-hub/hangar/internal/datasets"
	"github.com/hangar-hub/hangar/internal/httpx"
	"github.com/hangar-hub/hangar/internal/index"
	"github.com/hangar-hub/hangar/internal/jobs"
	"github.com/hangar-hub/hangar/internal/org"
	"github.com/hangar-hub/hangar/internal/repo"
	"github.com/hangar-hub/hangar/internal/scan"
	"github.com/hangar-hub/hangar/internal/upstream"
	"github.com/hangar-hub/hangar/internal/webhook"
	"github.com/hangar-hub/hangar/internal/xet"
)

type need struct {
	iface   reflect.Type
	methods []string
}

func iface[T any](methods ...string) need { return need{reflect.TypeFor[T](), methods} }

// requirement ties a contract item to the interface methods that serve it.
type requirement struct {
	id, source string
	needs      []need
}

var requirements = []requirement{
	{"HF-RESOLVE", "hf-hub-api-contract.md §1.2, §1.5, §1.6", []need{
		iface[repo.Service]("Get", "Resolve", "Entry", "OpenFile"),
		iface[repo.LFSStore]("PresignGet", "Open"),
		iface[auth.Authz]("Can"),
		iface[community.Service]("RecordDownload"),
		iface[blob.Store]("PresignGet"),
	}},
	{"HF-REPO-INFO", "§2.1, §2.2", []need{
		iface[repo.Service]("Get", "Resolve", "Tree"),
		iface[index.Indexer]("Get"),
		iface[scan.Scanner]("Results"),
		iface[community.Service]("Likes", "Downloads"),
	}},
	{"HF-TREE", "§3.1", []need{iface[repo.Service]("Tree", "Entry")}},
	{"HF-PATHS-INFO", "§3.2", []need{iface[repo.Service]("Entries")}},
	{"HF-REFS", "§4.1", []need{iface[repo.Service]("Refs")}},
	{"HF-COMMITS", "§4.2", []need{iface[repo.Service]("Log")}},
	{"HF-BRANCH", "§4.3, §4.4", []need{iface[repo.Service]("CreateBranch", "DeleteBranch")}},
	{"HF-TAG", "§4.5, §4.6", []need{iface[repo.Service]("CreateTag", "DeleteTag")}},
	{"HF-LIST", "§5", []need{iface[repo.Service]("List")}},
	{"HF-WHOAMI", "§6.1, §6.2", []need{iface[auth.Authenticator]("Authenticate"), iface[org.Service]("MembershipsOf"), iface[httpx.Authenticator]("Authenticate")}},
	{"HF-DEVICE-LOGIN", "§6.3, 03-hf-api.md scope", []need{iface[auth.DeviceFlow]("Start", "Approve", "Deny", "Poll"), iface[auth.Tokens]("Create")}},
	{"HF-CREATE", "§7.1", []need{iface[repo.Service]("Create"), iface[org.Service]("Get"), iface[auth.Authz]("Can")}},
	{"HF-DELETE", "§7.2", []need{iface[repo.Service]("Delete")}},
	{"HF-SETTINGS", "§7.3", []need{iface[repo.Service]("UpdateSettings")}},
	{"HF-MOVE", "§7.3b, §10", []need{iface[repo.Service]("Rename", "Transfer", "Get")}},
	{"HF-PREUPLOAD", "§7.5", []need{iface[repo.Service]("Resolve", "Entries", "OpenFile")}},
	{"HF-LFS-UPLOAD", "§7.6", []need{iface[repo.LFSStore]("StatMany", "CreateUpload", "GetUpload", "PresignPut", "PresignPart", "PutPart", "CompleteUpload", "AbortUpload", "Verify")}},
	{"HF-COMMIT", "§7.7", []need{iface[repo.Service]("Commit"), iface[repo.LFSStore]("Stat"), iface[community.Service]("Create")}},
	{"HF-DISCUSSIONS", "§7.8, §11.2, REPO-6", []need{iface[community.Service]("List", "Get", "Create", "Comment", "SetStatus", "Rename", "Merge")}},
	{"HF-COLLECTIONS", "BROWSE-5", []need{iface[collections.Service]("List", "Get", "Create", "Update", "Delete", "AddItem", "UpdateItem", "DeleteItem")}},
	{"HF-XET-TOKEN", "xet-upload-research.md §1.3", []need{iface[xet.Ingestor]("IssueWriteToken")}},
	{"HF-XET-CAS", "xet-upload-research.md §2", []need{iface[xet.Ingestor]("VerifyToken", "PutXorb", "AcceptShard")}},
	{"HF-XET-REASSEMBLY", "xet-upload-research.md §5", []need{iface[repo.LFSStore]("Ingest", "Stat"), iface[blob.Store]("Get", "Put")}},
	{"HF-PARQUET", "14-datasets.md Hub API additions", []need{iface[datasets.Viewer]("Parquet", "Croissant")}},
	{"HF-DATASETS-SERVER", "§11.5, 14-datasets.md DATA-4", []need{iface[datasets.Viewer]("IsValid", "Splits", "FirstRows", "Rows", "Search", "Filter", "Info", "Size", "Statistics")}},
	{"HF-SAFETENSORS-GUARD", "03-hf-api.md transformers safetensors auto-conversion", []need{iface[repo.Service]("Tree")}},
	{"GIT-FETCH", "04-git.md Fetch and clone", []need{iface[repo.Service]("OpenView", "Get"), iface[auth.Authz]("Can")}},
	{"GIT-PUSH", "04-git.md Push", []need{iface[repo.Service]("OpenView", "ApplyPack"), iface[repo.LFSStore]("StatMany"), iface[auth.Authz]("Can")}},
	{"GIT-REF-CAS", "04-git.md Ref updates", []need{iface[repo.Service]("UpdateRefs")}},
	{"GIT-LFS-DOWNLOAD", "04-git.md Batch, git-server-research.md §3.2", []need{iface[repo.Service]("LFSRefs"), iface[repo.LFSStore]("PresignGet", "Open", "StatMany")}},
	{"GIT-LFS-VERIFY", "04-git.md Verify", []need{iface[repo.LFSStore]("Verify")}},
	{"GIT-MIRROR", "04-git.md Mirrors", []need{iface[upstream.Client]("FetchPack"), iface[repo.Service]("ApplyPack", "SetLFSPresence", "LFSRefs")}},
	{"OLLAMA", "08-local-tools.md Ollama registry endpoint", []need{iface[index.Indexer]("Quants", "Get"), iface[repo.LFSStore]("Open"), iface[auth.OllamaAuth]("IssueRegistryToken")}},
	{"PULL-PLAN", "06-upstreams-and-pulls.md Pull lifecycle", []need{
		iface[upstream.Client]("RepoInfo", "Tree", "FetchPack"),
		iface[org.Service]("EnsureMirrorNamespace"),
		iface[repo.Service]("Create", "ApplyPack", "SetLFSPresence"),
		iface[jobs.Queue]("Enqueue"),
		iface[datasets.Layout]("Resolve", "Files"),
	}},
	{"PULL-TRANSFER", "06-upstreams-and-pulls.md Transfer", []need{iface[upstream.Client]("Download"), iface[repo.LFSStore]("Ingest", "CreateUpload", "PutPart", "CompleteUpload"), iface[jobs.Limiter]("WaitN")}},
	{"EVENTS", "10-native-api.md Webhook events, 05-auth.md Audit events", []need{iface[webhook.Emitter]("Emit"), iface[audit.Recorder]("Record")}},
}

func TestInterfacesCoverTheContracts(t *testing.T) {
	for _, r := range requirements {
		for _, n := range r.needs {
			if n.iface.Kind() != reflect.Interface {
				t.Errorf("%s: %s is not an interface", r.id, n.iface)
				continue
			}
			for _, m := range n.methods {
				if _, ok := n.iface.MethodByName(m); !ok {
					t.Errorf("%s (%s): %s has no method %s", r.id, r.source, n.iface, m)
				}
			}
		}
	}
}

func TestCoverageDocumentListsEveryRequirement(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	doc, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", "docs", "build", "interface-coverage.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range requirements {
		if !strings.Contains(string(doc), "`"+r.id+"`") {
			t.Errorf("docs/build/interface-coverage.md does not list %s", r.id)
		}
	}
}
