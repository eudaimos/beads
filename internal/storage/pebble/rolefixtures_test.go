package pebble

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/backend/conformance"
	"github.com/steveyegge/beads/internal/types"
	publicops "github.com/steveyegge/beads/issueops"
)

// Each constructor below builds one role's conformance fixture over a fresh
// in-memory store, mirroring the Dolt package's newDolt*Fixture helpers.
// History hooks stay nil (no commit log here); CommitPending is a no-op; the
// versioned-history switch is a no-op because the store keeps no versions.

func newReaderFixture(t *testing.T, prefix string) (conformance.ReaderFixture, context.Context) {
	t.Helper()
	kit, ctx := newRoleKit(t, prefix)
	reader, err := kit.store.IssueReader()
	if err != nil {
		t.Fatalf("IssueReader(): %v", err)
	}
	return conformance.ReaderFixture{
		IssuePrefix:   kit.IssuePrefix,
		Reader:        reader,
		CreateIssue:   kit.CreateIssue,
		CreateWisp:    kit.CreateWisp,
		AddDependency: kit.AddDependency,
		AddComment: func(ctx context.Context, issueID, author, text string) error {
			_, err := kit.store.AddIssueComment(ctx, issueID, author, text)
			return err
		},
		PromoteWisp: kit.store.PromoteFromEphemeral,
	}, ctx
}

func newBatchApplyFixture(t *testing.T, prefix string) (conformance.BatchApplyFixture, context.Context) {
	t.Helper()
	kit, ctx := newRoleKit(t, prefix)
	applier, err := kit.store.BatchApplier()
	if err != nil {
		t.Fatalf("BatchApplier(): %v", err)
	}
	return conformance.BatchApplyFixture{
		IssuePrefix:   kit.IssuePrefix,
		BatchApplier:  applier,
		CreateIssue:   kit.CreateIssue,
		CreateWisp:    kit.CreateWisp,
		QueryScalar:   kit.QueryScalar,
		CommitPending: kit.CommitPending,
	}, ctx
}

func newEdgeReaderFixture(t *testing.T, prefix string) (conformance.EdgeReaderFixture, context.Context) {
	t.Helper()
	kit, ctx := newRoleKit(t, prefix)
	reader, err := kit.store.EdgeReader()
	if err != nil {
		t.Fatalf("EdgeReader(): %v", err)
	}
	return conformance.EdgeReaderFixture{
		IssuePrefix:   kit.IssuePrefix,
		EdgeReader:    reader,
		CreateIssue:   kit.CreateIssue,
		CreateWisp:    kit.CreateWisp,
		AddDependency: kit.AddDependency,
	}, ctx
}

func newDependencyEditorFixture(t *testing.T, prefix string) (conformance.DependencyEditorFixture, context.Context) {
	t.Helper()
	kit, ctx := newRoleKit(t, prefix)
	editor, err := kit.store.DependencyEditor()
	if err != nil {
		t.Fatalf("DependencyEditor(): %v", err)
	}
	return conformance.DependencyEditorFixture{
		IssuePrefix:                kit.IssuePrefix,
		Editor:                     editor,
		CreateIssue:                kit.CreateIssue,
		CreateWisp:                 kit.CreateWisp,
		AddDependency:              kit.AddDependency,
		QueryScalar:                kit.QueryScalar,
		SetJournalEnabled:          kit.store.SetEventsJournalEnabled,
		SetVersionedHistoryEnabled: func(bool) {},
	}, ctx
}

func newDeleterFixture(t *testing.T, prefix string) (conformance.DeleterFixture, context.Context) {
	t.Helper()
	kit, ctx := newRoleKit(t, prefix)
	deleter, err := kit.store.Deleter()
	if err != nil {
		t.Fatalf("Deleter(): %v", err)
	}
	return conformance.DeleterFixture{
		IssuePrefix:   kit.IssuePrefix,
		Deleter:       deleter,
		CreateIssue:   kit.CreateIssue,
		CreateWisp:    kit.CreateWisp,
		AddDependency: kit.AddDependency,
		QueryScalar:   kit.QueryScalar,
		CommitPending: kit.CommitPending,
	}, ctx
}

func newWorkspaceConfigFixture(t *testing.T, prefix string) (conformance.WorkspaceConfigFixture, context.Context) {
	t.Helper()
	kit, ctx := newRoleKit(t, prefix)
	settings, err := kit.store.WorkspaceConfig()
	if err != nil {
		t.Fatalf("WorkspaceConfig(): %v", err)
	}
	if err := kit.SetConfig(ctx, "issue_prefix", prefix); err != nil {
		t.Fatalf("seed issue_prefix: %v", err)
	}
	return conformance.WorkspaceConfigFixture{
		IssuePrefix:     kit.IssuePrefix,
		WorkspaceConfig: settings,
		SetConfig:       kit.SetConfig,
		QueryScalar:     kit.QueryScalar,
		Vocabulary:      kit.Vocabulary,
	}, ctx
}

func newStatsReporterFixture(t *testing.T, prefix string) (conformance.StatsReporterFixture, context.Context) {
	t.Helper()
	kit, ctx := newRoleKit(t, prefix)
	reporter, err := kit.store.StatsReporter()
	if err != nil {
		t.Fatalf("StatsReporter(): %v", err)
	}
	return conformance.StatsReporterFixture{
		IssuePrefix:   kit.IssuePrefix,
		StatsReporter: reporter,
		CreateIssue:   kit.CreateIssue,
		CreateWisp:    kit.CreateWisp,
		AddDependency: kit.AddDependency,
	}, ctx
}

func newReleaserFixture(t *testing.T, prefix string) (conformance.ReleaserFixture, context.Context) {
	t.Helper()
	kit, ctx := newRoleKit(t, prefix)
	releaser, err := kit.store.Releaser()
	if err != nil {
		t.Fatalf("Releaser(): %v", err)
	}
	return conformance.ReleaserFixture{
		IssuePrefix:   kit.IssuePrefix,
		Releaser:      releaser,
		CreateIssue:   kit.CreateIssue,
		CreateWisp:    kit.CreateWisp,
		QueryScalar:   kit.QueryScalar,
		CommitPending: kit.CommitPending,
	}, ctx
}

func newMetadataCASFixture(t *testing.T, prefix string) (conformance.MetadataCASFixture, context.Context) {
	t.Helper()
	kit, ctx := newRoleKit(t, prefix)
	cas, err := kit.store.MetadataCAS()
	if err != nil {
		t.Fatalf("MetadataCAS(): %v", err)
	}
	return conformance.MetadataCASFixture{
		IssuePrefix:   kit.IssuePrefix,
		MetadataCAS:   cas,
		CreateIssue:   kit.CreateIssue,
		CreateWisp:    kit.CreateWisp,
		QueryScalar:   kit.QueryScalar,
		CommitPending: kit.CommitPending,
	}, ctx
}

func newRelationsFixture(t *testing.T, prefix string) (conformance.RelationsFixture, context.Context) {
	t.Helper()
	kit, ctx := newRoleKit(t, prefix)
	relations, err := kit.store.IssueRelations()
	if err != nil {
		t.Fatalf("IssueRelations(): %v", err)
	}
	return conformance.RelationsFixture{
		IssuePrefix:   kit.IssuePrefix,
		Relations:     relations,
		CreateIssue:   kit.CreateIssue,
		CreateWisp:    kit.CreateWisp,
		AddDependency: kit.AddDependency,
		QueryScalar:   kit.QueryScalar,
	}, ctx
}

func newClaimerFixture(t *testing.T, prefix string) (conformance.ClaimerFixture, context.Context) {
	t.Helper()
	kit, ctx := newRoleKit(t, prefix)
	claimer, err := kit.store.IssueClaimer()
	if err != nil {
		t.Fatalf("IssueClaimer(): %v", err)
	}
	return conformance.ClaimerFixture{
		IssuePrefix: kit.IssuePrefix,
		Claimer:     claimer,
		CreateIssue: kit.CreateIssue,
		CreateWisp:  kit.CreateWisp,
		SetConfig:   kit.SetConfig,
		QueryScalar: kit.QueryScalar,
	}, ctx
}

// servedRoles is every role accessor this store answers with a real
// implementation rather than the generated unsupported shell.
var servedRoles = map[string]bool{
	"IssueLifecycle": true, "IssueReader": true, "BatchApplier": true, "EdgeReader": true,
	"DependencyEditor": true, "Deleter": true, "WorkspaceConfig": true, "StatsReporter": true,
	"Releaser": true, "MetadataCAS": true, "IssueRelations": true, "IssueClaimer": true,
	"Counter": true, "CycleDetector": true, "BatchCloser": true, "Commenter": true, "BatchCreator": true,
	// The commit family is answered as a no-op (vc.go), not refused.
	"Commit": true, "CommitWithConfig": true, "CommitMergeResolution": true, "CommitConfigOnly": true,
	"CommitConfigUserKVOnly": true, "CommitAll": true, "CommitPending": true,
}

// TestServedRolesAreWired proves each served accessor answers without the
// unsupported error, so the allowlist and the wiring cannot drift apart.
func TestServedRolesAreWired(t *testing.T) {
	kit, ctx := newRoleKit(t, "wired")
	s := kit.store
	checks := map[string]func() error{
		"IssueLifecycle":         func() error { _, err := s.IssueLifecycle(); return err },
		"IssueReader":            func() error { _, err := s.IssueReader(); return err },
		"BatchApplier":           func() error { _, err := s.BatchApplier(); return err },
		"EdgeReader":             func() error { _, err := s.EdgeReader(); return err },
		"DependencyEditor":       func() error { _, err := s.DependencyEditor(); return err },
		"Deleter":                func() error { _, err := s.Deleter(); return err },
		"WorkspaceConfig":        func() error { _, err := s.WorkspaceConfig(); return err },
		"StatsReporter":          func() error { _, err := s.StatsReporter(); return err },
		"Releaser":               func() error { _, err := s.Releaser(); return err },
		"MetadataCAS":            func() error { _, err := s.MetadataCAS(); return err },
		"IssueRelations":         func() error { _, err := s.IssueRelations(); return err },
		"IssueClaimer":           func() error { _, err := s.IssueClaimer(); return err },
		"Counter":                func() error { _, err := s.Counter(); return err },
		"CycleDetector":          func() error { _, err := s.CycleDetector(); return err },
		"BatchCloser":            func() error { _, err := s.BatchCloser(); return err },
		"Commenter":              func() error { _, err := s.Commenter(); return err },
		"BatchCreator":           func() error { _, err := s.BatchCreator(); return err },
		"Commit":                 func() error { return s.Commit(ctx, "probe") },
		"CommitWithConfig":       func() error { return s.CommitWithConfig(ctx, "probe") },
		"CommitMergeResolution":  func() error { return s.CommitMergeResolution(ctx, "probe") },
		"CommitConfigOnly":       func() error { return s.CommitConfigOnly(ctx, "probe") },
		"CommitConfigUserKVOnly": func() error { return s.CommitConfigUserKVOnly(ctx, "probe") },
		"CommitAll":              func() error { _, err := s.CommitAll(ctx, "probe"); return err },
		"CommitPending":          func() error { _, err := s.CommitPending(ctx, "probe"); return err },
	}
	for name := range servedRoles {
		check, ok := checks[name]
		if !ok {
			t.Fatalf("servedRoles names %q but this test has no probe for it", name)
		}
		if err := check(); err != nil {
			t.Errorf("%s(): %v", name, err)
		}
	}
	// A served role must also answer a request: the reader is the cheapest probe.
	reader, _ := s.IssueReader()
	if _, err := reader.List(ctx, publicopsListRequest()); err != nil {
		t.Errorf("IssueReader.List on an empty store: %v", err)
	}
	_ = types.StatusOpen
}

func publicopsListRequest() publicops.ListRequest { return publicops.ListRequest{} }

// wispProbe reports whether the ephemeral plane holds a row at id.
func wispProbe(s *Store) func(context.Context, string) (bool, error) {
	return func(ctx context.Context, id string) (bool, error) {
		found := false
		err := s.read(func(r kv) error {
			rec, err := getRecord(r, id)
			if err != nil {
				if isNotFound(err) {
					return nil
				}
				return err
			}
			found = rec.Plane == planeWisps
			return nil
		})
		return found, err
	}
}

func dependencyLister(s *Store) func(context.Context, string) ([]*types.Dependency, error) {
	return func(ctx context.Context, issueID string) ([]*types.Dependency, error) {
		records, err := s.GetDependenciesWithMetadata(ctx, issueID)
		if err != nil {
			return nil, err
		}
		edges := make([]*types.Dependency, 0, len(records))
		for _, record := range records {
			if record == nil {
				continue
			}
			edges = append(edges, &types.Dependency{IssueID: issueID, DependsOnID: record.ID, Type: record.DependencyType})
		}
		return edges, nil
	}
}

func newLifecycleCreateFixture(t *testing.T, prefix string) (conformance.LifecycleCreateFixture, context.Context) {
	t.Helper()
	kit, ctx := newRoleKit(t, prefix)
	lifecycle, err := kit.store.IssueLifecycle()
	if err != nil {
		t.Fatalf("IssueLifecycle(): %v", err)
	}
	if err := kit.SetConfig(ctx, "issue_prefix", prefix); err != nil {
		t.Fatalf("set issue_prefix: %v", err)
	}
	return conformance.LifecycleCreateFixture{
		IssuePrefix: kit.IssuePrefix,
		Lifecycle:   lifecycle,
		CreateIssue: kit.CreateIssue,
		GetIssue:    kit.store.GetIssue,
		WispExists:  wispProbe(kit.store),
	}, ctx
}

func newLifecycleUpdateFixture(t *testing.T, prefix string) (conformance.LifecycleUpdateFixture, context.Context) {
	t.Helper()
	kit, ctx := newRoleKit(t, prefix)
	lifecycle, err := kit.store.IssueLifecycle()
	if err != nil {
		t.Fatalf("IssueLifecycle(): %v", err)
	}
	return conformance.LifecycleUpdateFixture{
		IssuePrefix:   kit.IssuePrefix,
		Lifecycle:     lifecycle,
		CreateIssue:   kit.CreateIssue,
		CreateWisp:    kit.CreateWisp,
		AddDependency: kit.AddDependency,
		SetConfig:     kit.SetConfig,
		GetIssue:      kit.store.GetIssue,
		ListEvents: func(ctx context.Context, issueID string) ([]*types.Event, error) {
			return kit.store.GetEvents(ctx, issueID, 0)
		},
		ListDependencies: dependencyLister(kit.store),
		WispExists:       wispProbe(kit.store),
	}, ctx
}

func newLifecycleCloseReopenFixture(t *testing.T, prefix string) (conformance.LifecycleCloseReopenFixture, context.Context) {
	t.Helper()
	kit, ctx := newRoleKit(t, prefix)
	lifecycle, err := kit.store.IssueLifecycle()
	if err != nil {
		t.Fatalf("IssueLifecycle(): %v", err)
	}
	return conformance.LifecycleCloseReopenFixture{
		IssuePrefix:   kit.IssuePrefix,
		Lifecycle:     lifecycle,
		CreateIssue:   kit.CreateIssue,
		CreateWisp:    kit.CreateWisp,
		AddDependency: kit.AddDependency,
		SetConfig:     kit.SetConfig,
		QueryScalar:   kit.QueryScalar,
	}, ctx
}

func newBatchCloserFixture(t *testing.T, prefix string) (conformance.BatchCloserFixture, context.Context) {
	t.Helper()
	kit, ctx := newRoleKit(t, prefix)
	closer, err := kit.store.BatchCloser()
	if err != nil {
		t.Fatalf("BatchCloser(): %v", err)
	}
	return conformance.BatchCloserFixture{
		IssuePrefix:   kit.IssuePrefix,
		Closer:        closer,
		CreateIssue:   kit.CreateIssue,
		AddDependency: kit.AddDependency,
		CreateWisp:    kit.CreateWisp,
		AddComment: func(ctx context.Context, issueID, actor, text string) error {
			_, err := kit.store.AddIssueComment(ctx, issueID, actor, text)
			return err
		},
		QueryScalar: kit.QueryScalar,
	}, ctx
}

func newCommenterFixture(t *testing.T, prefix string) (conformance.CommenterFixture, context.Context) {
	t.Helper()
	kit, ctx := newRoleKit(t, prefix)
	commenter, err := kit.store.Commenter()
	if err != nil {
		t.Fatalf("Commenter(): %v", err)
	}
	return conformance.CommenterFixture{
		IssuePrefix: kit.IssuePrefix,
		Commenter:   commenter,
		CreateIssue: kit.CreateIssue,
		CreateWisp:  kit.CreateWisp,
		QueryScalar: kit.QueryScalar,
		// CountHistory stays nil: the store keeps no commit log.
		// ImportIssueComment is the import shape, the one path that keeps
		// the caller's stamp, which is what the hook promises.
		SeedCommentAt: func(ctx context.Context, issueID, author, text string, at time.Time) error {
			_, err := kit.store.ImportIssueComment(ctx, issueID, author, text, at)
			return err
		},
	}, ctx
}

func newBatchCreatorFixture(t *testing.T, prefix string) (conformance.BatchCreatorFixture, context.Context) {
	t.Helper()
	kit, ctx := newRoleKit(t, prefix)
	creator, err := kit.store.BatchCreator()
	if err != nil {
		t.Fatalf("BatchCreator(): %v", err)
	}
	return conformance.BatchCreatorFixture{
		IssuePrefix:  kit.IssuePrefix,
		BatchCreator: creator,
		CreateIssue:  kit.CreateIssue,
		QueryScalar:  kit.QueryScalar,
		SetConfig:    kit.SetConfig,
		// CountHistory stays nil: the store keeps no commit log.
	}, ctx
}

func newCycleDetectorFixture(t *testing.T, prefix string) (conformance.CycleDetectorFixture, context.Context) {
	t.Helper()
	kit, ctx := newRoleKit(t, prefix)
	detector, err := kit.store.CycleDetector()
	if err != nil {
		t.Fatalf("CycleDetector(): %v", err)
	}
	return conformance.CycleDetectorFixture{
		IssuePrefix: kit.IssuePrefix,
		Detector:    detector,
		CreateIssue: kit.CreateIssue,
		CreateWisp:  kit.CreateWisp,
		Exec:        execRawEdges(kit.store),
	}, ctx
}

// rawEdgeInsertRe matches the one raw statement the cycle detector contract
// seeds with: an edge inserted straight into a dependency table, which is how
// a cycle gets into a store whose verbs refuse to create one.
var rawEdgeInsertRe = regexp.MustCompile(`(?i)^INSERT INTO (dependencies|wisp_dependencies) \(id, issue_id, (depends_on_issue_id|depends_on_wisp_id|depends_on_external), type, created_at, created_by, metadata\) VALUES \(\?, \?, \?, \?, NOW\(\), 'seed', '\{\}'\)$`)

// execRawEdges answers the cycle detector fixture's Exec hook: foreign-key
// toggles are no-ops (the edge keys carry no constraint), and an edge insert
// writes the edge key directly, bypassing every verb-level check.
func execRawEdges(s *Store) func(context.Context, []conformance.SQLStatement) error {
	return func(ctx context.Context, statements []conformance.SQLStatement) error {
		return s.run(ctx, func(t *txn) error {
			for _, st := range statements {
				query := strings.Join(strings.Fields(st.Query), " ")
				if strings.HasPrefix(strings.ToUpper(query), "SET FOREIGN_KEY_CHECKS") {
					continue
				}
				if !rawEdgeInsertRe.MatchString(query) {
					return fmt.Errorf("role fixture: unsupported raw statement %q", st.Query)
				}
				if len(st.Args) != 4 {
					return fmt.Errorf("role fixture: edge insert wants 4 args, got %d", len(st.Args))
				}
				dep := &types.Dependency{
					ID:          fmt.Sprint(st.Args[0]),
					IssueID:     fmt.Sprint(st.Args[1]),
					DependsOnID: fmt.Sprint(st.Args[2]),
					Type:        types.DependencyType(fmt.Sprint(st.Args[3])),
					CreatedAt:   nowUTC(),
					CreatedBy:   "seed",
					Metadata:    "{}",
				}
				if err := putEdge(t.kv, dep); err != nil {
					return err
				}
			}
			return nil
		})
	}
}
