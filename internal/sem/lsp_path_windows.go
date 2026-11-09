//go:build windows

package sem

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"path/filepath"
)

func lspNormalizeURIPath(uri *url.URL) (string, error) {
	path := uri.Path
	if uri.Host != "" {
		path = "//" + uri.Host + path
	} else if len(path) > 0 && path[0] == '/' && scipHasDriveLetter(path[1:]) {
		path = path[1:]
	}
	path = filepath.FromSlash(path)
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("not an absolute file URI")
	}
	// EvalSymlinks expands 8.3 names and normalizes drive and component case.
	// Exact-source buffers need not exist on disk, so normalize the nearest
	// existing ancestor and retain the missing suffix in its original case.
	var suffix []string
	for {
		normal, err := filepath.EvalSymlinks(path)
		if err == nil {
			for index := len(suffix) - 1; index >= 0; index-- {
				normal = filepath.Join(normal, suffix[index])
			}
			return normal, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", err
		}
		suffix = append(suffix, filepath.Base(path))
		path = parent
	}
}
