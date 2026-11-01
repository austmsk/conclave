package activity

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver for goose
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/austmsk/conclave/internal/agent"
	"github.com/austmsk/conclave/internal/blob"
	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/gate"
	"github.com/austmsk/conclave/internal/github"
	"github.com/austmsk/conclave/internal/llm"
	"github.com/austmsk/conclave/internal/llm/faketest"
	"github.com/austmsk/conclave/internal/sandbox"
	"github.com/austmsk/conclave/internal/sandbox/sandboxtest"
	"github.com/austmsk/conclave/internal/store"
	"github.com/austmsk/conclave/internal/testenv"
)

// The same image as deploy/docker-compose.yml.
const postgresImage = "pgvector/pgvector:0.8.6-pg16"

const (
	testRepo  = "austmsk/election-tally"
	testOwner = int64(4242)
	fakeModel = "fake-model"
)

var (
	// ownerPool connects as the database owner, for bookkeeping queries
	// the application role may not make. appPool runs every activity as
	// conclave_app, with exactly the grants a worker has.
	ownerPool *pgxpool.Pool
	appPool   *pgxpool.Pool
	repoID    string
	issueSeq  atomic.Int64
)

// TestMain starts one Postgres container for the package's database
// tests. Without Docker they fail rather than silently pass; -short skips
// them.
func TestMain(m *testing.M) {
	flag.Parse()
	if testing.Short() {
		os.Exit(m.Run())
	}
	ctx := context.Background()
	ctr, err := postgres.Run(ctx, postgresImage,
		postgres.WithDatabase("conclave"), postgres.WithUsername("conclave"),
		postgres.WithPassword("canary_conclave_test_only"), postgres.BasicWaitStrategies(),
		testcontainers.WithLogger(log.New(os.Stderr, "", 0)),
	)
	if err != nil {
		log.Fatalf("starting postgres: %v", err)
	}
	code, err := runWithDB(ctx, m, ctr)
	if terr := ctr.Terminate(ctx); terr != nil {
		log.Printf("terminating postgres: %v", terr)
	}
	if err != nil {
		log.Fatal(err)
	}
	os.Exit(code)
}

func runWithDB(ctx context.Context, m *testing.M, ctr *postgres.PostgresContainer) (int, error) {
	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return 1, fmt.Errorf("connection string: %w", err)
	}
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return 1, err
	}
	defer func() { _ = sqlDB.Close() }()
	if err := store.Migrate(ctx, sqlDB); err != nil {
		return 1, fmt.Errorf("migrating: %w", err)
	}
	if ownerPool, err = pgxpool.New(ctx, dsn); err != nil {
		return 1, err
	}
	defer ownerPool.Close()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return 1, err
	}
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, `SET ROLE conclave_app`)
		return err
	}
	if appPool, err = pgxpool.NewWithConfig(ctx, cfg); err != nil {
		return 1, err
	}
	defer appPool.Close()
	r, err := store.EnsureRepo(ctx, appPool, contracts.Repo{FullName: testRepo, Tier: contracts.TierPublic, DefaultBranch: "main"})
	if err != nil {
		return 1, fmt.Errorf("seeding repo: %w", err)
	}
	repoID = r.ID
	return m.Run(), nil
}

// fixture is one test's activities over the shared database, with fakes
// for GitHub, the sandbox provider, the model and the gate.
type fixture struct {
	t         *testing.T
	ctx       context.Context
	a         *Activities
	gh        *fakeGitHub
	sandboxes *fakeSandboxes
	model     *faketest.Provider
	blobDir   string
	issue     int

	// gateFn and rebaseFn answer Deps.Gate and Deps.Rebase when set.
	gateFn   GateFunc
	rebaseFn RebaseFunc
}

// newFixture builds activities whose model replays turns. Each fixture
// gets its own issue number, so tests share the database without
// sharing rows.
func newFixture(t *testing.T, turns ...faketest.Turn) *fixture {
	t.Helper()
	if appPool == nil {
		testenv.Skip(t, "no database: run without -short to start one")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	f := &fixture{t: t, ctx: ctx, gh: newFakeGitHub(t), sandboxes: newFakeSandboxes(), model: faketest.New(turns...), blobDir: t.TempDir()}
	f.issue = int(issueSeq.Add(1))
	blobs, err := blob.NewDir(f.blobDir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := llm.Config{
		Budgets: llm.Budgets{FeatureRequestTokens: 1 << 40, MonthlyUSD: 1e9},
		Models: map[string]llm.ModelConfig{fakeModel: {
			Provider: llm.ProviderAnthropic, InputUSDPerMTok: 3, OutputUSDPerMTok: 15, MaxOutputTokens: 4096,
		}},
		// The test writer is routed and allowed, but only the tests that
		// add it to Deps.Roles can run it.
		Roles: map[contracts.Role]llm.RoleRoute{
			contracts.RoleImplementer: {Model: fakeModel}, contracts.RoleTestWriter: {Model: fakeModel}, contracts.RolePRWriter: {Model: fakeModel},
		},
	}
	access := map[string]llm.RepoAccess{testRepo: {Tier: contracts.TierPublic, Roles: []contracts.Role{
		contracts.RoleImplementer, contracts.RoleTestWriter, contracts.RolePRWriter,
	}}}
	router, err := llm.New(cfg, map[string]llm.Provider{llm.ProviderAnthropic: f.model},
		llm.NewDirectoryRepos(store.NewRepoDirectory(appPool), access), store.NewCostLedger(appPool))
	if err != nil {
		t.Fatal(err)
	}
	f.a, err = New(Deps{
		DB: appPool, GitHub: f.gh, Trees: GitHubTrees{Client: f.gh, InstallationID: 5}, Sandboxes: f.sandboxes, Router: router, RouterConfig: cfg, Blobs: blobs,
		Gate: func(ctx context.Context, in gate.Input) (gate.Outcome, error) {
			if f.gateFn != nil {
				return f.gateFn(ctx, in)
			}
			return gate.Outcome{}, errors.New("the fixture's gate is never reached")
		},
		Rebase: func(ctx context.Context, sb contracts.Sandbox, blobs blob.Store, original gate.Artifact, diff []byte, base string) (gate.Artifact, error) {
			if f.rebaseFn != nil {
				return f.rebaseFn(ctx, sb, blobs, original, diff, base)
			}
			return gate.Artifact{}, errors.New("the fixture's rebase is never reached")
		},
		Bot:         BotIdentity{Name: "conclave-test[bot]", Email: "77+conclave-test[bot]@users.noreply.github.com"},
		Roles:       map[contracts.Role]contracts.RoleSpec{contracts.RoleImplementer: agent.Implementer, contracts.RolePRWriter: agent.PRWriter},
		Repos:       map[string]Repo{testRepo: testRepoConfig()},
		OwnerUserID: testOwner, ConfigDigests: map[string]string{"repos.yaml": "sha256:00"},
		WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func testRepoConfig() Repo {
	return Repo{
		Gates: gate.Policy{CI: gate.CIPolicy{
			RequiredChecks: []string{"build", "test"}, CheckApp: "github-actions", PollEvery: time.Minute, Timeout: time.Hour,
		}},
		Access: llm.RepoAccess{Tier: contracts.TierPublic, Roles: []contracts.Role{contracts.RoleImplementer}},
		Sandbox: sandbox.RepoSandbox{
			DefaultBranch: "main", Image: "example.invalid/sandbox@sha256:" + fmt.Sprintf("%064d", 1),
			Limits:         contracts.ResourceLimits{MemoryBytes: 1 << 30, CPUShares: 1024, DiskBytes: 1 << 30, MaxProcs: 64, Timeout: 25 * time.Minute},
			CommandTimeout: 5 * time.Minute,
		},
		Toolchain: agent.Toolchain{
			Commands: agent.Commands{Test: []string{"true"}, Build: []string{"true"}, Lint: []string{"true"}},
			Profile:  contracts.RepoProfile{FullName: testRepo, Language: "Go", LayoutHint: "src/", Conventions: "gofmt"},
		},
	}
}

// ownerIssue is an issue the owner wrote with every section.
func (f *fixture) ownerIssue() github.IssueSnapshot {
	return github.IssueSnapshot{
		Number: f.issue, Title: "Export tallies",
		Body:   "## Task\nExport tallies as CSV.\n\n## Acceptance criteria\n- one row per precinct\n- a header row\n",
		Author: github.Author{ID: testOwner, Type: "User"},
	}
}

func (f *fixture) repoRef() github.RepoRef {
	return github.RepoRef{ID: 99, FullName: testRepo, InstallationID: 5}
}

func (f *fixture) workflowID() string {
	return github.FeatureWorkflowID(99, f.issue)
}

// must fails the test on err.
func (f *fixture) must(err error) {
	f.t.Helper()
	if err != nil {
		f.t.Fatal(err)
	}
}

// analyzed runs Analyze on an owner-written issue.
func (f *fixture) analyzed() AnalyzeResult {
	f.t.Helper()
	res, err := f.a.Analyze(f.ctx, AnalyzeInput{WorkflowID: f.workflowID(), Repo: f.repoRef(), Issue: f.ownerIssue()})
	f.must(err)
	return res
}

// approved takes a fresh issue through analysis and approval, and
// returns the feature request, the plan and the pinned base.
func (f *fixture) approved() (AnalyzeResult, ApproveResult) {
	f.t.Helper()
	res := f.analyzed()
	rev, err := f.a.Transition(f.ctx, TransitionInput{
		Kind: contracts.EntityFeatureRequest, ID: res.FeatureRequestID, From: string(contracts.FeaturePlanning),
		To: string(contracts.FeatureAwaitingApproval), Revision: res.Revision, Actor: "workflow:test",
	})
	f.must(err)
	ap, err := f.a.Approve(f.ctx, ApproveInput{
		Repo: f.repoRef(), FeatureRequestID: res.FeatureRequestID, FeatureRevision: rev,
		PlanID: res.PlanID, PlanRevision: res.PlanRevision, Actor: store.ActorGitHub(testOwner),
	})
	f.must(err)
	return res, ap
}

// attempt takes a fresh issue to a prepared first attempt.
func (f *fixture) attempt() (TaskRef, AttemptRef, ApproveResult) {
	f.t.Helper()
	res, ap := f.approved()
	task, err := f.a.CreateTask(f.ctx, res.PlanID)
	f.must(err)
	ref, err := f.a.PrepareAttempt(f.ctx, PrepareAttemptInput{TaskID: task.TaskID, Ordinal: 1, Role: contracts.RoleImplementer})
	f.must(err)
	return task, ref, ap
}

// count runs a counting query as the owner.
func (f *fixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	f.must(ownerPool.QueryRow(f.ctx, query, args...).Scan(&n))
	return n
}

// transitions counts the audit rows for one record: one per state change;
// creating a row writes none.
func (f *fixture) transitions(id string) int {
	return f.count(`SELECT count(*) FROM state_transitions WHERE entity_id = $1`, id)
}

// fakeGitHub is github.Client over a local repository: Clone copies it,
// and comments live in memory. lostReplies makes the next CreateComment
// post the comment and then report a failure, as a worker that died after
// GitHub answered would.
type fakeGitHub struct {
	mu          sync.Mutex
	src         string
	head        string
	clones      int
	comments    []github.Comment
	creates     int
	edits       int
	lostReplies int

	// Delivery state (delivery_fakes_test.go): the bare remote pushes
	// land in, the pull requests opened, and the check runs to report.
	remote    string
	pushes    int
	pulls     []fakePull
	checkRuns map[string][]github.CheckRun
	readies   int
}

var _ github.Client = (*fakeGitHub)(nil)

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	dir := t.TempDir()
	sandboxtest.Git(t, dir, "init", "-q", "-b", "main", ".")
	sandboxtest.Git(t, dir, "config", "user.name", "Fixture")
	sandboxtest.Git(t, dir, "config", "user.email", "fixture@example.invalid")
	sandboxtest.WriteFile(t, dir, "README.md", "# Election Tally\n")
	sandboxtest.Git(t, dir, "add", "-A")
	sandboxtest.Git(t, dir, "commit", "-q", "-m", "initial")
	g := &fakeGitHub{src: dir, remote: t.TempDir(), checkRuns: map[string][]github.CheckRun{}}
	sandboxtest.Git(t, g.remote, "init", "-q", "--bare", ".")
	g.head = trimmed(sandboxtest.Git(t, dir, "rev-parse", "HEAD"))
	return g
}

// advance commits to the default branch, moving its head.
func (g *fakeGitHub) advance(t *testing.T) string {
	t.Helper()
	sandboxtest.WriteFile(t, g.src, "CHANGES.md", "moved\n")
	sandboxtest.Git(t, g.src, "add", "-A")
	sandboxtest.Git(t, g.src, "commit", "-q", "-m", "move")
	g.mu.Lock()
	defer g.mu.Unlock()
	g.head = trimmed(sandboxtest.Git(t, g.src, "rev-parse", "HEAD"))
	return g.head
}

// Clone copies the source repository, checks out ref, and prepares the
// tree as the real clone does.
func (g *fakeGitHub) Clone(ctx context.Context, _ github.RepoRef, dir, ref string) error {
	g.mu.Lock()
	g.clones++
	g.mu.Unlock()
	if err := os.CopyFS(dir, os.DirFS(g.src)); err != nil {
		return fmt.Errorf("fake clone: %w", err)
	}
	if ref != "" {
		if out, err := exec.CommandContext(ctx, "git", "-C", dir, "checkout", "-q", ref).CombinedOutput(); err != nil {
			return fmt.Errorf("fake clone: checking out %s: %w: %s", ref, err, out)
		}
	}
	return sandbox.PrepareTree(dir)
}

func (g *fakeGitHub) CreateComment(_ context.Context, _ github.RepoRef, _ int, body string) (github.Comment, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.creates++
	c := github.Comment{ID: int64(1000 + len(g.comments)), Body: body}
	g.comments = append(g.comments, c)
	if g.lostReplies > 0 {
		g.lostReplies--
		return github.Comment{}, errors.New("connection reset after the comment was created")
	}
	return github.Comment{ID: c.ID}, nil
}

func (g *fakeGitHub) ListIssueComments(context.Context, github.RepoRef, int) ([]github.Comment, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]github.Comment(nil), g.comments...), nil
}

// EditComment replaces the body of a comment the fake holds; an unknown
// ID is refused, as GitHub refuses a comment this App did not post.
func (g *fakeGitHub) EditComment(_ context.Context, _ github.RepoRef, id int64, body string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i := range g.comments {
		if g.comments[i].ID == id {
			g.comments[i].Body = body
			g.edits++
			return nil
		}
	}
	return fmt.Errorf("comment %d: %w", id, github.ErrRefused)
}
func (g *fakeGitHub) AddReaction(context.Context, github.RepoRef, github.ReactionTarget, github.Reaction) error {
	return errors.New("not in ticket 8")
}

// fakeSandboxes is a sandbox provider that tracks which sandboxes exist.
// Release of an unknown ID is success, as Docker's is.
type fakeSandboxes struct {
	mu       sync.Mutex
	next     int
	live     map[string]bool
	creates  int
	installs int
	releases int
	reaps    int
	files    map[string]string
}

var _ contracts.SandboxProvider = (*fakeSandboxes)(nil)

func newFakeSandboxes() *fakeSandboxes {
	return &fakeSandboxes{live: map[string]bool{}, files: map[string]string{}}
}

func (s *fakeSandboxes) Create(_ context.Context, _ contracts.SandboxSpec) (contracts.Sandbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	s.creates++
	id := fmt.Sprintf("fake-sandbox-%d", s.next)
	s.live[id] = true
	return &fakeSandbox{id: id, files: s.files}, nil
}

func (s *fakeSandboxes) CreateWithInstall(ctx context.Context, spec contracts.SandboxSpec, _ []contracts.ExecRequest) (contracts.Sandbox, []contracts.ExecResult, error) {
	s.mu.Lock()
	s.installs++
	s.mu.Unlock()
	sb, err := s.Create(ctx, spec)
	return sb, nil, err
}

func (s *fakeSandboxes) Release(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releases++
	delete(s.live, id)
	return nil
}

// Reap removes every live sandbox: the fake has no clock, so every
// sandbox counts as old enough.
func (s *fakeSandboxes) Reap(context.Context, time.Duration) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reaps++
	n := len(s.live)
	clear(s.live)
	return n, nil
}

// orphan creates a sandbox no attempt records, as a worker that died
// between Create and SetAttemptSandbox leaves behind.
func (s *fakeSandboxes) orphan() string {
	sb, _ := s.Create(context.Background(), contracts.SandboxSpec{})
	return sb.ID()
}

func (s *fakeSandboxes) liveIDs() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.live)
}

// fakeSandbox serves files from a shared map; it runs no commands.
type fakeSandbox struct {
	id    string
	files map[string]string
}

func (b *fakeSandbox) ID() string { return b.id }
func (b *fakeSandbox) Exec(context.Context, contracts.ExecRequest) (contracts.ExecResult, error) {
	return contracts.ExecResult{}, errors.New("the fake sandbox runs no commands")
}
func (b *fakeSandbox) ReadFile(_ context.Context, path string) ([]byte, error) {
	v, ok := b.files[path]
	if !ok {
		return nil, fmt.Errorf("%s: %w", path, os.ErrNotExist)
	}
	return []byte(v), nil
}
func (b *fakeSandbox) WriteFile(context.Context, string, []byte) error {
	return errors.New("the fake sandbox is read-only")
}
func (b *fakeSandbox) Diff(context.Context) ([]byte, error) { return nil, nil }
func (b *fakeSandbox) Close(context.Context) error          { return nil }

func trimmed(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
