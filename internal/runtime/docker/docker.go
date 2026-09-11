package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/system"
	"github.com/docker/docker/api/types/versions"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"

	cerrdefs "github.com/containerd/errdefs"

	"github.com/tmls-ai/guino/internal/runtime"
	"github.com/tmls-ai/guino/internal/storage"
	"github.com/tmls-ai/guino/internal/store"

	dockermount "github.com/docker/docker/api/types/mount"
)

const (
	labelPrefix  = "den."
	labelID      = labelPrefix + "id"
	labelCreated = labelPrefix + "created"

	// Network-only ownership/identity labels (NOT applied to containers).
	// Reconcile (Phase 2) treats den.managed=true as the authoritative,
	// spoof-resistant ownership signal — a name-only match is never enough.
	labelNetManaged = labelPrefix + "managed"      // "true"
	labelNetMode    = labelPrefix + "network.mode" // internal|bridge
	labelNetICC     = labelPrefix + "network.icc"  // "false"

	// dockerAPIFloor is the minimum negotiated Docker API version. Below
	// this the typed network.CreateOptions.EnableIPv6 *bool is not honored
	// (pre-1.42 daemons used a driver label), so the IPv6-off guarantee
	// cannot be made.
	dockerAPIFloor = "1.42"
)

func boolPtr(b bool) *bool { return &b }

// DockerRuntime implements runtime.Runtime using Docker containers.
type DockerRuntime struct {
	cli               *client.Client
	networkID         string
	networkMode       runtime.NetworkMode
	reconcileNetwork  bool
	allowUnsafeBridge bool
	logger            *slog.Logger
}

// Option configures the DockerRuntime.
type Option func(*DockerRuntime)

// WithNetworkID sets the Docker network to attach sandboxes to.
func WithNetworkID(id string) Option {
	return func(r *DockerRuntime) {
		r.networkID = id
	}
}

// WithNetworkMode sets the global default network mode the managed network
// (and fresh sandboxes) are created with. "" is treated as internal.
func WithNetworkMode(m runtime.NetworkMode) Option {
	return func(r *DockerRuntime) {
		if m == "" {
			m = runtime.NetworkModeInternal
		}
		r.networkMode = m
	}
}

// WithReconcileNetwork enables operator-initiated network reconciliation.
func WithReconcileNetwork(enabled bool) Option {
	return func(r *DockerRuntime) {
		r.reconcileNetwork = enabled
	}
}

// WithAllowUnsafeBridge records the bridge opt-in (used by reconcile/EnsureNetwork
// bookkeeping; the fatal bridge-refusal guard itself lives in cmd/guino).
func WithAllowUnsafeBridge(allowed bool) Option {
	return func(r *DockerRuntime) {
		r.allowUnsafeBridge = allowed
	}
}

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) Option {
	return func(r *DockerRuntime) {
		r.logger = l
	}
}

// New creates a new DockerRuntime.
func New(opts ...Option) (*DockerRuntime, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("creating docker client: %w", err)
	}

	r := &DockerRuntime{
		cli:         cli,
		networkID:   "den-net",
		networkMode: runtime.NetworkModeInternal,
		logger:      slog.Default(),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r, nil
}

// Ping verifies Docker daemon connectivity.
func (r *DockerRuntime) Ping(ctx context.Context) error {
	_, err := r.cli.Ping(ctx)
	return err
}

// DaemonHost returns the negotiated (post-resolution) Docker daemon endpoint —
// moby's func (cli *Client) DaemonHost() string { return cli.host }. This is
// the ONLY endpoint the platform classifier may reason about; it reflects a
// programmatic WithHost and never the dead runtime.docker_host. Distinct from
// the container-inspect Info(ctx, id).
func (r *DockerRuntime) DaemonHost() string {
	return r.cli.DaemonHost()
}

// SystemInfo returns `docker info` (moby's client.Info(ctx)). Distinct from
// the container-inspect Info(ctx, id) — named to avoid colliding with it.
func (r *DockerRuntime) SystemInfo(ctx context.Context) (system.Info, error) {
	return r.cli.Info(ctx)
}

// NetworkMode returns the configured global default network mode.
func (r *DockerRuntime) NetworkMode() runtime.NetworkMode {
	return r.networkMode
}

// EnsureNetwork creates the Guino Docker network if it doesn't exist.
//
// Mode-aware:
//   - none:     no-op (none sandboxes use empty EndpointsConfig; no managed
//     network is needed).
//   - internal: den-net with Internal:true.
//   - bridge:   den-net with Internal:false (egress + 127.0.0.1 publishing).
//
// Both connected modes set enable_icc=false, EnableIPv6:ptr(false), and the
// den.managed/den.network.* labels. A daemon below the API floor is fatal
// (the typed EnableIPv6 *bool is silently ignored pre-1.42). Reconciliation
// of an existing network whose mode changed is Phase 2.
func (r *DockerRuntime) EnsureNetwork(ctx context.Context) error {
	mode := r.networkMode
	if mode == "" {
		mode = runtime.NetworkModeInternal
	}
	if mode == runtime.NetworkModeNone {
		r.logger.Info("network_mode=none: skipping managed network creation")
		return nil
	}

	if v := r.cli.ClientVersion(); v != "" && versions.LessThan(v, dockerAPIFloor) {
		return fmt.Errorf("docker API version %s is below the required floor %s "+
			"(the typed network EnableIPv6 control is not honored on older daemons)", v, dockerAPIFloor)
	}

	networks, err := r.cli.NetworkList(ctx, network.ListOptions{
		Filters: filters.NewArgs(filters.Arg("name", r.networkID)),
	})
	if err != nil {
		return fmt.Errorf("listing networks: %w", err)
	}
	for _, n := range networks {
		if n.Name == r.networkID {
			return nil
		}
	}

	return r.createManagedNetwork(ctx, mode)
}

// createManagedNetwork creates den-net at the given (connected) mode with the
// den.managed/den.network.* ownership labels, IPv6 off and ICC off, then
// re-inspects to verify IPv6 is actually disabled (Inspect().EnableIPv6 is
// sourced from the live network, not the requested option). Shared by
// EnsureNetwork and the Reconcile recreate path so both produce an identical,
// reconcile-recognizable network. The caller is responsible for distinguishing
// a 409 name/ID conflict (concurrent actor) from other errors.
func (r *DockerRuntime) createManagedNetwork(ctx context.Context, mode runtime.NetworkMode) error {
	internal := mode == runtime.NetworkModeInternal
	if _, err := r.cli.NetworkCreate(ctx, r.networkID, network.CreateOptions{
		Driver:     "bridge",
		Internal:   internal,
		EnableIPv6: boolPtr(false),
		Options: map[string]string{
			"com.docker.network.bridge.enable_icc": "false",
		},
		Labels: map[string]string{
			labelNetManaged: "true",
			labelNetMode:    string(mode),
			labelNetICC:     "false",
		},
	}); err != nil {
		return fmt.Errorf("creating network %s: %w", r.networkID, err)
	}

	insp, err := r.cli.NetworkInspect(ctx, r.networkID, network.InspectOptions{})
	if err != nil {
		return fmt.Errorf("inspecting created network %s: %w", r.networkID, err)
	}
	if insp.EnableIPv6 {
		return fmt.Errorf("network %s created with IPv6 enabled despite EnableIPv6=false", r.networkID)
	}

	r.logger.Info("created docker network",
		"network", r.networkID, "mode", mode, "internal", internal)
	return nil
}

// buildContainerCreateSpec builds the Docker create spec for a sandbox. Pure
// (no client, no I/O) so it is unit-testable. mode is the resolved effective
// mode; networkID is the managed network name for connected modes.
//
//   - none:               EndpointsConfig is empty (the r.networkID default is
//     NOT substituted — empty/nil EndpointsConfig is the PRIMARY, load-bearing
//     control), HostConfig.NetworkMode="none" (defense-in-depth, NOT
//     daemon-backstopped), and PortBindings + ExposedPorts are BOTH cleared.
//   - internal / bridge:  attached to networkID.
//
// Guino-set den.id/den.created labels are applied AFTER the caller label loop so
// a caller can never spoof them (the validator also strips caller den.*).
func buildContainerCreateSpec(id string, cfg runtime.SandboxConfig, networkID string, mode runtime.NetworkMode) (*container.Config, *container.HostConfig, *network.NetworkingConfig, error) {
	if mode == "" {
		mode = runtime.NetworkModeInternal
	}

	var envList []string
	for k, v := range cfg.Env {
		envList = append(envList, k+"="+v)
	}

	// Caller labels first, then authoritative den.* labels overwrite —
	// den.id/den.created can never be caller-spoofed.
	labels := map[string]string{}
	for k, v := range cfg.Labels {
		labels[k] = v
	}
	labels[labelID] = id
	labels[labelCreated] = time.Now().UTC().Format(time.RFC3339)

	exposedPorts := nat.PortSet{}
	portBindings := nat.PortMap{}
	if mode != runtime.NetworkModeNone {
		for _, pm := range cfg.Ports {
			// Defense-in-depth: only tcp reaches here (validator already
			// normalized/rejected), but never trust the upstream blindly.
			proto := strings.ToLower(pm.Protocol)
			if proto == "" {
				proto = "tcp"
			}
			if proto != "tcp" {
				return nil, nil, nil, fmt.Errorf("unsupported port protocol %q for sandbox %s", pm.Protocol, id)
			}
			containerPort := nat.Port(fmt.Sprintf("%d/tcp", pm.SandboxPort))
			exposedPorts[containerPort] = struct{}{}
			portBindings[containerPort] = []nat.PortBinding{
				{HostIP: "127.0.0.1", HostPort: fmt.Sprintf("%d", pm.HostPort)},
			}
		}
	}

	containerCfg := &container.Config{
		Image:        cfg.Image,
		Env:          envList,
		Labels:       labels,
		ExposedPorts: exposedPorts,
		Tty:          true,
		OpenStdin:    true,
	}
	if cfg.WorkDir != "" {
		containerCfg.WorkingDir = cfg.WorkDir
	}
	if len(cfg.Cmd) > 0 {
		containerCfg.Cmd = cfg.Cmd
	}

	var mounts []dockermount.Mount
	if cfg.Storage != nil {
		for _, vol := range cfg.Storage.Volumes {
			mounts = append(mounts, dockermount.Mount{
				Type:     dockermount.TypeVolume,
				Source:   storage.NamespacedVolumeName(vol.Name),
				Target:   vol.MountPath,
				ReadOnly: vol.ReadOnly,
			})
		}
	}

	hostCfg := &container.HostConfig{
		PortBindings: portBindings,
		Resources: container.Resources{
			NanoCPUs: cfg.CPU,
			Memory:   cfg.Memory,
		},
		// Docker applies the default seccomp profile automatically when
		// no seccomp option is specified (blocks ~44 dangerous syscalls).
		SecurityOpt: []string{"no-new-privileges"},
		CapDrop:     []string{"ALL"},
		CapAdd:      []string{"NET_BIND_SERVICE", "CHOWN", "SETUID", "SETGID", "DAC_OVERRIDE", "FOWNER"},
		// Read-only rootfs is the secure default (WritableRootfs zero-value is
		// false). Callers opt out only for heavyweight images that write
		// outside the tmpfs/volume mounts (e.g. a Chrome+VNC desktop).
		ReadonlyRootfs: !cfg.WritableRootfs,
		Tmpfs:          cfg.TmpfsMap,
		Mounts:         mounts,
	}
	if cfg.PidLimit > 0 {
		hostCfg.PidsLimit = &cfg.PidLimit
	} else {
		defaultPidLimit := int64(256)
		hostCfg.PidsLimit = &defaultPidLimit
	}

	networkCfg := &network.NetworkingConfig{}
	if mode == runtime.NetworkModeNone {
		// PRIMARY control: empty EndpointsConfig + NetworkMode="none". Do
		// NOT substitute r.networkID here.
		hostCfg.NetworkMode = container.NetworkMode("none")
	} else {
		netID := cfg.NetworkID
		if netID == "" {
			netID = networkID
		}
		if netID != "" {
			networkCfg.EndpointsConfig = map[string]*network.EndpointSettings{
				netID: {},
			}
		}
	}

	return containerCfg, hostCfg, networkCfg, nil
}

// Create creates a new Docker container for the sandbox.
func (r *DockerRuntime) Create(ctx context.Context, id string, cfg runtime.SandboxConfig) error {
	mode := cfg.NetworkMode
	if mode == "" {
		mode = r.networkMode
	}
	if mode == "" {
		mode = runtime.NetworkModeInternal
	}

	containerCfg, hostCfg, networkCfg, err := buildContainerCreateSpec(id, cfg, r.networkID, mode)
	if err != nil {
		return err
	}

	containerName := "den-" + id

	if _, err := r.cli.ContainerCreate(ctx, containerCfg, hostCfg, networkCfg, nil, containerName); err != nil {
		return fmt.Errorf("creating container %s: %w", id, err)
	}

	// none post-create assertion. Runs strictly BEFORE any ContainerStart
	// (the engine calls Start only after Create returns nil), so a created-
	// but-not-started container has no process and cannot egress — the TOCTOU
	// window is closed by construction. A correct `none` container ALWAYS has
	// NetworkSettings.Networks == {"none":{zeroed}}; breach iff any key !=
	// "none" OR any endpoint with a non-empty EndpointID/NetworkID/IPAddress.
	if mode == runtime.NetworkModeNone {
		if err := r.assertNoNetwork(ctx, id, containerName); err != nil {
			return err
		}
	}
	return nil
}

// assertNoNetwork enforces the none invariant. On breach it logs the full
// NetworkSettings at ERROR, force-removes the container, and returns the
// committed error EVEN IF cleanup partially fails (the cleanup error is
// wrapped as context — never leave a network-attached sandbox behind).
func (r *DockerRuntime) assertNoNetwork(ctx context.Context, id, containerName string) error {
	inspect, err := r.cli.ContainerInspect(ctx, containerName)
	if err != nil {
		return fmt.Errorf("inspecting container %s for none assertion: %w", id, err)
	}
	if inspect.NetworkSettings == nil {
		return nil
	}

	breach := false
	for key, ep := range inspect.NetworkSettings.Networks {
		if key != "none" {
			breach = true
			break
		}
		if ep != nil && (ep.EndpointID != "" || ep.NetworkID != "" || ep.IPAddress != "") {
			breach = true
			break
		}
	}
	if !breach {
		return nil
	}

	nsJSON, _ := json.Marshal(inspect.NetworkSettings)
	r.logger.Error("network_mode=none isolation breach: container is network-attached",
		"id", id, "network_settings", string(nsJSON))

	rmErr := r.cli.ContainerRemove(ctx, containerName, container.RemoveOptions{Force: true})
	if rmErr != nil {
		return fmt.Errorf("network_mode=none isolation breach for sandbox %s "+
			"(and force-remove of the breached container failed: %v)", id, rmErr)
	}
	return fmt.Errorf("network_mode=none isolation breach for sandbox %s: "+
		"container was attached to a network despite none mode (force-removed)", id)
}

// networkStale reports whether an existing Guino network's posture deviates from
// the desired mode. v9 predicate: ANY-deviation (OR, never AND) so an operator
// internal→bridge migration (Internal flips) is correctly detected — the v8
// AND-joined predicate missed this and left port-forwarding broken. Each
// clause is compared against the *desired* config, not a fixed constant.
func (r *DockerRuntime) networkStale(insp network.Inspect, desired runtime.NetworkMode) bool {
	if insp.Internal != (desired == runtime.NetworkModeInternal) {
		return true
	}
	if insp.EnableIPv6 {
		return true
	}
	// ICC must be off. Prefer the authoritative den.network.icc label; only
	// fall back to the bridge Options string for a legacy/unlabeled network.
	if v, ok := insp.Labels[labelNetICC]; ok {
		if v != "false" {
			return true
		}
	} else if insp.Options["com.docker.network.bridge.enable_icc"] != "false" {
		return true
	}
	return false
}

// Reconcile brings the managed Guino network into agreement with the configured
// default network mode after an operator-initiated mode change. It is
// spoof-resistant and store-fail-closed: it NEVER mutates a network it cannot
// prove it owns, and any sandbox-store read failure fails closed BEFORE any
// Docker mutation.
//
// Behavior:
//   - none mode, or a non-existent network, or a network already matching the
//     desired posture ⇒ no-op (the last is a tested invariant).
//   - A stale network is destroyed+recreated ONLY when all three ownership
//     signals hold (den.managed=true LABEL — never name-only — AND every
//     attached container is name-prefixed den-<id> with the authoritative
//     den.id label AND present in Guino's store) AND runtime.reconcile_network
//     (GUINO_RUNTIME__RECONCILE_NETWORK) is true. Otherwise a typed actionable
//     error is returned and nothing is touched. Disconnected sandboxes are
//     NOT auto-restarted.
//
// Pinned signature: store is a method parameter (no engine→store field).
func (r *DockerRuntime) Reconcile(ctx context.Context, st store.Store) error {
	mode := r.networkMode
	if mode == "" {
		mode = runtime.NetworkModeInternal
	}
	if mode == runtime.NetworkModeNone {
		return nil
	}

	if v := r.cli.ClientVersion(); v != "" && versions.LessThan(v, dockerAPIFloor) {
		return fmt.Errorf("docker API version %s is below the required floor %s "+
			"(reconcile cannot guarantee the typed EnableIPv6 control)", v, dockerAPIFloor)
	}

	insp, err := r.cli.NetworkInspect(ctx, r.networkID, network.InspectOptions{})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return nil // nothing to reconcile; EnsureNetwork creates it fresh
		}
		return fmt.Errorf("inspecting network %s for reconcile: %w", r.networkID, err)
	}

	if !r.networkStale(insp, mode) {
		return nil // no-op happy path (tested invariant)
	}

	// Stale. Ownership signal #1: den.managed=true LABEL (never name-only — a
	// configured network_id can collide with an operator-owned network).
	if insp.Labels[labelNetManaged] != "true" {
		return fmt.Errorf("network %s is stale for mode %s but is NOT Guino-managed "+
			"(missing %s=true label): refusing to mutate a network Guino does not own — "+
			"change runtime.network_id or remove the conflicting network manually",
			r.networkID, mode, labelNetManaged)
	}

	// Store read failure ⇒ fail-closed BEFORE any Docker mutation.
	records, err := st.ListSandboxes()
	if err != nil {
		return fmt.Errorf("reconcile fail-closed: cannot read the sandbox store, "+
			"refusing to mutate network %s without verified ownership: %w", r.networkID, err)
	}
	known := make(map[string]bool, len(records))
	for _, rec := range records {
		known[rec.ID] = true
	}

	// Ownership signals #2/#3: every attached container is a Guino sandbox by
	// name-prefix AND authoritative den.id label AND present in the store.
	type attached struct{ id, name string }
	var toDisconnect []attached
	for ctrID, ep := range insp.Containers {
		name := strings.TrimPrefix(ep.Name, "/")
		if !strings.HasPrefix(name, "den-") {
			return fmt.Errorf("network %s has a non-Guino container %q attached: "+
				"refusing destructive reconcile (ownership unverifiable)", r.networkID, name)
		}
		sbID := strings.TrimPrefix(name, "den-")
		ci, err := r.cli.ContainerInspect(ctx, ctrID)
		if err != nil {
			return fmt.Errorf("inspecting attached container %q for reconcile ownership: %w", name, err)
		}
		if ci.Config == nil || ci.Config.Labels[labelID] != sbID {
			return fmt.Errorf("attached container %q lacks the authoritative %s=%s label: "+
				"refusing destructive reconcile", name, labelID, sbID)
		}
		if !known[sbID] {
			return fmt.Errorf("attached sandbox %s is not present in Guino's store: "+
				"refusing destructive reconcile (ownership unverifiable)", sbID)
		}
		toDisconnect = append(toDisconnect, attached{ctrID, name})
	}

	if !r.reconcileNetwork {
		return fmt.Errorf("network %s is Guino-managed but stale for mode %s; destructive "+
			"reconcile is OFF — set runtime.reconcile_network=true "+
			"(GUINO_RUNTIME__RECONCILE_NETWORK=true) to recreate it (this stops and "+
			"disconnects %d attached sandbox(es), which are NOT auto-restarted)",
			r.networkID, mode, len(toDisconnect))
	}

	// Destroy: stop → disconnect(force) per container, then remove the network.
	for _, a := range toDisconnect {
		stopTimeout := 10
		// Best-effort stop; a stopped container must still be disconnected.
		_ = r.cli.ContainerStop(ctx, a.id, container.StopOptions{Timeout: &stopTimeout})
		if err := r.cli.NetworkDisconnect(ctx, r.networkID, a.id, true); err != nil {
			if cerrdefs.IsNotFound(err) {
				continue // idempotent: endpoint/container/network already gone
			}
			return fmt.Errorf("disconnecting %q from network %s during reconcile: %w",
				a.name, r.networkID, err)
		}
	}

	if err := r.cli.NetworkRemove(ctx, r.networkID); err != nil {
		switch {
		case cerrdefs.IsNotFound(err):
			// Already gone — proceed to recreate.
		case cerrdefs.IsPermissionDenied(err):
			return fmt.Errorf("network %s still reports active endpoints; refusing to "+
				"force-loop (reconcile aborted, network left intact): %w", r.networkID, err)
		default:
			return fmt.Errorf("removing stale network %s during reconcile: %w", r.networkID, err)
		}
	}

	if err := r.createManagedNetwork(ctx, mode); err != nil {
		if cerrdefs.IsConflict(err) {
			// 409 NetworkNameError (name OR ID collision): a concurrent actor
			// recreated it. Re-inspect and re-run the FULL predicate; succeed
			// only if it now matches — NEVER re-destroy.
			insp2, ierr := r.cli.NetworkInspect(ctx, r.networkID, network.InspectOptions{})
			if ierr != nil {
				return fmt.Errorf("reconcile recreate hit a name/ID conflict and "+
					"re-inspecting %s failed: %w", r.networkID, ierr)
			}
			if r.networkStale(insp2, mode) {
				return fmt.Errorf("network %s recreate conflicted (concurrent actor) and "+
					"the existing network is STILL stale for mode %s: failing loud, "+
					"not re-destroying", r.networkID, mode)
			}
			r.logger.Warn("reconcile: recreate conflicted but the existing network already "+
				"matches the desired posture; accepting", "network", r.networkID, "mode", mode)
			return nil
		}
		return fmt.Errorf("recreating network %s during reconcile: %w", r.networkID, err)
	}

	r.logger.Info("reconciled docker network to new mode",
		"network", r.networkID, "mode", mode, "disconnected_sandboxes", len(toDisconnect))
	return nil
}

// Start starts the container.
func (r *DockerRuntime) Start(ctx context.Context, id string) error {
	return r.cli.ContainerStart(ctx, r.containerName(id), container.StartOptions{})
}

// Stop stops the container with the given timeout.
func (r *DockerRuntime) Stop(ctx context.Context, id string, timeout time.Duration) error {
	timeoutSec := int(timeout.Seconds())
	opts := container.StopOptions{Timeout: &timeoutSec}
	return r.cli.ContainerStop(ctx, r.containerName(id), opts)
}

// Remove forcefully removes the container.
func (r *DockerRuntime) Remove(ctx context.Context, id string) error {
	return r.cli.ContainerRemove(ctx, r.containerName(id), container.RemoveOptions{
		Force:         true,
		RemoveVolumes: false, // Preserve named volumes (may be shared between sandboxes)
	})
}

// Info returns information about the sandbox container.
func (r *DockerRuntime) Info(ctx context.Context, id string) (*runtime.SandboxInfo, error) {
	inspect, err := r.cli.ContainerInspect(ctx, r.containerName(id))
	if err != nil {
		return nil, fmt.Errorf("inspecting container %s: %w", id, err)
	}

	status := mapContainerStatus(inspect.State)
	createdAt, _ := time.Parse(time.RFC3339Nano, inspect.Created)

	var ports []runtime.PortMapping
	for containerPort, bindings := range inspect.NetworkSettings.Ports {
		for _, b := range bindings {
			hp := 0
			_, _ = fmt.Sscanf(b.HostPort, "%d", &hp)
			ports = append(ports, runtime.PortMapping{
				SandboxPort: containerPort.Int(),
				HostPort:    hp,
				Protocol:    containerPort.Proto(),
			})
		}
	}

	// Container IP on the managed bridge network (den-net). Directly
	// reachable for any port from a co-resident Linux host. Prefer the
	// managed network, then fall back to the first endpoint with an address.
	var ip string
	if inspect.NetworkSettings != nil {
		if ep := inspect.NetworkSettings.Networks[r.networkID]; ep != nil && ep.IPAddress != "" {
			ip = ep.IPAddress
		} else {
			for _, ep := range inspect.NetworkSettings.Networks {
				if ep != nil && ep.IPAddress != "" {
					ip = ep.IPAddress
					break
				}
			}
		}
	}

	return &runtime.SandboxInfo{
		ID:        id,
		Name:      strings.TrimPrefix(inspect.Name, "/"),
		Image:     inspect.Config.Image,
		Status:    status,
		CreatedAt: createdAt,
		Ports:     ports,
		Labels:    inspect.Config.Labels,
		Pid:       inspect.State.Pid,
		IP:        ip,
	}, nil
}

// List returns all Guino-managed containers.
func (r *DockerRuntime) List(ctx context.Context) ([]runtime.SandboxInfo, error) {
	containers, err := r.cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", labelID)),
	})
	if err != nil {
		return nil, fmt.Errorf("listing containers: %w", err)
	}

	infos := make([]runtime.SandboxInfo, 0, len(containers))
	for _, c := range containers {
		id := c.Labels[labelID]
		status := runtime.StatusStopped
		if c.State == "running" {
			status = runtime.StatusRunning
		}

		var ports []runtime.PortMapping
		for _, p := range c.Ports {
			ports = append(ports, runtime.PortMapping{
				SandboxPort: int(p.PrivatePort),
				HostPort:    int(p.PublicPort),
				Protocol:    p.Type,
			})
		}

		infos = append(infos, runtime.SandboxInfo{
			ID:        id,
			Name:      strings.TrimPrefix(c.Names[0], "/"),
			Image:     c.Image,
			Status:    status,
			CreatedAt: time.Unix(c.Created, 0),
			Ports:     ports,
			Labels:    c.Labels,
		})
	}
	return infos, nil
}

// Stats returns resource usage stats for the sandbox.
func (r *DockerRuntime) Stats(ctx context.Context, id string) (*runtime.SandboxStats, error) {
	resp, err := r.cli.ContainerStatsOneShot(ctx, r.containerName(id))
	if err != nil {
		return nil, fmt.Errorf("getting stats for %s: %w", id, err)
	}
	defer func() { _ = resp.Body.Close() }()

	var statsResp container.StatsResponse
	if err := decodeStats(resp.Body, &statsResp); err != nil {
		return nil, err
	}

	return mapStats(&statsResp), nil
}

// ContainerName returns the Docker container name for a sandbox ID.
func ContainerName(id string) string {
	return "den-" + id
}

func (r *DockerRuntime) containerName(id string) string {
	return ContainerName(id)
}

// UpdateMemoryLimit dynamically updates the memory limit for a container.
func (r *DockerRuntime) UpdateMemoryLimit(ctx context.Context, id string, memoryBytes int64) error {
	cm := &CgroupManager{cli: r.cli, logger: r.logger}
	return cm.UpdateMemoryHigh(ctx, id, memoryBytes)
}

func mapContainerStatus(state *container.State) runtime.SandboxStatus {
	if state == nil {
		return runtime.StatusError
	}
	switch {
	case state.Running:
		return runtime.StatusRunning
	case state.Paused:
		return runtime.StatusStopped
	case state.Dead, state.OOMKilled:
		return runtime.StatusError
	default:
		return runtime.StatusStopped
	}
}
