package apiv2

import (
	"context"
	"net/http"
)

// ItemCollectionsCapability reports that item detail carries the collections
// row: the visible collections the item belongs to.
type ItemCollectionsCapability struct {
	Capability
}

// ItemCollectionsCapabilityOutput is the getItemCollectionsCapability response.
type ItemCollectionsCapabilityOutput struct {
	Status       int
	ETag         string `header:"ETag"`
	CacheControl string `header:"Cache-Control"`
	Body         ItemCollectionsCapability
}

func registerItemCollectionsCapability(reg *Registry) {
	Register(reg, viewerOperation(humaOp(http.MethodGet, Prefix+"/capabilities/item-collections", "getItemCollectionsCapability", "catalog",
		"Whether item detail carries the collections an item belongs to.")), reg.getItemCollectionsCapability)
}

// getItemCollectionsCapability answers available when the assembled server
// wires the collection index behind item detail, and unsupported when it does
// not. Route presence never changes with configuration.
func (reg *Registry) getItemCollectionsCapability(_ context.Context, _ *CapabilityInput) (*ItemCollectionsCapabilityOutput, error) {
	state := StateUnsupported
	if _, ok := reg.deps.LibraryCollections.(ItemCollectionIndex); ok {
		state = StateAvailable
	}
	return &ItemCollectionsCapabilityOutput{
		CacheControl: cacheControlPrivateNoCache,
		Body:         ItemCollectionsCapability{Capability: Capability{State: state}},
	}, nil
}
