package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/index"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/scan"
)

// seedListing inserts n Repos across 100 Organisations and the viewer's own
// Namespace. The viewer belongs to 5 of the Organisations. 70% of the
// Repos are public, 15% internal and 15% private; 80% are models
// (transformers with safetensors, or GGUF quantisations) and 20% datasets.
// Every Repo has a branch head, a Commit row, index results and a scan
// result; about a third have likes and downloads. It can run more than
// once on one database.
func seedListing(t *testing.T, d *db.DB, viewer model.Principal, n int) {
	t.Helper()
	ctx := context.Background()
	rng := rand.New(rand.NewPCG(uint64(n), 7))
	now := time.Now().UTC()
	run := uuid.NewString()[:8]

	var orgs []db.Namespace
	for i := range 100 {
		orgs = append(orgs, db.Namespace{ID: db.NewID(), Name: fmt.Sprintf("org%03d-%s", i, run), Kind: "org", DisplayName: fmt.Sprintf("Org %d", i)})
	}
	insertAll(t, d, orgs)
	var members []db.Membership
	for i := range 5 {
		members = append(members, db.Membership{ID: db.NewID(), OrgNamespaceID: orgs[i*20].ID, UserID: viewer.UserID, Role: "read", Source: "manual"})
	}
	insertAll(t, d, members)
	var likerNS []db.Namespace
	var likers []db.User
	for i := range 20 {
		ns := db.Namespace{ID: db.NewID(), Name: fmt.Sprintf("liker%02d-%s", i, run), Kind: "user"}
		likerNS = append(likerNS, ns)
		likers = append(likers, db.User{ID: db.NewID(), NamespaceID: ns.ID})
	}
	insertAll(t, d, likerNS)
	insertAll(t, d, likers)

	nsIDs := []uuid.UUID{viewer.NamespaceID}
	for _, o := range orgs {
		nsIDs = append(nsIDs, o.ID)
	}
	licences := []string{"apache-2.0", "mit", "llama3.1", "cc-by-4.0", "other"}
	libraries := []string{"transformers", "transformers", "transformers", "diffusers", "sentence-transformers", "timm", "peft"}
	tasks := []string{"text-generation", "text-classification", "feature-extraction", "image-classification", "fill-mask"}
	quants := []string{"Q2_K", "Q3_K_M", "Q4_K_M", "Q5_K_M", "Q6_K", "Q8_0", "F16"}
	const batch = 500
	for start := 0; start < n; start += batch {
		var (
			repos     []db.Repo
			refs      []db.Ref
			commits   []db.Commit
			metadata  []db.Metadata
			scans     []db.ScanResult
			likes     []db.Like
			downloads []db.DownloadCount
		)
		for i := start; i < min(start+batch, n); i++ {
			id := db.NewID()
			created := now.Add(-time.Duration(rng.IntN(365*24)) * time.Hour)
			modified := created.Add(time.Duration(rng.IntN(1000)) * time.Minute)
			vis := "public"
			switch v := rng.IntN(100); {
			case v >= 85:
				vis = "private"
			case v >= 70:
				vis = "internal"
			}
			typ := "model"
			if rng.IntN(5) == 0 {
				typ = "dataset"
			}
			settings := map[string]any{}
			if rng.IntN(3) == 0 {
				settings["licence"] = licences[rng.IntN(len(licences))]
			}
			repos = append(repos, db.Repo{ID: id, NamespaceID: nsIDs[rng.IntN(len(nsIDs))], Name: fmt.Sprintf("repo-%06d-%s", i, run), Type: typ,
				Visibility: vis, DefaultBranch: "main", HeadRef: "refs/heads/main", Description: "A seeded Repo for the listing load test.",
				Tags: []string{"seeded"}, Settings: settings, Gating: map[string]any{}, LastModifiedAt: &modified,
				SizeBytes: rng.Int64N(50 << 30), LFSBytes: rng.Int64N(50 << 30), Timestamps: db.Timestamps{CreatedAt: created, UpdatedAt: modified}})
			sha := fmt.Sprintf("%040x", rng.Uint64())
			refs = append(refs, db.Ref{RepoID: id, Name: "refs/heads/main", Target: sha})
			sig := db.GitSignature{Name: "Seeder", Email: "seed@example.com", When: modified}
			commits = append(commits, db.Commit{RepoID: id, SHA: sha, Tree: sha, Parents: []string{}, Author: sig, Committer: sig,
				Title: "Seed", Message: "Seed", AuthoredAt: modified, CommittedAt: modified})
			sum := seededSummary(rng, typ, licences, libraries, tasks, quants)
			raw, err := json.Marshal(sum)
			require.NoError(t, err)
			metadata = append(metadata, db.Metadata{RepoID: id, Commit: sha, IndexerVersion: index.Version, Summary: raw, IndexedAt: modified})
			scans = append(scans, db.ScanResult{RepoID: id, Commit: sha, Path: "", ScannerVersion: scan.Version, Status: "safe", Findings: db.JSON("[]")})
			if rng.IntN(3) == 0 {
				for _, l := range rng.Perm(len(likers))[:1+rng.IntN(len(likers)-1)] {
					likes = append(likes, db.Like{UserID: likers[l].ID, RepoID: id})
				}
				for _, day := range rng.Perm(60)[:10] {
					downloads = append(downloads, db.DownloadCount{RepoID: id, Day: time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -day),
						Count: 1 + rng.Int64N(500)})
				}
			}
		}
		require.NoError(t, d.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
			for _, rows := range []any{&repos, &refs, &commits, &metadata, &scans, &likes, &downloads} {
				if v, ok := rows.(*[]db.Like); ok && len(*v) == 0 {
					continue
				}
				if v, ok := rows.(*[]db.DownloadCount); ok && len(*v) == 0 {
					continue
				}
				if _, err := tx.NewInsert().Model(rows).Exec(ctx); err != nil {
					return err
				}
			}
			return nil
		}))
	}
}

func insertAll[T any](t *testing.T, d *db.DB, rows []T) {
	t.Helper()
	_, err := d.NewInsert().Model(&rows).Exec(context.Background())
	require.NoError(t, err)
}

// seededSummary returns index results shaped like real ones: a
// transformers model with safetensors, a GGUF model with quantisations and
// a chat template, or a dataset, each with files, card data and tags.
func seededSummary(rng *rand.Rand, typ string, licences, libraries, tasks, quants []string) *index.Summary {
	lic := licences[rng.IntN(len(licences))]
	card := map[string]any{"license": lic, "language": []string{"en"}, "tags": []string{"seeded", "load-test"}}
	if typ == "dataset" {
		rows := rng.Int64N(10_000_000)
		files := []index.FileInfo{{Path: "README.md"}, {Path: "data/train-00000-of-00002.parquet", Format: "parquet"},
			{Path: "data/train-00001-of-00002.parquet", Format: "parquet"}, {Path: "data/test-00000-of-00001.parquet", Format: "parquet"}}
		return &index.Summary{Formats: []string{"parquet"}, Licence: index.Licence{ID: lic}, CardData: card, Tags: []string{"format:parquet", "modality:text"},
			Size: index.Size{Total: rows * 100, Present: rows * 100}, Files: files,
			Dataset: &index.DatasetSummary{Configs: []string{"default"}, Splits: map[string]int{"default": 2},
				Modalities: []string{"text"}, Formats: []string{"parquet"}, Tasks: []string{tasks[rng.IntN(len(tasks))]},
				Languages: []string{[]string{"en", "fr", "de", "ja"}[rng.IntN(4)]}, SizeCategory: []string{"n<1K", "1K<n<10K", "10K<n<100K", "100K<n<1M", "1M<n<10M"}[rng.IntN(5)],
				Rows: rows}}
	}
	params := rng.Int64N(70e9)
	if rng.IntN(3) == 0 {
		var qs []index.Quantisation
		var files []index.FileInfo
		for _, q := range rng.Perm(len(quants))[:2+rng.IntN(len(quants)-2)] {
			f := "model-" + quants[q] + ".gguf"
			qs = append(qs, index.Quantisation{Name: quants[q], Files: []string{f}})
			files = append(files, index.FileInfo{Path: f, Format: "gguf", Quant: quants[q], TensorCount: 291, Parameters: params})
		}
		template := strings.Repeat("{% for message in messages %}{{ message['role'] }}: {{ message['content'] }}\n{% endfor %}", 8)
		return &index.Summary{Formats: []string{"gguf"}, Architecture: "llama", Parameters: &index.Parameters{Total: params},
			Quantisations: qs, ContextLength: 131072, ChatTemplate: template, Licence: index.Licence{ID: lic}, CardData: card,
			PipelineTag: "text-generation", Tags: []string{"gguf", "text-generation"}, Files: append(files, index.FileInfo{Path: "README.md"}),
			GGUF: &index.GGUF{Total: params, Architecture: "llama", ContextLength: 131072, ChatTemplate: template}}
	}
	files := []index.FileInfo{{Path: "README.md"}, {Path: "config.json"}, {Path: "generation_config.json"}, {Path: "tokenizer.json"},
		{Path: "tokenizer_config.json"}, {Path: "special_tokens_map.json"}}
	for s := range 4 {
		files = append(files, index.FileInfo{Path: fmt.Sprintf("model-%05d-of-00004.safetensors", s+1), Format: "safetensors", DType: "BF16",
			TensorCount: 100, Parameters: params / 4})
	}
	cfg, _ := json.Marshal(map[string]any{"architectures": []string{"LlamaForCausalLM"}, "model_type": "llama", "hidden_size": 4096,
		"num_hidden_layers": 32, "num_attention_heads": 32, "vocab_size": 128256, "max_position_embeddings": 8192, "torch_dtype": "bfloat16"})
	return &index.Summary{Formats: []string{"safetensors"}, Architecture: "LlamaForCausalLM", ModelType: "llama",
		Parameters: &index.Parameters{Total: params, ByDType: map[string]int64{"BF16": params}}, ContextLength: 8192,
		Licence: index.Licence{ID: lic}, CardData: card, PipelineTag: tasks[rng.IntN(len(tasks))], LibraryName: libraries[rng.IntN(len(libraries))],
		Tags: []string{"safetensors", "llama"}, Size: index.Size{Total: params * 2, Present: params * 2}, Files: files, Config: cfg}
}
