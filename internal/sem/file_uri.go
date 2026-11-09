package sem

import (
	"net/url"
	"strings"
)

func localFileURI(root string) string {
	if root == "" {
		return ""
	}
	// Normalize before building the URI. url.URL takes Path verbatim, so a
	// Windows path went out malformed: "C:\\repo" became "file://C:%5Crepo",
	// with the drive read as an authority and the separators percent-escaped,
	// which no SCIP consumer can resolve back to a directory.
	path := strings.ReplaceAll(root, "\\", "/")
	// Extended-length and device prefixes ("\\\\?\\C:\\repo",
	// "\\\\.\\C:\\repo") normalize to "//?/C:/repo", which the UNC branch below
	// would otherwise read as a host of "?". Strip the prefix first and let the
	// drive-letter branch handle what is left.
	for _, prefix := range []string{"//?/UNC/", "//?/", "//./"} {
		if strings.HasPrefix(path, prefix) {
			rest := strings.TrimPrefix(path, prefix)
			if prefix == "//?/UNC/" {
				path = "//" + rest
			} else {
				path = rest
			}
			break
		}
	}
	if host, share, ok := scipUNCParts(path); ok {
		// A UNC path's server is the URI authority: \\server\share\dir is
		// file://server/share/dir.
		return (&url.URL{Scheme: "file", Host: host, Path: share}).String()
	}
	if scipHasDriveLetter(path) {
		// A drive letter is part of the path, not an authority, so it needs the
		// leading slash that makes the authority empty: file:///C:/repo.
		path = "/" + path
	}
	return (&url.URL{Scheme: "file", Path: path}).String()
}
