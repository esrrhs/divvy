package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// DownloadFile fetches a public URL and writes it verbatim to a workspace
// path, filling the gap left by web_fetch (which returns text only). All
// network safety comes from WebClient.Download (scheme/SSRF/port/redirect
// checks and the size cap); this wrapper confines the destination to the
// workspace and removes a partial file when the transfer fails.
func (s *Sandbox) DownloadFile(ctx context.Context, rawURL, relPath string) (string, error) {
	if s.Web == nil {
		return "", fmt.Errorf("download_file needs -web (network access is off by default)")
	}
	relPath = filepath.ToSlash(relPath)
	abs, err := s.Resolve(relPath)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0755); err != nil {
		return "", err
	}
	f, err := os.Create(abs)
	if err != nil {
		return "", err
	}
	contentType, n, err := s.Web.Download(ctx, rawURL, f)
	closeErr := f.Close()
	if err == nil && closeErr != nil {
		err = closeErr
	}
	if err != nil {
		// Never leave a truncated/partial download behind: the next attempt
		// would otherwise mistake it for a valid artifact.
		_ = os.Remove(abs)
		return "", err
	}
	return fmt.Sprintf("saved %s (%d bytes, %s)", relPath, n, contentType), nil
}
