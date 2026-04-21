package main

import "github.com/spf13/cobra"

// servicesParentCmd bundles the local-service-management subcommands
// under a single `services` parent. Plain commands like `install REPO`
// or `validate ./path` used to live at the top level; namespacing them
// frees the top level for daemon lifecycle and makes the menu of
// related operations visible in `clawvisor-local services --help`.
//
// Children:
//   list           — list discovered services + actions on disk
//   list-remote    — fetch the install manifest from a GitHub repo
//   inspect        — show one remote service without installing it
//   install        — install a remote service (or all, with --all)
//   upgrade        — upgrade an installed remote service
//   uninstall      — remove an installed remote service
//   validate       — validate a service.yaml or all discovered services
//   run            — manually invoke an action (for testing)
func servicesParentCmd() *cobra.Command {
	parent := &cobra.Command{
		Use:   "services",
		Short: "Manage local services (install, list, inspect, validate, run)",
		Long: `Local services are the plugins clawvisor-local supervises:
each one is a service.yaml + executable bundle on disk. These
commands install them from GitHub release manifests, inspect or
validate them, and let you run actions manually for testing.`,
	}
	parent.AddCommand(
		servicesListCmd(),
		listRemoteCmd(),
		inspectCmd(),
		installCmd(),
		upgradeCmd(),
		uninstallCmd(),
		validateCmd(),
		runCmd(),
	)
	return parent
}
