package state

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	bucketProjects = []byte("projects")
	bucketSubnets  = []byte("subnets")
	bucketReleases = []byte("releases")
	bucketSecrets  = []byte("secrets")
	bucketGCQueue  = []byte("gc_queue")
)

type ActiveServiceState struct {
	Service      string            `json:"service"`
	JailName     string            `json:"jail_name"`
	EpairHost    string            `json:"epair_host"`
	EpairJail    string            `json:"epair_jail"`
	Release      string            `json:"release"`
	Strategy     string            `json:"strategy"`
	IP           string            `json:"ip"`
	HostOctet    int               `json:"host_octet"`
	Port         int               `json:"port,omitempty"`
	ExposeListen string            `json:"expose_listen,omitempty"`
	ExposeUnix   string            `json:"expose_unix,omitempty"`
	Dataset      string            `json:"dataset"`
	Mounts       []string          `json:"mounts,omitempty"`
	ConfigDigest string            `json:"config_digest"`
	ImageDigest  string            `json:"image_digest"`
	Env          map[string]string `json:"env,omitempty"`
	Cmd          string            `json:"cmd"`
	User         string            `json:"user,omitempty"`
	Workdir      string            `json:"workdir,omitempty"`
	JailOpts     map[string]string `json:"jail_opts,omitempty"`
	UpdatedAt    time.Time         `json:"updated_at"`
}

type ProjectState struct {
	Name           string                        `json:"name"`
	FreeBSD        string                        `json:"freebsd"`
	SubnetOctet    int                           `json:"subnet_octet"`
	SubnetCIDR     string                        `json:"subnet_cidr"`
	GatewayIP      string                        `json:"gateway_ip"`
	BridgeName     string                        `json:"bridge_name"`
	NextRevision   int                           `json:"next_revision"`
	NextHostOctet  int                           `json:"next_host_octet"`
	ActiveServices map[string]ActiveServiceState `json:"active_services"`
	Volumes        []string                      `json:"volumes,omitempty"`
	UpdatedAt      time.Time                     `json:"updated_at"`
}

type ReleaseRecord struct {
	Project      string    `json:"project"`
	Service      string    `json:"service"`
	Revision     string    `json:"revision"` // e.g. "r42"
	RevNum       int       `json:"rev_num"`
	Dataset      string    `json:"dataset"`
	Snapshot     string    `json:"snapshot"`
	ImageDigest  string    `json:"image_digest"`
	ConfigDigest string    `json:"config_digest"`
	CreatedAt    time.Time `json:"created_at"`
}

type GCTask struct {
	ID         string    `json:"id"`
	Project    string    `json:"project"`
	Service    string    `json:"service"`
	JailName   string    `json:"jail_name,omitempty"`
	EpairHost  string    `json:"epair_host,omitempty"`
	Dataset    string    `json:"dataset,omitempty"`
	Mounts     []string  `json:"mounts,omitempty"`
	Attempts   int       `json:"attempts"`
	EnqueuedAt time.Time `json:"enqueued_at"`
	LastError  string    `json:"last_error,omitempty"`
}

// Store persists yakd metadata in an ACID bbolt database and coordinates per-project locks.
type Store struct {
	db      *bolt.DB
	locksMu sync.Mutex
	locks   map[string]chan struct{}
}

// Open opens (or initializes) the bbolt state database with strict 0700/0600 permissions.
func Open(path string) (*Store, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create state dir %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("chmod state dir %s: %w", dir, err)
	}

	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 3 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open bbolt db %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("chmod state db %s: %w", path, err)
	}

	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{bucketProjects, bucketSubnets, bucketReleases, bucketSecrets, bucketGCQueue} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init buckets: %w", err)
	}

	return &Store{
		db:    db,
		locks: make(map[string]chan struct{}),
	}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

// LockProject acquires an exclusive per-project lock, respecting ctx cancellation.
func (s *Store) LockProject(ctx context.Context, project string) (func(), error) {
	s.locksMu.Lock()
	ch, ok := s.locks[project]
	if !ok {
		ch = make(chan struct{}, 1)
		s.locks[project] = ch
	}
	s.locksMu.Unlock()

	select {
	case ch <- struct{}{}:
		var once sync.Once
		return func() {
			once.Do(func() {
				<-ch
			})
		}, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("waiting for project %q deploy lock: %w", project, ctx.Err())
	}
}

// EnsureProject loads an existing ProjectState or allocates an isolated /24 subnet and bridge.
func (s *Store) EnsureProject(name, freebsd string) (*ProjectState, error) {
	var out *ProjectState
	err := s.db.Update(func(tx *bolt.Tx) error {
		pb := tx.Bucket(bucketProjects)
		sb := tx.Bucket(bucketSubnets)
		if raw := pb.Get([]byte(name)); raw != nil {
			var ps ProjectState
			if err := json.Unmarshal(raw, &ps); err != nil {
				return err
			}
			if freebsd != "" && ps.FreeBSD != freebsd {
				ps.FreeBSD = freebsd
			}
			if ps.ActiveServices == nil {
				ps.ActiveServices = make(map[string]ActiveServiceState)
			}
			ps.UpdatedAt = time.Now().UTC()
			encoded, err := json.Marshal(&ps)
			if err != nil {
				return err
			}
			if err := pb.Put([]byte(name), encoded); err != nil {
				return err
			}
			out = &ps
			return nil
		}

		// Allocate subnet octet in 1..254
		octet := 0
		for cand := 1; cand <= 254; cand++ {
			cidr := fmt.Sprintf("10.88.%d.0/24", cand)
			if sb.Get([]byte(cidr)) == nil {
				octet = cand
				break
			}
		}
		if octet == 0 {
			return fmt.Errorf("no free /24 subnets available in 10.88.0.0/16")
		}

		cidr := fmt.Sprintf("10.88.%d.0/24", octet)
		if err := sb.Put([]byte(cidr), []byte(name)); err != nil {
			return err
		}

		ps := ProjectState{
			Name:           name,
			FreeBSD:        freebsd,
			SubnetOctet:    octet,
			SubnetCIDR:     cidr,
			GatewayIP:      fmt.Sprintf("10.88.%d.1", octet),
			BridgeName:     fmt.Sprintf("ykb_%d", octet),
			NextRevision:   1,
			NextHostOctet:  10,
			ActiveServices: make(map[string]ActiveServiceState),
			UpdatedAt:      time.Now().UTC(),
		}
		encoded, err := json.Marshal(&ps)
		if err != nil {
			return err
		}
		if err := pb.Put([]byte(name), encoded); err != nil {
			return err
		}
		out = &ps
		return nil
	})
	return out, err
}

func (s *Store) GetProject(name string) (*ProjectState, error) {
	var out *ProjectState
	err := s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(bucketProjects).Get([]byte(name))
		if raw == nil {
			return nil
		}
		var ps ProjectState
		if err := json.Unmarshal(raw, &ps); err != nil {
			return err
		}
		if ps.ActiveServices == nil {
			ps.ActiveServices = make(map[string]ActiveServiceState)
		}
		out = &ps
		return nil
	})
	return out, err
}

func (s *Store) SaveProject(ps *ProjectState) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		ps.UpdatedAt = time.Now().UTC()
		encoded, err := json.Marshal(ps)
		if err != nil {
			return err
		}
		return tx.Bucket(bucketProjects).Put([]byte(ps.Name), encoded)
	})
}

func (s *Store) SetActiveService(project string, svc ActiveServiceState) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		pb := tx.Bucket(bucketProjects)
		raw := pb.Get([]byte(project))
		if raw == nil {
			return fmt.Errorf("project %q not found", project)
		}
		var ps ProjectState
		if err := json.Unmarshal(raw, &ps); err != nil {
			return err
		}
		if ps.ActiveServices == nil {
			ps.ActiveServices = make(map[string]ActiveServiceState)
		}
		ps.ActiveServices[svc.Service] = svc
		ps.UpdatedAt = time.Now().UTC()
		encoded, err := json.Marshal(&ps)
		if err != nil {
			return err
		}
		return pb.Put([]byte(project), encoded)
	})
}

func (s *Store) RemoveActiveService(project, service string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		pb := tx.Bucket(bucketProjects)
		raw := pb.Get([]byte(project))
		if raw == nil {
			return nil
		}
		var ps ProjectState
		if err := json.Unmarshal(raw, &ps); err != nil {
			return err
		}
		delete(ps.ActiveServices, service)
		ps.UpdatedAt = time.Now().UTC()
		encoded, err := json.Marshal(&ps)
		if err != nil {
			return err
		}
		return pb.Put([]byte(project), encoded)
	})
}

func (s *Store) ListProjects() ([]*ProjectState, error) {
	var list []*ProjectState
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketProjects).ForEach(func(k, v []byte) error {
			var ps ProjectState
			if err := json.Unmarshal(v, &ps); err != nil {
				return err
			}
			if ps.ActiveServices == nil {
				ps.ActiveServices = make(map[string]ActiveServiceState)
			}
			list = append(list, &ps)
			return nil
		})
	})
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list, err
}

// AllocateIP returns the next usable host IP in the project's /24 subnet.
// Octets 10..250 are cycled while skipping IPs actively assigned to running services.
func (s *Store) AllocateIP(project string) (string, int, error) {
	var ip string
	var octet int
	err := s.db.Update(func(tx *bolt.Tx) error {
		pb := tx.Bucket(bucketProjects)
		raw := pb.Get([]byte(project))
		if raw == nil {
			return fmt.Errorf("project %q not found", project)
		}
		var ps ProjectState
		if err := json.Unmarshal(raw, &ps); err != nil {
			return err
		}

		used := make(map[int]bool, len(ps.ActiveServices))
		for _, svc := range ps.ActiveServices {
			if svc.HostOctet > 0 {
				used[svc.HostOctet] = true
			}
		}

		cand := ps.NextHostOctet
		if cand < 10 || cand > 250 {
			cand = 10
		}
		found := 0
		for i := 0; i < 241; i++ {
			try := 10 + ((cand - 10 + i) % 241)
			if !used[try] {
				found = try
				break
			}
		}
		if found == 0 {
			return fmt.Errorf("no free IPs in project %q subnet %s", project, ps.SubnetCIDR)
		}

		octet = found
		ip = fmt.Sprintf("10.88.%d.%d", ps.SubnetOctet, octet)
		ps.NextHostOctet = 10 + ((octet - 10 + 1) % 241)
		ps.UpdatedAt = time.Now().UTC()
		encoded, err := json.Marshal(&ps)
		if err != nil {
			return err
		}
		return pb.Put([]byte(project), encoded)
	})
	return ip, octet, err
}

// NextRevision atomically increments and returns the next revision tag (e.g., "r1", 1).
func (s *Store) NextRevision(project string) (string, int, error) {
	var revTag string
	var revNum int
	err := s.db.Update(func(tx *bolt.Tx) error {
		pb := tx.Bucket(bucketProjects)
		raw := pb.Get([]byte(project))
		if raw == nil {
			return fmt.Errorf("project %q not found", project)
		}
		var ps ProjectState
		if err := json.Unmarshal(raw, &ps); err != nil {
			return err
		}
		if ps.NextRevision <= 0 {
			ps.NextRevision = 1
		}
		revNum = ps.NextRevision
		revTag = fmt.Sprintf("r%d", revNum)
		ps.NextRevision++
		ps.UpdatedAt = time.Now().UTC()
		encoded, err := json.Marshal(&ps)
		if err != nil {
			return err
		}
		return pb.Put([]byte(project), encoded)
	})
	return revTag, revNum, err
}

func releaseKey(project, service, revision string) []byte {
	return []byte(fmt.Sprintf("%s\x00%s\x00%s", project, service, revision))
}

func (s *Store) SaveRelease(rec ReleaseRecord) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		encoded, err := json.Marshal(&rec)
		if err != nil {
			return err
		}
		return tx.Bucket(bucketReleases).Put(releaseKey(rec.Project, rec.Service, rec.Revision), encoded)
	})
}

func (s *Store) GetRelease(project, service, revision string) (*ReleaseRecord, error) {
	var out *ReleaseRecord
	err := s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(bucketReleases).Get(releaseKey(project, service, revision))
		if raw == nil {
			return nil
		}
		var rec ReleaseRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			return err
		}
		out = &rec
		return nil
	})
	return out, err
}

func (s *Store) ListReleases(project, service string) ([]ReleaseRecord, error) {
	prefix := []byte(fmt.Sprintf("%s\x00%s\x00", project, service))
	var out []ReleaseRecord
	err := s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(bucketReleases).Cursor()
		for k, v := c.Seek(prefix); k != nil && len(k) >= len(prefix) && string(k[:len(prefix)]) == string(prefix); k, v = c.Next() {
			var rec ReleaseRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return err
			}
			out = append(out, rec)
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].RevNum < out[j].RevNum })
	return out, err
}

func (s *Store) DeleteRelease(project, service, revision string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketReleases).Delete(releaseKey(project, service, revision))
	})
}

func (s *Store) EnqueueGC(task GCTask) error {
	if task.ID == "" {
		task.ID = fmt.Sprintf("%s-%s-%d", task.Project, task.Service, time.Now().UnixNano())
	}
	if task.EnqueuedAt.IsZero() {
		task.EnqueuedAt = time.Now().UTC()
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		encoded, err := json.Marshal(&task)
		if err != nil {
			return err
		}
		return tx.Bucket(bucketGCQueue).Put([]byte(task.ID), encoded)
	})
}

func (s *Store) ListGCTasks() ([]GCTask, error) {
	var tasks []GCTask
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketGCQueue).ForEach(func(k, v []byte) error {
			var t GCTask
			if err := json.Unmarshal(v, &t); err != nil {
				return err
			}
			tasks = append(tasks, t)
			return nil
		})
	})
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].EnqueuedAt.Before(tasks[j].EnqueuedAt) })
	return tasks, err
}

func (s *Store) DeleteGCTask(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketGCQueue).Delete([]byte(id))
	})
}

func (s *Store) UpdateGCTask(task GCTask) error {
	return s.EnqueueGC(task)
}
