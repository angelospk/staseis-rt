// Package buildinfo names the build that answers: the git commit it was made from and the
// contract revision it implements. /v1/status reports both.
package buildinfo

import "runtime/debug"

// ContractRev is the contract revision the server implements (docs/CONTRACT.md).
const ContractRev = 24

// Revision is the git commit of the build. The image build sets it:
// -ldflags "-X github.com/angelospk/athens-transit-rt/internal/buildinfo.Revision=<sha>".
var Revision string

// Rev is Revision if set, else the VCS revision Go embeds in a local build, else "dev".
func Rev() string {
	if Revision != "" {
		return Revision
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" && s.Value != "" {
				return s.Value
			}
		}
	}
	return "dev"
}
