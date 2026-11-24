package config

import "github.com/blackstorm/bsdyak/internal/protocol"

// Config represents the fully parsed and target-resolved Yakfile configuration.
type Config struct {
	Project           Project
	Targets           map[string]Target
	SelectedTarget    *Target
	Storage           map[string]Storage
	Images            map[string]Image
	Services          map[string]Service
	ServiceOrder      []string
	ReferencedSecrets []string
}

type Project struct {
	Name    string
	FreeBSD string
}

type Target struct {
	Name    string
	Host    string
	Default bool
	Vars    map[string]string
}

type Storage struct {
	Name                 string
	Size                 string
	SnapshotBeforeDeploy bool
	ZFS                  map[string]string
}

type Image struct {
	Name    string
	Stages  []Stage
	Runtime Runtime
}

type Stage struct {
	Name         string
	On           string // "local" | "server"
	Packages     []string
	Cache        []string
	Workdir      string
	Env          map[string]string
	BuildSecrets []string
	Copy         []CopySpec
	Steps        []string
}

type Runtime struct {
	Packages []string
	Setup    []string
	Copy     []CopySpec
	Env      map[string]string
	User     string
	Workdir  string
}

type CopySpec struct {
	FromStage string
	Src       string
	Dst       string
}

type Service struct {
	Name        string
	Image       string
	DependsOn   []string
	Strategy    string // "blue-green" | "recreate"
	StopGrace   string
	Jail        map[string]string
	Mounts      []Mount
	Env         map[string]string
	InitCmd     string
	Migrate     string
	Cmd         string
	Port        int
	Healthcheck *Healthcheck
	Expose      *Expose
	Resources   *Resources
	Hook        *Hook
}

type Mount struct {
	Source string
	Target string
	Owner  string
}

type Healthcheck struct {
	HTTP     string
	Cmd      string
	Interval string
	Timeout  string
}

type Expose struct {
	Listen string
	Unix   string
	To     int
}

type Resources struct {
	Memory string
	CPU    string
}

type Hook struct {
	OnSwitch string
}

// ToDeploySpec converts validated Config into the wire protocol DeploySpec sent to yakd.
func (c *Config) ToDeploySpec() protocol.DeploySpec {
	spec := protocol.DeploySpec{
		Project:           c.Project.Name,
		FreeBSD:           c.Project.FreeBSD,
		ReferencedSecrets: append([]string(nil), c.ReferencedSecrets...),
		Storage:           make(map[string]protocol.StorageSpec, len(c.Storage)),
		Images:            make(map[string]protocol.ImageSpec, len(c.Images)),
		Services:          make(map[string]protocol.ServiceSpec, len(c.Services)),
		ServiceOrder:      append([]string(nil), c.ServiceOrder...),
	}
	for k, st := range c.Storage {
		spec.Storage[k] = protocol.StorageSpec{
			Name:                 st.Name,
			Size:                 st.Size,
			SnapshotBeforeDeploy: st.SnapshotBeforeDeploy,
			ZFS:                  cloneMap(st.ZFS),
		}
	}
	for k, img := range c.Images {
		stages := make([]protocol.StageSpec, 0, len(img.Stages))
		for _, s := range img.Stages {
			stages = append(stages, protocol.StageSpec{
				Name:         s.Name,
				On:           s.On,
				Packages:     append([]string(nil), s.Packages...),
				Cache:        append([]string(nil), s.Cache...),
				Workdir:      s.Workdir,
				Env:          cloneMap(s.Env),
				BuildSecrets: append([]string(nil), s.BuildSecrets...),
				Copy:         toProtoCopy(s.Copy),
				Steps:        append([]string(nil), s.Steps...),
			})
		}
		spec.Images[k] = protocol.ImageSpec{
			Name:   img.Name,
			Stages: stages,
			Runtime: protocol.RuntimeSpec{
				Packages: append([]string(nil), img.Runtime.Packages...),
				Setup:    append([]string(nil), img.Runtime.Setup...),
				Copy:     toProtoCopy(img.Runtime.Copy),
				Env:      cloneMap(img.Runtime.Env),
				User:     img.Runtime.User,
				Workdir:  img.Runtime.Workdir,
			},
		}
	}
	for k, svc := range c.Services {
		mounts := make([]protocol.MountSpec, 0, len(svc.Mounts))
		for _, m := range svc.Mounts {
			mounts = append(mounts, protocol.MountSpec{
				Source: m.Source,
				Target: m.Target,
				Owner:  m.Owner,
			})
		}
		psvc := protocol.ServiceSpec{
			Name:      svc.Name,
			Image:     svc.Image,
			DependsOn: append([]string(nil), svc.DependsOn...),
			Strategy:  svc.Strategy,
			StopGrace: svc.StopGrace,
			Jail:      cloneMap(svc.Jail),
			Mounts:    mounts,
			Env:       cloneMap(svc.Env),
			InitCmd:   svc.InitCmd,
			Migrate:   svc.Migrate,
			Cmd:       svc.Cmd,
			Port:      svc.Port,
		}
		if svc.Healthcheck != nil {
			psvc.Healthcheck = &protocol.HealthcheckSpec{
				HTTP:     svc.Healthcheck.HTTP,
				Cmd:      svc.Healthcheck.Cmd,
				Interval: svc.Healthcheck.Interval,
				Timeout:  svc.Healthcheck.Timeout,
			}
		}
		if svc.Expose != nil {
			psvc.Expose = &protocol.ExposeSpec{
				Listen: svc.Expose.Listen,
				Unix:   svc.Expose.Unix,
				To:     svc.Expose.To,
			}
		}
		if svc.Resources != nil {
			psvc.Resources = &protocol.ResourcesSpec{
				Memory: svc.Resources.Memory,
				CPU:    svc.Resources.CPU,
			}
		}
		if svc.Hook != nil {
			psvc.Hook = &protocol.HookSpec{
				OnSwitch: svc.Hook.OnSwitch,
			}
		}
		spec.Services[k] = psvc
	}
	return spec
}

func toProtoCopy(in []CopySpec) []protocol.CopySpec {
	out := make([]protocol.CopySpec, 0, len(in))
	for _, c := range in {
		out = append(out, protocol.CopySpec{
			FromStage: c.FromStage,
			Src:       c.Src,
			Dst:       c.Dst,
		})
	}
	return out
}

func cloneMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
