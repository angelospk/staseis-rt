package buildinfo

import (
	"os"
	"strings"
	"testing"
)

// Rev is what /v1/status reports as `rev`: the commit set at build time (image.yml passes it as
// a Docker build arg), else the VCS revision Go embeds in a local build, else "dev".
func TestRev(t *testing.T) {
	saved := Revision
	t.Cleanup(func() { Revision = saved })

	Revision = "0123456789abcdef0123456789abcdef01234567"
	if got := Rev(); got != Revision {
		t.Fatalf("Rev() = %q, want the build-time revision %q", got, Revision)
	}
	Revision = ""
	if got := Rev(); got == "" {
		t.Fatal("Rev() is empty without a build-time revision; want the VCS revision or \"dev\"")
	}
}

func TestContractRev(t *testing.T) {
	if ContractRev < 24 {
		t.Fatalf("ContractRev = %d; rev 24 added rev and contract_rev to /v1/status", ContractRev)
	}
}

// The image build sets Revision through this package's import path: a rename or a missing build
// arg would leave every host reporting "dev", and the promotion gate could not confirm a deploy.
func TestImageSetsRevision(t *testing.T) {
	dockerfile := read(t, "../../Dockerfile")
	for _, want := range []string{"ARG REVISION", "-X github.com/angelospk/athens-transit-rt/internal/buildinfo.Revision=${REVISION}"} {
		if !strings.Contains(dockerfile, want) {
			t.Errorf("Dockerfile has no %q", want)
		}
	}
	if wf := read(t, "../../.github/workflows/image.yml"); !strings.Contains(wf, "REVISION=${{ github.sha }}") {
		t.Error("image.yml does not pass build-args REVISION=${{ github.sha }}")
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
