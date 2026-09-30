package store

import (
	"context"
	"database/sql"
	"fmt"
)

// lockNonPendingUserTx keeps administration and resource creation serialized
// with registration review. Callers that need billing-account locks must take
// all of those locks first; callers that lock several users use sorted IDs.
func lockNonPendingUserTx(ctx context.Context, tx *sql.Tx, userID string) error {
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM users WHERE id=$1 FOR UPDATE`, userID).Scan(&status); err != nil {
		return mapDBError("lock user for business operation", err)
	}
	if status == "pending" {
		return fmt.Errorf("%w: user registration is awaiting approval", ErrConflict)
	}
	return nil
}
