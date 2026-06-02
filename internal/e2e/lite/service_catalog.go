package lite

import (
	"fmt"
	"sort"

	"github.com/clawvisor/clawvisor/pkg/adapters/yamldef"
)

// scenarioServiceCatalogDefs returns the built-in service definitions
// the harness should plumb into the lite-proxy's Catalog for this
// scenario run. Scenarios opt in via the top-level
// `service_catalog:` YAML field, listing the service IDs they want
// the catalog populated with (e.g. ["github"]). Scenarios that don't
// declare the field get an empty catalog and the lite-proxy's
// scope-drift menu stays unreachable — that's the legacy behavior.
//
// Built-in defs live in catalog_defs_*.go alongside this file. We
// keep them minimal: just enough Method + Path coverage for the
// reverse-resolver to produce a (service, action) for the credentialed
// curls scenarios make. The full production adapter wiring (auth,
// risk metadata, response shapes) is intentionally absent — the
// harness doesn't need it.
//
// Unknown IDs return an error rather than silently no-op'ing — a typo
// like `gitub` would otherwise disable catalog wiring (and therefore
// the whole scope-drift code path the scenario means to exercise)
// without any signal in the harness logs.
func scenarioServiceCatalogDefs(scn *Scenario) ([]yamldef.ServiceDef, error) {
	if scn == nil || len(scn.ServiceCatalog) == 0 {
		return nil, nil
	}
	defs := make([]yamldef.ServiceDef, 0, len(scn.ServiceCatalog))
	for _, id := range scn.ServiceCatalog {
		switch id {
		case "github":
			defs = append(defs, githubServiceDef())
		default:
			return nil, fmt.Errorf("scenario %q: unknown service_catalog id %q (known: %v)", scn.ID, id, knownServiceCatalogIDs())
		}
	}
	return defs, nil
}

// knownServiceCatalogIDs lists every id scenarioServiceCatalogDefs
// recognises. Kept centralised so adding a new built-in def in this
// file only requires one change, and so the error message above can
// surface the candidate set for typo-correction.
func knownServiceCatalogIDs() []string {
	ids := []string{"github"}
	sort.Strings(ids)
	return ids
}

// githubServiceDef returns a minimal github ServiceDef with just enough
// actions for the scope-drift e2e scenarios. We deliberately keep the
// action set small and obvious so a scenario can craft a credentialed
// curl that the catalog will reverse-resolve and the task-scope
// checker will then accept or reject.
func githubServiceDef() yamldef.ServiceDef {
	return yamldef.ServiceDef{
		Service: yamldef.ServiceInfo{
			ID:          "github",
			DisplayName: "GitHub",
		},
		API: yamldef.APIDef{
			BaseURL: "https://api.github.com",
			Type:    "rest",
		},
		Actions: map[string]yamldef.Action{
			"list_issues": {
				Method: "GET",
				Path:   "/repos/{{.owner}}/{{.repo}}/issues",
				Risk:   yamldef.RiskDef{Category: "read", Sensitivity: "low"},
			},
			"get_issue": {
				Method: "GET",
				Path:   "/repos/{{.owner}}/{{.repo}}/issues/{{.number}}",
				Risk:   yamldef.RiskDef{Category: "read", Sensitivity: "low"},
			},
			"create_issue": {
				Method: "POST",
				Path:   "/repos/{{.owner}}/{{.repo}}/issues",
				Risk:   yamldef.RiskDef{Category: "write", Sensitivity: "medium"},
			},
			"add_issue_comment": {
				Method: "POST",
				Path:   "/repos/{{.owner}}/{{.repo}}/issues/{{.number}}/comments",
				Risk:   yamldef.RiskDef{Category: "write", Sensitivity: "medium"},
			},
		},
	}
}
