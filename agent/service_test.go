package agent_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"sync"
	"testing"

	"github.com/aungpwint/vibe-agent/agent"
	"github.com/aungpwint/vibe-agent/store"
)

var (
	liveService *agent.Service
	livePool    *store.Pool
)

func TestMain(m *testing.M) {
	dsn := os.Getenv("VIBE_AGENT_TEST_DATABASE_URL")
	if dsn == "" {
		os.Exit(m.Run())
	}

	ctx := context.Background()
	pool, err := store.Connect(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vibe-agent tests: cannot connect: %v\n", err)
		os.Exit(1)
	}
	if _, err := store.Migrate(ctx, pool); err != nil {
		fmt.Fprintf(os.Stderr, "vibe-agent tests: cannot migrate: %v\n", err)
		pool.Close()
		os.Exit(1)
	}

	livePool = pool
	liveService = agent.New(pool)

	code := m.Run()
	pool.Close()
	os.Exit(code)
}

func service(t *testing.T) *agent.Service {
	t.Helper()
	if liveService == nil {
		t.Skip("set VIBE_AGENT_TEST_DATABASE_URL to run the live database tests")
	}

	ctx := context.Background()
	if _, err := livePool.Exec(ctx, `DELETE FROM vibe_sessions`); err != nil {
		t.Fatalf("reset sessions: %v", err)
	}
	if _, err := livePool.Exec(ctx, `DELETE FROM vibe_profiles WHERE builtin = false`); err != nil {
		t.Fatalf("reset profiles: %v", err)
	}
	return liveService
}

func ptr[T any](value T) *T { return &value }

func wantCode(t *testing.T, err error, code agent.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a %s error, got nil", code)
	}
	if got := agent.CodeOf(err); got != code {
		t.Fatalf("error code = %s, want %s (error: %v)", got, code, err)
	}
}

func mustSession(t *testing.T, svc *agent.Service, params agent.CreateSessionParams) agent.Session {
	t.Helper()
	session, err := svc.CreateSession(context.Background(), params)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return session
}

func mustTurn(t *testing.T, svc *agent.Service, params agent.AppendTurnParams) agent.Turn {
	t.Helper()
	turn, err := svc.AppendTurn(context.Background(), params)
	if err != nil {
		t.Fatalf("AppendTurn: %v", err)
	}
	return turn
}

func appendText(t *testing.T, svc *agent.Service, sessionID agent.ID, role agent.TurnRole, text string) agent.Turn {
	t.Helper()
	return mustTurn(t, svc, agent.AppendTurnParams{
		SessionID: sessionID,
		Role:      role,
		Parts:     []agent.Part{{Type: "text", Text: text}},
	})
}

func TestSessionLifecycle(t *testing.T) {
	svc := service(t)
	ctx := context.Background()

	session := mustSession(t, svc, agent.CreateSessionParams{Title: "Lifecycle", AgentSlug: "build"})
	if session.Status != agent.SessionOpen {
		t.Fatalf("new session status = %s, want open", session.Status)
	}
	if session.AgentSlug != "build" {
		t.Fatalf("agent slug = %s, want build", session.AgentSlug)
	}

	first := appendText(t, svc, session.ID, agent.TurnUser, "first message")
	if first.Sequence != 1 {
		t.Fatalf("first turn sequence = %d, want 1", first.Sequence)
	}
	if first.Status != agent.TurnComplete || first.Body == nil || *first.Body != "first message" {
		t.Fatalf("first turn = %+v, want a complete turn carrying the body", first)
	}

	archived, err := svc.SetSessionArchived(ctx, session.ID, true)
	if err != nil {
		t.Fatalf("SetSessionArchived: %v", err)
	}
	if archived.ArchivedAt == nil {
		t.Fatal("archiving did not set archivedAt")
	}

	// Archiving is a filing decision, not a state change: the session stays open
	// and can still take turns.
	appendText(t, svc, session.ID, agent.TurnUser, "message while archived")

	active, err := svc.Sessions(ctx, 10, agent.SessionQuery{Archived: agent.ActiveOnly})
	if err != nil {
		t.Fatalf("Sessions(active): %v", err)
	}
	if len(active.Items) != 0 {
		t.Fatalf("active list holds %d sessions, want 0", len(active.Items))
	}
	onlyArchived, err := svc.Sessions(ctx, 10, agent.SessionQuery{Archived: agent.ArchivedOnly})
	if err != nil {
		t.Fatalf("Sessions(archived): %v", err)
	}
	if len(onlyArchived.Items) != 1 {
		t.Fatalf("archived list holds %d sessions, want 1", len(onlyArchived.Items))
	}

	closed, err := svc.CloseSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	if closed.Status != agent.SessionClosed || closed.ClosedAt == nil {
		t.Fatalf("closed session = %+v", closed)
	}
	_, err = svc.AppendTurn(ctx, agent.AppendTurnParams{
		SessionID: session.ID,
		Parts:     []agent.Part{{Type: "text", Text: "too late"}},
	})
	wantCode(t, err, agent.CodeConflict)

	reopened, err := svc.ReopenSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("ReopenSession: %v", err)
	}
	if reopened.Status != agent.SessionOpen || reopened.ClosedAt != nil {
		t.Fatalf("reopened session = %+v", reopened)
	}

	if err := svc.DeleteSession(ctx, session.ID); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := svc.Session(ctx, session.ID); !errors.Is(err, agent.ErrSessionNotFound) {
		t.Fatalf("Session after delete = %v, want ErrSessionNotFound", err)
	}
}

func TestSessionTreeAndDepthCap(t *testing.T) {
	svc := service(t)
	ctx := context.Background()

	root := mustSession(t, svc, agent.CreateSessionParams{Title: "Root", AgentSlug: "build"})

	_, err := svc.CreateSession(ctx, agent.CreateSessionParams{
		Title:     "Primary child",
		AgentSlug: "build",
		ParentID:  &root.ID,
	})
	wantCode(t, err, agent.CodeValidation)

	child := mustSession(t, svc, agent.CreateSessionParams{
		Title:     "Child",
		AgentSlug: "general",
		ParentID:  &root.ID,
	})
	if child.Depth != 1 || child.ParentID == nil || *child.ParentID != root.ID {
		t.Fatalf("child = %+v, want depth 1 under root", child)
	}
	if child.RootID == nil || *child.RootID != root.ID {
		t.Fatalf("child root = %v, want %s", child.RootID, root.ID)
	}

	grandchild := mustSession(t, svc, agent.CreateSessionParams{
		Title:     "Grandchild",
		AgentSlug: "explore",
		ParentID:  &child.ID,
	})
	if grandchild.Depth != 2 || grandchild.RootID == nil || *grandchild.RootID != root.ID {
		t.Fatalf("grandchild = %+v", grandchild)
	}

	children, err := svc.Sessions(ctx, 10, agent.SessionQuery{ParentID: &root.ID, Archived: agent.ActiveOnly})
	if err != nil {
		t.Fatalf("Sessions(children): %v", err)
	}
	if len(children.Items) != 1 || children.Items[0].ID != child.ID {
		t.Fatalf("children = %+v", children.Items)
	}

	if _, err := svc.CloseSession(ctx, child.ID); err != nil {
		t.Fatalf("CloseSession(child): %v", err)
	}
	_, err = svc.CreateSession(ctx, agent.CreateSessionParams{
		Title:     "Child of closed",
		AgentSlug: "general",
		ParentID:  &child.ID,
	})
	wantCode(t, err, agent.CodeConflict)

	// The tree cap is a refused write at depth 33, not an unbounded walk.
	parent := root
	for depth := 1; depth <= 32; depth++ {
		parent = mustSession(t, svc, agent.CreateSessionParams{
			Title:     fmt.Sprintf("Depth %d", depth),
			AgentSlug: "general",
			ParentID:  &parent.ID,
		})
	}
	if parent.Depth != 32 {
		t.Fatalf("deepest session depth = %d, want 32", parent.Depth)
	}
	_, err = svc.CreateSession(ctx, agent.CreateSessionParams{
		Title:     "Depth 33",
		AgentSlug: "general",
		ParentID:  &parent.ID,
	})
	wantCode(t, err, agent.CodeValidation)
}

func TestTurnSequencingAndIdempotency(t *testing.T) {
	svc := service(t)
	ctx := context.Background()

	session := mustSession(t, svc, agent.CreateSessionParams{Title: "Turns", AgentSlug: "build"})
	for i, role := range []agent.TurnRole{agent.TurnUser, agent.TurnAssistant, agent.TurnUser} {
		turn := appendText(t, svc, session.ID, role, fmt.Sprintf("turn %d", i+1))
		if turn.Sequence != int64(i+1) {
			t.Fatalf("turn %d sequence = %d, want %d", i+1, turn.Sequence, i+1)
		}
	}

	refreshed, err := svc.Session(ctx, session.ID)
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if refreshed.NextSequence != 3 || refreshed.TurnCount != 3 {
		t.Fatalf("session counters = next %d / turns %d, want 3/3", refreshed.NextSequence, refreshed.TurnCount)
	}

	params := agent.AppendTurnParams{
		SessionID: session.ID,
		Parts:     []agent.Part{{Type: "text", Text: "retried write"}},
		ClientID:  ptr("retry-key"),
	}
	first := mustTurn(t, svc, params)
	second := mustTurn(t, svc, params)
	if first.ID != second.ID || first.Sequence != second.Sequence {
		t.Fatalf("a retry produced a second turn: %s#%d and %s#%d",
			first.ID, first.Sequence, second.ID, second.Sequence)
	}

	refreshed, err = svc.Session(ctx, session.ID)
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if refreshed.NextSequence != 4 || refreshed.TurnCount != 4 {
		t.Fatalf("counters after retry = next %d / turns %d, want 4/4",
			refreshed.NextSequence, refreshed.TurnCount)
	}
}

func TestConcurrentAppendsGetUniqueSequences(t *testing.T) {
	svc := service(t)
	ctx := context.Background()

	session := mustSession(t, svc, agent.CreateSessionParams{Title: "Concurrent", AgentSlug: "build"})

	const writers = 8
	sequences := make([]int64, writers)
	errs := make([]error, writers)

	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			turn, err := svc.AppendTurn(context.Background(), agent.AppendTurnParams{
				SessionID: session.ID,
				Parts:     []agent.Part{{Type: "text", Text: fmt.Sprintf("writer %d", i)}},
				ClientID:  ptr(fmt.Sprintf("writer-%d", i)),
			})
			if err != nil {
				errs[i] = err
				return
			}
			sequences[i] = turn.Sequence
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d failed: %v", i, err)
		}
	}

	sort.Slice(sequences, func(i, j int) bool { return sequences[i] < sequences[j] })
	for i, sequence := range sequences {
		if sequence != int64(i+1) {
			t.Fatalf("sequences = %v, want 1..%d without gaps", sequences, writers)
		}
	}

	refreshed, err := svc.Session(ctx, session.ID)
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if refreshed.TurnCount != writers || refreshed.NextSequence != int64(writers) {
		t.Fatalf("counters = next %d / turns %d, want %d", refreshed.NextSequence, refreshed.TurnCount, writers)
	}
}

func TestCompleteTurnMovesTokenTotalsForward(t *testing.T) {
	svc := service(t)
	ctx := context.Background()

	session := mustSession(t, svc, agent.CreateSessionParams{Title: "Streaming", AgentSlug: "build"})
	streaming := mustTurn(t, svc, agent.AppendTurnParams{
		SessionID: session.ID,
		Role:      agent.TurnAssistant,
		Status:    agent.TurnStreaming,
		Parts:     []agent.Part{{Type: "text", Text: "partial"}},
		Tokens:    agent.Tokens{Prompt: 5, Completion: 1, CostMicros: 10},
	})
	if streaming.CompletedAt != nil {
		t.Fatal("a streaming turn already has completedAt")
	}

	_, err := svc.CompleteTurn(ctx, agent.CompleteTurnParams{
		ID:     streaming.ID,
		Status: agent.TurnComplete,
		Tokens: &agent.Tokens{Prompt: 3, Completion: 1, CostMicros: 10},
	})
	wantCode(t, err, agent.CodeValidation)

	completed, err := svc.CompleteTurn(ctx, agent.CompleteTurnParams{
		ID:     streaming.ID,
		Status: agent.TurnComplete,
		Parts:  []agent.Part{{Type: "text", Text: "final answer"}},
		Tokens: &agent.Tokens{Prompt: 20, Completion: 8, CostMicros: 200},
	})
	if err != nil {
		t.Fatalf("CompleteTurn: %v", err)
	}
	if completed.Status != agent.TurnComplete || completed.CompletedAt == nil {
		t.Fatalf("completed turn = %+v", completed)
	}
	if completed.Body == nil || *completed.Body != "final answer" {
		t.Fatalf("body = %v, want the replacement text", completed.Body)
	}

	refreshed, err := svc.Session(ctx, session.ID)
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	want := agent.Tokens{Prompt: 20, Completion: 8, CostMicros: 200}
	if refreshed.Tokens != want {
		t.Fatalf("session tokens = %+v, want %+v", refreshed.Tokens, want)
	}

	again, err := svc.CompleteTurn(ctx, agent.CompleteTurnParams{ID: streaming.ID, Status: agent.TurnComplete})
	if err != nil {
		t.Fatalf("CompleteTurn again: %v", err)
	}
	if again.ID != completed.ID {
		t.Fatalf("idempotent completion returned %s, want %s", again.ID, completed.ID)
	}
	_, err = svc.CompleteTurn(ctx, agent.CompleteTurnParams{ID: streaming.ID, Status: agent.TurnError})
	wantCode(t, err, agent.CodeConflict)
}

func TestRunsEnforceOneLiveRun(t *testing.T) {
	svc := service(t)
	ctx := context.Background()

	session := mustSession(t, svc, agent.CreateSessionParams{Title: "Runs", AgentSlug: "build"})

	run, err := svc.StartRun(ctx, agent.StartRunParams{SessionID: session.ID})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if run.Status != agent.RunRunning || run.StartedAt == nil {
		t.Fatalf("run = %+v, want running with a start time", run)
	}
	_, err = svc.StartRun(ctx, agent.StartRunParams{SessionID: session.ID})
	wantCode(t, err, agent.CodeConflict)

	finished, err := svc.FinishRun(ctx, agent.FinishRunParams{
		ID:     run.ID,
		Status: agent.RunCompleted,
		Steps:  4,
		Tokens: agent.Tokens{Prompt: 12, Completion: 6, CostMicros: 120},
	})
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	if finished.Status != agent.RunCompleted || finished.FinishedAt == nil {
		t.Fatalf("finished run = %+v", finished)
	}

	stats, err := svc.RunStats(ctx, session.ID)
	if err != nil {
		t.Fatalf("RunStats: %v", err)
	}
	if stats.Total != 1 || stats.Completed != 1 || stats.Steps != 4 {
		t.Fatalf("stats = %+v", stats)
	}
	if stats.Tokens != (agent.Tokens{Prompt: 12, Completion: 6, CostMicros: 120}) {
		t.Fatalf("stats tokens = %+v", stats.Tokens)
	}

	again, err := svc.FinishRun(ctx, agent.FinishRunParams{ID: run.ID, Status: agent.RunCompleted})
	if err != nil {
		t.Fatalf("FinishRun again: %v", err)
	}
	if again.ID != finished.ID {
		t.Fatalf("idempotent finish returned %s, want %s", again.ID, finished.ID)
	}
	_, err = svc.FinishRun(ctx, agent.FinishRunParams{ID: run.ID, Status: agent.RunFailed})
	wantCode(t, err, agent.CodeConflict)

	second, err := svc.StartRun(ctx, agent.StartRunParams{SessionID: session.ID})
	if err != nil {
		t.Fatalf("second StartRun: %v", err)
	}
	cancelled, err := svc.CancelRun(ctx, second.ID)
	if err != nil {
		t.Fatalf("CancelRun: %v", err)
	}
	if cancelled.Status != agent.RunCancelled || cancelled.FinishedAt == nil {
		t.Fatalf("cancelled run = %+v", cancelled)
	}

	other := mustSession(t, svc, agent.CreateSessionParams{Title: "Other", AgentSlug: "build"})
	third, err := svc.StartRun(ctx, agent.StartRunParams{SessionID: session.ID})
	if err != nil {
		t.Fatalf("third StartRun: %v", err)
	}
	_, err = svc.FinishRun(ctx, agent.FinishRunParams{
		ID:             third.ID,
		Status:         agent.RunCompleted,
		ChildSessionID: &other.ID,
	})
	wantCode(t, err, agent.CodeValidation)
}

func TestForkCopiesTranscriptUpToSequence(t *testing.T) {
	svc := service(t)
	ctx := context.Background()

	session := mustSession(t, svc, agent.CreateSessionParams{Title: "Original", AgentSlug: "build"})
	for i := 1; i <= 3; i++ {
		appendText(t, svc, session.ID, agent.TurnUser, fmt.Sprintf("turn %d", i))
	}

	at := int64(2)
	fork, err := svc.ForkSession(ctx, agent.ForkSessionParams{ID: session.ID, AtSequence: &at})
	if err != nil {
		t.Fatalf("ForkSession: %v", err)
	}
	if fork.ForkedFromID == nil || *fork.ForkedFromID != session.ID {
		t.Fatalf("fork source = %v, want %s", fork.ForkedFromID, session.ID)
	}
	if fork.ForkedFromSequence == nil || *fork.ForkedFromSequence != 2 {
		t.Fatalf("fork sequence = %v, want 2", fork.ForkedFromSequence)
	}
	if fork.TurnCount != 2 || fork.NextSequence != 2 {
		t.Fatalf("fork counters = %d/%d, want 2/2", fork.TurnCount, fork.NextSequence)
	}

	page, err := svc.Turns(ctx, agent.TurnQuery{SessionID: fork.ID})
	if err != nil {
		t.Fatalf("Turns(fork): %v", err)
	}
	if len(page.Items) != 2 || page.Items[0].Sequence != 1 || page.Items[1].Sequence != 2 {
		t.Fatalf("fork transcript = %+v", page.Items)
	}
	if page.Items[0].Body == nil || *page.Items[0].Body != "turn 1" {
		t.Fatalf("fork first body = %v", page.Items[0].Body)
	}

	next := appendText(t, svc, fork.ID, agent.TurnUser, "after the fork")
	if next.Sequence != 3 {
		t.Fatalf("turn appended to fork has sequence %d, want 3", next.Sequence)
	}

	full, err := svc.ForkSession(ctx, agent.ForkSessionParams{ID: session.ID, Title: ptr("Full copy")})
	if err != nil {
		t.Fatalf("ForkSession(full): %v", err)
	}
	if full.TurnCount != 3 || full.NextSequence != 3 {
		t.Fatalf("full fork counters = %d/%d, want 3/3", full.TurnCount, full.NextSequence)
	}

	tooFar := int64(4)
	_, err = svc.ForkSession(ctx, agent.ForkSessionParams{ID: session.ID, AtSequence: &tooFar})
	wantCode(t, err, agent.CodeValidation)
}

func TestExportImportRoundTrip(t *testing.T) {
	svc := service(t)
	ctx := context.Background()

	session := mustSession(t, svc, agent.CreateSessionParams{
		Title:     "Exportable",
		AgentSlug: "build",
		Model:     ptr("model-a"),
		Workspace: ptr("C:/work/project"),
	})
	appendText(t, svc, session.ID, agent.TurnUser, "first")
	mustTurn(t, svc, agent.AppendTurnParams{
		SessionID: session.ID,
		Role:      agent.TurnAssistant,
		Parts:     []agent.Part{{Type: "text", Text: "second"}},
		Tokens:    agent.Tokens{Prompt: 11, Completion: 7, CostMicros: 70},
	})

	doc, err := svc.ExportSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("ExportSession: %v", err)
	}
	if doc.Version != agent.ExportVersion || len(doc.Turns) != 2 {
		t.Fatalf("export = version %d with %d turns", doc.Version, len(doc.Turns))
	}

	turns := make([]agent.ImportTurn, 0, len(doc.Turns))
	for _, turn := range doc.Turns {
		turns = append(turns, agent.ImportTurn{
			Sequence:    turn.Sequence,
			Role:        turn.Role,
			Status:      turn.Status,
			Parts:       turn.Parts,
			Model:       turn.Model,
			Tokens:      turn.Tokens,
			CreatedAt:   turn.CreatedAt,
			CompletedAt: turn.CompletedAt,
		})
	}

	imported, err := svc.ImportSession(ctx, agent.ImportSessionParams{
		Title:       "Imported copy",
		AgentSlug:   doc.Session.AgentSlug,
		Model:       doc.Session.Model,
		Workspace:   doc.Session.Workspace,
		Metadata:    doc.Session.Metadata,
		Permissions: doc.Session.Permissions,
		Turns:       turns,
	})
	if err != nil {
		t.Fatalf("ImportSession: %v", err)
	}
	if imported.ID == session.ID {
		t.Fatal("import reused the original session id")
	}
	if imported.Title != "Imported copy" {
		t.Fatalf("imported title = %q", imported.Title)
	}
	if imported.TurnCount != 2 || imported.NextSequence != 2 {
		t.Fatalf("imported counters = %d/%d, want 2/2", imported.TurnCount, imported.NextSequence)
	}
	if imported.Tokens != doc.Session.Tokens {
		t.Fatalf("imported tokens = %+v, want %+v", imported.Tokens, doc.Session.Tokens)
	}

	page, err := svc.Turns(ctx, agent.TurnQuery{SessionID: imported.ID})
	if err != nil {
		t.Fatalf("Turns(imported): %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("imported transcript holds %d turns, want 2", len(page.Items))
	}
	if page.Items[1].Body == nil || *page.Items[1].Body != "second" {
		t.Fatalf("imported second body = %v", page.Items[1].Body)
	}
}

func TestImportPreservesSequenceGaps(t *testing.T) {
	svc := service(t)
	ctx := context.Background()

	imported, err := svc.ImportSession(ctx, agent.ImportSessionParams{
		Title:     "Gapped",
		AgentSlug: "build",
		Turns: []agent.ImportTurn{
			{Sequence: 2, Role: agent.TurnUser, Status: agent.TurnComplete,
				Parts: []agent.Part{{Type: "text", Text: "second"}}},
			{Sequence: 5, Role: agent.TurnAssistant, Status: agent.TurnComplete,
				Parts: []agent.Part{{Type: "text", Text: "fifth"}}},
		},
	})
	if err != nil {
		t.Fatalf("ImportSession: %v", err)
	}
	if imported.NextSequence != 5 || imported.TurnCount != 2 {
		t.Fatalf("imported counters = %d/%d, want 5/2", imported.NextSequence, imported.TurnCount)
	}

	next := appendText(t, svc, imported.ID, agent.TurnUser, "sixth")
	if next.Sequence != 6 {
		t.Fatalf("append after gap has sequence %d, want 6", next.Sequence)
	}
}

func TestSearchFindsTranscriptTextAndSkipsRedactedTurns(t *testing.T) {
	svc := service(t)
	ctx := context.Background()

	session := mustSession(t, svc, agent.CreateSessionParams{Title: "Searchable", AgentSlug: "build"})
	first := appendText(t, svc, session.ID, agent.TurnUser, "the quick brown fox jumps over the lazy dog")
	appendText(t, svc, session.ID, agent.TurnUser, "an unrelated grocery list")
	appendText(t, svc, session.ID, agent.TurnAssistant, "quick brown fox appears again")

	page, err := svc.SearchTurns(ctx, agent.TurnSearch{Text: "quick brown"})
	if err != nil {
		t.Fatalf("SearchTurns: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("search found %d turns, want 2", len(page.Items))
	}

	short, err := svc.SearchTurns(ctx, agent.TurnSearch{Text: "qu"})
	if err != nil {
		t.Fatalf("SearchTurns(short): %v", err)
	}
	if len(short.Items) != 0 {
		t.Fatalf("a two-character search found %d turns", len(short.Items))
	}

	assistant := agent.TurnAssistant
	page, err = svc.SearchTurns(ctx, agent.TurnSearch{Text: "quick brown", Role: &assistant})
	if err != nil {
		t.Fatalf("SearchTurns(role): %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].Role != agent.TurnAssistant {
		t.Fatalf("role-filtered search = %+v", page.Items)
	}

	other := mustSession(t, svc, agent.CreateSessionParams{Title: "Other", AgentSlug: "build"})
	page, err = svc.SearchTurns(ctx, agent.TurnSearch{Text: "quick brown", SessionID: &other.ID})
	if err != nil {
		t.Fatalf("SearchTurns(session): %v", err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("session-filtered search found %d turns", len(page.Items))
	}

	if _, err := svc.RedactTurn(ctx, first.ID); err != nil {
		t.Fatalf("RedactTurn: %v", err)
	}
	page, err = svc.SearchTurns(ctx, agent.TurnSearch{Text: "quick brown"})
	if err != nil {
		t.Fatalf("SearchTurns(after redaction): %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("search after redaction found %d turns, want 1", len(page.Items))
	}
}

func TestRedactTurnKeepsItsPlaceInTheTranscript(t *testing.T) {
	svc := service(t)
	ctx := context.Background()

	session := mustSession(t, svc, agent.CreateSessionParams{Title: "Redaction", AgentSlug: "build"})
	turn := mustTurn(t, svc, agent.AppendTurnParams{
		SessionID: session.ID,
		Parts:     []agent.Part{{Type: "text", Text: "a secret"}},
		Tokens:    agent.Tokens{Prompt: 7, Completion: 3, CostMicros: 70},
	})

	redacted, err := svc.RedactTurn(ctx, turn.ID)
	if err != nil {
		t.Fatalf("RedactTurn: %v", err)
	}
	if redacted.RedactedAt == nil {
		t.Fatal("redaction did not set redactedAt")
	}
	if redacted.Body != nil || len(redacted.Parts) != 0 {
		t.Fatalf("redacted turn still carries content: %+v", redacted)
	}
	if redacted.Sequence != turn.Sequence {
		t.Fatalf("redacted turn moved from sequence %d to %d", turn.Sequence, redacted.Sequence)
	}

	refreshed, err := svc.Session(ctx, session.ID)
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if refreshed.Tokens != turn.Tokens {
		t.Fatalf("redaction changed token totals to %+v, want %+v", refreshed.Tokens, turn.Tokens)
	}

	again, err := svc.RedactTurn(ctx, turn.ID)
	if err != nil {
		t.Fatalf("RedactTurn again: %v", err)
	}
	if again.ID != redacted.ID {
		t.Fatalf("idempotent redaction returned %s", again.ID)
	}
}

func TestProfileLifecycle(t *testing.T) {
	svc := service(t)
	ctx := context.Background()

	created, err := svc.CreateProfile(ctx, agent.CreateProfileParams{
		Slug:        "reviewer",
		Name:        "Reviewer",
		Description: "Reviews patches",
		Mode:        agent.ModeSubagent,
		Tools:       []string{"read", "grep", "read"},
		Permissions: agent.Ruleset{
			{Action: "edit", Resource: "*", Effect: agent.EffectDeny},
		},
		Metadata: map[string]any{"team": "local"},
	})
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	if created.Builtin || created.Mode != agent.ModeSubagent {
		t.Fatalf("created profile = %+v", created)
	}
	if len(created.Tools) != 2 {
		t.Fatalf("tools = %v, want duplicates removed", created.Tools)
	}

	_, err = svc.CreateProfile(ctx, agent.CreateProfileParams{Slug: "reviewer", Name: "Again"})
	wantCode(t, err, agent.CodeConflict)

	// A profile with no tools and no permissions must still store as JSON
	// arrays, not SQL NULLs.
	sparse, err := svc.CreateProfile(ctx, agent.CreateProfileParams{Slug: "sparse", Name: "Sparse"})
	if err != nil {
		t.Fatalf("CreateProfile without tools: %v", err)
	}
	if len(sparse.Tools) != 0 || len(sparse.Permissions) != 0 {
		t.Fatalf("sparse profile = %+v", sparse)
	}

	builtin, err := svc.ProfileBySlug(ctx, "build")
	if err != nil {
		t.Fatalf("ProfileBySlug(build): %v", err)
	}
	_, err = svc.UpdateProfile(ctx, agent.UpdateProfileParams{ID: builtin.ID, Name: ptr("Changed")})
	wantCode(t, err, agent.CodeConflict)
	_, err = svc.ArchiveProfile(ctx, "build")
	wantCode(t, err, agent.CodeConflict)

	archived, err := svc.ArchiveProfile(ctx, "reviewer")
	if err != nil {
		t.Fatalf("ArchiveProfile: %v", err)
	}
	if archived.ArchivedAt == nil {
		t.Fatal("archiving did not set archivedAt")
	}
	_, err = svc.CreateSession(ctx, agent.CreateSessionParams{Title: "Uses archived", AgentSlug: "reviewer"})
	wantCode(t, err, agent.CodeValidation)

	restored, err := svc.RestoreProfile(ctx, "reviewer")
	if err != nil {
		t.Fatalf("RestoreProfile: %v", err)
	}
	if restored.ArchivedAt != nil {
		t.Fatal("restoring did not clear archivedAt")
	}

	updated, err := svc.UpdateProfile(ctx, agent.UpdateProfileParams{
		ID:          created.ID,
		Name:        ptr("Reviewer Prime"),
		Tools:       &[]string{"read"},
		Permissions: &agent.Ruleset{{Action: "edit", Resource: "src/**", Effect: agent.EffectAllow}},
		Metadata:    &map[string]any{"team": "local", "tier": "1"},
		Temperature: ptr(0.2),
	})
	if err != nil {
		t.Fatalf("UpdateProfile: %v", err)
	}
	if updated.Name != "Reviewer Prime" || len(updated.Tools) != 1 {
		t.Fatalf("updated profile = %+v", updated)
	}
	if updated.Permissions.Decide("edit", "src/main.go") != agent.EffectAllow {
		t.Fatalf("updated permissions = %+v", updated.Permissions)
	}
	if updated.Temperature == nil || math.Abs(*updated.Temperature-0.2) > 1e-6 {
		t.Fatalf("updated temperature = %v", updated.Temperature)
	}

	_, err = svc.CreateProfile(ctx, agent.CreateProfileParams{Slug: "Not A Slug", Name: "Bad"})
	wantCode(t, err, agent.CodeValidation)
}

func TestSessionModelChangeAppendsASystemTurn(t *testing.T) {
	svc := service(t)
	ctx := context.Background()

	session := mustSession(t, svc, agent.CreateSessionParams{
		Title:     "Model change",
		AgentSlug: "build",
		Model:     ptr("model-a"),
	})

	updated, err := svc.UpdateSession(ctx, agent.UpdateSessionParams{
		ID:    session.ID,
		Model: ptr("model-b"),
		Title: ptr("Renamed session"),
	})
	if err != nil {
		t.Fatalf("UpdateSession: %v", err)
	}
	if updated.Model == nil || *updated.Model != "model-b" || updated.Title != "Renamed session" {
		t.Fatalf("updated session = %+v", updated)
	}

	page, err := svc.Turns(ctx, agent.TurnQuery{SessionID: session.ID})
	if err != nil {
		t.Fatalf("Turns: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("model change appended %d turns, want 1", len(page.Items))
	}
	turn := page.Items[0]
	if turn.Role != agent.TurnSystem || len(turn.Parts) != 1 || turn.Parts[0].Type != "model" {
		t.Fatalf("model-change turn = %+v", turn)
	}
	if turn.Parts[0].Model != "model-b" {
		t.Fatalf("model part names %q, want model-b", turn.Parts[0].Model)
	}

	// Writing the same model again is a no-op.
	if _, err := svc.UpdateSession(ctx, agent.UpdateSessionParams{ID: session.ID, Model: ptr("model-b")}); err != nil {
		t.Fatalf("UpdateSession(same model): %v", err)
	}
	page, err = svc.Turns(ctx, agent.TurnQuery{SessionID: session.ID})
	if err != nil {
		t.Fatalf("Turns: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("rewriting the same model appended a turn: %+v", page.Items)
	}

	// Clearing the model records nothing but does clear the column.
	cleared, err := svc.UpdateSession(ctx, agent.UpdateSessionParams{ID: session.ID, Model: ptr("")})
	if err != nil {
		t.Fatalf("UpdateSession(clear): %v", err)
	}
	if cleared.Model != nil {
		t.Fatalf("cleared model = %v, want nil", cleared.Model)
	}
	page, err = svc.Turns(ctx, agent.TurnQuery{SessionID: session.ID})
	if err != nil {
		t.Fatalf("Turns: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("clearing the model appended a turn: %+v", page.Items)
	}
}
