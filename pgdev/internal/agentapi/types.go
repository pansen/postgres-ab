// Package agentapi is the daemon's HTTP/JSON contract and its typed client.
// The machine stops being "a shell you inject scripts into over Apple's broken
// container exec" and becomes "a service you send requests to" (§5.8 of
// issues/0001): pgdev (host) → HTTP/JSON → pgdevd (machine).
//
// Two-machine model (spec 0002): each daemon serves EXACTLY ONE backend — its
// own machine's slot. The contract is therefore slot-implicit: the host holds
// one client per machine and routes by choosing the client, not by passing a
// slot. Active/staging roles and promote are host-side concepts and are not part
// of this contract.
package agentapi

// APIVersion is bumped when the wire contract changes incompatibly. v2 = the
// one-backend-per-machine, slot-implicit contract (dropped promote/staging).
const APIVersion = 2

// VersionResponse answers GET /v1/version — the handshake `pgdev agent deploy`
// polls to confirm the freshly-installed daemon is the one now running.
type VersionResponse struct {
	Version    string `json:"version"`    // git-describe stamp (main.version)
	APIVersion int    `json:"apiVersion"` // this package's APIVersion
}

// HealthResponse answers GET /v1/healthz.
type HealthResponse struct {
	OK bool `json:"ok"`
}

// SnapshotInfo is one checkpoint on the backend's timeline (creation-ordered).
type SnapshotInfo struct {
	Name        string `json:"name"`
	CreatedUnix int64  `json:"createdUnix"`
}

// StatusResponse answers GET /v1/status: this machine's single backend. Raw
// facts only — the host assigns the active/staging role (from its own pointer),
// renders endpoints/psql lines and formats credentials.
type StatusResponse struct {
	Slot             string         `json:"slot"`             // this machine's slot (a|b)
	Container        string         `json:"container"`        // pg-dev-a
	State            string         `json:"state"`            // RUNNING/STOPPED/… or "" if absent
	IPs              []string       `json:"ips"`              // container addresses (informational)
	BackendPort      int            `json:"backendPort"`      // port the backend is exposed on (machine eth0)
	ProxyDevice      bool           `json:"proxyDevice"`      // the eth0→backend proxy device is present
	DataStoreMounted bool           `json:"dataStoreMounted"` // the XFS reflink store is mounted
	IncusVersion     string         `json:"incusVersion"`
	Snapshots        []SnapshotInfo `json:"snapshots"`
}

// SnapshotsResponse answers GET /v1/snapshots.
type SnapshotsResponse struct {
	Slot      string         `json:"slot"`
	Snapshots []SnapshotInfo `json:"snapshots"`
}

// SnapshotRequest is the body of POST /v1/snapshot (slot implicit — the daemon
// owns exactly one).
type SnapshotRequest struct {
	Name  string `json:"name"`
	Force bool   `json:"force"`
}

// RestoreRequest is the body of POST /v1/restore. Last selects the most recent
// snapshot (Name ignored). Force skips the newer-timeline confirmation, which
// the host has already resolved on its TTY (no interactive prompt in-machine).
type RestoreRequest struct {
	Name  string `json:"name"`
	Last  bool   `json:"last"`
	Force bool   `json:"force"`
}

// OpResponse is the generic result of a mutation.
type OpResponse struct {
	Message string `json:"message"`
}

// ReconcileResponse answers POST /v1/reconcile: whether the backend is running
// and which forward device was (re)asserted.
type ReconcileResponse struct {
	BackendRunning bool     `json:"backendRunning"`
	Actions        []string `json:"actions"`
}

// errorBody is the JSON shape of a non-2xx response.
type errorBody struct {
	Error string `json:"error"`
}
