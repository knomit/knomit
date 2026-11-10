package app

import (
	"context"
	"database/sql"
	"fmt"

	"knomit/internal/foundation/authz"
)

// authzBoot brings up the new authorization model (unified authn/z plan, P1)
// on control.db: it syncs the role table to authz.Seed and builds the policy
// materializer for this server's owner.
//
// It is DARK: nothing consults the materializer until the flip (P4). It still
// runs at every boot so the seed is exercised where it will live, and a
// failure stops the boot, as a seedOwnPrincipal failure does: a server whose
// role table could not be written would, after the flip, deny everyone but
// the owner while looking healthy.
//
// Read-only mode's in-memory anonymous -> guest binding is not passed here
// yet; the flip wires it together with deleting the old read-only gate.
func authzBoot(ctx context.Context, db *sql.DB) (*authz.Store, *authz.Materializer, error) {
	s := authz.NewStore(db)
	if err := s.SeedRoles(ctx); err != nil {
		return nil, nil, err
	}
	owner, err := authz.Owner()
	if err != nil {
		return nil, nil, fmt.Errorf("resolve this machine's owner principal: %w", err)
	}
	return s, authz.NewMaterializer(s, owner, authz.MaterializerOptions{}), nil
}
