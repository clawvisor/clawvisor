package services

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DiscoverResult holds the results of scanning service directories.
type DiscoverResult struct {
	Services []*Service
	Excluded []ExcludedService
}

// Discover walks the given directories looking for service.yaml files,
// parses them, and returns loaded services plus any that were excluded.
func Discover(dirs []string, defaultTimeout time.Duration, disabledIDs map[string]bool) *DiscoverResult {
	result := &DiscoverResult{}

	type candidate struct {
		service *Service
		relPath string
		baseDir string
	}

	var candidates []candidate

	for _, dir := range dirs {
		dir = filepath.Clean(dir)
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			continue
		}

		_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			if info.Name() != "service.yaml" {
				return nil
			}

			svc, parseErr := ParseManifest(path, defaultTimeout)
			if parseErr != nil {
				// Categorize the error.
				cat := "invalid"
				if _, ok := parseErr.(*PlatformMismatchError); ok {
					cat = "unsupported"
				}
				rel, _ := filepath.Rel(dir, path)
				result.Excluded = append(result.Excluded, ExcludedService{
					Path:     rel,
					Category: cat,
					Error:    parseErr.Error(),
				})
				return nil
			}

			// Derive service ID from path if not explicitly set.
			svcDir := filepath.Dir(path)
			rel, _ := filepath.Rel(dir, svcDir)
			derivedID := strings.ReplaceAll(rel, string(filepath.Separator), ".")

			if svc.ID == "" {
				svc.ID = derivedID
			}

			candidates = append(candidates, candidate{
				service: svc,
				relPath: rel,
				baseDir: dir,
			})
			return nil
		})
	}

	// Check for duplicate IDs.
	idMap := make(map[string][]candidate)
	for _, c := range candidates {
		idMap[c.service.ID] = append(idMap[c.service.ID], c)
	}

	for id, cs := range idMap {
		if len(cs) == 1 {
			if disabledIDs != nil && disabledIDs[id] {
				result.Excluded = append(result.Excluded, ExcludedService{
					ID:       cs[0].service.ID,
					Name:     cs[0].service.Name,
					Path:     filepath.Join(cs[0].relPath, "service.yaml"),
					Category: "disabled",
					Error:    "disabled by user",
				})
				continue
			}
			result.Services = append(result.Services, cs[0].service)
		} else {
			// All duplicates are excluded. Use relative paths for display.
			relPaths := make([]string, len(cs))
			for i, c := range cs {
				relPaths[i] = filepath.Join(c.relPath, "service.yaml")
			}
			for i := range cs {
				otherPaths := make([]string, 0, len(relPaths)-1)
				for j, p := range relPaths {
					if j != i {
						otherPaths = append(otherPaths, p)
					}
				}
				result.Excluded = append(result.Excluded, ExcludedService{
					ID:       cs[i].service.ID,
					Name:     cs[i].service.Name,
					Path:     relPaths[i],
					Category: "conflict",
					Error:    fmt.Sprintf("duplicate ID: %s (also defined in %s)", id, strings.Join(otherPaths, ", ")),
				})
			}
		}
	}

	return result
}
