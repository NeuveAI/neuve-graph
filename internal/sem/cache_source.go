package sem

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/entireio/entire-graph/internal/gitutil"
)

// cacheSource separates repository ownership and scan coordinates from the
// disposable directory serving a committed view.
type cacheSource struct {
	CommonDir string `json:"common_dir"`
	Scope     string `json:"scope"`
}

func resolveCacheSource(ctx context.Context, repo string) (cacheSource, error) {
	if !gitMetadataSafeForSubprocessContext(ctx, repo) {
		return cacheSource{}, gitMetadataRefusalError(repo)
	}
	root, err := gitutil.RepoCommandRoot(ctx, repo)
	if err != nil {
		return cacheSource{}, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return cacheSource{}, err
	}
	serving, err := filepath.EvalSymlinks(repo)
	if err != nil {
		return cacheSource{}, err
	}
	scope, err := filepath.Rel(root, serving)
	if err != nil || scope == ".." || strings.HasPrefix(scope, ".."+string(filepath.Separator)) || filepath.IsAbs(scope) {
		return cacheSource{}, fmt.Errorf("serving scope is outside the Git top level")
	}
	prefix, err := gitutil.RepoPrefix(ctx, serving)
	if err != nil {
		return cacheSource{}, err
	}
	want := filepath.ToSlash(scope)
	if scope == "." {
		want = ""
	}
	if strings.TrimSuffix(prefix, "/") != want {
		return cacheSource{}, fmt.Errorf("serving scope disagrees with Git prefix")
	}
	common, err := gitutil.CommonDir(ctx, serving)
	if err != nil {
		return cacheSource{}, err
	}
	return cacheSource{CommonDir: common, Scope: want}, nil
}

func captureSearchCacheSource(ctx context.Context, repo string, options ProviderSnapshotOptions) (ProviderSnapshotOptions, error) {
	source, err := resolveCacheSource(ctx, repo)
	if err != nil {
		return options, err
	}
	if _, err := ExecutableBuildIdentity(); err != nil {
		return options, err
	}
	options.cacheSource = &source
	return options, nil
}

func validCachedSearchSource(cache cachedSearchSnapshot, options ProviderSnapshotOptions) bool {
	build, err := ExecutableBuildIdentity()
	return err == nil && options.cacheSource != nil &&
		cache.Source == *options.cacheSource && cache.BuildIdentity == build
}
