package main

import (
	"go/ast"
	"go/types"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"

	"github.com/hangar-hub/hangar/internal/api"
	"github.com/hangar-hub/hangar/internal/httpx"
	"github.com/hangar-hub/hangar/internal/upstream"
)

// byteUse says how a protocol, route or function that reaches a Repo
// file's bytes is covered by scan.block_unsafe_downloads: the download
// routes in unsafeDownloadRoutes that check it, or why it serves no Repo
// file bytes to a client.
type byteUse struct {
	routes []string
	reason string
}

// protocolUses classifies every field of httpx.Protocols, the protocols
// the router dispatches to.
var protocolUses = map[string]byteUse{
	"Hub":    {routes: []string{routeResolve}},
	"LFS":    {routes: []string{routeLFSBatch, routeLFSObject}},
	"Ollama": {routes: []string{routeOllamaBlob}},
	"Native": {routes: []string{routeZipStart}},
	"Git":    {routes: []string{routeGitUploadPack}},
	"Xet":    {reason: "the CAS takes uploads and answers chunk dedupe lookups; Hangar never offers Xet downloads (no xet-read-token)"},
	"Viewer": {reason: "serves Dataset Viewer responses and Parquet exports, which Hangar derives; never a Repo file"},
	"Auth":   {reason: "sign-in, sign-out and OIDC callbacks"},
	"UI":     {reason: "the web UI's own assets"},
	"Health": {reason: "health checks"},
}

// directRouteUses classifies every signed-link route the router serves
// before authentication (directRoutes).
var directRouteUses = map[string]byteUse{
	api.ExportPath:           {routes: []string{routeZipDownload}},
	upstream.PullThroughPath: {routes: []string{routePullThrough}},
}

// fileByteReaders classifies every function in the binary that reads a
// Repo file's bytes: it calls OpenFile, Open or PresignGet of internal/repo,
// or Get or PresignGet of a Blob store. Keys are "package.function"
// relative to the module. internal/repo and internal/blob, which provide
// the reads, are not listed.
var fileByteReaders = map[string]byteUse{
	// Downloads.
	"internal/hfapi.(*Handler).resolve":                {routes: []string{routeResolve}},
	"internal/gitserver.(*Server).downloadAction":      {routes: []string{routeLFSBatch}},
	"internal/gitserver.(*Server).download":            {routes: []string{routeLFSObject}},
	"internal/ollama.(*Handler).blob":                  {routes: []string{routeOllamaBlob}},
	"internal/ollama.(*Handler).fileLayer":             {routes: []string{routeOllamaBlob}},
	"internal/ollama.(*Handler).redirect":              {routes: []string{routeOllamaBlob}},
	"internal/api.ExportURL":                           {routes: []string{routeZipStart}},
	"internal/api.ExportHandler":                       {routes: []string{routeZipDownload}},
	"internal/upstream.(*PullThrough).stream":          {routes: []string{routePullThrough, routeResolve}},
	"internal/upstream.(*PullThrough).afterTransfer":   {routes: []string{routePullThrough, routeResolve}},
	"internal/gitstore.(*Store).download":              {routes: []string{routeGitUploadPack}},
	"internal/datasets/viewer.(*Service).cached":       {reason: "serves derived Dataset Viewer data"},
	"internal/datasets/pipeline.(*Store).Info":         {reason: "reads the metadata of a derived Parquet export"},
	"internal/hfapi.(*Handler).readRepoFile":           {reason: "reads .gitattributes and .gitignore to answer preupload"},
	"internal/api.(*UploadsAPI).lfsRules":              {reason: "reads .gitattributes to plan an upload"},
	"internal/ollama.(*Handler).build":                 {reason: "reads small files to build a manifest"},
	"internal/ollama.(*Handler).readSmall":             {reason: "reads small files to build a manifest"},
	"internal/community/communitydb.(*DB).Merge":       {reason: "copies a pull request's files into a Commit"},
	"internal/datasets/layout.(*RepoFile).block":       {reason: "reads Dataset files to find Configs and Splits"},
	"internal/datasets/layout.readSmall":               {reason: "reads Dataset files to find Configs and Splits"},
	"internal/datasets/pipeline.(*Pipeline).readSmall": {reason: "converts Dataset files to Parquet"},
	"internal/datasets/pipeline.(*readWatch).OpenFile": {reason: "converts Dataset files to Parquet"},
	"internal/datasets/pipeline.sourceFile":            {reason: "converts Dataset files to Parquet"},
	"internal/index/indexdb.repoSource.ReadAll":        {reason: "indexes metadata"},
	"internal/index/indexdb.repoSource.ReaderAt":       {reason: "indexes metadata"},
	"internal/scan.(*Service).open":                    {reason: "the security scan reads files to scan them"},
	"internal/scan.(*Service).scanEntry":               {reason: "the security scan reads files to scan them"},
	"internal/upstream.(*Service).resolveAndStore":     {reason: "reads config files to resolve Dependencies"},
	"internal/xet.(*Service).loadIndex":                {reason: "reads xorb indexes to check uploads"},
	"internal/xet.(*fileReader).openTerm":              {reason: "rebuilds an uploaded file to verify it"},
	"internal/api.checkBlobStore":                      {reason: "the admin storage check reads the Blob store sentinel"},
	"internal/cli.(*app).doctor":                       {reason: "hangar doctor checks the Blob store from the command line"},
	"internal/cli.copyBlobsOut":                        {reason: "hangar admin backup copies the Blob store from the command line"},
	"internal/cli.largeReadCheck":                      {reason: "hangar admin doctor --large-read reads back an object of random bytes it wrote itself"},
}

// checkUse fails when u neither names checked download routes nor gives
// a reason.
func checkUse(t *testing.T, what string, u byteUse) {
	t.Helper()
	if len(u.routes) == 0 {
		require.NotEmpty(t, u.reason, "%s: classify it with the download routes that check it, or say why it serves no Repo file bytes", what)
		return
	}
	for _, r := range u.routes {
		require.Contains(t, unsafeDownloadRoutes, r, "%s: %s has no check in TestUnsafeDownloadGateCoversEveryRoute", what, r)
	}
}

// TestServingSurfaceIsClassified enumerates what the router serves: every
// protocol it dispatches to and every signed-link route before
// authentication. A new one fails until it is classified, and each
// download route it names is checked by TestUnsafeDownloadGateCoversEveryRoute.
func TestServingSurfaceIsClassified(t *testing.T) {
	covered := map[string]bool{}
	fields := reflect.TypeFor[httpx.Protocols]()
	var names []string
	for i := range fields.NumField() {
		f := fields.Field(i)
		names = append(names, f.Name)
		u, ok := protocolUses[f.Name]
		require.True(t, ok, "httpx.Protocols.%s is not classified in protocolUses", f.Name)
		checkUse(t, "protocol "+f.Name, u)
		for _, r := range u.routes {
			covered[r] = true
		}
	}
	for name := range protocolUses {
		require.Contains(t, names, name, "protocolUses lists %s, which httpx.Protocols no longer has", name)
	}

	in := newTestInstance(t, nil)
	var prefixes []string
	for _, d := range directRoutes(in) {
		prefixes = append(prefixes, d.Prefix)
		u, ok := directRouteUses[d.Prefix]
		require.True(t, ok, "the signed-link route %s is not classified in directRouteUses", d.Prefix)
		checkUse(t, "route "+d.Prefix, u)
		for _, r := range u.routes {
			covered[r] = true
		}
	}
	for p := range directRouteUses {
		require.Contains(t, prefixes, p, "directRouteUses lists %s, which the router no longer serves", p)
	}
	for _, r := range unsafeDownloadRoutes {
		require.True(t, covered[r], "%s is checked but no protocol or signed-link route serves it", r)
	}
}

// TestFileByteReadersAreClassified enumerates every function in the
// binary's packages that reads a Repo file's bytes, so a new download path
// cannot skip scan.block_unsafe_downloads unnoticed: it fails until the
// function is classified in fileByteReaders.
func TestFileByteReadersAreClassified(t *testing.T) {
	const module = "github.com/hangar-hub/hangar/"
	pkgs, err := packages.Load(&packages.Config{
		Mode: packages.NeedName | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo | packages.NeedImports,
		Dir:  "../..",
	}, "./cmd/...", "./internal/...")
	require.NoError(t, err)
	var loadErrs []string
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		for _, e := range p.Errors {
			loadErrs = append(loadErrs, e.Error())
		}
	})
	require.Empty(t, loadErrs)

	// The reads: methods of these packages with these names.
	sources := map[string][]string{
		module + "internal/repo": {"OpenFile", "Open", "PresignGet"},
		module + "internal/blob": {"Get", "PresignGet"},
	}
	isSource := func(obj types.Object) bool {
		fn, ok := obj.(*types.Func)
		if !ok || fn.Pkg() == nil || fn.Signature().Recv() == nil {
			return false
		}
		return slices.Contains(sources[fn.Pkg().Path()], fn.Name())
	}
	skip := func(path string) bool {
		rel := strings.TrimPrefix(path, module)
		last := rel[strings.LastIndex(rel, "/")+1:]
		// Test fakes are never in the binary.
		return rel == "internal/repo" || rel == "internal/blob" || strings.HasPrefix(rel, "internal/blob/") ||
			strings.HasSuffix(last, "test")
	}

	found := map[string]bool{}
	for _, p := range pkgs {
		if skip(p.PkgPath) {
			continue
		}
		rel := strings.TrimPrefix(p.PkgPath, module)
		for _, f := range p.Syntax {
			for _, decl := range f.Decls {
				name := ""
				switch d := decl.(type) {
				case *ast.FuncDecl:
					name = d.Name.Name
					if d.Recv != nil && len(d.Recv.List) > 0 {
						name = receiverName(d.Recv.List[0].Type) + "." + name
					}
				case *ast.GenDecl:
					name = "var block at " + p.Fset.Position(d.Pos()).String()
				}
				ast.Inspect(decl, func(n ast.Node) bool {
					if sel, ok := n.(*ast.SelectorExpr); ok && isSource(p.TypesInfo.Uses[sel.Sel]) {
						found[rel+"."+name] = true
					}
					return true
				})
			}
		}
	}

	var unclassified, stale []string
	for fn := range found {
		u, ok := fileByteReaders[fn]
		if !ok {
			unclassified = append(unclassified, fn)
			continue
		}
		checkUse(t, fn, u)
	}
	for fn := range fileByteReaders {
		if !found[fn] {
			stale = append(stale, fn)
		}
	}
	sort.Strings(unclassified)
	sort.Strings(stale)
	require.Empty(t, unclassified, "these functions read Repo file bytes: add each to fileByteReaders with the download "+
		"routes that check scan.block_unsafe_downloads for it (and a check in TestUnsafeDownloadGateCoversEveryRoute), or the reason it serves no Repo file to a client")
	require.Empty(t, stale, "fileByteReaders lists functions that no longer read Repo file bytes; remove them")
}

// receiverName returns a method receiver as written, without type
// parameters: "(*Handler)" or "repoSource".
func receiverName(e ast.Expr) string {
	ptr := false
	if s, ok := e.(*ast.StarExpr); ok {
		ptr, e = true, s.X
	}
	switch x := e.(type) {
	case *ast.IndexExpr:
		e = x.X
	case *ast.IndexListExpr:
		e = x.X
	}
	if ptr {
		return "(*" + types.ExprString(e) + ")"
	}
	return types.ExprString(e)
}
