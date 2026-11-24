package config

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/blackstorm/bsdyak/internal/sys"
)

var freebsdRelRe = regexp.MustCompile(`^[0-9]+\.[0-9]+-(RELEASE|STABLE|CURRENT)$`)

// Validate enforces structural, graph, and safety invariants across Config.
func Validate(cfg *Config) error {
	if strings.TrimSpace(cfg.Project.Name) == "" {
		return fmt.Errorf("project name cannot be empty")
	}
	if !freebsdRelRe.MatchString(cfg.Project.FreeBSD) {
		return fmt.Errorf("invalid project.freebsd %q (expected e.g. 14.2-RELEASE)", cfg.Project.FreeBSD)
	}
	if len(cfg.Services) == 0 {
		return fmt.Errorf("Yakfile must declare at least one `service` block")
	}

	// Validate images & stages.
	for name, img := range cfg.Images {
		stageNames := make(map[string]bool)
		for _, st := range img.Stages {
			if st.On != "local" && st.On != "server" {
				return fmt.Errorf("image %q stage %q has invalid `on` %q (must be \"local\" or \"server\")", name, st.Name, st.On)
			}
			for _, cp := range st.Copy {
				if cp.FromStage != "" && !stageNames[cp.FromStage] {
					return fmt.Errorf("image %q stage %q copies from unknown or later stage %q", name, st.Name, cp.FromStage)
				}
			}
			stageNames[st.Name] = true
		}
		for _, cp := range img.Runtime.Copy {
			if cp.FromStage != "" && !stageNames[cp.FromStage] {
				return fmt.Errorf("image %q runtime copies from unknown stage %q", name, cp.FromStage)
			}
		}
	}

	// Validate services, mounts, jail params, expose uniqueness.
	usedListen := make(map[string]string)
	usedUnix := make(map[string]string)

	for name, svc := range cfg.Services {
		if _, ok := cfg.Images[svc.Image]; !ok {
			return fmt.Errorf("service %q references unknown image %q", name, svc.Image)
		}
		if svc.Strategy != "blue-green" && svc.Strategy != "recreate" {
			return fmt.Errorf("service %q has invalid strategy %q (must be \"blue-green\" or \"recreate\")", name, svc.Strategy)
		}
		if strings.TrimSpace(svc.Cmd) == "" {
			return fmt.Errorf("service %q cmd cannot be empty", name)
		}
		if err := sys.ValidateJailParams(svc.Jail); err != nil {
			return fmt.Errorf("service %q: %w", name, err)
		}
		for _, m := range svc.Mounts {
			if _, ok := cfg.Storage[m.Source]; !ok {
				return fmt.Errorf("service %q mount references unknown storage %q", name, m.Source)
			}
			if !strings.HasPrefix(m.Target, "/") {
				return fmt.Errorf("service %q mount target %q must be an absolute path inside jail", name, m.Target)
			}
		}
		if svc.Healthcheck != nil && svc.Healthcheck.HTTP != "" && svc.Port <= 0 {
			return fmt.Errorf("service %q uses http healthcheck but does not specify `port`", name)
		}
		if svc.Expose != nil {
			if svc.Expose.Listen == "" && svc.Expose.Unix == "" {
				return fmt.Errorf("service %q expose block must specify `listen` or `unix`", name)
			}
			if svc.Expose.Listen != "" && svc.Expose.Unix != "" {
				return fmt.Errorf("service %q expose block cannot specify both `listen` and `unix`", name)
			}
			if svc.Expose.To <= 0 {
				return fmt.Errorf("service %q expose `to` port must be > 0", name)
			}
			if svc.Expose.Listen != "" {
				if prev, dup := usedListen[svc.Expose.Listen]; dup {
					return fmt.Errorf("expose listen %q conflicts between services %q and %q", svc.Expose.Listen, prev, name)
				}
				usedListen[svc.Expose.Listen] = name
			}
			if svc.Expose.Unix != "" {
				if prev, dup := usedUnix[svc.Expose.Unix]; dup {
					return fmt.Errorf("expose unix %q conflicts between services %q and %q", svc.Expose.Unix, prev, name)
				}
				usedUnix[svc.Expose.Unix] = name
			}
		}
	}

	order, err := topoSortServices(cfg.Services)
	if err != nil {
		return err
	}
	cfg.ServiceOrder = order
	return nil
}

func topoSortServices(services map[string]Service) ([]string, error) {
	names := make([]string, 0, len(services))
	for k := range services {
		names = append(names, k)
	}
	sort.Strings(names)

	const (
		unvisited = 0
		visiting  = 1
		visited   = 2
	)
	state := make(map[string]int, len(services))
	var order []string

	var visit func(n string) error
	visit = func(n string) error {
		switch state[n] {
		case visiting:
			return fmt.Errorf("dependency cycle detected involving service %q", n)
		case visited:
			return nil
		}
		state[n] = visiting
		svc := services[n]
		for _, dep := range svc.DependsOn {
			if _, ok := services[dep]; !ok {
				return fmt.Errorf("service %q depends_on unknown service %q", n, dep)
			}
			if err := visit(dep); err != nil {
				return err
			}
		}
		state[n] = visited
		order = append(order, n)
		return nil
	}

	for _, n := range names {
		if err := visit(n); err != nil {
			return nil, err
		}
	}
	return order, nil
}
