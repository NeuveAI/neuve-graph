//go:build !windows

package sem

import (
	"fmt"
	"net/url"
	"path/filepath"
)

func lspNormalizeURIPath(uri, _ *url.URL) (string, error) {
	if uri.Host != "" || !filepath.IsAbs(uri.Path) {
		return "", fmt.Errorf("not a local absolute file URI")
	}
	return filepath.FromSlash(uri.Path), nil
}
