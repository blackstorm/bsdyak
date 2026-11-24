package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/blackstorm/bsdyak/internal/builder"
	"github.com/blackstorm/bsdyak/internal/dns"
	"github.com/blackstorm/bsdyak/internal/protocol"
	"github.com/blackstorm/bsdyak/internal/proxy"
	"github.com/blackstorm/bsdyak/internal/state"
	"github.com/blackstorm/bsdyak/internal/sys"
)

type HealthProbeFunc func(ctx context.Context, jailName, jailIP string, svc protocol.ServiceSpec) error

type Reconciler struct {
	cfg     sys.ServerConfig
	store   *state.Store
	runner  sys.Runner
	zfs     *sys.ZFS
	jail    *sys.JailManager
	net     *sys.NetManager
	rctl    *sys.RctlManager
	builder *builder.ServerBuilder
	expose  *proxy.ExposeManager
	logs    *LogManager
	gc      *GCWorker

	// DNSBindAddrOverride allows unit tests on non-FreeBSD hosts to bind "127.0.0.1:0".
	DNSBindAddrOverride string
	// HealthProbe can be overridden in tests; defaults to real HTTP / jexec probing.
	HealthProbe HealthProbeFunc

	dnsMu      sync.Mutex
	dnsServers map[string]*dns.Server
}

func NewReconciler(
	cfg sys.ServerConfig,
	store *state.Store,
	runner sys.Runner,
	expose *proxy.ExposeManager,
	logs *LogManager,
	gc *GCWorker,
) *Reconciler {
	zfs := sys.NewZFS(runner, cfg.ZFSRoot)
	r := &Reconciler{
		cfg:        cfg,
		store:      store,
		runner:     runner,
		zfs:        zfs,
		jail:       sys.NewJailManager(runner),
		net:        sys.NewNetManager(runner),
		rctl:       sys.NewRctlManager(runner),
		builder:    builder.NewServerBuilder(runner, zfs),
		expose:     expose,
		logs:       logs,
		gc:         gc,
		dnsServers: make(map[string]*dns.Server),
	}
	r.HealthProbe = r.defaultHealthProbe
	return r
}

func (r *Reconciler) Close() {
	r.dnsMu.Lock()
	defer r.dnsMu.Unlock()
	for k, s := range r.dnsServers {
		_ = s.Close()
		delete(r.dnsServers, k)
	}
}

func (r *Reconciler) SourceDir(project string) string {
	return filepath.Join(r.cfg.StateDir, "projects", project, "source")
}

func (r *Reconciler) ArtifactsDir(project string) string {
	return filepath.Join(r.cfg.StateDir, "projects", project, "artifacts")
}

func (r *Reconciler) ensureDNS(ps *state.ProjectState) (*dns.Server, error) {
	r.dnsMu.Lock()
	defer r.dnsMu.Unlock()
	if s, ok := r.dnsServers[ps.Name]; ok {
		return s, nil
	}
	addr := ps.GatewayIP + ":53"
	if r.DNSBindAddrOverride != "" {
		addr = r.DNSBindAddrOverride
	}
	srv, err := dns.StartServer(addr, ps.Name, "")
	if err != nil {
		return nil, err
	}
	for svcName, svc := range ps.ActiveServices {
		if svc.IP != "" {
			_ = srv.SetRecord(svcName, svc.IP)
		}
	}
	r.dnsServers[ps.Name] = srv
	return srv, nil
}

func (r *Reconciler) Deploy(ctx context.Context, req protocol.DeployRequest, emit func(protocol.DeployEvent)) error {
	spec := req.Spec
	if spec.Project == "" {
		return fmt.Errorf("project name is required")
	}

	unlock, err := r.store.LockProject(ctx, spec.Project)
	if err != nil {
		return err
	}
	defer unlock()

	// 1. Verify all referenced secrets exist BEFORE mutating anything.
	resolvedSecrets, err := r.store.ResolveSecrets(spec.Project, spec.ReferencedSecrets)
	if err != nil {
		return err
	}

	// 2. Check ZFS pool capacity guardrail (< 85%).
	if !req.DryRun {
		if err := r.zfs.CheckCapacityGuard(ctx); err != nil {
			return err
		}
	}

	ps, err := r.store.EnsureProject(spec.Project, spec.FreeBSD)
	if err != nil {
		return err
	}

	targetFilter := make(map[string]bool)
	for _, s := range req.Services {
		targetFilter[s] = true
	}

	sourceDigest := dirContentDigest(r.SourceDir(spec.Project), r.ArtifactsDir(spec.Project))

	plan := r.buildPlan(ctx, ps, req, resolvedSecrets, sourceDigest, targetFilter)
	emit(protocol.DeployEvent{
		Type:      "plan",
		Timestamp: time.Now().UTC(),
		Plan:      plan,
	})
	if req.DryRun {
		emit(protocol.DeployEvent{
			Type:      "done",
			Timestamp: time.Now().UTC(),
			Message:   "dry-run complete (no changes applied)",
		})
		return nil
	}

	// 3. Provision project network, PF anchor, and embedded DNS.
	emit(protocol.DeployEvent{Type: "step", Timestamp: time.Now().UTC(), Message: fmt.Sprintf("reconciling VNET bridge %s (%s)", ps.BridgeName, ps.SubnetCIDR)})
	if err := r.net.EnsureBridge(ctx, ps.BridgeName, ps.GatewayIP+"/24"); err != nil {
		return fmt.Errorf("ensure project bridge: %w", err)
	}
	allProjects, _ := r.store.ListProjects()
	var otherSubnets []string
	for _, op := range allProjects {
		if op.Name != ps.Name && op.SubnetCIDR != "" {
			otherSubnets = append(otherSubnets, op.SubnetCIDR)
		}
	}
	if err := r.net.ApplyPFAnchor(ctx, ps.Name, ps.SubnetCIDR, r.cfg.Egress, otherSubnets); err != nil {
		return fmt.Errorf("apply pf anchor: %w", err)
	}
	dnsSrv, err := r.ensureDNS(ps)
	if err != nil {
		return fmt.Errorf("start embedded DNS: %w", err)
	}

	// 4. Reconcile ZFS storage volumes & pre-deploy snapshots.
	volumePaths := make(map[string]string, len(spec.Storage))
	for _, stName := range sortedStorageNames(spec.Storage) {
		st := spec.Storage[stName]
		ds := fmt.Sprintf("%s/projects/%s/volumes/%s", r.zfs.Root(), spec.Project, st.Name)
		if err := r.zfs.EnsureDataset(ctx, ds, st.ZFS); err != nil {
			return fmt.Errorf("storage %s: %w", st.Name, err)
		}
		if st.Size != "" {
			if err := r.zfs.SetQuota(ctx, ds, st.Size); err != nil {
				return fmt.Errorf("storage %s quota: %w", st.Name, err)
			}
		}
		mp, err := r.zfs.Mountpoint(ctx, ds)
		if err != nil {
			return fmt.Errorf("storage %s mountpoint: %w", st.Name, err)
		}
		volumePaths[st.Name] = mp

		if st.SnapshotBeforeDeploy {
			snapName := fmt.Sprintf("pre-deploy-%d", time.Now().UTC().UnixNano())
			if _, err := r.zfs.Snapshot(ctx, ds, snapName); err != nil {
				return fmt.Errorf("pre-deploy snapshot %s: %w", st.Name, err)
			}
			emit(protocol.DeployEvent{Type: "log", Timestamp: time.Now().UTC(), Message: fmt.Sprintf("snapshot %s@%s created", ds, snapName)})
		}
	}

	// 5. Reconcile services in topological dependency order.
	for _, svcName := range spec.ServiceOrder {
		if len(targetFilter) > 0 && !targetFilter[svcName] {
			continue
		}
		svc := spec.Services[svcName]
		img, ok := spec.Images[svc.Image]
		if !ok {
			return fmt.Errorf("service %q references missing image %q", svcName, svc.Image)
		}

		if err := r.reconcileService(ctx, ps, spec, svc, img, resolvedSecrets, volumePaths, sourceDigest, req.Release, dnsSrv, emit); err != nil {
			return err
		}
	}

	if err := r.writeEndpointsFile(spec.Project); err != nil {
		return err
	}
	emit(protocol.DeployEvent{
		Type:      "done",
		Timestamp: time.Now().UTC(),
		Message:   fmt.Sprintf("project %q converged successfully", spec.Project),
	})
	return nil
}

func (r *Reconciler) buildPlan(
	ctx context.Context,
	ps *state.ProjectState,
	req protocol.DeployRequest,
	secrets map[string]string,
	sourceDigest string,
	filter map[string]bool,
) []protocol.PlanAction {
	var plan []protocol.PlanAction
	spec := req.Spec

	for _, stName := range sortedStorageNames(spec.Storage) {
		st := spec.Storage[stName]
		ds := fmt.Sprintf("%s/projects/%s/volumes/%s", r.zfs.Root(), spec.Project, st.Name)
		act := "unchanged"
		if !r.zfs.DatasetExists(ctx, ds) {
			act = "create"
		}
		plan = append(plan, protocol.PlanAction{Kind: "storage", Target: st.Name, Action: act})
		if st.SnapshotBeforeDeploy {
			plan = append(plan, protocol.PlanAction{Kind: "snapshot", Target: st.Name, Action: "snapshot", Detail: "pre-deploy snapshot"})
		}
	}

	for _, svcName := range spec.ServiceOrder {
		if len(filter) > 0 && !filter[svcName] {
			continue
		}
		svc := spec.Services[svcName]
		img := spec.Images[svc.Image]
		imgDigest := serviceImageDigest(spec.FreeBSD, img, sourceDigest)
		cfgDigest := serviceConfigDigest(svc, imgDigest, secrets)

		if req.Release != "" {
			plan = append(plan, protocol.PlanAction{
				Kind:   "service",
				Target: svcName,
				Action: "rollback",
				Detail: "rollback to " + req.Release,
			})
			continue
		}

		cur, exists := ps.ActiveServices[svcName]
		if !exists {
			plan = append(plan, protocol.PlanAction{Kind: "service", Target: svcName, Action: "create", Detail: svc.Strategy})
			continue
		}
		if cur.ConfigDigest == cfgDigest && cur.ImageDigest == imgDigest {
			plan = append(plan, protocol.PlanAction{Kind: "service", Target: svcName, Action: "unchanged", Detail: cur.Release})
			continue
		}
		plan = append(plan, protocol.PlanAction{Kind: "service", Target: svcName, Action: svc.Strategy, Detail: "from " + cur.Release})
	}
	return plan
}

func (r *Reconciler) reconcileService(
	ctx context.Context,
	ps *state.ProjectState,
	spec protocol.DeploySpec,
	svc protocol.ServiceSpec,
	img protocol.ImageSpec,
	secrets map[string]string,
	volumePaths map[string]string,
	sourceDigest string,
	targetRelease string,
	dnsSrv *dns.Server,
	emit func(protocol.DeployEvent),
) error {
	imgDigest := serviceImageDigest(spec.FreeBSD, img, sourceDigest)
	cfgDigest := serviceConfigDigest(svc, imgDigest, secrets)

	prev, hasPrev := ps.ActiveServices[svc.Name]
	if targetRelease == "" && hasPrev && prev.ConfigDigest == cfgDigest && prev.ImageDigest == imgDigest {
		running, _ := r.jail.IsRunning(ctx, prev.JailName)
		if running {
			emit(protocol.DeployEvent{Type: "log", Timestamp: time.Now().UTC(), Service: svc.Name, Message: fmt.Sprintf("%s unchanged (%s), keeping running", svc.Name, prev.Release)})
			return nil
		}
	}

	var releaseTag string
	var releaseDS string
	var releaseSnap string
	var jailSeqRev string

	if targetRelease != "" {
		rec, err := r.store.GetRelease(spec.Project, svc.Name, targetRelease)
		if err != nil {
			return err
		}
		if rec == nil {
			return fmt.Errorf("release %q not found for %s/%s", targetRelease, spec.Project, svc.Name)
		}
		seqRev, _, err := r.store.NextRevision(spec.Project)
		if err != nil {
			return err
		}
		releaseTag = rec.Revision
		releaseSnap = rec.Snapshot
		releaseDS = rec.Dataset
		jailSeqRev = seqRev
		emit(protocol.DeployEvent{Type: "step", Timestamp: time.Now().UTC(), Service: svc.Name, Message: fmt.Sprintf("rolling back %s to %s", svc.Name, releaseTag)})
	} else {
		revTag, revNum, err := r.store.NextRevision(spec.Project)
		if err != nil {
			return err
		}
		releaseTag = revTag
		jailSeqRev = revTag
		emit(protocol.DeployEvent{Type: "step", Timestamp: time.Now().UTC(), Service: svc.Name, Message: fmt.Sprintf("building %s (%s)", svc.Name, releaseTag)})

		var logBuf bytes.Buffer
		ds, snap, err := r.builder.BuildReleaseImage(
			ctx, spec.Project, svc.Name, releaseTag, spec.FreeBSD, img,
			r.SourceDir(spec.Project), r.ArtifactsDir(spec.Project), secrets, &logBuf,
		)
		if logBuf.Len() > 0 {
			for _, line := range strings.Split(strings.TrimSpace(logBuf.String()), "\n") {
				_ = r.logs.AppendLine(spec.Project, svc.Name, line)
				emit(protocol.DeployEvent{Type: "log", Timestamp: time.Now().UTC(), Service: svc.Name, Message: line})
			}
		}
		if err != nil {
			return fmt.Errorf("build %s: %w", svc.Name, err)
		}
		releaseDS = ds
		releaseSnap = snap

		if err := r.store.SaveRelease(state.ReleaseRecord{
			Project:      spec.Project,
			Service:      svc.Name,
			Revision:     releaseTag,
			RevNum:       revNum,
			Dataset:      releaseDS,
			Snapshot:     releaseSnap,
			ImageDigest:  imgDigest,
			ConfigDigest: cfgDigest,
			CreatedAt:    time.Now().UTC(),
		}); err != nil {
			return err
		}
	}

	// For recreate strategy, gracefully stop old jail BEFORE mounting shared stateful volumes and starting new one.
	if svc.Strategy == "recreate" && hasPrev {
		emit(protocol.DeployEvent{Type: "step", Timestamp: time.Now().UTC(), Service: svc.Name, Message: fmt.Sprintf("stopping previous %s (%s) [recreate]", svc.Name, prev.JailName)})
		r.stopAndQueueGC(ctx, spec.Project, prev)
		delete(ps.ActiveServices, svc.Name)
		_ = r.store.RemoveActiveService(spec.Project, svc.Name)
	}

	// Clone active jail rootfs from immutable release snapshot.
	jailName := fmt.Sprintf("yk_%s_%s_%s", sanitizeIdent(spec.Project), sanitizeIdent(svc.Name), jailSeqRev)
	activeDS := fmt.Sprintf("%s/projects/%s/jails/%s", r.zfs.Root(), spec.Project, jailName)
	if err := r.zfs.Clone(ctx, releaseSnap, activeDS); err != nil {
		return fmt.Errorf("clone release snapshot %s: %w", releaseSnap, err)
	}
	rootPath, err := r.zfs.Mountpoint(ctx, activeDS)
	if err != nil {
		return err
	}

	// Resolve runtime env (interpolating ${secret:<key>} purely in memory; never saved to ZFS).
	resolvedEnv, svcSecrets := splitEnvAndSecrets(svc.Env, img.Runtime.Env, secrets)

	// Optional migration command before launching / switching traffic.
	if strings.TrimSpace(svc.Migrate) != "" && targetRelease == "" {
		emit(protocol.DeployEvent{Type: "step", Timestamp: time.Now().UTC(), Service: svc.Name, Message: fmt.Sprintf("running migrate for %s", svc.Name)})
		if err := r.runMigration(ctx, jailName+"_mig", rootPath, ps, svc, resolvedEnv, svcSecrets); err != nil {
			_ = r.zfs.Destroy(context.Background(), activeDS, true)
			return fmt.Errorf("migrate %s: %w", svc.Name, err)
		}
	}

	ip, octet, err := r.store.AllocateIP(spec.Project)
	if err != nil {
		_ = r.zfs.Destroy(context.Background(), activeDS, true)
		return err
	}
	hostIf, jailIf, err := r.net.CreateEpair(ctx, ps.BridgeName, fmt.Sprintf("%s:%s:%s", spec.Project, svc.Name, releaseTag))
	if err != nil {
		_ = r.zfs.Destroy(context.Background(), activeDS, true)
		return err
	}

	var mounts []sys.JailMount
	for _, m := range svc.Mounts {
		hp, ok := volumePaths[m.Source]
		if !ok {
			_ = r.net.DestroyInterface(context.Background(), hostIf)
			_ = r.zfs.Destroy(context.Background(), activeDS, true)
			return fmt.Errorf("storage %q path not found", m.Source)
		}
		mounts = append(mounts, sys.JailMount{
			HostPath:   hp,
			TargetPath: m.Target,
			Owner:      m.Owner,
		})
	}

	mountedPaths, err := r.jail.Start(ctx, sys.JailStartOpts{
		Name:      jailName,
		RootPath:  rootPath,
		Hostname:  svc.Name,
		EpairJail: jailIf,
		IPCIDR:    ip + "/24",
		GatewayIP: ps.GatewayIP,
		DNSServer: ps.GatewayIP,
		Params:    svc.Jail,
		Mounts:    mounts,
		Env:       resolvedEnv,
		Secrets:   svcSecrets,
		User:      img.Runtime.User,
		Workdir:   img.Runtime.Workdir,
		InitCmd:   svc.InitCmd,
		Cmd:       svc.Cmd,
	})
	if err != nil {
		_ = r.net.DestroyInterface(context.Background(), hostIf)
		_ = r.zfs.Destroy(context.Background(), activeDS, true)
		return fmt.Errorf("start jail %s: %w", jailName, err)
	}
	_ = r.logs.AppendLine(spec.Project, svc.Name, fmt.Sprintf("started %s (%s) at %s", jailName, releaseTag, ip))

	if svc.Resources != nil {
		_ = r.rctl.ApplyLimits(ctx, jailName, svc.Resources.Memory, svc.Resources.CPU)
	}

	// Wait for healthcheck before switching traffic.
	if svc.Healthcheck != nil {
		emit(protocol.DeployEvent{Type: "step", Timestamp: time.Now().UTC(), Service: svc.Name, Message: fmt.Sprintf("waiting for healthcheck on %s (%s)", svc.Name, releaseTag)})
		if err := r.waitHealth(ctx, jailName, ip, svc); err != nil {
			_ = r.logs.AppendLine(spec.Project, svc.Name, fmt.Sprintf("healthcheck failed for %s: %v", jailName, err))
			_ = r.rctl.ClearLimits(context.Background(), jailName)
			r.stopAndQueueGC(context.Background(), spec.Project, state.ActiveServiceState{
				Service:   svc.Name,
				JailName:  jailName,
				EpairHost: hostIf,
				Dataset:   activeDS,
				Mounts:    mountedPaths,
			})
			return fmt.Errorf("healthcheck failed for %s (%s): %w", svc.Name, releaseTag, err)
		}
	}

	// Atomically cut over L4 expose forwarder + DNS.
	var exposeListen, exposeUnix string
	if svc.Expose != nil {
		exposeListen = svc.Expose.Listen
		exposeUnix = svc.Expose.Unix
		targetAddr := fmt.Sprintf("%s:%d", ip, svc.Expose.To)
		actualAddr, err := r.expose.Ensure(spec.Project+"/"+svc.Name, exposeListen, exposeUnix, targetAddr, parseGrace(svc.StopGrace))
		if err != nil {
			return fmt.Errorf("expose %s: %w", svc.Name, err)
		}
		if exposeListen != "" {
			exposeListen = actualAddr
		}
		emit(protocol.DeployEvent{Type: "log", Timestamp: time.Now().UTC(), Service: svc.Name, Message: fmt.Sprintf("switched expose %s -> %s", actualAddr, targetAddr)})
	}

	_ = dnsSrv.SetRecord(svc.Name, ip)

	newState := state.ActiveServiceState{
		Service:      svc.Name,
		JailName:     jailName,
		EpairHost:    hostIf,
		EpairJail:    jailIf,
		Release:      releaseTag,
		Strategy:     svc.Strategy,
		IP:           ip,
		HostOctet:    octet,
		Port:         svc.Port,
		ExposeListen: exposeListen,
		ExposeUnix:   exposeUnix,
		Dataset:      activeDS,
		Mounts:       mountedPaths,
		ConfigDigest: cfgDigest,
		ImageDigest:  imgDigest,
		Env:          resolvedEnv,
		Cmd:          svc.Cmd,
		User:         img.Runtime.User,
		Workdir:      img.Runtime.Workdir,
		JailOpts:     svc.Jail,
		UpdatedAt:    time.Now().UTC(),
	}
	ps.ActiveServices[svc.Name] = newState
	if err := r.store.SetActiveService(spec.Project, newState); err != nil {
		return err
	}

	if svc.Hook != nil && strings.TrimSpace(svc.Hook.OnSwitch) != "" {
		_, _ = r.runner.Run(ctx, "/bin/sh", "-ec", svc.Hook.OnSwitch)
	}

	// For blue-green, drain & stop previous jail AFTER atomic cutover.
	if svc.Strategy == "blue-green" && hasPrev && prev.JailName != jailName {
		emit(protocol.DeployEvent{Type: "step", Timestamp: time.Now().UTC(), Service: svc.Name, Message: fmt.Sprintf("draining & stopping old %s (%s)", svc.Name, prev.JailName)})
		_ = r.rctl.ClearLimits(context.Background(), prev.JailName)
		r.stopAndQueueGC(ctx, spec.Project, prev)
	}

	_ = r.gc.PruneOldReleases(spec.Project, svc.Name, releaseTag)
	return nil
}

func (r *Reconciler) runMigration(
	ctx context.Context,
	migJailName, rootPath string,
	ps *state.ProjectState,
	svc protocol.ServiceSpec,
	env, secrets map[string]string,
) error {
	ip, _, err := r.store.AllocateIP(ps.Name)
	if err != nil {
		return err
	}
	hostIf, jailIf, err := r.net.CreateEpair(ctx, ps.BridgeName, ps.Name+":"+svc.Name+":migrate")
	if err != nil {
		return err
	}
	defer func() {
		_ = r.net.DestroyInterface(context.Background(), hostIf)
	}()

	mounts, err := r.jail.Start(ctx, sys.JailStartOpts{
		Name:      migJailName,
		RootPath:  rootPath,
		Hostname:  svc.Name + "-migrate",
		EpairJail: jailIf,
		IPCIDR:    ip + "/24",
		GatewayIP: ps.GatewayIP,
		DNSServer: ps.GatewayIP,
		Params:    svc.Jail,
		Env:       env,
		Secrets:   secrets,
		InitCmd:   svc.Migrate,
	})
	_ = r.jail.Stop(context.Background(), migJailName, mounts)
	return err
}

func (r *Reconciler) stopAndQueueGC(ctx context.Context, project string, svc state.ActiveServiceState) {
	err := r.jail.Stop(ctx, svc.JailName, svc.Mounts)
	if err == nil && svc.EpairHost != "" {
		err = r.net.DestroyInterface(ctx, svc.EpairHost)
	}
	if err == nil && svc.Dataset != "" {
		err = r.zfs.Destroy(ctx, svc.Dataset, true)
	}
	if err != nil {
		_ = r.store.EnqueueGC(state.GCTask{
			Project:   project,
			Service:   svc.Service,
			JailName:  svc.JailName,
			EpairHost: svc.EpairHost,
			Dataset:   svc.Dataset,
			Mounts:    svc.Mounts,
			LastError: err.Error(),
		})
	}
}

func (r *Reconciler) waitHealth(ctx context.Context, jailName, jailIP string, svc protocol.ServiceSpec) error {
	interval := parseDurationOr(svc.Healthcheck.Interval, 1*time.Second)
	timeout := parseDurationOr(svc.Healthcheck.Timeout, 15*time.Second)
	deadline := time.Now().Add(timeout)

	var lastErr error
	for {
		if err := r.HealthProbe(ctx, jailName, jailIP, svc); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout after %s: %w", timeout, lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

func (r *Reconciler) defaultHealthProbe(ctx context.Context, jailName, jailIP string, svc protocol.ServiceSpec) error {
	if svc.Healthcheck == nil {
		return nil
	}
	if svc.Healthcheck.Cmd != "" {
		_, err := r.jail.Exec(ctx, jailName, []string{"/bin/sh", "-ec", svc.Healthcheck.Cmd})
		return err
	}
	if svc.Healthcheck.HTTP != "" {
		url := fmt.Sprintf("http://%s:%d%s", jailIP, svc.Port, svc.Healthcheck.HTTP)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		client := &http.Client{Timeout: 2 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 400 {
			return fmt.Errorf("http healthcheck %s returned status %d", url, resp.StatusCode)
		}
	}
	return nil
}

func (r *Reconciler) Down(ctx context.Context, project string, services []string) ([]string, error) {
	unlock, err := r.store.LockProject(ctx, project)
	if err != nil {
		return nil, err
	}
	defer unlock()

	ps, err := r.store.GetProject(project)
	if err != nil || ps == nil {
		return nil, err
	}

	target := make(map[string]bool)
	for _, s := range services {
		target[s] = true
	}

	r.dnsMu.Lock()
	dnsSrv := r.dnsServers[project]
	r.dnsMu.Unlock()

	var stopped []string
	for name, active := range ps.ActiveServices {
		if len(target) > 0 && !target[name] {
			continue
		}
		_ = r.expose.Remove(project+"/"+name, parseGrace("5s"))
		if dnsSrv != nil {
			dnsSrv.RemoveRecord(name)
		}
		_ = r.rctl.ClearLimits(ctx, active.JailName)
		r.stopAndQueueGC(ctx, project, active)
		delete(ps.ActiveServices, name)
		_ = r.store.RemoveActiveService(project, name)
		stopped = append(stopped, name)
	}
	sort.Strings(stopped)

	if len(ps.ActiveServices) == 0 && len(target) == 0 {
		if dnsSrv != nil {
			r.dnsMu.Lock()
			_ = dnsSrv.Close()
			delete(r.dnsServers, project)
			r.dnsMu.Unlock()
		}
		_ = r.net.FlushPFAnchor(ctx, project)
		_ = r.net.DestroyInterface(ctx, ps.BridgeName)
	}
	_ = r.writeEndpointsFile(project)
	return stopped, nil
}

func (r *Reconciler) RecoverOnBoot(ctx context.Context) error {
	projects, err := r.store.ListProjects()
	if err != nil {
		return err
	}
	var allSubnets []string
	for _, p := range projects {
		if p.SubnetCIDR != "" {
			allSubnets = append(allSubnets, p.SubnetCIDR)
		}
	}
	for _, ps := range projects {
		if len(ps.ActiveServices) == 0 {
			continue
		}
		_ = r.net.EnsureBridge(ctx, ps.BridgeName, ps.GatewayIP+"/24")
		_ = r.net.ApplyPFAnchor(ctx, ps.Name, ps.SubnetCIDR, r.cfg.Egress, allSubnets)
		dnsSrv, err := r.ensureDNS(ps)
		if err != nil {
			continue
		}
		for svcName, svc := range ps.ActiveServices {
			_ = dnsSrv.SetRecord(svcName, svc.IP)
			if svc.ExposeListen != "" || svc.ExposeUnix != "" {
				targetAddr := fmt.Sprintf("%s:%d", svc.IP, svc.Port)
				_, _ = r.expose.Ensure(ps.Name+"/"+svcName, svc.ExposeListen, svc.ExposeUnix, targetAddr, 10*time.Second)
			}
		}
		_ = r.writeEndpointsFile(ps.Name)
	}
	return nil
}

func (r *Reconciler) writeEndpointsFile(project string) error {
	ps, err := r.store.GetProject(project)
	if err != nil || ps == nil {
		return err
	}
	dir := filepath.Join(r.cfg.RunDir, "projects", ps.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var entries []protocol.EndpointEntry
	for _, name := range sortedActiveNames(ps.ActiveServices) {
		svc := ps.ActiveServices[name]
		entries = append(entries, protocol.EndpointEntry{
			Service:      svc.Service,
			Release:      svc.Release,
			JailName:     svc.JailName,
			IP:           svc.IP,
			Port:         svc.Port,
			ExposeListen: svc.ExposeListen,
			ExposeUnix:   svc.ExposeUnix,
			UpdatedAt:    svc.UpdatedAt,
		})
	}
	doc := protocol.EndpointsFile{
		Project:   ps.Name,
		Subnet:    ps.SubnetCIDR,
		UpdatedAt: time.Now().UTC(),
		Endpoints: entries,
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "endpoints.json.tmp")
	final := filepath.Join(dir, "endpoints.json")
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}

func splitEnvAndSecrets(svcEnv, rtEnv, secrets map[string]string) (envMap, secMap map[string]string) {
	envMap = make(map[string]string)
	secMap = make(map[string]string)
	combined := make(map[string]string, len(rtEnv)+len(svcEnv))
	for k, v := range rtEnv {
		combined[k] = v
	}
	for k, v := range svcEnv {
		combined[k] = v
	}
	for k, v := range combined {
		if strings.Contains(v, "${secret:") {
			resolved := v
			for sk, sv := range secrets {
				resolved = strings.ReplaceAll(resolved, "${secret:"+sk+"}", sv)
			}
			secMap[k] = resolved
		} else {
			envMap[k] = v
		}
	}
	return envMap, secMap
}

func serviceImageDigest(freebsd string, img protocol.ImageSpec, sourceDigest string) string {
	base := builder.ImageDigest(freebsd, img)
	if len(img.Stages) == 0 && len(img.Runtime.Copy) == 0 {
		return base
	}
	h := sha256.New()
	_, _ = io.WriteString(h, base+"\n"+sourceDigest)
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func serviceConfigDigest(svc protocol.ServiceSpec, imgDigest string, secrets map[string]string) string {
	h := sha256.New()
	raw, _ := json.Marshal(svc)
	h.Write(raw)
	_, _ = io.WriteString(h, imgDigest)
	keys := make([]string, 0, len(secrets))
	for k := range secrets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		_, _ = io.WriteString(h, k+"="+secrets[k]+";")
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func dirContentDigest(dirs ...string) string {
	h := sha256.New()
	for _, dir := range dirs {
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		var paths []string
		_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() {
				paths = append(paths, p)
			}
			return nil
		})
		sort.Strings(paths)
		for _, p := range paths {
			rel, _ := filepath.Rel(dir, p)
			_, _ = io.WriteString(h, rel+"\n")
			f, err := os.Open(p)
			if err == nil {
				_, _ = io.Copy(h, f)
				_ = f.Close()
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func parseDurationOr(raw string, def time.Duration) time.Duration {
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

func parseGrace(raw string) time.Duration {
	return parseDurationOr(raw, 10*time.Second)
}

func sortedStorageNames(m map[string]protocol.StorageSpec) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

func sortedActiveNames(m map[string]state.ActiveServiceState) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

func sanitizeIdent(s string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '_'
	}, s)
}
