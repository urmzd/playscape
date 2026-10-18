// Package updater replaces the running binary with the latest GitHub
// release, implementing the portfolio-standard `update` subcommand.
package updater

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

const repo = "urmzd/playscape"

type release struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

// Update fetches the latest release and, when it is newer than current,
// swaps the running binary for the matching platform asset. It reports what
// happened on stderr and returns the new version tag (or the current one if
// already up to date).
func Update(current string) (string, error) {
	if runtime.GOOS == "windows" {
		return "", fmt.Errorf("self-update is not supported on windows; download from https://github.com/%s/releases", repo)
	}

	rel, err := latest()
	if err != nil {
		return "", err
	}
	if rel.TagName == "v"+current || rel.TagName == current {
		return rel.TagName, nil
	}

	want := fmt.Sprintf("playscape-%s-%s", runtime.GOOS, runtime.GOARCH)
	var url string
	for _, a := range rel.Assets {
		if a.Name == want {
			url = a.BrowserDownloadURL
			break
		}
	}
	if url == "" {
		return "", fmt.Errorf("release %s has no asset %q", rel.TagName, want)
	}

	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate running binary: %w", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return "", fmt.Errorf("resolve running binary: %w", err)
	}

	tmp, err := download(url, filepath.Dir(exe))
	if err != nil {
		return "", err
	}
	if err := os.Rename(tmp, exe); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("replace %s: %w", exe, err)
	}
	return rel.TagName, nil
}

func latest() (*release, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo))
	if err != nil {
		return nil, fmt.Errorf("fetch latest release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch latest release: %s", resp.Status)
	}
	var rel release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("decode release: %w", err)
	}
	return &rel, nil
}

// download fetches url into a temp file inside dir (same filesystem as the
// target so the final rename is atomic) and marks it executable.
func download(url, dir string) (string, error) {
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: %s", url, resp.Status)
	}

	f, err := os.CreateTemp(dir, ".playscape-update-*")
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", fmt.Errorf("download %s: %w", url, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	if err := os.Chmod(f.Name(), 0o755); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}
