package pebble

// Generated from the conformance package's exported (t, ctx, fixture) runners
// for every role this store serves; each case runs on a fresh in-memory store.
// Regenerate by re-running the wiring script from this package's history after
// the conformance package gains or renames a runner.
//
// Not wired, because they assert the bd_events_journal rows or the
// issue_versions rows that this store does not keep:
//   - RunDependencyEditorRemoveIsIdempotent
//   - RunDependencyEditorSameTypeReAddIsIdempotent
//   - RunDependencyEditorSameTypeReAddWithChangedMetadataMintsOneVersion
//   - RunDependencyEditorSameTypeReAddWithChangedThreadMintsOneVersion
//   - RunDependencyEditorSameTypeReAddWithIdenticalMetadataIsANoOp

import (
	"context"
	"testing"

	"github.com/steveyegge/beads/backend/conformance"
	publicops "github.com/steveyegge/beads/issueops"
)

func TestLifecycleCreateContract(t *testing.T) {
	cases := []struct {
		name string
		run  func(*testing.T, context.Context, conformance.LifecycleCreateFixture)
	}{
		{"RejectsMissingDependencyTargets", conformance.RunLifecycleCreateRejectsMissingDependencyTargets},
		{"RefusesAnOccupiedID", conformance.RunLifecycleCreateRefusesAnOccupiedID},
		{"RefusesAForeignIDPrefix", conformance.RunLifecycleCreateRefusesAForeignIDPrefix},
		{"InheritsParentLabels", conformance.RunLifecycleCreateInheritsParentLabels},
		{"WritesEveryScalarField", conformance.RunLifecycleCreateWritesEveryScalarField},
		{"EchoesSubSecondTimestamps", conformance.RunLifecycleCreateEchoesSubSecondTimestamps},
		{"AppliesTheDefaultPriority", conformance.RunLifecycleCreateAppliesTheDefaultPriority},
		{"WritesTheGateMembers", conformance.RunLifecycleCreateWritesTheGateMembers},
		{"BoundsTheGateMembers", conformance.RunLifecycleCreateBoundsTheGateMembers},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture, ctx := newLifecycleCreateFixture(t, "lcc")
			tc.run(t, ctx, fixture)
		})
	}
}

func TestLifecycleUpdateContract(t *testing.T) {
	cases := []struct {
		name string
		run  func(*testing.T, context.Context, conformance.LifecycleUpdateFixture)
	}{
		{"PatchesTheGateMembers", conformance.RunLifecycleUpdatePatchesTheGateMembers},
		{"BoundsTheGateMembers", conformance.RunLifecycleUpdateBoundsTheGateMembers},
		{"PersistsThePatchAndHydratesTheResult", conformance.RunLifecycleUpdatePersistsThePatchAndHydratesTheResult},
		{"PreservesTheCreationStamp", conformance.RunLifecycleUpdatePreservesTheCreationStamp},
		{"ReportsNoChangeForASameValuePatch", conformance.RunLifecycleUpdateReportsNoChangeForASameValuePatch},
		{"AppendsNotesWithoutReplacingThem", conformance.RunLifecycleUpdateAppendsNotesWithoutReplacingThem},
		{"ClearsTheNullableMembers", conformance.RunLifecycleUpdateClearsTheNullableMembers},
		{"ReplacesTheLabelSet", conformance.RunLifecycleUpdateReplacesTheLabelSet},
		{"ResolvesBothPlanesUnlessRestricted", conformance.RunLifecycleUpdateResolvesBothPlanesUnlessRestricted},
		{"RefusesUnknownIDsAndActorlessRequests", conformance.RunLifecycleUpdateRefusesUnknownIDsAndActorlessRequests},
		{"RefusalWritesNoMemberOfThePatch", conformance.RunLifecycleUpdateRefusalWritesNoMemberOfThePatch},
		{"ConditionalGuardsGateOrdinaryEdits", conformance.RunLifecycleUpdateConditionalGuardsGateOrdinaryEdits},
		{"ExpectedVersionSingleWinnerUnderConcurrency", conformance.RunLifecycleUpdateExpectedVersionSingleWinnerUnderConcurrency},
		{"ExpectedVersionSingleWinnerWithDisjointColumnsUnderConcurrency", conformance.RunLifecycleUpdateExpectedVersionSingleWinnerWithDisjointColumnsUnderConcurrency},
		{"LifecycleCloseExpectedVersionSingleWinnerUnderConcurrency", conformance.RunLifecycleCloseExpectedVersionSingleWinnerUnderConcurrency},
		{"ExpectedVersionSingleWinnerAcrossUpdateAndCloseUnderConcurrency", conformance.RunLifecycleUpdateExpectedVersionSingleWinnerAcrossUpdateAndCloseUnderConcurrency},
		{"ConditionalGuardAcceptsRespelledAssignee", conformance.RunLifecycleUpdateConditionalGuardAcceptsRespelledAssignee},
		{"MetadataPatchOrdersMergeSetUnset", conformance.RunLifecycleUpdateMetadataPatchOrdersMergeSetUnset},
		{"ClosePolicy", conformance.RunLifecycleUpdateClosePolicy},
		{"AssigneeTransferFence", conformance.RunLifecycleUpdateAssigneeTransferFence},
		{"ClaimIsAMutationWhenThePatchRestoresTheRow", conformance.RunLifecycleUpdateClaimIsAMutationWhenThePatchRestoresTheRow},
		{"ParentIDReplacesTheParentEdge", conformance.RunLifecycleUpdateParentIDReplacesTheParentEdge},
		{"ParentIDReplacesEveryParent", conformance.RunLifecycleUpdateParentIDReplacesEveryParent},
		{"PersistentPreservesUnversionedClass", conformance.RunLifecycleUpdatePersistentPreservesUnversionedClass},
		{"WispModesRefuseAnUnversionedRow", conformance.RunLifecycleUpdateWispModesRefuseAnUnversionedRow},
		{"PersistentPromotesAWisp", conformance.RunLifecycleUpdatePersistentPromotesAWisp},
		{"ProvenanceLabelsHistory", conformance.RunLifecycleUpdateProvenanceLabelsHistory},
		{"RefusesATemplate", conformance.RunLifecycleUpdateRefusesATemplate},
		{"AllowTemplateEditsATemplate", conformance.RunLifecycleUpdateAllowTemplateEditsATemplate},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture, ctx := newLifecycleUpdateFixture(t, "lcu")
			tc.run(t, ctx, fixture)
		})
	}
}

func TestLifecycleCloseReopenContract(t *testing.T) {
	cases := []struct {
		name string
		run  func(*testing.T, context.Context, conformance.LifecycleCloseReopenFixture)
	}{
		{"LifecycleCloseRefusalsCarryTheirTypesAndWriteNothing", conformance.RunLifecycleCloseRefusalsCarryTheirTypesAndWriteNothing},
		{"LifecycleCloseAdmitsATransitivelyBlockedTarget", conformance.RunLifecycleCloseAdmitsATransitivelyBlockedTarget},
		{"LifecycleCloseCountsOpenChildrenInBothPlanes", conformance.RunLifecycleCloseCountsOpenChildrenInBothPlanes},
		{"LifecycleCloseAdmitsAStaleBlockFlagWhoseBlockersHaveClosed", conformance.RunLifecycleCloseAdmitsAStaleBlockFlagWhoseBlockersHaveClosed},
		{"LifecycleCloseIsIdempotentOnAClosedRowThatStillLooksBlocked", conformance.RunLifecycleCloseIsIdempotentOnAClosedRowThatStillLooksBlocked},
		{"LifecycleCloseIsIdempotentAndKeepsTheFirstClose", conformance.RunLifecycleCloseIsIdempotentAndKeepsTheFirstClose},
		{"LifecycleCloseAndReopenKeepTheClaimHolder", conformance.RunLifecycleCloseAndReopenKeepTheClaimHolder},
		{"LifecycleReopenLeavesNonDoneStatusesUnchanged", conformance.RunLifecycleReopenLeavesNonDoneStatusesUnchanged},
		{"LifecycleCloseAndReopenSpanTheConfiguredDoneCategory", conformance.RunLifecycleCloseAndReopenSpanTheConfiguredDoneCategory},
		{"LifecycleExpectedVersionIsCheckedBeforeTheNoOps", conformance.RunLifecycleExpectedVersionIsCheckedBeforeTheNoOps},
		{"LifecycleReopenRecordsItsReason", conformance.RunLifecycleReopenRecordsItsReason},
		{"LifecycleResultsAreHydratedPostStateSnapshots", conformance.RunLifecycleResultsAreHydratedPostStateSnapshots},
		{"LifecycleResultsCarryThePostWriteRowVersion", conformance.RunLifecycleResultsCarryThePostWriteRowVersion},
		{"LifecycleCloseAndReopenRequireActorAndIssueID", conformance.RunLifecycleCloseAndReopenRequireActorAndIssueID},
		{"LifecycleReopenProvenanceLabelsHistory", conformance.RunLifecycleReopenProvenanceLabelsHistory},
		{"LifecycleCloseSettlesItsTransitiveAndCrossPlaneDependers", conformance.RunLifecycleCloseSettlesItsTransitiveAndCrossPlaneDependers},
		{"LifecycleCloseSettlesTheClosedRowItselfAndItsChild", conformance.RunLifecycleCloseSettlesTheClosedRowItselfAndItsChild},
		{"LifecycleCloseOnASpawnersLastChildSatisfiesAWaitsForGate", conformance.RunLifecycleCloseOnASpawnersLastChildSatisfiesAWaitsForGate},
		{"LifecycleReopenReblocksItsDependers", conformance.RunLifecycleReopenReblocksItsDependers},
		{"LifecycleCloseEnforcesTheCloseGuards", conformance.RunLifecycleCloseEnforcesTheCloseGuards},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture, ctx := newLifecycleCloseReopenFixture(t, "lcx")
			tc.run(t, ctx, fixture)
		})
	}
}

func TestReaderContract(t *testing.T) {
	cases := []struct {
		name string
		run  func(*testing.T, context.Context, conformance.ReaderFixture)
	}{
		{"ReadyDefaultTypeExclusionsYieldToAnExplicitType", conformance.RunReaderReadyDefaultTypeExclusionsYieldToAnExplicitType},
		{"ReadyDeferredAndEphemeralGates", conformance.RunReaderReadyDeferredAndEphemeralGates},
		{"ReadyLimitBoundary", conformance.RunReaderReadyLimitBoundary},
		{"OffsetSkipsTheRowsBeforeThePage", conformance.RunReaderOffsetSkipsTheRowsBeforeThePage},
		{"ReadySortPoliciesOrderTheSameRows", conformance.RunReaderReadySortPoliciesOrderTheSameRows},
		{"ListDefaultExclusionsAndTheirOverrides", conformance.RunReaderListDefaultExclusionsAndTheirOverrides},
		{"ListRejectsATypeOutsideTheWorkspaceVocabulary", conformance.RunReaderListRejectsATypeOutsideTheWorkspaceVocabulary},
		{"ListNaturalNumericIDSortTrimsAfterTheFetch", conformance.RunReaderListNaturalNumericIDSortTrimsAfterTheFetch},
		{"ListKeysetPositionResumesTheCreatedDescIDAscOrder", conformance.RunReaderListKeysetPositionResumesTheCreatedDescIDAscOrder},
		{"ListReadyFlagAnswersTheBlockerAwareSet", conformance.RunReaderListReadyFlagAnswersTheBlockerAwareSet},
		{"ListReadyFlagRefusesAFilterItCannotCarry", conformance.RunReaderListReadyFlagRefusesAFilterItCannotCarry},
		{"ListQueryMatchesTheTitleOrTheID", conformance.RunReaderListQueryMatchesTheTitleOrTheID},
		{"ListEmptyPageIsWellFormed", conformance.RunReaderListEmptyPageIsWellFormed},
		{"ListMaxRowsIsHonored", conformance.RunReaderListMaxRowsIsHonored},
		{"ListMaxRowsBoundaryIsLimitPlusOffset", conformance.RunReaderListMaxRowsBoundaryIsLimitPlusOffset},
		{"ListSkipCountsDropsTheCardinalitiesAndNothingElse", conformance.RunReaderListSkipCountsDropsTheCardinalitiesAndNothingElse},
		{"GetResolvesTheExactIDAcrossBothPlanes", conformance.RunReaderGetResolvesTheExactIDAcrossBothPlanes},
		{"GetMissIsNotFoundAndBackendFailureDoesNotDecay", conformance.RunReaderGetMissIsNotFoundAndBackendFailureDoesNotDecay},
		{"GetOptionalRowListsAreOffByDefault", conformance.RunReaderGetOptionalRowListsAreOffByDefault},
		{"GetBriefDepsProjectsTheDependencyRows", conformance.RunReaderGetBriefDepsProjectsTheDependencyRows},
		{"GetDetailShapeMatchesTheSeededIssue", conformance.RunReaderGetDetailShapeMatchesTheSeededIssue},
		{"DoesNotMutateTheCallerRequest", conformance.RunReaderDoesNotMutateTheCallerRequest},
		{"ListLimitBoundaryUnderASortTheDatabaseCanExpress", conformance.RunReaderListLimitBoundaryUnderASortTheDatabaseCanExpress},
		{"ReadySetOwnsItsStatusPinnedAndTemplateDecisions", conformance.RunReaderReadySetOwnsItsStatusPinnedAndTemplateDecisions},
		{"ListReadyFlagCarriesTheAssigneeAndPriorityFilters", conformance.RunReaderListReadyFlagCarriesTheAssigneeAndPriorityFilters},
		{"ListStatusAcceptsACommaSeparatedORSet", conformance.RunReaderListStatusAcceptsACommaSeparatedORSet},
		{"ReadyPageIsThePrefixOfTheUnboundedAnswerCountsIncluded", conformance.RunReaderReadyPageIsThePrefixOfTheUnboundedAnswerCountsIncluded},
		{"ReadyEphemeralPageKeepsBothPlanesCountsAtItsBoundary", conformance.RunReaderReadyEphemeralPageKeepsBothPlanesCountsAtItsBoundary},
		{"ReadyPageWiderThanTheHydrationBatchIsStillThatPrefix", conformance.RunReaderReadyPageWiderThanTheHydrationBatchIsStillThatPrefix},
		{"ListCountsAreBlocksOnlyWhereGetCountsEveryEdge", conformance.RunReaderListCountsAreBlocksOnlyWhereGetCountsEveryEdge},
		{"ReadyParentScopesToItsTransitiveDescendants", conformance.RunReaderReadyParentScopesToItsTransitiveDescendants},
		{"ListParentReachesEveryDescendantAndOnlyItsOwn", conformance.RunReaderListParentReachesEveryDescendantAndOnlyItsOwn},
		{"ListKeysetWalkOverAnOversizedGroupLosesNothingAndRepeatsNothing", conformance.RunReaderListKeysetWalkOverAnOversizedGroupLosesNothingAndRepeatsNothing},
		{"ListPriorityKeysetWalkOverAnOversizedEqualKeyRunLosesNothingAndRepeatsNothing", conformance.RunReaderListPriorityKeysetWalkOverAnOversizedEqualKeyRunLosesNothingAndRepeatsNothing},
		{"ListKeysetPositionNarrowsWithoutReplacingTheOtherPredicates", conformance.RunReaderListKeysetPositionNarrowsWithoutReplacingTheOtherPredicates},
		{"ListIncludeEphemeralMergesThePlanesIntoOneOrder", conformance.RunReaderListIncludeEphemeralMergesThePlanesIntoOneOrder},
		{"ListWispTypeNarrowsTheAdmittedPlaneRatherThanAdmittingIt", conformance.RunReaderListWispTypeNarrowsTheAdmittedPlaneRatherThanAdmittingIt},
		{"ListBriefDropsTheFreeFormTextAndNothingElse", conformance.RunReaderListBriefDropsTheFreeFormTextAndNothingElse},
		{"ReadyBriefDropsTheFreeFormTextAndNothingElse", conformance.RunReaderReadyBriefDropsTheFreeFormTextAndNothingElse},
		{"ListTimeBoundsAreStrictAndWholeSecond", conformance.RunReaderListTimeBoundsAreStrictAndWholeSecond},
		{"ListFilterFamiliesNarrowToTheirRows", conformance.RunReaderListFilterFamiliesNarrowToTheirRows},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture, ctx := newReaderFixture(t, "rdr")
			tc.run(t, ctx, fixture)
		})
	}
}

func TestBatchApplyContract(t *testing.T) {
	cases := []struct {
		name string
		run  func(*testing.T, context.Context, conformance.BatchApplyFixture)
	}{
		{"CarriesTheGateMembers", conformance.RunBatchApplyCarriesTheGateMembers},
		{"AppliesEveryItemInDeclarationOrder", conformance.RunBatchApplyAppliesEveryItemInDeclarationOrder},
		{"BindsEachNamedKeyToItsMintedID", conformance.RunBatchApplyBindsEachNamedKeyToItsMintedID},
		{"ResolvesABackwardKeyRef", conformance.RunBatchApplyResolvesABackwardKeyRef},
		{"RefusesAKeyDeclaredLater", conformance.RunBatchApplyRefusesAKeyDeclaredLater},
		{"RefusesAKeyNoItemDeclares", conformance.RunBatchApplyRefusesAKeyNoItemDeclares},
		{"RefusesARefNamingNeitherOrBoth", conformance.RunBatchApplyRefusesARefNamingNeitherOrBoth},
		{"RollsBackEverythingWhenTheLastItemRefuses", conformance.RunBatchApplyRollsBackEverythingWhenTheLastItemRefuses},
		{"NeverReordersItsItems", conformance.RunBatchApplyNeverReordersItsItems},
		{"EndGateRefusesAHierarchyTheRequestBuilt", conformance.RunBatchApplyEndGateRefusesAHierarchyTheRequestBuilt},
		{"EndGateCycleSurvivesSkipPerEdgeCycleCheck", conformance.RunBatchApplyEndGateCycleSurvivesSkipPerEdgeCycleCheck},
		{"ExpectedVersionThatMatchesLetsTheItemThrough", conformance.RunBatchApplyExpectedVersionThatMatchesLetsTheItemThrough},
		{"StaleExpectedVersionRefusesTheWholeRequest", conformance.RunBatchApplyStaleExpectedVersionRefusesTheWholeRequest},
		{"RefusesExpectedVersionOnARowAnEarlierItemTouched", conformance.RunBatchApplyRefusesExpectedVersionOnARowAnEarlierItemTouched},
		{"RefusesExpectedVersionOnARowAnEarlierItemCreated", conformance.RunBatchApplyRefusesExpectedVersionOnARowAnEarlierItemCreated},
		{"EvaluatesExpectedStatusAsModified", conformance.RunBatchApplyEvaluatesExpectedStatusAsModified},
		{"EvaluatesExpectedAssigneeAsModified", conformance.RunBatchApplyEvaluatesExpectedAssigneeAsModified},
		{"ClosePolicyEvaluatesAtTheCloseItem", conformance.RunBatchApplyClosePolicyEvaluatesAtTheCloseItem},
		{"AllowsAClosedParentToGainAnOpenChild", conformance.RunBatchApplyAllowsAClosedParentToGainAnOpenChild},
		{"UpdateAfterCloseInOneRequest", conformance.RunBatchApplyUpdateAfterCloseInOneRequest},
		{"ReportsChangedPerItem", conformance.RunBatchApplyReportsChangedPerItem},
		{"ANoOpBatchRecordsNoHistory", conformance.RunBatchApplyANoOpBatchRecordsNoHistory},
		{"RecordsOneEntryForAWriteThatLandedNothing", conformance.RunBatchApplyRecordsOneEntryForAWriteThatLandedNothing},
		{"RecordsExactlyOneHistoryEntry", conformance.RunBatchApplyRecordsExactlyOneHistoryEntry},
		{"HistoryNamesTheActorAndReadsTheProvenance", conformance.RunBatchApplyHistoryNamesTheActorAndReadsTheProvenance},
		{"ARefusedRequestRecordsNoHistory", conformance.RunBatchApplyARefusedRequestRecordsNoHistory},
		{"AnEphemeralBatchKeepsItsWispsAndRecordsNoDurableHistory", conformance.RunBatchApplyAnEphemeralBatchKeepsItsWispsAndRecordsNoDurableHistory},
		{"RefusesACrossPlaneEdgeBetweenRowsItCreated", conformance.RunBatchApplyRefusesACrossPlaneEdgeBetweenRowsItCreated},
		{"AcceptsAnExternalEdgeTarget", conformance.RunBatchApplyAcceptsAnExternalEdgeTarget},
		{"NormalizesTheWaitsForGate", conformance.RunBatchApplyNormalizesTheWaitsForGate},
		{"StampsSpawnerIDOnlyWhenNamed", conformance.RunBatchApplyStampsSpawnerIDOnlyWhenNamed},
		{"CarriesThreadIDOntoTheStoredEdge", conformance.RunBatchApplyCarriesThreadIDOntoTheStoredEdge},
		{"CarriesThreadIDOntoAnExistingEdge", conformance.RunBatchApplyCarriesThreadIDOntoAnExistingEdge},
		{"RefusesAThreadIDLongerThanItsColumn", conformance.RunBatchApplyRefusesAThreadIDLongerThanItsColumn},
		{"SplicesAForwardMetadataRef", conformance.RunBatchApplySplicesAForwardMetadataRef},
		{"SplicesASelfMetadataRef", conformance.RunBatchApplySplicesASelfMetadataRef},
		{"RefusesAMetadataRefNoItemDeclares", conformance.RunBatchApplyRefusesAMetadataRefNoItemDeclares},
		{"TheSpliceRecordsAnUpdateEvent", conformance.RunBatchApplyTheSpliceRecordsAnUpdateEvent},
		{"KeepsAStoredNullApartFromAnEmptyString", conformance.RunBatchApplyKeepsAStoredNullApartFromAnEmptyString},
		{"LandsAnIdempotencyRecordWithItsWork", conformance.RunBatchApplyLandsAnIdempotencyRecordWithItsWork},
		{"BoundsTheItemCount", conformance.RunBatchApplyBoundsTheItemCount},
		{"ReplayMintsANewSetOfRows", conformance.RunBatchApplyReplayMintsANewSetOfRows},
		{"DoesNotMutateTheCallerRequest", conformance.RunBatchApplyDoesNotMutateTheCallerRequest},
		{"RefusesAnUnusableRequest", conformance.RunBatchApplyRefusesAnUnusableRequest},
		{"UpdateItemsRefuseATemplate", conformance.RunBatchApplyUpdateItemsRefuseATemplate},
		{"SplicesTheMetadataOfATemplateItCreates", conformance.RunBatchApplySplicesTheMetadataOfATemplateItCreates},
		{"CloseItemsAnswerToTheCloseGuards", conformance.RunBatchApplyCloseItemsAnswerToTheCloseGuards},
		{"RefusesADottedChildGatedOnItsOwnParent", conformance.RunBatchApplyRefusesADottedChildGatedOnItsOwnParent},
		{"AppliesTheDefaultPriority", conformance.RunBatchApplyAppliesTheDefaultPriority},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture, ctx := newBatchApplyFixture(t, "bap")
			tc.run(t, ctx, fixture)
		})
	}
	t.Run("BoundsTheItemCountAtScale", func(t *testing.T) {
		fixture, ctx := newBatchApplyFixture(t, "bap")
		conformance.RunBatchApplyBoundsTheItemCountAtScale(t, ctx, fixture, publicops.MaxApplyBatchItems)
	})
}

func TestEdgeReaderContract(t *testing.T) {
	cases := []struct {
		name string
		run  func(*testing.T, context.Context, conformance.EdgeReaderFixture)
	}{
		{"AnswersOnePerAnchorInRequestOrder", conformance.RunEdgeReaderAnswersOnePerAnchorInRequestOrder},
		{"ReportsAMissingAnchorRatherThanFailing", conformance.RunEdgeReaderReportsAMissingAnchorRatherThanFailing},
		{"DistinguishesNoEdgesFromNoAnchor", conformance.RunEdgeReaderDistinguishesNoEdgesFromNoAnchor},
		{"ReturnsTargetsVerbatim", conformance.RunEdgeReaderReturnsTargetsVerbatim},
		{"CollapsesRepeatedAnchors", conformance.RunEdgeReaderCollapsesRepeatedAnchors},
		{"OrdersEdgesByTarget", conformance.RunEdgeReaderOrdersEdgesByTarget},
		{"FiltersEdgesNotAnchors", conformance.RunEdgeReaderFiltersEdgesNotAnchors},
		{"ReadsBothPlanes", conformance.RunEdgeReaderReadsBothPlanes},
		{"ResolvesExactIDsOnly", conformance.RunEdgeReaderResolvesExactIDsOnly},
		{"AnswersAnEmptyRequest", conformance.RunEdgeReaderAnswersAnEmptyRequest},
		{"RefusesAnEmptyID", conformance.RunEdgeReaderRefusesAnEmptyID},
		{"RefusesAnUnusableType", conformance.RunEdgeReaderRefusesAnUnusableType},
		{"LeavesTheRequestAlone", conformance.RunEdgeReaderLeavesTheRequestAlone},
		{"WritesNothing", conformance.RunEdgeReaderWritesNothing},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture, ctx := newEdgeReaderFixture(t, "edg")
			tc.run(t, ctx, fixture)
		})
	}
}

func TestDependencyEditorContract(t *testing.T) {
	cases := []struct {
		name string
		run  func(*testing.T, context.Context, conformance.DependencyEditorFixture)
	}{
		{"RoutesWispSourcedEdgeToTheWispPlane", conformance.RunDependencyEditorRoutesWispSourcedEdgeToTheWispPlane},
		{"MixedBatchWritesBothPlanes", conformance.RunDependencyEditorMixedBatchWritesBothPlanes},
		{"MixedBatchRefusalRollsBackBothPlanes", conformance.RunDependencyEditorMixedBatchRefusalRollsBackBothPlanes},
		{"RefusesCrossPlaneCycle", conformance.RunDependencyEditorRefusesCrossPlaneCycle},
		{"AddedEchoesTheRequestOrder", conformance.RunDependencyEditorAddedEchoesTheRequestOrder},
		{"RepeatsWithinOneRequestCollapse", conformance.RunDependencyEditorRepeatsWithinOneRequestCollapse},
		{"AttributesItsEventsToTheActor", conformance.RunDependencyEditorAttributesItsEventsToTheActor},
		{"RetypeRefusalLeavesTheOriginalEdge", conformance.RunDependencyEditorRetypeRefusalLeavesTheOriginalEdge},
		{"RefusalWritesNothing", conformance.RunDependencyEditorRefusalWritesNothing},
		{"AppliesParentChildBeforeBlockingEdges", conformance.RunDependencyEditorAppliesParentChildBeforeBlockingEdges},
		{"AcceptsAnExternalTarget", conformance.RunDependencyEditorAcceptsAnExternalTarget},
		{"AcceptsAForeignRepoTarget", conformance.RunDependencyEditorAcceptsAForeignRepoTarget},
		{"RefusesBlockingEdgeAcrossItsOwnHierarchy", conformance.RunDependencyEditorRefusesBlockingEdgeAcrossItsOwnHierarchy},
		{"RefusesSelfDependencyWithTheProbeSkipped", conformance.RunDependencyEditorRefusesSelfDependencyWithTheProbeSkipped},
		{"RecordsOneHistoryEntryPerLandedRequest", conformance.RunDependencyEditorRecordsOneHistoryEntryPerLandedRequest},
		{"RecordsNoHistoryForAnAllEphemeralRequest", conformance.RunDependencyEditorRecordsNoHistoryForAnAllEphemeralRequest},
		{"SnapshotsTheRequest", conformance.RunDependencyEditorSnapshotsTheRequest},
		{"ValidationRefusalsWriteNothing", conformance.RunDependencyEditorValidationRefusalsWriteNothing},
		{"RoutesWispSourcedRemovalToTheWispPlane", conformance.RunDependencyEditorRoutesWispSourcedRemovalToTheWispPlane},
		{"RefusesAGhostSource", conformance.RunDependencyEditorRefusesAGhostSource},
		{"RefusesAMissingLocalTarget", conformance.RunDependencyEditorRefusesAMissingLocalTarget},
		{"AcceptsATypeOutsideTheConstants", conformance.RunDependencyEditorAcceptsATypeOutsideTheConstants},
		{"RemovesOnlyTheNamedEdge", conformance.RunDependencyEditorRemovesOnlyTheNamedEdge},
		{"SkipPerEdgeCycleCheckDropsOnlyTheProbe", conformance.RunDependencyEditorSkipPerEdgeCycleCheckDropsOnlyTheProbe},
		{"RecordsOneHistoryEntryForAMixedPlaneRequest", conformance.RunDependencyEditorRecordsOneHistoryEntryForAMixedPlaneRequest},
		{"WritesTheTargetIntoItsTypedColumn", conformance.RunDependencyEditorWritesTheTargetIntoItsTypedColumn},
		{"RefusesBlockingEdgeAcrossAWispHierarchy", conformance.RunDependencyEditorRefusesBlockingEdgeAcrossAWispHierarchy},
		{"RefusesACycleThroughAParentChildHop", conformance.RunDependencyEditorRefusesACycleThroughAParentChildHop},
		{"RefusesASamePlaneEdgeClosingACrossPlaneCycle", conformance.RunDependencyEditorRefusesASamePlaneEdgeClosingACrossPlaneCycle},
		{"AddMarksItsSourceInTheSameVerb", conformance.RunDependencyEditorAddMarksItsSourceInTheSameVerb},
		{"RemoveUnmarksItsSourceAndDescendants", conformance.RunDependencyEditorRemoveUnmarksItsSourceAndDescendants},
		{"MaintainsBlockedStateAcrossPlanes", conformance.RunDependencyEditorMaintainsBlockedStateAcrossPlanes},
		{"ClosedChildAddSatisfiesAnAnyChildrenGate", conformance.RunDependencyEditorClosedChildAddSatisfiesAnAnyChildrenGate},
		{"RelatesToAddLeavesItsSourceUnblocked", conformance.RunDependencyEditorRelatesToAddLeavesItsSourceUnblocked},
		{"AcceptsADiamond", conformance.RunDependencyEditorAcceptsADiamond},
		{"GateScopeFollowsTheEdgeType", conformance.RunDependencyEditorGateScopeFollowsTheEdgeType},
		{"AcceptsBlockingAcrossIssueTypes", conformance.RunDependencyEditorAcceptsBlockingAcrossIssueTypes},
		{"RefusesADottedChildGatedOnItsOwnParent", conformance.RunDependencyEditorRefusesADottedChildGatedOnItsOwnParent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture, ctx := newDependencyEditorFixture(t, "dep")
			tc.run(t, ctx, fixture)
		})
	}
}

func TestDeleterContract(t *testing.T) {
	cases := []struct {
		name string
		run  func(*testing.T, context.Context, conformance.DeleterFixture)
	}{
		{"RefusesAMalformedRequest", conformance.RunDeleterRefusesAMalformedRequest},
		{"RefusesAnAbsentID", conformance.RunDeleterRefusesAnAbsentID},
		{"RefusesDependentsOutsideTheRequest", conformance.RunDeleterRefusesDependentsOutsideTheRequest},
		{"ForceOrphansDependents", conformance.RunDeleterForceOrphansDependents},
		{"CascadeDeletesTheClosure", conformance.RunDeleterCascadeDeletesTheClosure},
		{"CascadeFromAWispRootDeletesTheClosure", conformance.RunDeleterCascadeFromAWispRootDeletesTheClosure},
		{"CascadeFollowsOnlyContainment", conformance.RunDeleterCascadeFollowsOnlyContainment},
		{"CascadeReportsSurvivingDependents", conformance.RunDeleterCascadeReportsSurvivingDependents},
		{"GuardsAWispNamedWithADurableDependent", conformance.RunDeleterGuardsAWispNamedWithADurableDependent},
		{"GuardsADurableNamedWithAWispDependent", conformance.RunDeleterGuardsADurableNamedWithAWispDependent},
		{"CountsCrossPlaneEdgesItRemoves", conformance.RunDeleterCountsCrossPlaneEdgesItRemoves},
		{"NeverCallsALiveRowDeleted", conformance.RunDeleterNeverCallsALiveRowDeleted},
		{"ErasesAcrossBothPlanes", conformance.RunDeleterErasesAcrossBothPlanes},
		{"CollapsesDuplicateIDs", conformance.RunDeleterCollapsesDuplicateIDs},
		{"RewritesReferencesInNeighbors", conformance.RunDeleterRewritesReferencesInNeighbors},
		{"DryRunChangesNothing", conformance.RunDeleterDryRunChangesNothing},
		{"RecordsExactlyOneHistoryEntry", conformance.RunDeleterRecordsExactlyOneHistoryEntry},
		{"DoesNotMutateTheCallerRequest", conformance.RunDeleterDoesNotMutateTheCallerRequest},
		{"SettlesTheSurvivorsOfADeletedBlocker", conformance.RunDeleterSettlesTheSurvivorsOfADeletedBlocker},
		{"SettlesTheChildrenOfADeletedParent", conformance.RunDeleterSettlesTheChildrenOfADeletedParent},
		{"SettlesTheSurvivorsOfADeletedWispBlocker", conformance.RunDeleterSettlesTheSurvivorsOfADeletedWispBlocker},
		{"DeletesOnAMatchingExpectedVersion", conformance.RunDeleterDeletesOnAMatchingExpectedVersion},
		{"RefusesAStaleExpectedVersion", conformance.RunDeleterRefusesAStaleExpectedVersion},
		{"VersionOutranksForceAndCascade", conformance.RunDeleterVersionOutranksForceAndCascade},
		{"RefusesAnExpectedVersionAcrossSeveralIDs", conformance.RunDeleterRefusesAnExpectedVersionAcrossSeveralIDs},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture, ctx := newDeleterFixture(t, "del")
			tc.run(t, ctx, fixture)
		})
	}
}

func TestWorkspaceConfigContract(t *testing.T) {
	cases := []struct {
		name string
		run  func(*testing.T, context.Context, conformance.WorkspaceConfigFixture)
	}{
		{"StoresAValueVerbatim", conformance.RunWorkspaceConfigStoresAValueVerbatim},
		{"ReplacesAnExistingValue", conformance.RunWorkspaceConfigReplacesAnExistingValue},
		{"ConflatesAnUnsetKeyWithAnEmptyValue", conformance.RunWorkspaceConfigConflatesAnUnsetKeyWithAnEmptyValue},
		{"ListsEveryStoredSetting", conformance.RunWorkspaceConfigListsEveryStoredSetting},
		{"ListExcludesTheKVPlane", conformance.RunWorkspaceConfigListExcludesTheKVPlane},
		{"PointReadRefusesTheKVPlane", conformance.RunWorkspaceConfigPointReadRefusesTheKVPlane},
		{"UnsetRemovesTheSetting", conformance.RunWorkspaceConfigUnsetRemovesTheSetting},
		{"UnsetOfAnAbsentKeySucceeds", conformance.RunWorkspaceConfigUnsetOfAnAbsentKeySucceeds},
		{"RefusesAnEmptyKey", conformance.RunWorkspaceConfigRefusesAnEmptyKey},
		{"RefusesTheProtectedKeyOnSet", conformance.RunWorkspaceConfigRefusesTheProtectedKeyOnSet},
		{"UnsetDoesNotRefuseTheProtectedKey", conformance.RunWorkspaceConfigUnsetDoesNotRefuseTheProtectedKey},
		{"RefusesAnUnparseableCustomStatus", conformance.RunWorkspaceConfigRefusesAnUnparseableCustomStatus},
		{"BoundsTheKeyAndTheValueOnSet", conformance.RunWorkspaceConfigBoundsTheKeyAndTheValueOnSet},
		{"ProjectsCustomStatuses", conformance.RunWorkspaceConfigProjectsCustomStatuses},
		{"ProjectsCustomTypes", conformance.RunWorkspaceConfigProjectsCustomTypes},
		{"UnsetLeavesTheProjectionBehind", conformance.RunWorkspaceConfigUnsetLeavesTheProjectionBehind},
		{"ARefusedWriteRecordsNoHistory", conformance.RunWorkspaceConfigARefusedWriteRecordsNoHistory},
		{"KeysAreCaseSensitive", conformance.RunWorkspaceConfigKeysAreCaseSensitive},
		{"CustomStatusReadsAreOrderedByName", conformance.RunWorkspaceConfigCustomStatusReadsAreOrderedByName},
		{"CustomTypeReadsAreOrderedByName", conformance.RunWorkspaceConfigCustomTypeReadsAreOrderedByName},
		{"ConfiguredInfraTypesReplaceTheDefaultSet", conformance.RunWorkspaceConfigConfiguredInfraTypesReplaceTheDefaultSet},
		{"UnconfiguredVocabularyReadsAreEmptyNotErrors", conformance.RunWorkspaceConfigUnconfiguredVocabularyReadsAreEmptyNotErrors},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture, ctx := newWorkspaceConfigFixture(t, "wcfg")
			tc.run(t, ctx, fixture)
		})
	}
}

func TestStatsReporterContract(t *testing.T) {
	cases := []struct {
		name string
		run  func(*testing.T, context.Context, conformance.StatsReporterFixture)
	}{
		{"CountsEveryDurableRowByStatus", conformance.RunStatsReporterCountsEveryDurableRowByStatus},
		{"ExcludesTheWispTier", conformance.RunStatsReporterExcludesTheWispTier},
		{"BreaksOutTheRowsTheDefaultListingSuppresses", conformance.RunStatsReporterBreaksOutTheRowsTheDefaultListingSuppresses},
		{"BreaksOutAGateThatIsAlsoATemplate", conformance.RunStatsReporterBreaksOutAGateThatIsAlsoATemplate},
		{"AStatusOutsideTheTalliesIsCountedOnlyInTotal", conformance.RunStatsReporterAStatusOutsideTheTalliesIsCountedOnlyInTotal},
		{"BlockedCountsTheGraphNotTheStatus", conformance.RunStatsReporterBlockedCountsTheGraphNotTheStatus},
		{"BlockedExcludesByStatusNotByThePinnedFlag", conformance.RunStatsReporterBlockedExcludesByStatusNotByThePinnedFlag},
		{"BlockedCountsEveryUnfinishedStatusNotJustOpen", conformance.RunStatsReporterBlockedCountsEveryUnfinishedStatusNotJustOpen},
		{"ReadyIsOpenMinusBlocked", conformance.RunStatsReporterReadyIsOpenMinusBlocked},
		{"SkipBlockedPairsTheTwoPointers", conformance.RunStatsReporterSkipBlockedPairsTheTwoPointers},
		{"ExtendedFieldsAreAlwaysZero", conformance.RunStatsReporterExtendedFieldsAreAlwaysZero},
		{"WritesNothing", conformance.RunStatsReporterWritesNothing},
		{"AssigneeStatsScopesToOneActor", conformance.RunStatsReporterAssigneeStatsScopesToOneActor},
		{"AssigneeBlockedCountsTheStatusNotTheGraph", conformance.RunStatsReporterAssigneeBlockedCountsTheStatusNotTheGraph},
		{"AssigneeStatsMergesTheWispTier", conformance.RunStatsReporterAssigneeStatsMergesTheWispTier},
		{"AssigneeStatsBreaksOutTheSuppressedRows", conformance.RunStatsReporterAssigneeStatsBreaksOutTheSuppressedRows},
		{"AssigneeStatsPopulatesBothPointers", conformance.RunStatsReporterAssigneeStatsPopulatesBothPointers},
		{"AssigneeStatsRefusesAnEmptyAssignee", conformance.RunStatsReporterAssigneeStatsRefusesAnEmptyAssignee},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture, ctx := newStatsReporterFixture(t, "sts")
			tc.run(t, ctx, fixture)
		})
	}
}

func TestReleaserContract(t *testing.T) {
	cases := []struct {
		name string
		run  func(*testing.T, context.Context, conformance.ReleaserFixture)
	}{
		{"ReleasesItsOwnClaim", conformance.RunReleaserReleasesItsOwnClaim},
		{"RefusesAForeignClaimUntilForced", conformance.RunReleaserRefusesAForeignClaimUntilForced},
		{"ReleasesOnlyTheExpectedHolder", conformance.RunReleaserReleasesOnlyTheExpectedHolder},
		{"RefusesAnUnheldIssue", conformance.RunReleaserRefusesAnUnheldIssue},
		{"RefusesAStatusThatCannotBeReleased", conformance.RunReleaserRefusesAStatusThatCannotBeReleased},
		{"RefusesAMalformedRequest", conformance.RunReleaserRefusesAMalformedRequest},
		{"RefusesAnAbsentID", conformance.RunReleaserRefusesAnAbsentID},
		{"AttributesTheReleaseToTheActor", conformance.RunReleaserAttributesTheReleaseToTheActor},
		{"RecordsExactlyOneHistoryEntry", conformance.RunReleaserRecordsExactlyOneHistoryEntry},
		{"ReleasesAWispClaimWithoutVersioning", conformance.RunReleaserReleasesAWispClaimWithoutVersioning},
		{"DoesNotMutateTheCallerRequest", conformance.RunReleaserDoesNotMutateTheCallerRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture, ctx := newReleaserFixture(t, "rel")
			tc.run(t, ctx, fixture)
		})
	}
}

func TestMetadataCASContract(t *testing.T) {
	cases := []struct {
		name string
		run  func(*testing.T, context.Context, conformance.MetadataCASFixture)
	}{
		{"CreatesAKeyThatWasAbsent", conformance.RunMetadataCASCreatesAKeyThatWasAbsent},
		{"RefusesASecondCreateAndReportsTheHolder", conformance.RunMetadataCASRefusesASecondCreateAndReportsTheHolder},
		{"SwapsOnAMatchAndReportsTheNewValue", conformance.RunMetadataCASSwapsOnAMatchAndReportsTheNewValue},
		{"RefusalReportsTheCurrentValueAndWritesNothing", conformance.RunMetadataCASRefusalReportsTheCurrentValueAndWritesNothing},
		{"ComparesCanonically", conformance.RunMetadataCASComparesCanonically},
		{"ReportsTheValueTheRowHolds", conformance.RunMetadataCASReportsTheValueTheRowHolds},
		{"DistinguishesAnAbsentKeyFromAStoredNull", conformance.RunMetadataCASDistinguishesAnAbsentKeyFromAStoredNull},
		{"RemovesTheKeyWhenTheValueIsAbsent", conformance.RunMetadataCASRemovesTheKeyWhenTheValueIsAbsent},
		{"PreservesSiblingKeys", conformance.RunMetadataCASPreservesSiblingKeys},
		{"NoOpSwapWritesNothing", conformance.RunMetadataCASNoOpSwapWritesNothing},
		{"RefusesAnIDOnNeitherPlane", conformance.RunMetadataCASRefusesAnIDOnNeitherPlane},
		{"RefusesAnUnusableRequest", conformance.RunMetadataCASRefusesAnUnusableRequest},
		{"ResolvesAWispAnchor", conformance.RunMetadataCASResolvesAWispAnchor},
		{"AWispSwapRecordsNoDurableHistory", conformance.RunMetadataCASAWispSwapRecordsNoDurableHistory},
		{"RecordsExactlyOneHistoryEntry", conformance.RunMetadataCASRecordsExactlyOneHistoryEntry},
		{"HistoryEntryNamesTheActor", conformance.RunMetadataCASHistoryEntryNamesTheActor},
		{"ARefusedSwapRecordsNoHistory", conformance.RunMetadataCASARefusedSwapRecordsNoHistory},
		{"DoesNotMutateTheCallerRequest", conformance.RunMetadataCASDoesNotMutateTheCallerRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture, ctx := newMetadataCASFixture(t, "mcas")
			tc.run(t, ctx, fixture)
		})
	}
}

func TestRelationsContract(t *testing.T) {
	cases := []struct {
		name string
		run  func(*testing.T, context.Context, conformance.RelationsFixture)
	}{
		{"AnswersInThePinnedOrder", conformance.RunRelationsAnswersInThePinnedOrder},
		{"OrdersNeighborsFromBothPlanesTogether", conformance.RunRelationsOrdersNeighborsFromBothPlanesTogether},
		{"AnswersAWispTargetInTheOutDirection", conformance.RunRelationsAnswersAWispTargetInTheOutDirection},
		{"RefusesTheZeroDirection", conformance.RunRelationsRefusesTheZeroDirection},
		{"SeparatesNoNeighborsFromNoSuchIssue", conformance.RunRelationsSeparatesNoNeighborsFromNoSuchIssue},
		{"ResolvesAWispAnchor", conformance.RunRelationsResolvesAWispAnchor},
		{"FiltersByAnOpenTypeVocabulary", conformance.RunRelationsFiltersByAnOpenTypeVocabulary},
		{"RefusesAnUnusableTypeFilter", conformance.RunRelationsRefusesAnUnusableTypeFilter},
		{"RefusesATypeFilterOverTheColumnLength", conformance.RunRelationsRefusesATypeFilterOverTheColumnLength},
		{"DirectionSelectsTheInverseGraph", conformance.RunRelationsDirectionSelectsTheInverseGraph},
		{"LeavesTheCallersRequestAlone", conformance.RunRelationsLeavesTheCallersRequestAlone},
		{"LeavesAnExternalTargetOutOfTheAnswer", conformance.RunRelationsLeavesAnExternalTargetOutOfTheAnswer},
		{"ResolvesTheAnchorIDExactly", conformance.RunRelationsResolvesTheAnchorIDExactly},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture, ctx := newRelationsFixture(t, "rln")
			tc.run(t, ctx, fixture)
		})
	}
}

func TestClaimerContract(t *testing.T) {
	cases := []struct {
		name string
		run  func(*testing.T, context.Context, conformance.ClaimerFixture)
	}{
		{"ClaimsAnUnassignedOpenIssueAndAnswersTheBareRow", conformance.RunClaimerClaimsAnUnassignedOpenIssueAndAnswersTheBareRow},
		{"TakesAnOpenIssueTheActorAlreadyHolds", conformance.RunClaimerTakesAnOpenIssueTheActorAlreadyHolds},
		{"ReclaimsItsOwnInProgressIssueWithoutWriting", conformance.RunClaimerReclaimsItsOwnInProgressIssueWithoutWriting},
		{"ReclaimsAcrossSpellingWithoutWriting", conformance.RunClaimerReclaimsAcrossSpellingWithoutWriting},
		{"AcceptsAConfiguredActiveStatusAndRefusesAConfiguredWipOne", conformance.RunClaimerAcceptsAConfiguredActiveStatusAndRefusesAConfiguredWipOne},
		{"TakesAPoolAssignedIssueButRefusesAForeignHolder", conformance.RunClaimerTakesAPoolAssignedIssueButRefusesAForeignHolder},
		{"RefusesABuiltInIneligibleStatusWithTheStateThatRefusedIt", conformance.RunClaimerRefusesABuiltInIneligibleStatusWithTheStateThatRefusedIt},
		{"RefusesAWispIDAsNotFound", conformance.RunClaimerRefusesAWispIDAsNotFound},
		{"RefusesIncompleteRequestsAndAnAbsentIDWithoutTouchingState", conformance.RunClaimerRefusesIncompleteRequestsAndAnAbsentIDWithoutTouchingState},
		{"GrantsALiveLeaseOnTheRowItWonAndLeavesARefusedOneAlone", conformance.RunClaimerGrantsALiveLeaseOnTheRowItWonAndLeavesARefusedOneAlone},
		{"StampsStartedAtOnceAcrossTheTwoWritesItChoosesBetween", conformance.RunClaimerStampsStartedAtOnceAcrossTheTwoWritesItChoosesBetween},
		{"RefusalMessagesCarryTheirFragments", conformance.RunClaimerRefusalMessagesCarryTheirFragments},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture, ctx := newClaimerFixture(t, "clm")
			tc.run(t, ctx, fixture)
		})
	}
}
