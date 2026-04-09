package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/clawvisor/clawvisor/pkg/adapters/yamlloader"
	"github.com/clawvisor/clawvisor/pkg/adapters/yamlvalidate"
)

var validateCmd = &cobra.Command{
	Use:   "validate [file.yaml ...]",
	Short: "Validate adapter YAML definitions",
	Long: `Validate one or more adapter YAML files for structural correctness.

If no files are specified, validates all .yaml files in ~/.clawvisor/adapters/.`,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		files, err := resolveValidateFiles(args)
		if err != nil {
			return err
		}
		if len(files) == 0 {
			fmt.Println("No adapter YAML files found.")
			return nil
		}

		hasErrors := false
		for _, path := range files {
			def, err := yamlloader.LoadFile(path)
			if err != nil {
				fmt.Fprintf(os.Stderr, "\033[31m✗\033[0m %s\n  parse error: %v\n\n", path, err)
				hasErrors = true
				continue
			}

			result := yamlvalidate.Validate(&def)
			if result.OK() && len(result.Warnings) == 0 {
				fmt.Printf("\033[32m✓\033[0m %s (%s)\n", path, def.Service.ID)
				continue
			}

			if !result.OK() {
				hasErrors = true
				fmt.Fprintf(os.Stderr, "\033[31m✗\033[0m %s (%s)\n", path, def.Service.ID)
				for _, e := range result.Errors {
					fmt.Fprintf(os.Stderr, "  \033[31merror:\033[0m %s\n", e)
				}
			} else {
				fmt.Printf("\033[33m⚠\033[0m %s (%s)\n", path, def.Service.ID)
			}
			for _, w := range result.Warnings {
				fmt.Fprintf(os.Stderr, "  \033[33mwarn:\033[0m  %s\n", w)
			}
			fmt.Println()
		}

		if hasErrors {
			return fmt.Errorf("validation failed")
		}
		return nil
	},
}

// resolveValidateFiles returns the list of YAML files to validate.
// If args are given, uses those paths directly.
// Otherwise scans ~/.clawvisor/adapters/.
func resolveValidateFiles(args []string) ([]string, error) {
	if len(args) > 0 {
		return args, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("could not determine home directory: %w", err)
	}
	dir := filepath.Join(home, ".clawvisor", "adapters")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s: %w", dir, err)
	}

	var files []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".yaml") {
			files = append(files, filepath.Join(dir, entry.Name()))
		}
	}
	return files, nil
}
