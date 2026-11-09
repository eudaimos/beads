package pebble

import (
	"context"
	"testing"

	"github.com/steveyegge/beads/backend/conformance"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
)

// TestJournalContract runs the Journal contract against the pebble store, the
// way the Dolt package runs it (dolt/journal_contract_test.go): one store for
// the whole role suite, the cases in declaration order with no t.Parallel,
// because two of them prune the journal and every case rebaselines off the
// live head.
func TestJournalContract(t *testing.T) {
	fixture, ctx := newJournalFixture(t, "jc")

	t.Run("PagesAreSeqAscendingAndSinceExclusive", func(t *testing.T) {
		conformance.RunJournalPagesAreSeqAscendingAndSinceExclusive(t, ctx, fixture)
	})
	t.Run("HeadArrivesWithItsRowsAndDetectsCaughtUp", func(t *testing.T) {
		conformance.RunJournalHeadArrivesWithItsRowsAndDetectsCaughtUp(t, ctx, fixture)
	})
	t.Run("LimitCapsRowsNotHead", func(t *testing.T) {
		conformance.RunJournalLimitCapsRowsNotHead(t, ctx, fixture)
	})
	t.Run("TruncationIsTypedAndNamesTheWindow", func(t *testing.T) {
		conformance.RunJournalTruncationIsTypedAndNamesTheWindow(t, ctx, fixture)
	})
	t.Run("HeadSurvivesAFullPrune", func(t *testing.T) {
		conformance.RunJournalHeadSurvivesAFullPrune(t, ctx, fixture)
	})
	t.Run("ArrivalStampsAWispCreateWithItsRecordSeq", func(t *testing.T) {
		conformance.RunJournalArrivalStampsAWispCreateWithItsRecordSeq(t, ctx, fixture)
	})
	t.Run("ArrivalOrdersABatchByItem", func(t *testing.T) {
		conformance.RunJournalArrivalOrdersABatchByItem(t, ctx, fixture)
	})
	t.Run("ArrivalIsOnEveryReadPath", func(t *testing.T) {
		conformance.RunJournalArrivalIsOnEveryReadPath(t, ctx, fixture)
	})
	t.Run("ArrivalNeverChanges", func(t *testing.T) {
		conformance.RunJournalArrivalNeverChanges(t, ctx, fixture)
	})
	t.Run("ArrivalOnlyWhileJournaling", func(t *testing.T) {
		conformance.RunJournalArrivalOnlyWhileJournaling(t, ctx, fixture)
	})
	t.Run("ArrivalOnlyOnTheWispPlane", func(t *testing.T) {
		conformance.RunJournalArrivalOnlyOnTheWispPlane(t, ctx, fixture)
	})
	t.Run("ArrivalRecoversCreateOrderAfterTruncation", func(t *testing.T) {
		conformance.RunJournalArrivalRecoversCreateOrderAfterTruncation(t, ctx, fixture)
	})
	t.Run("ArrivalIsNeverReused", func(t *testing.T) {
		conformance.RunJournalArrivalIsNeverReused(t, ctx, fixture)
	})
	t.Run("ArrivalIgnoresSuppliedValues", func(t *testing.T) {
		conformance.RunJournalArrivalIgnoresSuppliedValues(t, ctx, fixture)
	})
	t.Run("ArrivalReimportKeepsTheStamp", func(t *testing.T) {
		conformance.RunJournalArrivalReimportKeepsTheStamp(t, ctx, fixture)
	})
	t.Run("ArrivalFirstCreateRecordStampsAnUnstampedRow", func(t *testing.T) {
		conformance.RunJournalArrivalFirstCreateRecordStampsAnUnstampedRow(t, ctx, fixture)
	})
	t.Run("ArrivalRenameKeepsTheStamp", func(t *testing.T) {
		conformance.RunJournalArrivalRenameKeepsTheStamp(t, ctx, fixture)
	})
	t.Run("EveryMutationKindLandsARow", func(t *testing.T) {
		conformance.RunJournalEveryMutationKindLandsARow(t, ctx, fixture)
	})
	t.Run("EveryMutationKindRecordsItsActor", func(t *testing.T) {
		conformance.RunJournalEveryMutationKindRecordsItsActor(t, ctx, fixture)
	})
}

func newJournalFixture(t *testing.T, prefix string) (conformance.JournalFixture, context.Context) {
	t.Helper()
	kit, ctx := newRoleKit(t, prefix)
	store := kit.store
	// Through the type assertion bd serve and gascity make, never the
	// concrete method set: the journal is not on storage.DoltStorage.
	cursor, ok := any(store).(storage.EventsJournalCursor)
	if !ok {
		t.Fatalf("%T does not implement storage.EventsJournalCursor", store)
	}
	lifecycle, err := store.IssueLifecycle()
	if err != nil {
		t.Fatalf("IssueLifecycle(): %v", err)
	}
	reader, err := store.IssueReader()
	if err != nil {
		t.Fatalf("IssueReader(): %v", err)
	}
	editor, err := store.DependencyEditor()
	if err != nil {
		t.Fatalf("DependencyEditor(): %v", err)
	}
	creator, err := store.BatchCreator()
	if err != nil {
		t.Fatalf("BatchCreator(): %v", err)
	}
	commenter, err := store.Commenter()
	if err != nil {
		t.Fatalf("Commenter(): %v", err)
	}
	arrival := conformance.ArrivalHooksFromRoles(conformance.ArrivalRoles{
		Lifecycle: lifecycle, BatchCreator: creator, Reader: reader, Commenter: commenter, DependencyEditor: editor,
		Promote: func(ctx context.Context, id string) error {
			return store.PromoteFromEphemeral(ctx, id, "actor")
		},
	})
	// The store hands out no Importer role: `bd import` runs through its bulk
	// upsert, so the import cases drive that door.
	arrival.Import = conformance.ArrivalUpsertImportHook(store)
	arrival.Rename = conformance.ArrivalRenameHook(store)
	fixture := conformance.JournalFixture{
		IssuePrefix:       prefix,
		Journal:           cursor,
		SetJournalEnabled: store.SetEventsJournalEnabled,
		Prune:             store.PruneEventsJournal,
		Arrival:           arrival,
		Mutations: conformance.JournalMutations{
			Create: func(ctx context.Context, id string) error {
				return store.CreateIssue(ctx, &types.Issue{
					ID: id, Title: "t-" + id, IssueType: types.TypeTask, Status: types.StatusOpen,
				}, "actor")
			},
			Update: func(ctx context.Context, id string) error {
				return store.UpdateIssue(ctx, id, map[string]any{"title": "renamed " + id}, "actor")
			},
			Close: func(ctx context.Context, id string) error {
				return store.CloseIssue(ctx, id, "done", "actor", "")
			},
			Delete: func(ctx context.Context, id string) error {
				return store.DeleteIssue(ctx, id)
			},
			AddDependency: func(ctx context.Context, from, to string) error {
				return store.AddDependency(ctx, &types.Dependency{
					IssueID: from, DependsOnID: to, Type: types.DepBlocks,
				}, "actor")
			},
			RemoveDependency: func(ctx context.Context, from, to string) error {
				return store.RemoveDependency(ctx, from, to, "actor")
			},
			Comment: func(ctx context.Context, id, text string) error {
				return store.AddComment(ctx, id, "actor", text)
			},
			// DeleteIssue takes no actor, so its record names nobody.
			Actor:           "actor",
			ActorlessDelete: true,
		},
	}
	return fixture, ctx
}
