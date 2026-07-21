package agentapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeService returns canned data and records the last mutation request.
type fakeService struct {
	lastSnap SnapshotReqSeen
	failNext error
}

type SnapshotReqSeen struct {
	req SnapshotRequest
	set bool
}

func (f *fakeService) Version() VersionResponse {
	return VersionResponse{Version: "v1.2.3", APIVersion: APIVersion}
}
func (f *fakeService) Status(context.Context) (StatusResponse, error) {
	if f.failNext != nil {
		return StatusResponse{}, f.failNext
	}
	return StatusResponse{Active: "a", ProxyName: "pg-proxy", ProxyState: "RUNNING"}, nil
}
func (f *fakeService) Snapshots(_ context.Context, slot string) ([]SnapshotInfo, error) {
	return []SnapshotInfo{{Name: "initial", CreatedUnix: 1}}, nil
}
func (f *fakeService) Promote(context.Context) (PromoteResponse, error) {
	return PromoteResponse{From: "a", To: "b"}, nil
}
func (f *fakeService) Snapshot(_ context.Context, req SnapshotRequest) (OpResponse, error) {
	f.lastSnap = SnapshotReqSeen{req: req, set: true}
	return OpResponse{Message: "ok"}, nil
}
func (f *fakeService) Restore(context.Context, RestoreRequest) (OpResponse, error) {
	return OpResponse{Message: "restored"}, nil
}
func (f *fakeService) Reconcile(context.Context) (ReconcileResponse, error) {
	return ReconcileResponse{ProxyRunning: true, Actions: []string{"main → x"}}, nil
}
func (f *fakeService) Up(context.Context) (StatusResponse, error) {
	return StatusResponse{Active: "a"}, nil
}
func (f *fakeService) Down(context.Context) (OpResponse, error) {
	return OpResponse{Message: "down"}, nil
}
func (f *fakeService) StartStaging(context.Context) (OpResponse, error) {
	return OpResponse{Message: "started pg-dev-b (PostgreSQL ready)"}, nil
}
func (f *fakeService) StopStaging(context.Context) (OpResponse, error) {
	return OpResponse{Message: "stopped pg-dev-b"}, nil
}

func newTest(t *testing.T, token string) (*Client, *fakeService, func()) {
	t.Helper()
	fake := &fakeService{}
	ts := httptest.NewServer(NewServer(fake, "secret"))
	return NewClient(ts.URL, token), fake, ts.Close
}

func TestUnauthorizedWithoutToken(t *testing.T) {
	cl, _, done := newTest(t, "wrong")
	defer done()
	if _, err := cl.Status(context.Background()); err == nil {
		t.Fatal("expected 401 with a wrong token")
	}
}

func TestHealthNeedsNoAuth(t *testing.T) {
	cl, _, done := newTest(t, "") // empty token
	defer done()
	if !cl.Healthy(context.Background()) {
		t.Fatal("healthz should not require auth")
	}
}

func TestVersionHandshake(t *testing.T) {
	cl, _, done := newTest(t, "secret")
	defer done()
	v, err := cl.Version(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v.Version != "v1.2.3" || v.APIVersion != APIVersion {
		t.Fatalf("version = %+v", v)
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	cl, fake, done := newTest(t, "secret")
	defer done()
	_, err := cl.Snapshot(context.Background(), SnapshotRequest{Slot: "b", Name: "x", Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if !fake.lastSnap.set || fake.lastSnap.req.Slot != "b" || !fake.lastSnap.req.Force {
		t.Fatalf("server saw %+v", fake.lastSnap)
	}
}

func TestStagingLifecycleRoundTrip(t *testing.T) {
	cl, _, done := newTest(t, "secret")
	defer done()
	res, err := cl.StartStaging(context.Background())
	if err != nil || res.Message == "" {
		t.Fatalf("start staging: res=%+v err=%v", res, err)
	}
	res, err = cl.StopStaging(context.Background())
	if err != nil || res.Message == "" {
		t.Fatalf("stop staging: res=%+v err=%v", res, err)
	}
}

func TestErrorBodyPropagates(t *testing.T) {
	cl, fake, done := newTest(t, "secret")
	defer done()
	fake.failNext = errors.New("store not mounted")
	_, err := cl.Status(context.Background())
	if err == nil || err.Error() != "agent: store not mounted" {
		t.Fatalf("err = %v, want wrapped 'store not mounted'", err)
	}
}

func TestUnknownFieldsRejected(t *testing.T) {
	cl, _, done := newTest(t, "secret")
	defer done()
	// A raw POST with an unknown field should 400 (DisallowUnknownFields).
	req, _ := http.NewRequest(http.MethodPost, cl.BaseURL+"/v1/snapshot", strings.NewReader(`{"slot":"a","bogus":1}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}
