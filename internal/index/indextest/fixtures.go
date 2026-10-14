package indextest

import (
	"bytes"
	"context"
	"path"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/index/gguf"
	"github.com/jamesagarside/hangar/internal/index/safetensors"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/repo/repotest"
)

// Owner returns a Principal that owns the Namespace ns, for creating
// fixture Repos in a repotest.Fake.
func Owner(ns string) model.Principal {
	return model.Principal{Kind: model.PrincipalUser, UserID: uuid.NewSHA1(uuid.NameSpaceOID, []byte("user:"+ns)),
		NamespaceID: uuid.NewSHA1(uuid.NameSpaceOID, []byte("ns:"+ns)), Username: ns}
}

// lfsExtensions are stored as LFS objects, as the Hub's default
// .gitattributes does.
var lfsExtensions = map[string]bool{
	".safetensors": true, ".gguf": true, ".bin": true, ".pt": true, ".pth": true, ".ckpt": true, ".pkl": true,
	".pickle": true, ".joblib": true, ".npy": true, ".onnx": true, ".h5": true, ".msgpack": true, ".parquet": true,
	".zip": true, ".tflite": true, ".mlmodel": true,
}

// IsLFSPath reports whether a fixture path is stored as an LFS object.
func IsLFSPath(p string) bool { return lfsExtensions[strings.ToLower(path.Ext(p))] }

// CreateRepo creates a Repo in the fake, owned by the Namespace in id.
func CreateRepo(t testing.TB, f *repotest.Fake, id model.RepoID, vis model.Visibility) *repo.Repo {
	t.Helper()
	r, err := f.Create(context.Background(), repo.CreateParams{ID: id, Visibility: vis, Actor: Owner(id.Namespace)})
	if err != nil {
		t.Fatalf("create %s: %v", id.Path(), err)
	}
	return r
}

// Commit adds files to the default branch of a Repo in the fake. Paths
// with LFS extensions are stored as LFS objects, the rest as git blobs.
func Commit(t testing.TB, f *repotest.Fake, id model.RepoID, files map[string][]byte) model.CommitSHA {
	t.Helper()
	ctx := context.Background()
	var ops []repo.Operation
	for p, content := range files {
		if IsLFSPath(p) {
			obj, err := f.LFS.Put(ctx, content)
			if err != nil {
				t.Fatalf("store %s: %v", p, err)
			}
			ops = append(ops, repo.Operation{Kind: repo.OpAddLFS, Path: p, OID: obj.OID, Size: obj.Size})
			continue
		}
		ops = append(ops, repo.Operation{Kind: repo.OpAdd, Path: p, Content: bytes.NewReader(content)})
	}
	res, err := f.Commit(ctx, repo.CommitParams{Repo: id, Revision: model.DefaultBranch, Summary: "add files", Operations: ops, Actor: Owner(id.Namespace)})
	if err != nil {
		t.Fatalf("commit to %s: %v", id.Path(), err)
	}
	return res.SHA
}

// InsertRepoRow adds the namespaces and repos rows a fake Repo needs so
// tables that reference repos accept its UUID.
func InsertRepoRow(t testing.TB, d *db.DB, r *repo.Repo) {
	t.Helper()
	ctx := context.Background()
	nsID := r.NamespaceID
	if nsID == uuid.Nil {
		nsID = uuid.NewSHA1(uuid.NameSpaceOID, []byte("ns:"+r.ID.Namespace))
	}
	ns := &db.Namespace{ID: nsID, Name: r.ID.Namespace, Kind: "user", DisplayName: r.ID.Namespace}
	if _, err := d.NewInsert().Model(ns).On("CONFLICT DO NOTHING").Exec(ctx); err != nil {
		t.Fatalf("insert namespace: %v", err)
	}
	row := &db.Repo{ID: r.UUID, NamespaceID: nsID, Name: r.ID.Name, Type: string(r.ID.Type), Visibility: string(r.Visibility),
		DefaultBranch: r.DefaultBranch, HeadRef: r.HeadRef, Tags: []string{}, Settings: map[string]any{}, Gating: map[string]any{}}
	if _, err := d.NewInsert().Model(row).Exec(ctx); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
}

// InsertUser adds namespaces and users rows for a Principal from Owner,
// so tables that reference users accept its ID.
func InsertUser(t testing.TB, d *db.DB, p model.Principal) {
	t.Helper()
	ctx := context.Background()
	ns := &db.Namespace{ID: p.NamespaceID, Name: p.Username, Kind: "user", DisplayName: p.Username}
	if _, err := d.NewInsert().Model(ns).On("CONFLICT DO NOTHING").Exec(ctx); err != nil {
		t.Fatalf("insert namespace: %v", err)
	}
	email := p.Username + "@example.com"
	u := &db.User{ID: p.UserID, NamespaceID: p.NamespaceID, Email: &email}
	if _, err := d.NewInsert().Model(u).On("CONFLICT DO NOTHING").Exec(ctx); err != nil {
		t.Fatalf("insert user: %v", err)
	}
}

// Safetensors builds a safetensors file; it fails the test on error.
func Safetensors(t testing.TB, metadata map[string]string, tensors ...safetensors.Tensor) []byte {
	t.Helper()
	b, err := safetensors.Encode(metadata, tensors)
	if err != nil {
		t.Fatalf("encode safetensors: %v", err)
	}
	return b
}

// GGUF builds a GGUF file; it fails the test on error.
func GGUF(t testing.TB, kvs []gguf.KV, tensors ...gguf.EncodeTensor) []byte {
	t.Helper()
	b, err := gguf.Encode(kvs, tensors)
	if err != nil {
		t.Fatalf("encode gguf: %v", err)
	}
	return b
}

// LlamaGGUF builds a small llama GGUF file with a vocabulary, a chat
// template and one tensor of the given ggml type and general.file_type.
func LlamaGGUF(t testing.TB, fileType int, tensorType string, extra ...gguf.KV) []byte {
	t.Helper()
	kvs := []gguf.KV{
		{Key: "general.architecture", Value: "llama"},
		{Key: "general.name", Value: "Tiny Llama"},
		{Key: "general.file_type", Value: uint32(fileType)},
		{Key: "llama.context_length", Value: uint32(2048)},
		{Key: "llama.block_count", Value: uint32(1)},
		{Key: "tokenizer.ggml.model", Value: "llama"},
		{Key: "tokenizer.ggml.tokens", Value: []string{"<unk>", "<s>", "</s>", "hello", "world"}},
		{Key: "tokenizer.ggml.scores", Value: []float32{0, 0, 0, -1, -2}},
		{Key: "tokenizer.ggml.bos_token_id", Value: uint32(1)},
		{Key: "tokenizer.ggml.eos_token_id", Value: uint32(2)},
		{Key: "tokenizer.chat_template", Value: Llama3Template},
	}
	kvs = append(kvs, extra...)
	return GGUF(t, kvs, gguf.EncodeTensor{Name: "token_embd.weight", Type: tensorType, Dims: []uint64{64, 8}})
}

// Llama3Template is the chat template of Llama 3 Instruct models, which
// Ollama's template index matches to llama3-instruct.
const Llama3Template = "{% set loop_messages = messages %}{% for message in loop_messages %}{% set content = '<|start_header_id|>' + message['role'] + '<|end_header_id|>\n\n'+ message['content'] | trim + '<|eot_id|>' %}{% if loop.index0 == 0 %}{% set content = bos_token + content %}{% endif %}{{ content }}{% endfor %}{% if add_generation_prompt %}{{ '<|start_header_id|>assistant<|end_header_id|>\n\n' }}{% endif %}"
