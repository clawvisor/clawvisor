package version

// Pinned reference to the iMessage helper tarballs hosted on GitHub Releases.
//
// Decoupled from clawvisor's own Version: most clawvisor releases reuse the
// same helper, so this points at a known-good release tag and SHA-256 per
// platform. Any build path — official release, `go install`, or local
// `go build` — has identical pin data and can verify a download.
//
// To rotate (after a real helper change, e.g. protocol bump):
//  1. Run `scripts/release-imessage-helper.sh`. It builds the helper for both
//     darwin arches, computes SHAs, and rewrites this file.
//  2. Commit the rewrite, tag/release with the new helper tarballs uploaded.
const IMessageHelperReleaseTag = "v0.9.3"

// IMessageHelperSHAs maps GOOS/GOARCH to the SHA-256 of the helper tarball
// hosted at IMessageHelperReleaseTag. Only darwin platforms are populated;
// iMessage is macOS-only.
var IMessageHelperSHAs = map[string]string{
	"darwin/arm64": "c8a68c2eb809a69ea954c1af161245f64bab6caec3dec893fe26962a022d6bf8",
	"darwin/amd64": "abdbdf656f47678f89719cb946afd3107232602d0c588b155558478c660b5139",
}

// IMessageHelperSHA returns the expected SHA-256 for the helper tarball that
// matches osArch (e.g. "darwin/arm64"), or empty string if not pinned.
func IMessageHelperSHA(osArch string) string {
	return IMessageHelperSHAs[osArch]
}
