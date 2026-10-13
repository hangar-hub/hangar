package telemetry

import (
	"strings"

	"go.opentelemetry.io/otel/attribute"
)

// Span and resource attribute keys that Hangar adds to the semantic conventions.
const (
	AttrRole        = attribute.Key("hangar.role")
	AttrRepoID      = attribute.Key("hangar.repo.id")
	AttrRepoType    = attribute.Key("hangar.repo.type")
	AttrProtocol    = attribute.Key("hangar.protocol")
	AttrJobKind     = attribute.Key("hangar.job.kind")
	AttrJobID       = attribute.Key("hangar.job.id")
	AttrJobAttempt  = attribute.Key("hangar.job.attempt")
	AttrBlobBackend = attribute.Key("hangar.blob.backend")
	AttrBlobPrefix  = attribute.Key("hangar.blob.key_prefix")
	AttrBlobBytes   = attribute.Key("hangar.blob.bytes")
	AttrEndUserID   = attribute.Key("enduser.id")
)

// Protocol is the value of the hangar.protocol attribute.
type Protocol string

// Protocols Hangar serves.
const (
	ProtocolHub     Protocol = "hub"
	ProtocolGit     Protocol = "git"
	ProtocolLFS     Protocol = "lfs"
	ProtocolOllama  Protocol = "ollama"
	ProtocolNative  Protocol = "native"
	ProtocolUI      Protocol = "ui"
	ProtocolXet     Protocol = "xet"
	ProtocolViewer  Protocol = "viewer"
	ProtocolHealth  Protocol = "health"
	ProtocolUnknown Protocol = "unknown"
)

// BlobKeyPrefix returns the part of a Blob store key that is safe to record
// on spans: the key's top-level area and, for LFS and Xet, the fixed
// sub-area, never the object name. For example `lfs/sha256/ab/cd/<oid>`
// gives `lfs/sha256` and `git/<repo-uuid>/pack/x.pack` gives `git`.
func BlobKeyPrefix(key string) string {
	parts := strings.SplitN(key, "/", 3)
	switch parts[0] {
	case "lfs", "xet":
		if len(parts) > 1 {
			return parts[0] + "/" + parts[1]
		}
	}
	return parts[0]
}
