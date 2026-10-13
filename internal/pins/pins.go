//go:build pins

// Package pins keeps the dependencies chosen in docs/spec/02-architecture.md
// and 14-datasets.md in go.mod before the workstreams that use them import
// them, so `go mod tidy` keeps their pinned versions. It is never built
// (the pins build tag is not set) and can be deleted once every module
// below is imported by real code (build plan phase 2).
package pins

import (
	_ "github.com/coreos/go-oidc/v3/oidc"
	_ "github.com/go-git/go-git/v6"
	_ "github.com/go-git/go-git/v6/plumbing/transport"
	_ "github.com/gpustack/gguf-parser-go"
	_ "github.com/minio/minio-go/v7"
	_ "github.com/parquet-go/parquet-go"
	_ "github.com/testcontainers/testcontainers-go/modules/minio"
	_ "golang.org/x/crypto/argon2"
	_ "golang.org/x/oauth2"
	_ "golang.org/x/sync/singleflight"
)
