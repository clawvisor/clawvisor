package isolation

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// imageTagPrefix is the repository name for the isolation netns-holder image.
const imageTagPrefix = "clawvisor-isolation"

// ImageTag returns the content-addressed tag derived from the embedded assets.
func ImageTag() (string, error) {
	hash, err := assetsHash()
	if err != nil {
		return "", err
	}
	short := hash
	if len(short) > 16 {
		short = short[:16]
	}
	return fmt.Sprintf("%s:%s", imageTagPrefix, short), nil
}

// EnsureImage builds the isolation image if it isn't already cached locally.
// dockerBin is the path to the `docker` binary.
func EnsureImage(ctx context.Context, dockerBin string) (string, error) {
	tag, err := ImageTag()
	if err != nil {
		return "", err
	}
	if imageExists(ctx, dockerBin, tag) {
		return tag, nil
	}
	tarball, err := assetsTar()
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, dockerBin, "build", "-t", tag, "-")
	cmd.Stdin = bytes.NewReader(tarball)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker build %s: %w (stderr: %s)", tag, err, strings.TrimSpace(stderr.String()))
	}
	return tag, nil
}

func imageExists(ctx context.Context, dockerBin, tag string) bool {
	cmd := exec.CommandContext(ctx, dockerBin, "image", "inspect", tag)
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run() == nil
}
