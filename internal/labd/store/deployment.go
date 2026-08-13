package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// DeploymentID derives a stable identity for this labd deployment from
// the database it runs against: a short hash of the Postgres cluster's
// system identifier plus the current database's OID. Two labds on
// different databases (e.g. the dev daemon and the e2e suite's, on the
// same Docker daemon) therefore can never share an id. Derived, not
// persisted — no migration, and it never changes for a given database.
//
// The id is stamped as the lab.deployment label on every agent
// container; the boot sweep treats containers without a matching label
// as foreign and never removes them.
func (s *Store) DeploymentID(ctx context.Context) (string, error) {
	var systemID int64
	var dbOID uint32
	err := s.pool.QueryRow(ctx, `
		SELECT cs.system_identifier, d.oid
		FROM pg_control_system() cs, pg_database d
		WHERE d.datname = current_database()`).Scan(&systemID, &dbOID)
	if err != nil {
		return "", fmt.Errorf("deployment id: %w", err)
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%d", systemID, dbOID)))
	return hex.EncodeToString(sum[:])[:12], nil
}
