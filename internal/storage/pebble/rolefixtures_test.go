package pebble

import (
	"context"
	"testing"

	"github.com/steveyegge/beads/backend/conformance"
	"github.com/steveyegge/beads/internal/types"
	publicops "github.com/steveyegge/beads/issueops"
)

// Each constructor below builds one role's conformance fixture over a fresh
// in-memory store, mirroring the Dolt package's newDolt*Fixture helpers.
// History hooks stay nil (no commit log here); CommitPending is a no-op; the
// journal and versioned-history switches are no-ops because the store has
// neither.

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
		SetJournalEnabled:          func(bool) {},
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
}

// TestServedRolesAreWired proves each served accessor answers without the
// unsupported error, so the allowlist and the wiring cannot drift apart.
func TestServedRolesAreWired(t *testing.T) {
	kit, ctx := newRoleKit(t, "wired")
	s := kit.store
	checks := map[string]func() error{
		"IssueLifecycle":   func() error { _, err := s.IssueLifecycle(); return err },
		"IssueReader":      func() error { _, err := s.IssueReader(); return err },
		"BatchApplier":     func() error { _, err := s.BatchApplier(); return err },
		"EdgeReader":       func() error { _, err := s.EdgeReader(); return err },
		"DependencyEditor": func() error { _, err := s.DependencyEditor(); return err },
		"Deleter":          func() error { _, err := s.Deleter(); return err },
		"WorkspaceConfig":  func() error { _, err := s.WorkspaceConfig(); return err },
		"StatsReporter":    func() error { _, err := s.StatsReporter(); return err },
		"Releaser":         func() error { _, err := s.Releaser(); return err },
		"MetadataCAS":      func() error { _, err := s.MetadataCAS(); return err },
		"IssueRelations":   func() error { _, err := s.IssueRelations(); return err },
		"IssueClaimer":     func() error { _, err := s.IssueClaimer(); return err },
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
