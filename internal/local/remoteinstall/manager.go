package remoteinstall

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/clawvisor/clawvisor/internal/local/config"
	"github.com/clawvisor/clawvisor/internal/local/services"
)

type Manager struct {
	BaseDir       string
	HomeDir       string
	HTTPClient    *http.Client
	githubAPIBase string
}

func NewManager(baseDir string) *Manager {
	home, _ := os.UserHomeDir()
	return &Manager{
		BaseDir: baseDir,
		HomeDir: home,
	}
}

func (m *Manager) managedServiceDir() string {
	return filepath.Join(m.BaseDir, "services")
}

func (m *Manager) runtimeBinDir() string {
	return filepath.Join(m.HomeDir, ".clawvisor", "bin")
}

func (m *Manager) FetchList(ctx context.Context, repoRef string) (*ManifestBundle, error) {
	return m.FetchManifest(ctx, repoRef)
}

func (m *Manager) Inspect(ctx context.Context, repoRef, selector string) (*ManifestBundle, *ManifestService, error) {
	bundle, err := m.FetchManifest(ctx, repoRef)
	if err != nil {
		return nil, nil, err
	}
	svc, err := bundle.Manifest.ResolveSelector(selector)
	if err != nil {
		return bundle, nil, err
	}
	return bundle, svc, nil
}

func (m *Manager) Install(ctx context.Context, repoRef string, selectors []string, installAll bool) (*InstallResult, error) {
	bundle, err := m.FetchManifest(ctx, repoRef)
	if err != nil {
		return nil, err
	}
	if err := bundle.Manifest.CheckCompatibility(); err != nil {
		return nil, err
	}

	var targets []ManifestService
	switch {
	case installAll:
		for _, svc := range bundle.Manifest.Services {
			if svc.SupportedOn(runtime.GOOS) {
				targets = append(targets, svc)
			}
		}
		if len(targets) == 0 {
			return nil, fmt.Errorf("no services from %s support %s", bundle.Repo, platformLabel())
		}
	case len(selectors) > 0:
		for _, selector := range selectors {
			svc, resolveErr := bundle.Manifest.ResolveSelector(selector)
			if resolveErr != nil {
				return nil, resolveErr
			}
			targets = append(targets, *svc)
		}
	default:
		return nil, fmt.Errorf("service selector is required")
	}

	state, err := loadInstallState(m.BaseDir)
	if err != nil {
		return nil, err
	}

	cfg, err := config.Load(m.BaseDir)
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}

	result := &InstallResult{
		Repo:       bundle.Repo,
		Version:    bundle.Manifest.Version,
		ReleaseURL: bundle.Release.HTMLURL,
	}
	for _, svc := range targets {
		installed, installErr := m.installService(ctx, bundle, state, cfg, svc)
		if installErr != nil {
			return nil, installErr
		}
		result.Services = append(result.Services, *installed)
		if err := saveInstallState(m.BaseDir, state); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (m *Manager) Upgrade(ctx context.Context, serviceID string) (*InstallResult, error) {
	state, err := loadInstallState(m.BaseDir)
	if err != nil {
		return nil, err
	}
	service, ok := state.Services[serviceID]
	if !ok {
		return nil, fmt.Errorf("service %s is not installed", serviceID)
	}
	return m.Install(ctx, service.Repo, []string{serviceID}, false)
}

func (m *Manager) Uninstall(serviceID string, pruneAssets bool) (*UninstallResult, error) {
	state, err := loadInstallState(m.BaseDir)
	if err != nil {
		return nil, err
	}
	service, ok := state.Services[serviceID]
	if !ok {
		return nil, fmt.Errorf("service %s is not installed", serviceID)
	}

	result := &UninstallResult{
		ServiceID:   serviceID,
		ServicePath: m.absoluteInstallPath(service.ServicePath),
	}

	serviceRecord, ok := state.Paths[service.ServicePath]
	if ok {
		serviceRecord.Owners = removeOwner(serviceRecord.Owners, serviceID)
		if len(serviceRecord.Owners) == 0 {
			if err := os.RemoveAll(serviceRecord.InstalledPath); err != nil && !os.IsNotExist(err) {
				return nil, fmt.Errorf("removing service dir: %w", err)
			}
			delete(state.Paths, service.ServicePath)
		} else {
			state.Paths[service.ServicePath] = serviceRecord
		}
	}

	for _, rel := range service.RuntimePaths {
		record, ok := state.Paths[rel]
		if !ok {
			continue
		}
		record.Owners = removeOwner(record.Owners, serviceID)
		if len(record.Owners) == 0 {
			if pruneAssets {
				if err := os.RemoveAll(record.InstalledPath); err != nil && !os.IsNotExist(err) {
					return nil, fmt.Errorf("removing runtime asset %s: %w", rel, err)
				}
				result.PrunedRuntimePaths = append(result.PrunedRuntimePaths, record.InstalledPath)
				delete(state.Paths, rel)
				continue
			}
		}
		state.Paths[rel] = record
	}

	delete(state.Services, serviceID)
	if err := saveInstallState(m.BaseDir, state); err != nil {
		return nil, err
	}
	sort.Strings(result.PrunedRuntimePaths)
	return result, nil
}

func (m *Manager) installService(ctx context.Context, bundle *ManifestBundle, state *InstallState, cfg *config.Config, svc ManifestService) (*InstalledService, error) {
	if !svc.SupportedOn(runtime.GOOS) {
		return nil, fmt.Errorf("service %s is unsupported on %s (supported: %s)", svc.ID, platformLabel(), strings.Join(svc.Platforms, ", "))
	}

	selections := []SelectedAsset{{
		Kind:     AssetKindServiceBundle,
		Service:  svc,
		Manifest: svc.ServiceBundle,
	}}
	for _, asset := range svc.RuntimeAssetsFor(runtime.GOOS, runtime.GOARCH) {
		selections = append(selections, SelectedAsset{
			Kind:     AssetKindRuntime,
			Service:  svc,
			Manifest: asset,
		})
	}

	prepared := make([]preparedAsset, 0, len(selections))
	for _, selection := range selections {
		releaseAsset, ok := bundle.Release.Assets[selection.Manifest.AssetName]
		if !ok {
			return nil, fmt.Errorf("missing release asset %s for service %s", selection.Manifest.AssetName, svc.ID)
		}
		item, err := m.prepareAsset(ctx, releaseAsset.URL, selection, cfg)
		if err != nil {
			return nil, err
		}
		prepared = append(prepared, *item)
		defer os.RemoveAll(item.TempRoot)
	}

	for _, item := range prepared {
		if err := validateOwnership(state, item, svc.ID); err != nil {
			return nil, err
		}
	}

	rollbacks := make([]func(), 0, len(prepared))
	finalizers := make([]func(), 0, len(prepared))
	for _, item := range prepared {
		rollback, finalize, err := applyPreparedAsset(item)
		if err != nil {
			for i := len(rollbacks) - 1; i >= 0; i-- {
				rollbacks[i]()
			}
			return nil, err
		}
		rollbacks = append(rollbacks, rollback)
		finalizers = append(finalizers, finalize)
	}

	for _, item := range prepared {
		record := state.Paths[item.RelativeInstallTo]
		record.InstalledPath = item.TargetPath
		record.Kind = item.Kind
		record.SourceAssetName = item.AssetName
		record.SourceSHA256 = item.SourceSHA256
		record.ContentDigest = item.ContentDigest
		record.Owners = addOwner(record.Owners, svc.ID)
		state.Paths[item.RelativeInstallTo] = record
	}

	runtimePaths := make([]string, 0, len(prepared))
	for _, item := range prepared {
		if item.Kind == AssetKindRuntime {
			runtimePaths = append(runtimePaths, item.TargetPath)
		}
	}
	sort.Strings(runtimePaths)

	state.Services[svc.ID] = InstalledServiceState{
		Repo:         bundle.Repo,
		Version:      bundle.Manifest.Version,
		InstalledAt:  time.Now().UTC(),
		ServicePath:  svc.ServiceBundle.InstallTo,
		RuntimePaths: collectRuntimeInstallTargets(prepared),
	}

	for _, finalize := range finalizers {
		finalize()
	}

	return &InstalledService{
		ID:           svc.ID,
		Name:         svc.Name,
		Version:      bundle.Manifest.Version,
		ServicePath:  m.absoluteInstallPath(svc.ServiceBundle.InstallTo),
		RuntimePaths: runtimePaths,
		PostInstall:  svc.PostInstall,
	}, nil
}

func collectRuntimeInstallTargets(prepared []preparedAsset) []string {
	out := make([]string, 0, len(prepared))
	for _, item := range prepared {
		if item.Kind == AssetKindRuntime {
			out = append(out, item.RelativeInstallTo)
		}
	}
	sort.Strings(out)
	return out
}

type preparedAsset struct {
	Kind              string
	AssetName         string
	SourceSHA256      string
	RelativeInstallTo string
	TargetPath        string
	ReplaceMode       string
	ContentDigest     string
	PayloadPath       string
	TempRoot          string
}

func (m *Manager) prepareAsset(ctx context.Context, url string, selection SelectedAsset, cfg *config.Config) (*preparedAsset, error) {
	workDir, err := os.MkdirTemp("", "clawvisor-local-install-*")
	if err != nil {
		return nil, fmt.Errorf("creating temp dir: %w", err)
	}

	archivePath := filepath.Join(workDir, selection.Manifest.AssetName)
	if err := m.downloadFile(ctx, url, archivePath); err != nil {
		return nil, fmt.Errorf("downloading %s: %w", selection.Manifest.AssetName, err)
	}
	digest, err := sha256File(archivePath)
	if err != nil {
		return nil, fmt.Errorf("hashing %s: %w", selection.Manifest.AssetName, err)
	}
	if !strings.EqualFold(digest, selection.Manifest.SHA256) {
		return nil, fmt.Errorf("checksum mismatch for %s: expected %s, got %s", selection.Manifest.AssetName, selection.Manifest.SHA256, digest)
	}

	extractDir := filepath.Join(workDir, "extract")
	if err := os.MkdirAll(extractDir, 0755); err != nil {
		return nil, fmt.Errorf("creating extract dir: %w", err)
	}
	top, err := extractTarGz(archivePath, extractDir)
	if err != nil {
		return nil, fmt.Errorf("extracting %s: %w", selection.Manifest.AssetName, err)
	}
	if len(top) != 1 {
		return nil, fmt.Errorf("asset %s must contain exactly one payload root", selection.Manifest.AssetName)
	}

	payloadPath := filepath.Join(extractDir, top[0])
	if selection.Kind == AssetKindServiceBundle {
		if err := validateServicePayload(payloadPath, selection.Service.ID, cfg.DefaultTimeout.Duration); err != nil {
			return nil, err
		}
	}

	contentDigest, err := treeDigest(payloadPath)
	if err != nil {
		return nil, fmt.Errorf("hashing extracted payload for %s: %w", selection.Manifest.AssetName, err)
	}

	rel := filepath.ToSlash(filepath.Clean(selection.Manifest.InstallTo))
	return &preparedAsset{
		Kind:              selection.Kind,
		AssetName:         selection.Manifest.AssetName,
		SourceSHA256:      selection.Manifest.SHA256,
		RelativeInstallTo: rel,
		TargetPath:        m.absoluteInstallPath(rel),
		ReplaceMode:       selection.Manifest.ReplaceMode,
		ContentDigest:     contentDigest,
		PayloadPath:       payloadPath,
		TempRoot:          workDir,
	}, nil
}

func (m *Manager) absoluteInstallPath(rel string) string {
	rel = filepath.ToSlash(rel)
	switch {
	case strings.HasPrefix(rel, "services/"):
		return filepath.Join(m.managedServiceDir(), filepath.FromSlash(strings.TrimPrefix(rel, "services/")))
	case strings.HasPrefix(rel, "bin/"):
		return filepath.Join(m.runtimeBinDir(), filepath.FromSlash(strings.TrimPrefix(rel, "bin/")))
	default:
		return filepath.Join(m.BaseDir, filepath.FromSlash(rel))
	}
}

func (m *Manager) downloadFile(ctx context.Context, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := m.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

func validateOwnership(state *InstallState, item preparedAsset, serviceID string) error {
	record, ok := state.Paths[item.RelativeInstallTo]
	if !ok {
		return nil
	}
	if record.SourceSHA256 == item.SourceSHA256 {
		return nil
	}
	if record.ContentDigest == item.ContentDigest {
		return nil
	}
	if _, statErr := os.Stat(record.InstalledPath); statErr == nil {
		existingDigest, err := treeDigest(record.InstalledPath)
		if err == nil && existingDigest == item.ContentDigest {
			return nil
		}
	}
	return fmt.Errorf("path collision for %s while installing %s: target is already owned by %s with different bytes", item.RelativeInstallTo, serviceID, strings.Join(record.Owners, ", "))
}

func applyPreparedAsset(item preparedAsset) (func(), func(), error) {
	parent := filepath.Dir(item.TargetPath)
	if err := os.MkdirAll(parent, 0755); err != nil {
		return nil, nil, fmt.Errorf("creating parent dir for %s: %w", item.TargetPath, err)
	}

	stageBase, err := os.MkdirTemp(parent, "."+filepath.Base(item.TargetPath)+".stage-*")
	if err != nil {
		return nil, nil, fmt.Errorf("creating staging dir: %w", err)
	}

	stagedPath := filepath.Join(stageBase, filepath.Base(item.TargetPath))
	if err := copyPayload(item.PayloadPath, stagedPath); err != nil {
		_ = os.RemoveAll(stageBase)
		return nil, nil, fmt.Errorf("staging %s: %w", item.TargetPath, err)
	}

	backupPath := ""
	if _, err := os.Stat(item.TargetPath); err == nil {
		backupPath = filepath.Join(parent, "."+filepath.Base(item.TargetPath)+".bak-"+fmt.Sprint(time.Now().UnixNano()))
		if err := os.Rename(item.TargetPath, backupPath); err != nil {
			_ = os.RemoveAll(stageBase)
			return nil, nil, fmt.Errorf("preparing replacement for %s: %w", item.TargetPath, err)
		}
	}

	if err := os.Rename(stagedPath, item.TargetPath); err != nil {
		if backupPath != "" {
			_ = os.Rename(backupPath, item.TargetPath)
		}
		_ = os.RemoveAll(stageBase)
		return nil, nil, fmt.Errorf("installing %s: %w", item.TargetPath, err)
	}
	_ = os.RemoveAll(stageBase)

	rollback := func() {
		_ = os.RemoveAll(item.TargetPath)
		if backupPath != "" {
			_ = os.Rename(backupPath, item.TargetPath)
		}
	}
	finalize := func() {
		if backupPath != "" {
			_ = os.RemoveAll(backupPath)
		}
	}
	return rollback, finalize, nil
}

func validateServicePayload(payloadPath, serviceID string, defaultTimeout time.Duration) error {
	manifestPath := filepath.Join(payloadPath, "service.yaml")
	if _, err := os.Stat(manifestPath); err != nil {
		return fmt.Errorf("service bundle is missing service.yaml")
	}
	svc, err := services.ParseManifest(manifestPath, defaultTimeout)
	if err != nil {
		return fmt.Errorf("service bundle contains invalid service.yaml: %w", err)
	}
	if svc.ID != "" && svc.ID != serviceID {
		return fmt.Errorf("service bundle id %q does not match manifest id %q", svc.ID, serviceID)
	}
	return nil
}

func extractTarGz(src, dest string) ([]string, error) {
	f, err := os.Open(src)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	gzr, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	topLevel := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		name := filepath.ToSlash(filepath.Clean(strings.TrimPrefix(hdr.Name, "./")))
		if name == "." || strings.HasPrefix(name, "../") || strings.HasPrefix(name, "/") {
			return nil, fmt.Errorf("invalid archive path %q", hdr.Name)
		}
		parts := strings.Split(name, "/")
		topLevel[parts[0]] = true
		target := filepath.Join(dest, filepath.FromSlash(name))
		if !strings.HasPrefix(target, dest) {
			return nil, fmt.Errorf("invalid archive path %q", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return nil, err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return nil, err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fs.FileMode(hdr.Mode)&0777)
			if err != nil {
				return nil, err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return nil, err
			}
			if err := out.Close(); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("unsupported archive entry type for %q", hdr.Name)
		}
	}

	entries := make([]string, 0, len(topLevel))
	for entry := range topLevel {
		entries = append(entries, entry)
	}
	sort.Strings(entries)
	return entries, nil
}

func copyPayload(src, dest string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(src, path)
			if err != nil {
				return err
			}
			target := dest
			if rel != "." {
				target = filepath.Join(dest, rel)
			}
			if info.IsDir() {
				return os.MkdirAll(target, info.Mode()&0777)
			}
			return copyFile(path, target, info.Mode()&0777)
		})
	}
	return copyFile(src, dest, info.Mode()&0777)
}

func copyFile(src, dest string, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

func treeDigest(path string) (string, error) {
	h := sha256.New()
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		if err := hashFileInto(h, path, filepath.Base(path)); err != nil {
			return "", err
		}
		return hex.EncodeToString(h.Sum(nil)), nil
	}
	err = filepath.Walk(path, func(cur string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(path, cur)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			rel = ""
		}
		if info.IsDir() {
			_, _ = h.Write([]byte("dir:" + rel + "\n"))
			return nil
		}
		return hashFileInto(h, cur, rel)
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func hashFileInto(h io.Writer, path, rel string) error {
	if _, err := h.Write([]byte("file:" + rel + "\n")); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(h, f)
	return err
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
