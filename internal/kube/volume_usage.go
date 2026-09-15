package kube

import (
	"context"

	"github.com/labring-sigs/pvc-migrate/internal/domain"
)

// VolumeUsageReadOptions identifies a source volume whose used bytes must be measured.
type VolumeUsageReadOptions struct {
	OperationID string
	SourcePVC   domain.ObjectReference
	SourcePV    domain.ObjectReference
}

// VolumeUsageReadResult reports a conservative upper bound for the source
// data that must fit in the destination volume.
type VolumeUsageReadResult struct {
	UsedBytes int64
	Source    string
}

// VolumeUsageReader reads usage only from a known storage backend API or CRD.
// An unsupported backend must return an error instead of estimating usage from
// provisioned capacity.
type VolumeUsageReader interface {
	Read(ctx context.Context, options VolumeUsageReadOptions) (VolumeUsageReadResult, error)
}

// FilesystemUsageReader measures a mounted filesystem through a controlled
// data-plane probe. It is kept separate from CRD-backed usage readers.
type FilesystemUsageReader interface {
	Read(ctx context.Context, options VolumeUsageReadOptions) (VolumeUsageReadResult, error)
}
