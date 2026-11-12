package sections

import "github.com/Silo-Server/silo-server/internal/catalog"

func NewFilterBuilder(alias string) *catalog.QueryBuilder {
	return catalog.NewQueryBuilder(alias)
}
