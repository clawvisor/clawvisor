package isolation

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"sort"
)

//go:embed assets
var assetsFS embed.FS

const (
	assetDir          = "assets"
	dockerfileName    = "Dockerfile"
	initFirewallName  = "init-firewall.sh"
	entrypointName    = "entrypoint-holder.sh"
	executablePerm    = 0o755
	regularFilePerm   = 0o644
)

// assetFiles returns the embedded asset files in deterministic order.
func assetFiles() ([]string, error) {
	entries, err := fs.ReadDir(assetsFS, assetDir)
	if err != nil {
		return nil, fmt.Errorf("read embedded asset dir: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

// assetsHash returns a hex sha256 over (name, mode, contents) of every embedded asset.
// Stable across runs as long as assets don't change.
func assetsHash() (string, error) {
	names, err := assetFiles()
	if err != nil {
		return "", err
	}
	h := sha256.New()
	for _, name := range names {
		data, err := fs.ReadFile(assetsFS, assetDir+"/"+name)
		if err != nil {
			return "", fmt.Errorf("read embedded asset %q: %w", name, err)
		}
		mode := assetMode(name)
		fmt.Fprintf(h, "%s\x00%o\x00%d\x00", name, mode, len(data))
		_, _ = h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// assetsTar returns a tar archive containing every embedded asset at the archive root.
// Suitable for piping into `docker build -t <tag> -`.
func assetsTar() ([]byte, error) {
	names, err := assetFiles()
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range names {
		data, err := fs.ReadFile(assetsFS, assetDir+"/"+name)
		if err != nil {
			return nil, fmt.Errorf("read embedded asset %q: %w", name, err)
		}
		hdr := &tar.Header{
			Name: name,
			Mode: int64(assetMode(name)),
			Size: int64(len(data)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, fmt.Errorf("tar header %q: %w", name, err)
		}
		if _, err := tw.Write(data); err != nil {
			return nil, fmt.Errorf("tar write %q: %w", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("tar close: %w", err)
	}
	return buf.Bytes(), nil
}

func assetMode(name string) uint32 {
	switch name {
	case initFirewallName, entrypointName:
		return executablePerm
	default:
		return regularFilePerm
	}
}
