package main

import (
	"github.com/jamesagarside/hangar/internal/api"
	"github.com/jamesagarside/hangar/internal/api/gen"
	"github.com/jamesagarside/hangar/internal/config"
)

// nativeServer serves every operation of the native API
// (api/openapi.yaml). Each workstream implements its operations on its own
// handler type; together they implement gen.ServerInterface with no
// fallback, so an operation without a handler fails to compile.
type nativeServer struct {
	// W1: /jobs, /admin/storage, /admin/quotas, /admin/gc, /admin/system.
	*api.Jobs
	*api.AdminStorage
	*api.AdminSystem
	// W2: /repos, uploads, /search, /namespaces.
	*api.ReposAPI
	*api.UploadsAPI
	*api.SearchAPI
	*api.NamespacesAPI
	// W5: /me, /auth, /orgs, collaborators, access requests, /admin users,
	// invites, settings, Tokens and audit.
	*api.Identity
	// W6: /upstreams, /discover, /pulls, Mirrors, Dependencies, /admin/upstreams.
	*api.Upstreams
	// W7: metadata, file headers, quants, likes, stats, discussions,
	// Collections and webhooks.
	*api.MetadataAPI
	*api.CommunityAPI
	*api.CollectionsAPI
	*api.WebhooksAPI
	// W10: dataset Configs.
	*api.DatasetsAPI
}

var _ gen.ServerInterface = (*nativeServer)(nil)

// newNativeServer builds every native API handler over the instance.
func newNativeServer(in *instance) (*nativeServer, error) {
	cfg := in.cfg
	authz := in.auth.Authz()
	s := &nativeServer{
		Jobs:         &api.Jobs{Queue: in.queue, Authz: authz, Repos: in.repos},
		AdminStorage: &api.AdminStorage{Authz: authz, Accounting: in.accounting, Queue: in.queue, Audit: in.audit},
		AdminSystem: &api.AdminSystem{Authz: authz, DB: in.db, Info: api.SystemInfo{Version: version, Commit: commit, Role: cfg.Server.Role,
			BlobBackend: blobBackend(cfg), DownloadMode: downloadMode(cfg), Telemetry: in.tel.Exporters},
			Checks: api.DoctorChecks(in.db, api.DoctorOptions{Store: in.blobs, Downloads: in.downloads, Config: cfg})},
		Identity:  api.NewIdentity(api.IdentityOptions{Auth: in.auth, Orgs: in.orgs, Audit: in.audit, Repos: in.repos, Log: in.log}),
		Upstreams: &api.Upstreams{Service: in.pulls, Client: in.client, Repos: in.repos, Authz: authz, Queue: in.queue, Log: in.log},
	}
	rd := api.RepoDeps{Repos: in.repos, LFS: in.lfs, Authz: authz, Orgs: in.orgs, Jobs: in.queue, Collections: in.collections,
		NamespaceSearch: in.orgs, Community: in.community, Index: in.indexer, Scanner: in.scanner, PublicURL: cfg.Server.PublicURL,
		UploadURLTTL: cfg.LFS.UploadURLTTL, InlineMax: cfg.HFAPI.RegularUploadMax.Int64(), Facets: true,
		MirrorStatus: in.mirrorStatus, Log: in.log}
	var err error
	if s.ReposAPI, err = api.NewReposAPI(rd); err != nil {
		return nil, err
	}
	if s.UploadsAPI, err = api.NewUploadsAPI(rd); err != nil {
		return nil, err
	}
	if s.SearchAPI, err = api.NewSearchAPI(rd); err != nil {
		return nil, err
	}
	if s.NamespacesAPI, err = api.NewNamespacesAPI(rd); err != nil {
		return nil, err
	}
	md := api.MetaDeps{Repos: in.repos, Authz: authz, Orgs: in.orgs, Index: in.indexer, Scanner: in.scanner, Community: in.community,
		Collections: in.collections, Webhooks: in.webhooks, Audit: in.audit, Log: in.log}
	if s.MetadataAPI, err = api.NewMetadataAPI(md); err != nil {
		return nil, err
	}
	if s.CommunityAPI, err = api.NewCommunityAPI(md); err != nil {
		return nil, err
	}
	if s.CollectionsAPI, err = api.NewCollectionsAPI(md); err != nil {
		return nil, err
	}
	if s.WebhooksAPI, err = api.NewWebhooksAPI(md); err != nil {
		return nil, err
	}
	if s.DatasetsAPI, err = api.NewDatasetsAPI(api.DatasetsDeps{Repos: in.repos, Authz: authz, Layout: in.layout, Store: in.datasets,
		Jobs: in.queue, Log: in.log}); err != nil {
		return nil, err
	}
	return s, nil
}

func blobBackend(cfg *config.Config) string {
	if cfg.Storage.Backend == "" {
		return config.BackendFS
	}
	return cfg.Storage.Backend
}

// downloadMode is storage.s3.download_mode on S3 and empty on the
// filesystem, which always streams.
func downloadMode(cfg *config.Config) string {
	if cfg.Storage.Backend != config.BackendS3 {
		return ""
	}
	return cfg.Storage.S3.DownloadMode
}
