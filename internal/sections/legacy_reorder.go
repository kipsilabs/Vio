package sections

import "context"

// ReorderLegacy applies an ordered entry list from the retired /api/v1 admin
// reorder route. The v2 route uses ReorderScopeIfRevision; this keeps the
// frozen handler's semantics available without exporting the internal helper.
func (r *Repository) ReorderLegacy(ctx context.Context, entries []ReorderEntry) error {
	return r.reorder(ctx, entries)
}
