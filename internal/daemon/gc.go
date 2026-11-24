package daemon

import (
	"context"
	"log"
	"time"

	"github.com/blackstorm/bsdyak/internal/state"
	"github.com/blackstorm/bsdyak/internal/sys"
)

const defaultKeepReleases = 5

// GCWorker asynchronously cleans up dying VNET jails, old release datasets,
// and unmounted epairs without blocking deploys on TIME_WAIT teardown.
type GCWorker struct {
	store  *state.Store
	runner sys.Runner
	zfs    *sys.ZFS
	jail   *sys.JailManager
	net    *sys.NetManager
}

func NewGCWorker(store *state.Store, runner sys.Runner, zfs *sys.ZFS) *GCWorker {
	return &GCWorker{
		store:  store,
		runner: runner,
		zfs:    zfs,
		jail:   sys.NewJailManager(runner),
		net:    sys.NewNetManager(runner),
	}
}

func (g *GCWorker) RunLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _ = g.SweepOnce(ctx)
		}
	}
}

// SweepOnce processes pending GC tasks once and returns how many tasks finished cleanly.
func (g *GCWorker) SweepOnce(ctx context.Context) (int, error) {
	tasks, err := g.store.ListGCTasks()
	if err != nil {
		return 0, err
	}

	cleaned := 0
	for _, task := range tasks {
		if ctx.Err() != nil {
			return cleaned, ctx.Err()
		}
		if err := g.processTask(ctx, task); err != nil {
			task.Attempts++
			task.LastError = err.Error()
			_ = g.store.UpdateGCTask(task)
			continue
		}
		if err := g.store.DeleteGCTask(task.ID); err != nil {
			log.Printf("yakd gc delete task %s: %v", task.ID, err)
			continue
		}
		cleaned++
	}
	return cleaned, nil
}

func (g *GCWorker) processTask(ctx context.Context, task state.GCTask) error {
	if task.JailName != "" {
		if err := g.jail.Stop(ctx, task.JailName, task.Mounts); err != nil {
			return err
		}
	}
	if task.EpairHost != "" {
		if err := g.net.DestroyInterface(ctx, task.EpairHost); err != nil {
			return err
		}
	}
	if task.Dataset != "" && g.zfs.DatasetExists(ctx, task.Dataset) {
		if err := g.zfs.Destroy(ctx, task.Dataset, true); err != nil {
			return err
		}
	}
	return nil
}

// PruneOldReleases keeps the newest defaultKeepReleases revisions (plus any active revision)
// and enqueues excess release datasets for asynchronous destruction.
func (g *GCWorker) PruneOldReleases(project, service, activeRev string) error {
	rels, err := g.store.ListReleases(project, service)
	if err != nil || len(rels) <= defaultKeepReleases {
		return err
	}

	cutoff := len(rels) - defaultKeepReleases
	for i := 0; i < cutoff; i++ {
		r := rels[i]
		if r.Revision == activeRev {
			continue
		}
		if err := g.store.EnqueueGC(state.GCTask{
			Project: project,
			Service: service,
			Dataset: r.Dataset,
		}); err != nil {
			return err
		}
		_ = g.store.DeleteRelease(project, service, r.Revision)
	}
	return nil
}
