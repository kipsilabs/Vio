package apiv2

import "net/http"

func itemCollectionsCapabilityFixtureCases() []fixtureCase {
	viewer := with(bearer(memberToken), "X-Profile-Id", "p-owner")
	problem := "#/components/schemas/Problem"
	return []fixtureCase{
		{name: "get_item_collections_capability_ok", operationID: "getItemCollectionsCapability",
			scenario: "The item-collections capability document on a server whose item detail carries the collections row.",
			method:   http.MethodGet, path: "/api/v2/capabilities/item-collections", headers: viewer,
			status: http.StatusOK, assertHeaders: []string{"Content-Type", "Cache-Control"}, schema: "#/components/schemas/ItemCollectionsCapability"},
		{name: "get_item_collections_capability_authentication_required", operationID: "getItemCollectionsCapability",
			scenario: "The item-collections capability document without a credential.",
			method:   http.MethodGet, path: "/api/v2/capabilities/item-collections",
			status: http.StatusUnauthorized, assertHeaders: []string{"Content-Type", "Cache-Control"}, schema: problem},
	}
}
