package pebble

import (
	"github.com/steveyegge/beads/internal/workapi/storequerier"
	"github.com/steveyegge/beads/internal/workapi/storereadycounter"
	"github.com/steveyegge/beads/internal/workapi/storeversionreconciler"
	publicops "github.com/steveyegge/beads/issueops"
)

// The roles whose bodies are shared across every store leg, for the
// reason the Dolt stores give: they differ from one another below the flat
// surface (SearchIssuesWithCounts, CountReadyWork, the local metadata pair),
// not above it, so a copy here would be a copy of nothing but a constructor.

// Querier serves the expression query role (`bd query`) over the store's
// search.
func (s *Store) Querier() (publicops.Querier, error) {
	return storequerier.New(s)
}

// ReadyCounter serves the ready-count role over the ready-candidate index.
func (s *Store) ReadyCounter() (publicops.ReadyCounter, error) {
	return storereadycounter.New(s)
}

// VersionReconciler serves the version markers `bd` records on every run,
// over the store's local metadata.
func (s *Store) VersionReconciler() (publicops.VersionReconciler, error) {
	return storeversionreconciler.New(s)
}

// MoleculeStepper serves the advance-a-molecule role the way every store
// backend does: issueops.NewMoleculeStepper composed over this store's own
// BatchGetter, Relations, EdgeReader, Claimer and Lifecycle, so the advance
// reads and claims exactly as those roles do and no second copy of the rule
// exists.
func (s *Store) MoleculeStepper() (publicops.MoleculeStepper, error) {
	roles, err := publicops.MoleculeStepperRolesOf(s)
	if err != nil {
		return nil, err
	}
	return publicops.NewMoleculeStepper(roles), nil
}
