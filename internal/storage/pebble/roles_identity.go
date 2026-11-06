package pebble

import (
	"context"

	"github.com/steveyegge/beads/internal/workapi"
	publicops "github.com/steveyegge/beads/issueops"
)

// The identity pair: the one-time write that turns a database bd can open
// into a workspace bd can use, and the read `bd init` reconciles against.
// They are two roles because their callers have different permissions
// (issueops.InitVerifier says why), and one key pair because a prefix read
// from one place and a project id from another is an identity nothing else
// can find.

// --- Bootstrapper -----------------------------------------------------------

// Bootstrapper serves the identity seed behind `bd init`.
func (s *Store) Bootstrapper() (publicops.Bootstrapper, error) {
	return &bootstrapper{s: s}, nil
}

type bootstrapper struct{ s *Store }

var _ publicops.Bootstrapper = (*bootstrapper)(nil)

// Bootstrap is issueops.BootstrapInTx over the key space: the read and the
// write it qualifies share one transaction, so two inits racing against one
// store cannot both pass the refusal. Validation runs before the transaction,
// so a refused request writes nothing and reads nothing; a canceled context
// is refused before the identity is read.
func (b *bootstrapper) Bootstrap(ctx context.Context, req publicops.BootstrapRequest) (publicops.BootstrapResult, error) {
	req, err := workapi.ValidateBootstrapRequest(req)
	if err != nil {
		return publicops.BootstrapResult{}, err
	}
	err = b.s.run(ctx, func(t *txn) error {
		prefix, projectID, err := workspaceIdentity(t.kv)
		if err != nil {
			return err
		}
		if err := workapi.RefuseIdentifiedSubstrate(prefix, projectID); err != nil {
			return err
		}
		if err := setConfig(t.kv, workapi.ConfigKeyIssuePrefix, req.Prefix); err != nil {
			return err
		}
		return t.kv.set(metadataKey(workapi.MetadataKeyProjectID), []byte(req.ProjectID))
	})
	if err != nil {
		return publicops.BootstrapResult{}, err
	}
	return publicops.BootstrapResult{Prefix: req.Prefix, ProjectID: req.ProjectID}, nil
}

// --- InitVerifier -----------------------------------------------------------

// InitVerifier serves the identity read behind `bd init`'s adopt-or-refuse
// decision.
func (s *Store) InitVerifier() (publicops.InitVerifier, error) {
	return &initVerifier{s: s}, nil
}

type initVerifier struct{ s *Store }

var _ publicops.InitVerifier = (*initVerifier)(nil)

// VerifyIdentity reads the pair from one snapshot and writes nothing. An
// absent marker is "" with a nil error; a read that cannot run (a canceled
// context) is an error, never an empty answer.
func (v *initVerifier) VerifyIdentity(ctx context.Context, _ publicops.VerifyIdentityRequest) (publicops.VerifyIdentityResult, error) {
	var result publicops.VerifyIdentityResult
	err := v.s.readSnapshotCtx(ctx, func(r kv) error {
		prefix, projectID, err := workspaceIdentity(r)
		if err != nil {
			return err
		}
		result = publicops.VerifyIdentityResult{Prefix: prefix, ProjectID: projectID}
		return nil
	})
	if err != nil {
		return publicops.VerifyIdentityResult{}, err
	}
	return result, nil
}

// workspaceIdentity reads the two markers the bootstrap refusal and the
// verifier both ask about, so the two cannot disagree about what
// "identified" means.
func workspaceIdentity(r kv) (prefix, projectID string, err error) {
	prefix, err = getConfig(r, workapi.ConfigKeyIssuePrefix)
	if err != nil {
		return "", "", err
	}
	projectID, _, err = getString(r, metadataKey(workapi.MetadataKeyProjectID))
	if err != nil {
		return "", "", err
	}
	return prefix, projectID, nil
}
