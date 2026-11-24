package protocol

import (
	"time"
)

// FileMeta describes a workspace file for incremental sync diffing.
type FileMeta struct {
	Size   int64  `json:"size"`
	Mode   uint32 `json:"mode"`
	SHA256 string `json:"sha256"`
}

type SyncManifestRequest struct {
	Files map[string]FileMeta `json:"files"`
}

type SyncManifestResponse struct {
	Missing []string `json:"missing"`
	Removed []string `json:"removed"`
}

// DeploySpec carries the validated, target-resolved configuration to yakd.
// It is kept JSON-serializable independently of HCL AST types.
type DeploySpec struct {
	Project           string                 `json:"project"`
	FreeBSD           string                 `json:"freebsd"`
	ReferencedSecrets []string               `json:"referenced_secrets,omitempty"`
	Storage           map[string]StorageSpec `json:"storage,omitempty"`
	Images            map[string]ImageSpec   `json:"images,omitempty"`
	Services          map[string]ServiceSpec `json:"services"`
	ServiceOrder      []string               `json:"service_order"`
}

type StorageSpec struct {
	Name                 string            `json:"name"`
	Size                 string            `json:"size,omitempty"`
	SnapshotBeforeDeploy bool              `json:"snapshot_before_deploy,omitempty"`
	ZFS                  map[string]string `json:"zfs,omitempty"`
}

type ImageSpec struct {
	Name    string      `json:"name"`
	Stages  []StageSpec `json:"stages,omitempty"`
	Runtime RuntimeSpec `json:"runtime"`
}

type StageSpec struct {
	Name         string            `json:"name"`
	On           string            `json:"on"` // "local" | "server"
	Packages     []string          `json:"packages,omitempty"`
	Cache        []string          `json:"cache,omitempty"`
	Workdir      string            `json:"workdir,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	BuildSecrets []string          `json:"build_secrets,omitempty"`
	Copy         []CopySpec        `json:"copy,omitempty"`
	Steps        []string          `json:"steps,omitempty"`
}

type RuntimeSpec struct {
	Packages []string          `json:"packages,omitempty"`
	Setup    []string          `json:"setup,omitempty"`
	Copy     []CopySpec        `json:"copy,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	User     string            `json:"user,omitempty"`
	Workdir  string            `json:"workdir,omitempty"`
}

type CopySpec struct {
	FromStage string `json:"from_stage,omitempty"`
	Src       string `json:"src"`
	Dst       string `json:"dst"`
}

type ServiceSpec struct {
	Name        string            `json:"name"`
	Image       string            `json:"image"`
	DependsOn   []string          `json:"depends_on,omitempty"`
	Strategy    string            `json:"strategy"` // "blue-green" | "recreate"
	StopGrace   string            `json:"stop_grace,omitempty"`
	Jail        map[string]string `json:"jail,omitempty"`
	Mounts      []MountSpec       `json:"mounts,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	InitCmd     string            `json:"init_cmd,omitempty"`
	Migrate     string            `json:"migrate,omitempty"`
	Cmd         string            `json:"cmd"`
	Port        int               `json:"port,omitempty"`
	Healthcheck *HealthcheckSpec  `json:"healthcheck,omitempty"`
	Expose      *ExposeSpec       `json:"expose,omitempty"`
	Resources   *ResourcesSpec    `json:"resources,omitempty"`
	Hook        *HookSpec         `json:"hook,omitempty"`
}

type MountSpec struct {
	Source string `json:"source"`
	Target string `json:"target"`
	Owner  string `json:"owner,omitempty"`
}

type HealthcheckSpec struct {
	HTTP     string `json:"http,omitempty"`
	Cmd      string `json:"cmd,omitempty"`
	Interval string `json:"interval,omitempty"`
	Timeout  string `json:"timeout,omitempty"`
}

type ExposeSpec struct {
	Listen string `json:"listen,omitempty"`
	Unix   string `json:"unix,omitempty"`
	To     int    `json:"to"`
}

type ResourcesSpec struct {
	Memory string `json:"memory,omitempty"`
	CPU    string `json:"cpu,omitempty"`
}

type HookSpec struct {
	OnSwitch string `json:"on_switch,omitempty"`
}

type DeployRequest struct {
	Spec     DeploySpec `json:"spec"`
	Services []string   `json:"services,omitempty"`
	DryRun   bool       `json:"dry_run,omitempty"`
	Release  string     `json:"release,omitempty"` // target revision e.g. "r41" for instant rollback
}

type PlanAction struct {
	Kind   string `json:"kind"`   // storage | network | image | service | snapshot
	Target string `json:"target"` // name
	Action string `json:"action"` // create | update | recreate | blue-green | rollback | unchanged | snapshot
	Detail string `json:"detail,omitempty"`
}

type DeployEvent struct {
	Type      string       `json:"type"` // plan | step | log | done | error
	Timestamp time.Time    `json:"timestamp"`
	Service   string       `json:"service,omitempty"`
	Message   string       `json:"message,omitempty"`
	Plan      []PlanAction `json:"plan,omitempty"`
	Error     string       `json:"error,omitempty"`
}

type DownRequest struct {
	Services []string `json:"services,omitempty"`
}

type DownResponse struct {
	Stopped []string `json:"stopped"`
}

type ServiceStatus struct {
	Project      string    `json:"project"`
	Service      string    `json:"service"`
	JailName     string    `json:"jail_name"`
	Release      string    `json:"release"`
	State        string    `json:"state"` // running | stopped | degraded
	Strategy     string    `json:"strategy"`
	IP           string    `json:"ip"`
	Port         int       `json:"port,omitempty"`
	ExposeListen string    `json:"expose_listen,omitempty"`
	ExposeUnix   string    `json:"expose_unix,omitempty"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type PsResponse struct {
	Project  string          `json:"project"`
	Subnet   string          `json:"subnet,omitempty"`
	Services []ServiceStatus `json:"services"`
}

type EndpointEntry struct {
	Service      string    `json:"service"`
	Release      string    `json:"release"`
	JailName     string    `json:"jail_name"`
	IP           string    `json:"ip"`
	Port         int       `json:"port,omitempty"`
	ExposeListen string    `json:"expose_listen,omitempty"`
	ExposeUnix   string    `json:"expose_unix,omitempty"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type EndpointsFile struct {
	Project   string          `json:"project"`
	Subnet    string          `json:"subnet"`
	UpdatedAt time.Time       `json:"updated_at"`
	Endpoints []EndpointEntry `json:"endpoints"`
}

type SecretSetRequest struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type SecretListResponse struct {
	Keys []string `json:"keys"`
}

type ExecRequest struct {
	Service string   `json:"service"`
	Command []string `json:"command"`
	TTY     bool     `json:"tty,omitempty"`
	Stdin   bool     `json:"stdin,omitempty"`
	Cols    int      `json:"cols,omitempty"`
	Rows    int      `json:"rows,omitempty"`
}

type HealthResponse struct {
	Status  string `json:"status"`
	Version string `json:"version"`
}
