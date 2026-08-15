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
	if err := e.u.Apply(); err == nil {
		t.Fatal("tampered binary must fail Apply")
	}
	if _, err := os.Stat(filepath.Join(e.dir, update.RequestFile)); !os.IsNotExist(err) {
		t.Fatal("no request.json may exist after a failed stage")
	}
	if _, err := os.Stat(filepath.Join(e.dir, update.StagingDir)); !os.IsNotExist(err) {
		t.Fatal("staging dir must be removed after a failed stage")
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
