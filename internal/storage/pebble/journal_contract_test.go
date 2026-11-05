package pebble

import (
	"context"
	"testing"

	"github.com/steveyegge/beads/backend/conformance"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
	publicops "github.com/steveyegge/beads/issueops"
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
}

// kitCommenter adapts the store's comment verb to the Commenter role shape the
// arrival hooks are bound through; the store serves no Commenter role.
type kitCommenter struct{ s *Store }

func (c kitCommenter) AddComment(ctx context.Context, request publicops.AddCommentRequest) (publicops.AddCommentResult, error) {
	comment, err := c.s.AddIssueComment(ctx, request.IssueID, request.Author, request.Text)
	if err != nil {
		return publicops.AddCommentResult{}, err
	}
	return publicops.AddCommentResult{Comment: comment}, nil
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
	applier, err := store.BatchApplier()
	if err != nil {
		t.Fatalf("BatchApplier(): %v", err)
	}
	arrival := conformance.ArrivalHooksFromRoles(conformance.ArrivalRoles{
		Lifecycle: lifecycle, Reader: reader, Commenter: kitCommenter{s: store}, DependencyEditor: editor,
		Promote: func(ctx context.Context, id string) error {
			return store.PromoteFromEphemeral(ctx, id, "actor")
		},
	})
	// The store serves no BatchCreator; the batch applier is its batched
	// create, and it stamps items in request order the same way.
	arrival.CreateWispBatch = func(ctx context.Context, ids []string) error {
		items := make([]publicops.ApplyItem, 0, len(ids))
		for _, id := range ids {
			items = append(items, publicops.ApplyItem{Kind: publicops.ItemCreate, Create: &publicops.CreateItem{
				Issue: &types.Issue{ID: id, Title: "t-" + id, IssueType: types.TypeTask, Status: types.StatusOpen, Priority: 2, Ephemeral: true},
			}})
		}
		_, err := applier.ApplyBatch(ctx, publicops.ApplyBatchRequest{Actor: "actor", Items: items, ForceIDPrefix: true})
		return err
	}
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
		},
	}
	return fixture, ctx
}
