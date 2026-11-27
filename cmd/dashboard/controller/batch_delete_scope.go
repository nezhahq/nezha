package controller

import (
	"errors"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// errBatchDeleteForeignServer rolls back a server batch delete that names a
// server the caller may not delete or that does not exist.
var errBatchDeleteForeignServer = errors.New("batch delete names a server the caller cannot delete")

// deletableRowIDs returns the IDs among ids that the caller may delete,
// read inside the deleting transaction tx: every existing row for an
// administrator, only the caller's own rows for anyone else.
//
// Batch deletes must DELETE exactly these IDs and drop only them from the
// in-memory registries. Their ownership checks run on a snapshot: the
// registries skip IDs they do not hold yet (create handlers commit the row
// before registering it) and a SELECT cannot see a row committed after it.
// An unscoped "DELETE ... WHERE id IN (?)" therefore let a member delete
// another user's freshly created rows by looping over the next IDs.
func deletableRowIDs(tx *gorm.DB, c *gin.Context, table any, ids []uint64) ([]uint64, error) {
	deletable := []uint64{}
	if len(ids) == 0 {
		return deletable, nil
	}
	query := tx.Model(table).Where("id IN (?)", ids)
	if !callerIsAdmin(c) {
		query = query.Where("user_id = ?", getUid(c))
	}
	err := query.Pluck("id", &deletable).Error
	return deletable, err
}
