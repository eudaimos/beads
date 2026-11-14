package storage

// The pebble leg's registration, in its own file the way the registry asks a
// leg brought from elsewhere to register: the leg and its ceiling together,
// in one change that is the leg's own.
//
// The leg is part-way through adopting the tier. It wires the whole contract
// of every role it serves (the twelve gascity's native store resolves its
// work through, plus Counter, CycleDetector, BatchCloser, Commenter and
// BatchCreator, and the journal and arrival contracts), and skips the
// contracts of the roles it does not serve yet, whose accessors answer
// *storage.ErrUnsupported through its generated shell. The ceiling is that
// count, and it falls with each role the leg takes on.
func init() {
	registerContractLeg(contractLeg{
		name:       "pebble",
		wiringRoot: "internal/storage/pebble",
		adopting: "the Phase 0 spike wires the contracts of the roles it serves and skips those of the " +
			"roles its generated unsupported shell still answers; the ceiling falls as roles land",
		adoptionCeiling: 141,
	})
}
