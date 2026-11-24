package builder

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/blackstorm/bsdyak/internal/protocol"
)

// LayerHash computes a deterministic content-addressed key for a package + setup layer.
func LayerHash(freebsd string, packages, setup []string) string {
	pkgs := append([]string(nil), packages...)
	sort.Strings(pkgs)

	h := sha256.New()
	fmt.Fprintf(h, "freebsd=%s\n", freebsd)
	for _, p := range pkgs {
		fmt.Fprintf(h, "pkg=%s\n", p)
	}
	for _, s := range setup {
		fmt.Fprintf(h, "setup=%s\n", s)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// ImageDigest hashes the full image specification to detect when a rebuild is required.
func ImageDigest(freebsd string, img protocol.ImageSpec) string {
	h := sha256.New()
	fmt.Fprintf(h, "freebsd=%s\nimage=%s\n", freebsd, img.Name)
	for _, st := range img.Stages {
		fmt.Fprintf(h, "stage=%s;on=%s;wd=%s\n", st.Name, st.On, st.Workdir)
		pkgs := append([]string(nil), st.Packages...)
		sort.Strings(pkgs)
		fmt.Fprintf(h, "pkgs=%s\n", strings.Join(pkgs, ","))
		for _, step := range st.Steps {
			fmt.Fprintf(h, "step=%s\n", step)
		}
		for _, cp := range st.Copy {
			fmt.Fprintf(h, "copy=%s:%s->%s\n", cp.FromStage, cp.Src, cp.Dst)
		}
	}
	rtPkgs := append([]string(nil), img.Runtime.Packages...)
	sort.Strings(rtPkgs)
	fmt.Fprintf(h, "rt_pkgs=%s;user=%s;wd=%s\n", strings.Join(rtPkgs, ","), img.Runtime.User, img.Runtime.Workdir)
	for _, s := range img.Runtime.Setup {
		fmt.Fprintf(h, "rt_setup=%s\n", s)
	}
	for _, cp := range img.Runtime.Copy {
		fmt.Fprintf(h, "rt_copy=%s:%s->%s\n", cp.FromStage, cp.Src, cp.Dst)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
