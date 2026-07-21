// Package backend abstracts the container/PostgreSQL control plane the snapshot
// and restore transactions drive. The interface is deliberately drawn around a
// "backend container" (a slot), not around Incus specifically, so a later slice
// can swap the Incus shell implementation for the typed Incus Go client — or,
// per Option Zero in the spec, for plain systemd clusters — without touching the
// task engine.
package backend

import "context"

type Backend interface {
	// ContainerRunning reports whether the backend container is RUNNING.
	ContainerRunning(ctx context.Context, container string) (bool, error)
	// PGActive reports whether PostgreSQL is active inside the container.
	PGActive(ctx context.Context, container string) (bool, error)

	// StopPG stops PostgreSQL cleanly (container keeps running).
	StopPG(ctx context.Context, container string) error
	// EnsurePGRunning starts PostgreSQL if the container is up but PG is down,
	// then waits for readiness. Used as a transaction postcondition.
	EnsurePGRunning(ctx context.Context, container string) error

	// StopContainer stops the container gracefully and verifies it reached
	// STOPPED, refusing otherwise (a caller is about to touch its data).
	StopContainer(ctx context.Context, container string) error
	// StopContainerForce force-stops the container (used in rollback, where an
	// unclean stop before restoring data beats leaving PG holding the mount).
	StopContainerForce(ctx context.Context, container string) error
	// StartContainerAndWait starts the container and waits for PostgreSQL.
	StartContainerAndWait(ctx context.Context, container string) error

	// RepairIP re-pins the container's eth0 to its static address (derived by the
	// implementation), so a networking fault surfaces before the data swap rather
	// than stranding an otherwise intact dataset.
	RepairIP(ctx context.Context, container string) error
}
