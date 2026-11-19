package replica

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

// VolumeIDFile is written once into each shared data root on the first PostgreSQL start; the
// database records the same ID, and every later start must find it in every root.
const VolumeIDFile = "replica-volume.id"

// volumeSetting is the global meta key recording the deployment's volume ID.
const volumeSetting = "replica_volume_id"

// BootIDPath is the kernel's per-boot ID: replicas sharing flock-fenced volumes must share
// one kernel.
const BootIDPath = "/proc/sys/kernel/random/boot_id"

var (
	// ErrVolumes refuses a replica that does not mount the deployment's shared volumes.
	ErrVolumes = errors.New("this replica does not mount the deployment's shared volumes")
	// ErrHost refuses a replica on another kernel than a live one.
	ErrHost = errors.New("KyPost replicas must run on one Docker host")
	// ErrReceiver refuses the Maddy receiver on PostgreSQL.
	ErrReceiver = errors.New("KYPOST_NATIVE_RECEIVER (receiving through Maddy on port 25) runs only in the single-container profile; it cannot run on PostgreSQL replicas. Use the single-container SQLite profile for Maddy, or receive through Cloudflare or an external IMAP account")
)

// Process is one api or daemon process starting on PostgreSQL.
type Process struct {
	Store  *state.Store // the global state store
	Role   string       // "api" or "daemon"
	Roots  []string     // the shared data roots: CONFIG_DIR, SECRET_DIR, STATE_DIR
	BootID string       // this kernel's boot ID (BootIDPath)
	Getenv func(string) string
}

// ReadBootID reads BootIDPath.
func ReadBootID() (string, error) {
	b, err := os.ReadFile(BootIDPath)
	if err != nil {
		return "", fmt.Errorf("reading the host's boot ID: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

// Start refuses a topology the shared flock fences cannot cover, then registers p as a live
// instance until release. It must run before any listener or worker starts.
func Start(ctx context.Context, p Process) (release func(), err error) {
	if v := strings.TrimSpace(p.Getenv("KYPOST_NATIVE_RECEIVER")); v != "" && !strings.EqualFold(v, "false") {
		return nil, ErrReceiver
	}
	if p.BootID == "" {
		return nil, errors.New("replica check: empty boot ID")
	}
	var volume string
	err = p.Store.Serialized(ctx, "replica-topology", func(context.Context) error {
		var err error
		if volume, err = checkVolumes(p.Store, p.Roots); err != nil {
			return err
		}
		live, err := p.Store.LiveInstances(ctx)
		if err != nil {
			return fmt.Errorf("replica check: %w", err)
		}
		for _, in := range live {
			if in.BootID != p.BootID {
				return fmt.Errorf("%w: instance %s (%s) runs on another kernel (boot ID %s, this one %s). Separate hosts, separate volumes and NFS/RWX volumes are not supported", ErrHost, in.ID, in.Role, in.BootID, p.BootID)
			}
			if in.VolumeID != volume {
				return fmt.Errorf("%w: instance %s mounts volume set %s", ErrVolumes, in.ID, in.VolumeID)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	id := p.Role + "-" + rand.Text()[:16]
	return p.Store.RegisterInstance(ctx, state.Instance{ID: id, BootID: p.BootID, VolumeID: volume, Role: p.Role, Started: time.Now().Unix()})
}

// checkVolumes returns the deployment's volume ID after checking that every root holds it.
// The first PostgreSQL start mints it into every root and the database.
func checkVolumes(st *state.Store, roots []string) (string, error) {
	recorded, err := st.Setting(volumeSetting)
	switch {
	case errors.Is(err, state.ErrNotFound):
		recorded = ""
	case err != nil:
		return "", fmt.Errorf("replica check: %w", err)
	}
	ids := make([]string, len(roots))
	for i, root := range roots {
		b, err := os.ReadFile(filepath.Join(root, VolumeIDFile))
		switch {
		case err == nil:
			ids[i] = strings.TrimSpace(string(b))
		case !errors.Is(err, os.ErrNotExist):
			return "", fmt.Errorf("replica check: %w", err)
		}
	}
	if recorded == "" {
		// First PostgreSQL start: every root must be fresh, or already carry one ID (a start that
		// died between writing the files and recording the ID).
		for i, id := range ids {
			switch {
			case id == "":
			case recorded == "":
				recorded = id
			case id != recorded:
				return "", fmt.Errorf("%w: %s carries volume ID %s, another root %s", ErrVolumes, roots[i], id, recorded)
			}
		}
		if recorded == "" {
			recorded = rand.Text()
		}
		for i, root := range roots {
			if ids[i] != "" {
				continue
			}
			if _, err := fsutil.CreateExclusive(filepath.Join(root, VolumeIDFile), []byte(recorded+"\n"), 0o600); err != nil {
				return "", fmt.Errorf("replica check: %w", err)
			}
		}
		if err := st.SetSetting(volumeSetting, recorded); err != nil {
			return "", fmt.Errorf("replica check: %w", err)
		}
		return recorded, nil
	}
	for i, id := range ids {
		switch {
		case id == "":
			return "", fmt.Errorf("%w: %s has no %s, but this deployment's database is already in use", ErrVolumes, roots[i], VolumeIDFile)
		case id != recorded:
			return "", fmt.Errorf("%w: %s belongs to another deployment (volume ID %s, this deployment %s)", ErrVolumes, roots[i], id, recorded)
		}
	}
	return recorded, nil
}
