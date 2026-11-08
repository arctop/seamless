package mcp

import (
	"context"

	"github.com/arctop/seamless/internal/access"
)

// hiddenByID reports whether an item a caller loaded by its globally unique
// id sits in a project the request's restricted account may not reach. Such a
// lookup answers exactly as a missing id does: the fence refusal would name
// the item's project, telling the account that the item exists and where,
// for an id it never had a way to reach (the same rule taskByIDFor applies to
// tasks). Everyone else keeps the fence's refusal, which they are owed.
func hiddenByID(ctx context.Context, project string) bool {
	return access.AccountOf(ctx) != "" && !access.Visible(ctx, project)
}
