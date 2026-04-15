package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/clawvisor/clawvisor/internal/local/config"
	"github.com/clawvisor/clawvisor/internal/local/remoteinstall"
)

func installCmd() *cobra.Command {
	var installAll bool

	cmd := &cobra.Command{
		Use:   "install REPO [SERVICE]",
		Short: "Install remote-managed local services from a GitHub release manifest",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			manager := remoteinstall.NewManager(baseDir())
			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
			defer cancel()

			selectors := make([]string, 0, 1)
			if len(args) == 2 {
				selectors = append(selectors, args[1])
			}

			if len(selectors) == 0 && !installAll {
				bundle, err := manager.FetchList(ctx, args[0])
				if err != nil {
					return err
				}
				if !isInteractiveInstall() {
					printAvailableServices(bundle)
					return fmt.Errorf("pass a service id, alias, or use --all")
				}
				selector, err := promptForService(bundle)
				if err != nil {
					return err
				}
				selectors = append(selectors, selector)
			}

			result, err := manager.Install(ctx, args[0], selectors, installAll)
			if err != nil {
				return err
			}

			printInstallResult(result)
			printReloadStatus(attemptDaemonReload(baseDir()))
			return nil
		},
	}

	cmd.Flags().BoolVar(&installAll, "all", false, "install all services supported on this machine")
	return cmd
}

func listRemoteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list REPO",
		Short: "List installable remote-managed local services",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			manager := remoteinstall.NewManager(baseDir())
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()

			bundle, err := manager.FetchList(ctx, args[0])
			if err != nil {
				return err
			}
			printAvailableServices(bundle)
			return nil
		},
	}
}

func inspectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "inspect REPO SERVICE",
		Short: "Inspect a remote-managed service without installing it",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			manager := remoteinstall.NewManager(baseDir())
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()

			bundle, svc, err := manager.Inspect(ctx, args[0], args[1])
			if err != nil {
				return err
			}
			printInspectResult(bundle, svc)
			return nil
		},
	}
}

func upgradeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "upgrade SERVICE_ID",
		Short: "Upgrade an installed remote-managed service",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			manager := remoteinstall.NewManager(baseDir())
			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
			defer cancel()

			result, err := manager.Upgrade(ctx, args[0])
			if err != nil {
				return err
			}
			printInstallResult(result)
			printReloadStatus(attemptDaemonReload(baseDir()))
			return nil
		},
	}
}

func uninstallCmd() *cobra.Command {
	var pruneAssets bool

	cmd := &cobra.Command{
		Use:   "uninstall SERVICE_ID",
		Short: "Remove an installed remote-managed service",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			manager := remoteinstall.NewManager(baseDir())
			result, err := manager.Uninstall(args[0], pruneAssets)
			if err != nil {
				return err
			}

			fmt.Printf("Removed %s\n", result.ServiceID)
			fmt.Printf("  Service path: %s\n", result.ServicePath)
			if len(result.PrunedRuntimePaths) > 0 {
				fmt.Println("  Pruned runtime assets:")
				for _, path := range result.PrunedRuntimePaths {
					fmt.Printf("    %s\n", path)
				}
			}
			printReloadStatus(attemptDaemonReload(baseDir()))
			return nil
		},
	}

	cmd.Flags().BoolVar(&pruneAssets, "prune-assets", false, "remove unreferenced runtime assets after uninstall")
	return cmd
}

func isInteractiveInstall() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

func promptForService(bundle *remoteinstall.ManifestBundle) (string, error) {
	services := sortedManifestServices(bundle)
	printAvailableServices(bundle)
	fmt.Print("\nChoose a service to install: ")
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return "", fmt.Errorf("service selection is required")
	}
	if idx, convErr := strconv.Atoi(line); convErr == nil {
		if idx < 1 || idx > len(services) {
			return "", fmt.Errorf("invalid selection %d", idx)
		}
		return services[idx-1].ID, nil
	}
	return line, nil
}

func printAvailableServices(bundle *remoteinstall.ManifestBundle) {
	fmt.Printf("Available services from %s:\n\n", bundle.Repo)
	services := sortedManifestServices(bundle)
	for idx, svc := range services {
		supported := "no"
		if svc.SupportedOn(runtime.GOOS) {
			supported = "yes"
		}
		aliases := "(none)"
		if len(svc.Aliases) > 0 {
			aliases = strings.Join(svc.Aliases, ", ")
		}
		fmt.Printf("%d. %s (%s) [%s]\n", idx+1, svc.Name, svc.ID, supportedLabel(supported))
		fmt.Printf("   %s\n", svc.Description)
		fmt.Printf("   Aliases: %s\n", aliases)
		fmt.Printf("   Supported here: %s\n\n", supported)
	}
}

func sortedManifestServices(bundle *remoteinstall.ManifestBundle) []remoteinstall.ManifestService {
	services := append([]remoteinstall.ManifestService(nil), bundle.Manifest.Services...)
	sort.Slice(services, func(i, j int) bool { return services[i].ID < services[j].ID })
	return services
}

func printInspectResult(bundle *remoteinstall.ManifestBundle, svc *remoteinstall.ManifestService) {
	fmt.Printf("%s\n", svc.ID)
	fmt.Printf("  Name: %s\n", svc.Name)
	fmt.Printf("  Description: %s\n", svc.Description)
	if len(svc.Aliases) == 0 {
		fmt.Printf("  Aliases: (none)\n")
	} else {
		fmt.Printf("  Aliases: %s\n", strings.Join(svc.Aliases, ", "))
	}
	fmt.Printf("  Supported platforms: %s\n", strings.Join(svc.Platforms, ", "))
	fmt.Printf("  Supported here: %v\n", svc.SupportedOn(runtime.GOOS))
	fmt.Printf("  Release: %s (%s)\n", bundle.Manifest.Version, bundle.Release.HTMLURL)
	fmt.Printf("  Service bundle: %s -> %s\n", svc.ServiceBundle.AssetName, svc.ServiceBundle.InstallTo)

	runtimeAssets := svc.RuntimeAssetsFor(runtime.GOOS, runtime.GOARCH)
	if len(runtimeAssets) == 0 {
		fmt.Printf("  Runtime assets for %s/%s: none\n", runtime.GOOS, runtime.GOARCH)
	} else {
		fmt.Printf("  Runtime assets for %s/%s:\n", runtime.GOOS, runtime.GOARCH)
		for _, asset := range runtimeAssets {
			fmt.Printf("    %s -> %s\n", asset.AssetName, asset.InstallTo)
		}
	}
	if len(svc.PostInstall.Notes) > 0 || len(svc.PostInstall.Permissions) > 0 {
		fmt.Println("  Post-install:")
		for _, permission := range svc.PostInstall.Permissions {
			fmt.Printf("    Permission: %s\n", permission)
		}
		for _, note := range svc.PostInstall.Notes {
			fmt.Printf("    %s\n", note)
		}
	}
}

func printInstallResult(result *remoteinstall.InstallResult) {
	for _, svc := range result.Services {
		fmt.Printf("Installed %s\n", svc.ID)
		fmt.Printf("  Version: %s\n", svc.Version)
		fmt.Printf("  Service path: %s\n", svc.ServicePath)
		if len(svc.RuntimePaths) > 0 {
			fmt.Println("  Runtime assets:")
			for _, path := range svc.RuntimePaths {
				fmt.Printf("    %s\n", path)
			}
		}
		if len(svc.PostInstall.Permissions) > 0 || len(svc.PostInstall.Notes) > 0 {
			fmt.Println("  Next steps:")
			for _, permission := range svc.PostInstall.Permissions {
				fmt.Printf("    Permission: %s\n", permission)
			}
			for _, note := range svc.PostInstall.Notes {
				fmt.Printf("    %s\n", note)
			}
		}
		fmt.Println()
	}
}

func supportedLabel(value string) string {
	if value == "yes" {
		return "supported"
	}
	return "unsupported"
}

func attemptDaemonReload(dir string) error {
	cfg, err := config.Load(dir)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/api/services/reload", cfg.Port)
	_, err = httpPost(url)
	return err
}

func printReloadStatus(err error) {
	if err == nil {
		fmt.Println("Daemon reloaded.")
		return
	}
	fmt.Printf("Filesystem changes succeeded, but the daemon has not picked them up yet: %v\n", err)
	fmt.Println("Run `clawvisor-local reload` or restart the daemon.")
}
