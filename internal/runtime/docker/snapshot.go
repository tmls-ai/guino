package docker

import (
	"context"
	"fmt"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/rs/xid"

	"github.com/tmls-ai/guino/internal/runtime"
)

const (
	labelSnapshot   = labelPrefix + "snapshot"
	labelSnapshotID = labelPrefix + "snapshot.id"
	labelSandboxRef = labelPrefix + "snapshot.sandbox_id"
	labelSnapName   = labelPrefix + "snapshot.name"
)

// Snapshot commits the current container state as a new Docker image.
func (r *DockerRuntime) Snapshot(ctx context.Context, id string, name string) (*runtime.SnapshotInfo, error) {
	containerName := r.containerName(id)
	snapshotID := "snap-" + xid.New().String()

	commitResp, err := r.cli.ContainerCommit(ctx, containerName, container.CommitOptions{
		Reference: "den/snapshot:" + snapshotID,
		Comment:   fmt.Sprintf("Snapshot '%s' of sandbox %s", name, id),
		Config: &container.Config{
			Labels: map[string]string{
				labelSnapshot:   "true",
				labelSnapshotID: snapshotID,
				labelSandboxRef: id,
				labelSnapName:   name,
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("committing container %s: %w", id, err)
	}

	return &runtime.SnapshotInfo{
		ID:        snapshotID,
		SandboxID: id,
		Name:      name,
		ImageID:   commitResp.ID,
		CreatedAt: time.Now().UTC(),
	}, nil
}

// Restore creates a new sandbox from a snapshot image.
func (r *DockerRuntime) Restore(ctx context.Context, snapshotID string) (string, error) {
	images, err := r.cli.ImageList(ctx, image.ListOptions{
		Filters: filters.NewArgs(filters.Arg("label", labelSnapshotID+"="+snapshotID)),
	})
	if err != nil {
		return "", fmt.Errorf("listing snapshot images: %w", err)
	}
	if len(images) == 0 {
		return "", fmt.Errorf("snapshot not found: %s", snapshotID)
	}

	imageRef := "den/snapshot:" + snapshotID

	newID := xid.New().String()
	err = r.Create(ctx, newID, runtime.SandboxConfig{
		Image: imageRef,
	})
	if err != nil {
		return "", fmt.Errorf("creating container from snapshot: %w", err)
	}

	if err := r.Start(ctx, newID); err != nil {
		_ = r.Remove(ctx, newID)
		return "", fmt.Errorf("starting restored container: %w", err)
	}

	return newID, nil
}

// ListSnapshots returns all snapshots, optionally filtered by sandbox ID.
func (r *DockerRuntime) ListSnapshots(ctx context.Context, sandboxID string) ([]runtime.SnapshotInfo, error) {
	filterArgs := filters.NewArgs(filters.Arg("label", labelSnapshot+"=true"))
	if sandboxID != "" {
		filterArgs.Add("label", labelSandboxRef+"="+sandboxID)
	}

	images, err := r.cli.ImageList(ctx, image.ListOptions{
		Filters: filterArgs,
	})
	if err != nil {
		return nil, fmt.Errorf("listing snapshot images: %w", err)
	}

	var snapshots []runtime.SnapshotInfo
	for _, img := range images {
		snapshots = append(snapshots, runtime.SnapshotInfo{
			ID:        img.Labels[labelSnapshotID],
			SandboxID: img.Labels[labelSandboxRef],
			Name:      img.Labels[labelSnapName],
			ImageID:   img.ID,
			CreatedAt: time.Unix(img.Created, 0),
			Size:      img.Size,
		})
	}
	return snapshots, nil
}

// RemoveSnapshot removes a snapshot image.
func (r *DockerRuntime) RemoveSnapshot(ctx context.Context, snapshotID string) error {
	images, err := r.cli.ImageList(ctx, image.ListOptions{
		Filters: filters.NewArgs(filters.Arg("label", labelSnapshotID+"="+snapshotID)),
	})
	if err != nil {
		return fmt.Errorf("finding snapshot image: %w", err)
	}
	if len(images) == 0 {
		return fmt.Errorf("snapshot not found: %s", snapshotID)
	}

	_, err = r.cli.ImageRemove(ctx, images[0].ID, image.RemoveOptions{Force: true})
	return err
}
