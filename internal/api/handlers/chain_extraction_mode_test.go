package handlers

import (
	"testing"

	"github.com/clawvisor/clawvisor/pkg/store"
)

func TestResolveChainExtractionMode(t *testing.T) {
	cases := []struct {
		name string
		task *store.Task
		want string
	}{
		{"nil task falls through to default", nil, chainExtractionFull},
		{"unset task field falls through to default", &store.Task{}, chainExtractionFull},
		{"explicit full", &store.Task{ChainExtractionMode: "full"}, chainExtractionFull},
		{"explicit builtins_only", &store.Task{ChainExtractionMode: "builtins_only"}, chainExtractionBuiltinsOnly},
		{"unrecognized value falls through to default", &store.Task{ChainExtractionMode: "garbage"}, chainExtractionFull},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveChainExtractionMode(tc.task)
			if got != tc.want {
				t.Errorf("resolveChainExtractionMode(%+v) = %q, want %q", tc.task, got, tc.want)
			}
		})
	}
}
