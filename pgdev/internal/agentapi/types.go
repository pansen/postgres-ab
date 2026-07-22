// Package agentapi is the daemon's HTTP/JSON contract and its typed client.
// The machine stops being "a shell you inject scripts into over Apple's broken
// container exec" and becomes "a service you send requests to" (§5.8 of
// issues/0001): pgdev (host) → HTTP/JSON → pgdevd (machine). Stateful control
// (status/promote/snapshot/restore/reconcile) rides this API; the daemon owns
// the journal, the single-mutation mutex and boot-time recovery centrally.
package agentapi

// APIVersion is bumped when the wire contract changes incompatibly.
const APIVersion = 1

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

// SnapshotInfo is one checkpoint on a slot's timeline (creation-ordered).
type SnapshotInfo struct {
	Name        string `json:"name"`
	CreatedUnix int64  `json:"createdUnix"`
}

// BackendStatus is one slot's live facts. Raw only — the host renders endpoints,
// psql lines and credentials from its own .env.
type BackendStatus struct {
	Slot      string         `json:"slot"`      // a | b
	Container string         `json:"container"` // pg-dev-a
	Role      string         `json:"role"`      // active | staging
	State     string         `json:"state"`     // RUNNING/STOPPED/… or "" if absent
	IPs       []string       `json:"ips"`
	Snapshots []SnapshotInfo `json:"snapshots"`
}

// StatusResponse answers GET /v1/status (ports cmd_status). Backends are ordered
// active-first so the host renders them like the shell did.
type StatusResponse struct {
	Active           string          `json:"active"`
	ProxyName        string          `json:"proxyName"`
	ProxyState       string          `json:"proxyState"` // "" if absent
	DataStoreMounted bool            `json:"dataStoreMounted"`
	IncusVersion     string          `json:"incusVersion"`
	Backends         []BackendStatus `json:"backends"`
}

// SnapshotsResponse answers GET /v1/snapshots?slot=.
type SnapshotsResponse struct {
	Slot      string         `json:"slot"`
	Snapshots []SnapshotInfo `json:"snapshots"`
}

// PromoteResponse answers POST /v1/promote.
type PromoteResponse struct {
	From   string         `json:"from"`
	To     string         `json:"to"`
	Status StatusResponse `json:"status"`
}

// SnapshotRequest is the body of POST /v1/snapshot.
type SnapshotRequest struct {
	Slot  string `json:"slot"`
	Name  string `json:"name"`
	Force bool   `json:"force"`
}

// RestoreRequest is the body of POST /v1/restore. Last selects the most recent
// snapshot (Name ignored). Force skips the newer-timeline confirmation, which
// the host has already resolved on its TTY (no interactive prompt in-machine).
type RestoreRequest struct {
	Slot  string `json:"slot"`
	Name  string `json:"name"`
	Last  bool   `json:"last"`
	Force bool   `json:"force"`
}

// OpResponse is the generic result of a mutation.
type OpResponse struct {
	Message string `json:"message"`
}

// ReconcileResponse answers POST /v1/reconcile.
type ReconcileResponse struct {
	ProxyRunning bool     `json:"proxyRunning"`
	Actions      []string `json:"actions"`
}

// errorBody is the JSON shape of a non-2xx response.
type errorBody struct {
	Error string `json:"error"`
}
