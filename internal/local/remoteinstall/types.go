package remoteinstall

import (
	"fmt"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/clawvisor/clawvisor/pkg/version"
)

const (
	ManifestAssetName      = "clawvisor-local-manifest.json"
	ManifestSchemaVersion  = 1
	ServiceSchemaVersion   = 1
	ReplaceModeDir         = "atomic_dir_replace"
	ReplaceModeFile        = "atomic_file_replace"
	AssetKindServiceBundle = "service_bundle"
	AssetKindRuntime       = "runtime_asset"
)

type Manifest struct {
	SchemaVersion            int               `json:"schema_version"`
	Repo                     string            `json:"repo"`
	Version                  string            `json:"version"`
	MinClawvisorLocalVersion string            `json:"min_clawvisor_local_version"`
	Services                 []ManifestService `json:"services"`
}

type ManifestService struct {
	ID                   string          `json:"id"`
	Name                 string          `json:"name"`
	Description          string          `json:"description"`
	Aliases              []string        `json:"aliases"`
	Platforms            []string        `json:"platforms"`
	ServiceSchemaVersion int             `json:"service_schema_version"`
	ServiceBundle        ManifestAsset   `json:"service_bundle"`
	RuntimeAssets        []RuntimeAsset  `json:"runtime_assets"`
	PostInstall          PostInstallInfo `json:"post_install"`
}

type ManifestAsset struct {
	AssetName   string `json:"asset_name"`
	SHA256      string `json:"sha256"`
	ArchiveType string `json:"archive_type"`
	InstallTo   string `json:"install_to"`
	ReplaceMode string `json:"replace_mode"`
}

type RuntimeAsset struct {
	ManifestAsset
	OS   string `json:"os"`
	Arch string `json:"arch"`
}

type PostInstallInfo struct {
	Permissions      []string `json:"permissions"`
	AppsRequiringFDA []string `json:"apps_requiring_fda"`
	Notes            []string `json:"notes"`
	RestartRequired  bool     `json:"restart_required"`
}

type Release struct {
	Repo    string
	TagName string
	HTMLURL string
	Assets  map[string]ReleaseAsset
}

type ReleaseAsset struct {
	Name string
	URL  string
}

type ManifestBundle struct {
	Repo     string
	Manifest *Manifest
	Release  *Release
}

type SelectedAsset struct {
	Kind     string
	Service  ManifestService
	Manifest ManifestAsset
}

type InstallResult struct {
	Repo       string
	Version    string
	Services   []InstalledService
	ReleaseURL string
}

type InstalledService struct {
	ID           string
	Name         string
	Version      string
	ServicePath  string
	RuntimePaths []string
	PostInstall  PostInstallInfo
}

type UninstallResult struct {
	ServiceID          string
	ServicePath        string
	PrunedRuntimePaths []string
}

func (m *Manifest) Validate(expectedRepo string) error {
	if m == nil {
		return fmt.Errorf("manifest is required")
	}
	if m.SchemaVersion != ManifestSchemaVersion {
		return fmt.Errorf("unsupported manifest schema version: %d", m.SchemaVersion)
	}
	if expectedRepo != "" && m.Repo != expectedRepo {
		return fmt.Errorf("manifest repo %q does not match requested repo %q", m.Repo, expectedRepo)
	}
	if m.Repo == "" {
		return fmt.Errorf("manifest repo is required")
	}
	if m.Version == "" {
		return fmt.Errorf("manifest version is required")
	}

	serviceIDs := make(map[string]bool, len(m.Services))
	aliases := make(map[string]string)
	for _, svc := range m.Services {
		if err := svc.validate(); err != nil {
			return fmt.Errorf("service %q: %w", svc.ID, err)
		}
		if serviceIDs[svc.ID] {
			return fmt.Errorf("duplicate service id: %s", svc.ID)
		}
		serviceIDs[svc.ID] = true
		for _, alias := range svc.Aliases {
			key := strings.ToLower(strings.TrimSpace(alias))
			if key == "" {
				return fmt.Errorf("service %q: alias cannot be empty", svc.ID)
			}
			if existing, ok := aliases[key]; ok {
				return fmt.Errorf("duplicate alias %q used by %s and %s", key, existing, svc.ID)
			}
			aliases[key] = svc.ID
		}
	}
	return nil
}

func (s *ManifestService) validate() error {
	if s.ID == "" {
		return fmt.Errorf("id is required")
	}
	if s.Name == "" {
		return fmt.Errorf("name is required")
	}
	if s.Description == "" {
		return fmt.Errorf("description is required")
	}
	if len(s.Platforms) == 0 {
		return fmt.Errorf("platforms is required")
	}
	if s.ServiceSchemaVersion != ServiceSchemaVersion {
		return fmt.Errorf("unsupported service schema version: %d", s.ServiceSchemaVersion)
	}
	if err := validateAsset(s.ServiceBundle, s.ID, AssetKindServiceBundle); err != nil {
		return err
	}
	expectedServicePath := filepath.ToSlash(filepath.Join("services", s.ID))
	if filepath.ToSlash(s.ServiceBundle.InstallTo) != expectedServicePath {
		return fmt.Errorf("service bundle install_to must be %q", expectedServicePath)
	}
	for _, asset := range s.RuntimeAssets {
		if asset.OS == "" || asset.Arch == "" {
			return fmt.Errorf("runtime asset %q must declare os and arch", asset.AssetName)
		}
		if err := validateAsset(asset.ManifestAsset, s.ID, AssetKindRuntime); err != nil {
			return err
		}
	}
	return nil
}

func validateAsset(asset ManifestAsset, serviceID string, kind string) error {
	if asset.AssetName == "" {
		return fmt.Errorf("%s asset_name is required", kind)
	}
	if asset.SHA256 == "" {
		return fmt.Errorf("%s sha256 is required", kind)
	}
	if asset.ArchiveType != "tar.gz" {
		return fmt.Errorf("%s archive_type %q is unsupported", kind, asset.ArchiveType)
	}
	if asset.InstallTo == "" {
		return fmt.Errorf("%s install_to is required", kind)
	}
	if err := validateInstallTo(asset.InstallTo); err != nil {
		return fmt.Errorf("%s install_to: %w", kind, err)
	}
	switch asset.ReplaceMode {
	case ReplaceModeDir, ReplaceModeFile:
	default:
		return fmt.Errorf("%s replace_mode %q is unsupported", kind, asset.ReplaceMode)
	}
	return nil
}

func validateInstallTo(installTo string) error {
	if filepath.IsAbs(installTo) {
		return fmt.Errorf("absolute paths are not allowed")
	}
	clean := filepath.ToSlash(filepath.Clean(installTo))
	if clean == "." || strings.HasPrefix(clean, "../") || clean == ".." {
		return fmt.Errorf("path traversal is not allowed")
	}
	if !strings.HasPrefix(clean, "services/") && !strings.HasPrefix(clean, "bin/") {
		return fmt.Errorf("path must start with services/ or bin/")
	}
	return nil
}

func (m *Manifest) ResolveSelector(selector string) (*ManifestService, error) {
	selector = strings.ToLower(strings.TrimSpace(selector))
	if selector == "" {
		return nil, fmt.Errorf("service selector is required")
	}
	for i := range m.Services {
		if strings.ToLower(m.Services[i].ID) == selector {
			return &m.Services[i], nil
		}
	}
	var match *ManifestService
	for i := range m.Services {
		for _, alias := range m.Services[i].Aliases {
			if strings.ToLower(strings.TrimSpace(alias)) == selector {
				if match != nil {
					return nil, fmt.Errorf("selector %q is ambiguous", selector)
				}
				match = &m.Services[i]
			}
		}
	}
	if match != nil {
		return match, nil
	}

	suggestions := m.Suggestions(selector)
	if len(suggestions) > 0 {
		return nil, fmt.Errorf("service selector %q not found. close matches: %s", selector, strings.Join(suggestions, ", "))
	}
	return nil, fmt.Errorf("service selector %q not found", selector)
}

func (m *Manifest) Suggestions(selector string) []string {
	type candidate struct {
		label string
		score int
	}
	var out []candidate
	for _, svc := range m.Services {
		for _, label := range append([]string{svc.ID}, svc.Aliases...) {
			score := levenshtein(strings.ToLower(selector), strings.ToLower(label))
			if strings.Contains(strings.ToLower(label), strings.ToLower(selector)) {
				score--
			}
			out = append(out, candidate{label: svc.ID, score: score})
		}
	}
	slices.SortFunc(out, func(a, b candidate) int {
		if a.score != b.score {
			return a.score - b.score
		}
		return strings.Compare(a.label, b.label)
	})
	seen := map[string]bool{}
	suggestions := make([]string, 0, 3)
	for _, item := range out {
		if seen[item.label] {
			continue
		}
		seen[item.label] = true
		suggestions = append(suggestions, item.label)
		if len(suggestions) == 3 {
			break
		}
	}
	return suggestions
}

func (s *ManifestService) SupportedOn(goos string) bool {
	for _, platform := range s.Platforms {
		if platform == goos {
			return true
		}
	}
	return false
}

func (s *ManifestService) RuntimeAssetsFor(goos, goarch string) []ManifestAsset {
	assets := make([]ManifestAsset, 0, len(s.RuntimeAssets))
	for _, asset := range s.RuntimeAssets {
		if asset.OS == goos && asset.Arch == goarch {
			assets = append(assets, asset.ManifestAsset)
		}
	}
	return assets
}

func (m *Manifest) CheckCompatibility() error {
	if version.Version == "dev" || version.Version == "" {
		return nil
	}
	if m.MinClawvisorLocalVersion == "" {
		return nil
	}
	if compareSemver(version.Version, m.MinClawvisorLocalVersion) < 0 {
		return fmt.Errorf("current clawvisor-local version %s is below required %s", version.Version, m.MinClawvisorLocalVersion)
	}
	return nil
}

func platformLabel() string {
	return runtime.GOOS + "/" + runtime.GOARCH
}

func compareSemver(a, b string) int {
	aMajor, aMinor, aPatch := parseSemver(a)
	bMajor, bMinor, bPatch := parseSemver(b)
	switch {
	case aMajor != bMajor:
		if aMajor < bMajor {
			return -1
		}
		return 1
	case aMinor != bMinor:
		if aMinor < bMinor {
			return -1
		}
		return 1
	case aPatch != bPatch:
		if aPatch < bPatch {
			return -1
		}
		return 1
	default:
		return 0
	}
}

func parseSemver(v string) (major, minor, patch int) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	parts := strings.SplitN(v, "-", 2)
	segments := strings.Split(parts[0], ".")
	if len(segments) > 0 {
		fmt.Sscanf(segments[0], "%d", &major)
	}
	if len(segments) > 1 {
		fmt.Sscanf(segments[1], "%d", &minor)
	}
	if len(segments) > 2 {
		fmt.Sscanf(segments[2], "%d", &patch)
	}
	return
}

func levenshtein(a, b string) int {
	if a == b {
		return 0
	}
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 0
			if a[i-1] != b[j-1] {
				cost = 1
			}
			cur[j] = min(
				cur[j-1]+1,
				prev[j]+1,
				prev[j-1]+cost,
			)
		}
		prev = cur
	}
	return prev[len(b)]
}

func min(values ...int) int {
	best := values[0]
	for _, value := range values[1:] {
		if value < best {
			best = value
		}
	}
	return best
}
