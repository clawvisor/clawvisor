package isolation

import (
	"archive/tar"
	"bytes"
	"io"
	"sort"
	"strings"
	"testing"
)

func TestEnsureImageHashStable(t *testing.T) {
	a, err := assetsHash()
	if err != nil {
		t.Fatalf("assetsHash err: %v", err)
	}
	b, err := assetsHash()
	if err != nil {
		t.Fatalf("assetsHash err: %v", err)
	}
	if a != b {
		t.Fatalf("assetsHash not stable across calls: %s vs %s", a, b)
	}
	if len(a) != 64 {
		t.Fatalf("assetsHash should be hex sha256, got len=%d (%q)", len(a), a)
	}
}

func TestImageTagFormat(t *testing.T) {
	tag, err := ImageTag()
	if err != nil {
		t.Fatalf("ImageTag err: %v", err)
	}
	if !strings.HasPrefix(tag, imageTagPrefix+":") {
		t.Fatalf("tag should start with %q, got %q", imageTagPrefix+":", tag)
	}
	suffix := strings.TrimPrefix(tag, imageTagPrefix+":")
	if len(suffix) == 0 {
		t.Fatalf("tag missing hash suffix: %q", tag)
	}
}

func TestAssetsTarContainsExpectedFiles(t *testing.T) {
	data, err := assetsTar()
	if err != nil {
		t.Fatalf("assetsTar err: %v", err)
	}
	tr := tar.NewReader(bytes.NewReader(data))
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar read: %v", err)
		}
		names = append(names, hdr.Name)
	}
	sort.Strings(names)
	want := []string{dockerfileName, entrypointName, initFirewallName}
	sort.Strings(want)
	if len(names) != len(want) {
		t.Fatalf("tar files: got %v want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("tar file[%d]: got %q want %q (full got=%v)", i, names[i], want[i], names)
		}
	}
}
