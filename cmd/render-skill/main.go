// Command render-skill renders SKILL.md.tmpl for a given target and writes the
// result to stdout. Used by CI to prepare the skill folder for clawhub publish.
package main

import (
	"fmt"
	"os"

	"github.com/clawvisor/clawvisor/skills"
)

func main() {
	target := skills.TargetClaudeCode
	if len(os.Args) > 1 {
		target = skills.Target(os.Args[1])
	}

	out, err := skills.Render(target)
	if err != nil {
		fmt.Fprintf(os.Stderr, "render-skill: %v\n", err)
		os.Exit(1)
	}
	fmt.Print(out)
}
