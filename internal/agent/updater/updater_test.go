package updater

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aead.dev/minisign"
	"github.com/ylabonte/poolpilot-relay/internal/agent/cloud"
	"github.com/ylabonte/poolpilot-relay/internal/agent/state"
	"github.com/ylabonte/poolpilot-relay/internal/update"
)

// fakeChecker stands in for *cloud.Client so tests need no real control plane.
type fakeChecker struct {
	calls int
	fn    func(ctx context.Context, ver string) (cloud.UpdateCheckResult, error)
}

func (f *fakeChecker) CheckUpdate(ctx context.Context, ver string) (cloud.UpdateCheckResult, error) {
	f.calls++
	return f.fn(ctx, ver)
}

// waitIdle blocks until a background Apply has finished staging (success or
// failure), so assertions see the settled state and the httptest download
// server is not torn down under an in-flight request.
func (u *Updater) waitIdle(t *testing.T) {
	t.Helper()
	for range 400 {
		u.mu.Lock()
		staging := u.staging
		u.mu.Unlock()
		if !staging {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("staging did not settle within 2s")
}

type relOpts struct {
	current       string
	tag           string
	includeHelper bool
	tamperBinary  bool
	advisory      *cloud.UpdateAdvisory
	checkErr      error
	notEnrolled   bool
}

type env struct {
	u         *Updater
	store     *state.Store
	statePath string
	dir       string
	checker   *fakeChecker
}

const testArch = "arm64"

func sha(t *testing.T, b []byte) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "x")
	if err := os.WriteFile(f, b, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := update.FileSHA256(f)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func newEnv(t *testing.T, o relOpts) *env {
	t.Helper()
	if o.current == "" {
		o.current = "v1.3.0"
	}
	if o.tag == "" {
		o.tag = "v1.4.0"
	}
	pk, sk, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubText, _ := pk.MarshalText()

	binary := []byte("new-agent-binary")
	helper := []byte("new-helper-binary")
	agentAsset := update.AgentAsset(testArch)
	helperAsset := update.HelperAsset(testArch)

	var sb strings.Builder
	sb.WriteString(sha(t, binary) + "  " + agentAsset + "\n")
	if o.includeHelper {
		sb.WriteString(sha(t, helper) + "  " + helperAsset + "\n")
	}
	sums := []byte(sb.String())
	sig := minisign.Sign(sk, sums)

	servedBinary := binary
	if o.tamperBinary {
		servedBinary = []byte("tampered-bytes-do-not-match-sha256sums")
	}

	dl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/"+o.tag+"/sha256sums.txt.minisig"):
			w.Write(sig)
		case strings.HasSuffix(p, "/"+o.tag+"/sha256sums.txt"):
			w.Write(sums)
		case strings.HasSuffix(p, "/"+o.tag+"/"+agentAsset):
			w.Write(servedBinary)
		case strings.HasSuffix(p, "/"+o.tag+"/"+helperAsset):
			w.Write(helper)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(dl.Close)

	checker := &fakeChecker{fn: func(ctx context.Context, ver string) (cloud.UpdateCheckResult, error) {
		if o.checkErr != nil {
			return cloud.UpdateCheckResult{}, o.checkErr
		}
		res := cloud.UpdateCheckResult{RecheckAfter: 6 * 3600, Advisory: o.advisory}
		// Mirror the control plane: a target only when strictly newer.
		if cmp, err := update.CompareVersions(ver, o.tag); err == nil && cmp < 0 {
			res.Target = o.tag
		}
		return res, nil
	}}

	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	st, err := state.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !o.notEnrolled {
		if err := st.Update(func(s *state.State) { s.Cloud.FrpcToken = "test-token" }); err != nil {
			t.Fatal(err)
		}
	}
	u := New(Options{
		Store: st, Version: o.current, Dir: filepath.Join(dir, "update"),
		Arch: testArch, PubKey: string(pubText), Checker: checker,
		DLBase: dl.URL, HTTPC: http.DefaultClient, Now: time.Now,
	})
	return &env{u: u, store: st, statePath: statePath, dir: filepath.Join(dir, "update"), checker: checker}
}

func TestCheckNowFindsUpdate(t *testing.T) {
	e := newEnv(t, relOpts{tag: "v1.4.0"})
	got := e.u.CheckNow(context.Background())
	if got.Available != "v1.4.0" || got.Current != "v1.3.0" || got.CheckError != "" {
		t.Fatalf("bad status: %+v", got)
	}
	if got.LastCheck == "" {
		t.Fatal("LastCheck not set")
	}
}

func TestCheckNowUpToDate(t *testing.T) {
	e := newEnv(t, relOpts{tag: "v1.3.0"})
	if got := e.u.CheckNow(context.Background()); got.Available != "" {
		t.Fatalf("same version offered as update: %+v", got)
	}
}

func TestApplyStagesAndWritesRequestLast(t *testing.T) {
	e := newEnv(t, relOpts{tag: "v1.4.0"})
	e.u.CheckNow(context.Background())
	if err := e.u.Apply(); err != nil {
		t.Fatal(err)
	}
	e.u.waitIdle(t) // staging is async; wait for it to commit request.json
	var req update.Request
	if err := update.ReadJSON(filepath.Join(e.dir, update.RequestFile), &req); err != nil {
		t.Fatal(err)
	}
	if req.Version != "v1.4.0" || req.Binary != update.AgentAsset(testArch) || req.Helper != "" {
		t.Fatalf("bad request: %+v", req)
	}
	if _, err := os.Stat(filepath.Join(e.dir, update.StagingDir, req.Binary)); err != nil {
		t.Fatal("binary not staged")
	}
	if !e.u.Status().InProgress {
		t.Fatal("staged request must report in_progress")
	}
	if err := e.u.Apply(); !errors.Is(err, ErrInProgress) {
		t.Fatalf("second apply: want ErrInProgress, got %v", err)
	}
}

func TestApplyStagesHelperWhenInManifest(t *testing.T) {
	e := newEnv(t, relOpts{tag: "v1.4.0", includeHelper: true})
	e.u.CheckNow(context.Background())
	if err := e.u.Apply(); err != nil {
		t.Fatal(err)
	}
	e.u.waitIdle(t)
	var req update.Request
	if err := update.ReadJSON(filepath.Join(e.dir, update.RequestFile), &req); err != nil {
		t.Fatal(err)
	}
	if req.Helper != update.HelperAsset(testArch) {
		t.Fatalf("helper not requested though in manifest: %+v", req)
	}
	if _, err := os.Stat(filepath.Join(e.dir, update.StagingDir, req.Helper)); err != nil {
		t.Fatal("helper not staged")
	}
}

func TestApplyWithoutUpdate(t *testing.T) {
	e := newEnv(t, relOpts{tag: "v1.3.0"})
	e.u.CheckNow(context.Background())
	if err := e.u.Apply(); !errors.Is(err, ErrNoUpdate) {
		t.Fatalf("want ErrNoUpdate, got %v", err)
	}
}

func TestApplyRejectsTamperedBinaryAndLeavesNoRequest(t *testing.T) {
	e := newEnv(t, relOpts{tag: "v1.4.0", tamperBinary: true})
	e.u.CheckNow(context.Background())
	if err := e.u.Apply(); err != nil {
		t.Fatalf("Apply accepts and stages asynchronously; want nil, got %v", err)
	}
	e.u.waitIdle(t) // the async stage must fail verification and clean up
	if _, err := os.Stat(filepath.Join(e.dir, update.RequestFile)); !os.IsNotExist(err) {
		t.Fatal("no request.json may exist after a failed stage")
	}
	if _, err := os.Stat(filepath.Join(e.dir, update.StagingDir)); !os.IsNotExist(err) {
		t.Fatal("staging dir must be removed after a failed stage")
	}
	// The failure must be visible to the app, or a manual "Update now" 202s and
	// then silently dies.
	if got := e.u.Status(); got.LastResult == nil || got.LastResult.Status != "rejected" || got.LastResult.To != "v1.4.0" {
		t.Fatalf("a failed stage must surface a rejected last_result: %+v", got.LastResult)
	}
}

func TestBadVersionIsSkipped(t *testing.T) {
	e := newEnv(t, relOpts{tag: "v1.4.0"})
	if err := e.store.Update(func(s *state.State) { s.Update.BadVersion = "v1.4.0" }); err != nil {
		t.Fatal(err)
	}
	if got := e.u.CheckNow(context.Background()); got.Available != "" {
		t.Fatalf("bad version offered again: %+v", got)
	}
}

func TestResultIngestionSetsBadVersion(t *testing.T) {
	e := newEnv(t, relOpts{tag: "v1.4.0"})
	if err := os.MkdirAll(e.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	update.WriteJSONAtomic(filepath.Join(e.dir, update.ResultFile), update.Result{
		Status: "rolled_back", From: "v1.3.0", To: "v1.4.0",
		Error: "health_timeout", FinishedAt: time.Now(),
	})
	got := e.u.Status() // lazy ingestion
	if got.LastResult == nil || got.LastResult.Status != "rolled_back" {
		t.Fatalf("result not ingested: %+v", got)
	}
	if e.store.Get().Update.BadVersion != "v1.4.0" {
		t.Fatal("rolled_back result must set BadVersion")
	}
	if _, err := os.Stat(filepath.Join(e.dir, update.ResultFile)); !os.IsNotExist(err) {
		t.Fatal("result.json must be consumed (deleted)")
	}
}

func TestSetAutoPersists(t *testing.T) {
	e := newEnv(t, relOpts{tag: "v1.3.0"})
	if err := e.u.SetAuto(false); err != nil {
		t.Fatal(err)
	}
	if e.store.Get().AutoUpdate() {
		t.Fatal("SetAuto(false) not persisted")
	}
	if e.u.Status().Auto {
		t.Fatal("status must reflect auto=false")
	}
}

func TestAutoWindowOffsetDeterministic(t *testing.T) {
	off1 := windowOffset("agent-a", 2*time.Hour)
	off2 := windowOffset("agent-a", 2*time.Hour)
	if off1 != off2 {
		t.Fatal("offset must be deterministic per agent")
	}
	if off1 < 0 || off1 >= 2*time.Hour {
		t.Fatalf("offset out of window: %v", off1)
	}
}

func TestCheckErrorIsInformational(t *testing.T) {
	e := newEnv(t, relOpts{tag: "v1.4.0", checkErr: errors.New("boom")})
	got := e.u.CheckNow(context.Background())
	if got.CheckError != "cloud_unreachable" {
		t.Fatalf("want cloud_unreachable, got %+v", got)
	}
}

func TestNotEnrolledSkipsCheck(t *testing.T) {
	e := newEnv(t, relOpts{tag: "v1.4.0", notEnrolled: true})
	got := e.u.CheckNow(context.Background())
	if got.CheckError != "" || got.Available != "" {
		t.Fatalf("not-enrolled check must be a silent skip: %+v", got)
	}
	if e.checker.calls != 0 {
		t.Fatalf("checker must not be called when not enrolled (calls=%d)", e.checker.calls)
	}
}

func TestAdvisoryPersistsAcrossRestart(t *testing.T) {
	adv := &cloud.UpdateAdvisory{Severity: "security", Message: "auth bypass", FixedIn: "v1.5.0"}
	e := newEnv(t, relOpts{tag: "v1.4.0", advisory: adv})
	e.u.CheckNow(context.Background())
	if got := e.u.Status(); got.Advisory == nil || got.Advisory.FixedIn != "v1.5.0" {
		t.Fatalf("advisory not surfaced: %+v", got)
	}
	// Simulate a restart: reopen state from disk, build a fresh Updater, and read
	// status BEFORE any check. The advisory must already be there.
	st2, err := state.Open(e.statePath)
	if err != nil {
		t.Fatal(err)
	}
	u2 := New(Options{Store: st2, Version: "v1.3.0", Dir: e.dir, Arch: testArch, Now: time.Now})
	got := u2.Status()
	if got.Advisory == nil || got.Advisory.Severity != "security" || got.Advisory.FixedIn != "v1.5.0" {
		t.Fatalf("advisory must survive restart via state: %+v", got)
	}
	if got.Available != "v1.4.0" {
		t.Fatalf("available must survive restart via state: %+v", got)
	}
}

func TestCheckIgnoresNonNewerTarget(t *testing.T) {
	// A hostile or buggy control plane offering the SAME or an OLDER version must
	// never be treated as an update — isCandidate's strict-newer gate is the only
	// thing standing between a signed-but-not-newer target and a needless
	// (or downgrade-attempting) stage.
	e := newEnv(t, relOpts{current: "v1.3.0"})
	for _, target := range []string{"v1.3.0", "v1.2.0"} {
		e.checker.fn = func(ctx context.Context, ver string) (cloud.UpdateCheckResult, error) {
			return cloud.UpdateCheckResult{Target: target, RecheckAfter: 6 * 3600}, nil
		}
		if got := e.u.CheckNow(context.Background()); got.Available != "" {
			t.Fatalf("non-newer target %q offered as update: %+v", target, got)
		}
	}
}

func TestApplyOnDisabledUpdaterRefuses(t *testing.T) {
	e := newEnv(t, relOpts{tag: "v1.4.0"})
	e.u.CheckNow(context.Background()) // populate LastAvailable
	e.u.disabled = true
	if err := e.u.Apply(); !errors.Is(err, ErrNoUpdate) {
		t.Fatalf("a disabled updater must refuse Apply with ErrNoUpdate, got %v", err)
	}
}

func TestBadVersionClearedByNewerRelease(t *testing.T) {
	e := newEnv(t, relOpts{current: "v1.3.0", tag: "v1.5.0"})
	if err := e.store.Update(func(s *state.State) { s.Update.BadVersion = "v1.4.0" }); err != nil {
		t.Fatal(err)
	}
	// v1.5.0 is newer than the blocked v1.4.0, so the block clears and the update
	// is offered (design §4).
	if got := e.u.CheckNow(context.Background()); got.Available != "v1.5.0" {
		t.Fatalf("newer release not offered: %+v", got)
	}
	if bv := e.store.Get().Update.BadVersion; bv != "" {
		t.Fatalf("BadVersion should be cleared by a newer release, got %q", bv)
	}
}

func TestUntilNextWindowIsAFutureSlotWithin24h(t *testing.T) {
	e := newEnv(t, relOpts{current: "v1.3.0"})
	agentID := e.store.Get().AgentID
	e.u.now = func() time.Time { return time.Date(2026, 8, 15, 12, 0, 0, 0, time.Local) }
	d := e.u.untilNextWindow()
	if d <= 0 || d > 24*time.Hour {
		t.Fatalf("untilNextWindow = %v, want (0, 24h]", d)
	}
	fire := e.u.now().Add(d)
	if slot := e.u.slotStart(agentID, fire); !fire.Equal(slot) {
		t.Fatalf("next window %v is not this device's slot start (%v)", fire, slot)
	}
}
