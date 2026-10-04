package api

import (
	"context"
	"errors"
	"strconv"

	"github.com/Silo-Server/silo-server/internal/plugins"
	mediarequests "github.com/Silo-Server/silo-server/internal/requests"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/monitor"
)

// PluginRequestRouterAdapter adapts plugins.Service to
// mediarequests.RouterClientResolver. The concrete *pluginhost.RequestRouterClient
// satisfies mediarequests.RouterClient, so the adapter names the interface as its
// return type (Go has no return-type covariance).
type PluginRequestRouterAdapter struct {
	Svc *plugins.Service
}

func (a PluginRequestRouterAdapter) RequestRouterClient(ctx context.Context, installationID int, capabilityID string) (mediarequests.RouterClient, error) {
	if a.Svc == nil {
		return nil, errors.New("request router plugin service is not configured")
	}
	return a.Svc.RequestRouterClient(ctx, installationID, capabilityID)
}

type virtualLibraryRequestRouter struct {
	vlSvc *virtuallibrary.Service
}

func requestDescriptorOf(req mediarequests.Request) *monitor.RequestDescriptor {
	var year int32
	if req.Year != nil {
		year = int32(*req.Year)
	}
	tmdbStr := ""
	if req.TMDBID > 0 {
		tmdbStr = strconv.Itoa(req.TMDBID)
	}
	tvdbStr := ""
	if req.TVDBID != nil && *req.TVDBID > 0 {
		tvdbStr = strconv.Itoa(*req.TVDBID)
	}
	return &monitor.RequestDescriptor{
		MediaType: string(req.MediaType),
		Title:     req.Title,
		Year:      year,
		ExternalIds: map[string]string{
			"imdb": req.IMDbID,
			"tmdb": tmdbStr,
			"tvdb": tvdbStr,
		},
	}
}

func (v *virtualLibraryRequestRouter) Fulfill(ctx context.Context, _ int, _ string, req mediarequests.Request, qualities []mediarequests.Quality, conns []mediarequests.ResolvedRouterConnection) ([]mediarequests.RouterTarget, string, error) {
	if v.vlSvc == nil || v.vlSvc.Monitor == nil {
		return nil, "", errors.New("virtual library monitor is unavailable")
	}
	reqDesc := requestDescriptorOf(req)
	qs := make([]*monitor.RequestedQuality, 0, len(qualities))
	for _, q := range qualities {
		qs = append(qs, &monitor.RequestedQuality{ID: string(q), Is4K: q == "4k"})
	}
	mConns := make([]*monitor.RouterConnection, 0, len(conns))
	for _, c := range conns {
		mConns = append(mConns, &monitor.RouterConnection{ID: c.ID, Config: c.Config})
	}
	resp, err := v.vlSvc.Monitor.Fulfill(ctx, &monitor.FulfillRequest{
		Request:     reqDesc,
		Qualities:   qs,
		Connections: mConns,
	})
	if err != nil {
		return nil, "", err
	}
	targets := make([]mediarequests.RouterTarget, 0, len(resp.Targets))
	for _, t := range resp.Targets {
		targets = append(targets, mediarequests.RouterTarget{
			Quality:        mediarequests.Quality(t.Quality),
			ConnectionID:   t.ConnectionId,
			ExternalID:     t.ExternalId,
			ExternalStatus: t.ExternalStatus,
			Status:         mediarequests.Status(t.Status),
			Message:        t.Message,
		})
	}
	return targets, resp.Message, nil
}

func (v *virtualLibraryRequestRouter) CheckStatus(ctx context.Context, _ int, _ string, req mediarequests.Request, targets []mediarequests.RouterTargetRef, conns []mediarequests.ResolvedRouterConnection) ([]mediarequests.RouterTargetStatus, error) {
	if v.vlSvc == nil || v.vlSvc.Monitor == nil {
		return nil, errors.New("virtual library monitor is unavailable")
	}
	reqDesc := requestDescriptorOf(req)
	mTargets := make([]*monitor.TargetRef, 0, len(targets))
	for _, t := range targets {
		mTargets = append(mTargets, &monitor.TargetRef{
			Quality:      string(t.Quality),
			ConnectionId: t.ConnectionID,
			ExternalId:   t.ExternalID,
		})
	}
	mConns := make([]*monitor.RouterConnection, 0, len(conns))
	for _, c := range conns {
		mConns = append(mConns, &monitor.RouterConnection{ID: c.ID, Config: c.Config})
	}
	resp, err := v.vlSvc.Monitor.CheckStatus(ctx, &monitor.CheckStatusRequest{
		Request:     reqDesc,
		Targets:     mTargets,
		Connections: mConns,
	})
	if err != nil {
		return nil, err
	}
	statuses := make([]mediarequests.RouterTargetStatus, 0, len(resp.Statuses))
	for _, s := range resp.Statuses {
		statuses = append(statuses, mediarequests.RouterTargetStatus{
			Quality:        mediarequests.Quality(s.Quality),
			ConnectionID:   s.ConnectionId,
			Status:         mediarequests.Status(s.Status),
			ExternalStatus: s.ExternalStatus,
			Message:        s.Message,
		})
	}
	return statuses, nil
}

func (v *virtualLibraryRequestRouter) ListConfigOptions(ctx context.Context, _ int, _ string, _ mediarequests.ResolvedRouterConnection) (map[string][]mediarequests.RouterOption, error) {
	if v.vlSvc == nil || v.vlSvc.Monitor == nil {
		return nil, nil
	}
	resp, err := v.vlSvc.Monitor.ListConfigOptions(ctx, &monitor.ListConfigOptionsRequest{})
	if err != nil {
		return nil, err
	}
	out := make(map[string][]mediarequests.RouterOption)
	for k, list := range resp.OptionsByField {
		if list == nil {
			continue
		}
		opts := make([]mediarequests.RouterOption, 0, len(list.Options))
		for _, o := range list.Options {
			opts = append(opts, mediarequests.RouterOption{
				Value: o.Value,
				Label: o.Label,
			})
		}
		out[k] = opts
	}
	return out, nil
}

func (v *virtualLibraryRequestRouter) TestConnection(ctx context.Context, _ int, _ string, _ mediarequests.ResolvedRouterConnection) (bool, string, error) {
	if v.vlSvc == nil || v.vlSvc.Monitor == nil {
		return false, "virtual library is unavailable", nil
	}
	resp, err := v.vlSvc.Monitor.TestConnection(ctx, &monitor.TestConnectionRequest{})
	if err != nil {
		return false, "", err
	}
	return resp.Ok, resp.Message, nil
}

func (v *virtualLibraryRequestRouter) Validate(_ context.Context, _ int, _ string, _ mediarequests.ResolvedRouterConnection, _ []mediarequests.ResolvedRouterConnection) (map[string]string, string, error) {
	return nil, "", nil
}

type compositeRequestRouter struct {
	virtual mediarequests.RequestRouterProvider
	plugin  mediarequests.RequestRouterProvider
}

func (c *compositeRequestRouter) isVirtual(installationID int, capabilityID string) bool {
	return installationID <= 0 || capabilityID == "virtual-library-requests"
}

func (c *compositeRequestRouter) Fulfill(ctx context.Context, installationID int, capabilityID string, req mediarequests.Request, qualities []mediarequests.Quality, conns []mediarequests.ResolvedRouterConnection) ([]mediarequests.RouterTarget, string, error) {
	if c.isVirtual(installationID, capabilityID) {
		if c.virtual == nil {
			return nil, "", errors.New("virtual library request router is unavailable")
		}
		return c.virtual.Fulfill(ctx, installationID, capabilityID, req, qualities, conns)
	}
	if c.plugin == nil {
		return nil, "", errors.New("request router plugin service is not configured")
	}
	return c.plugin.Fulfill(ctx, installationID, capabilityID, req, qualities, conns)
}

func (c *compositeRequestRouter) CheckStatus(ctx context.Context, installationID int, capabilityID string, req mediarequests.Request, targets []mediarequests.RouterTargetRef, conns []mediarequests.ResolvedRouterConnection) ([]mediarequests.RouterTargetStatus, error) {
	if c.isVirtual(installationID, capabilityID) {
		if c.virtual == nil {
			return nil, errors.New("virtual library request router is unavailable")
		}
		return c.virtual.CheckStatus(ctx, installationID, capabilityID, req, targets, conns)
	}
	if c.plugin == nil {
		return nil, errors.New("request router plugin service is not configured")
	}
	return c.plugin.CheckStatus(ctx, installationID, capabilityID, req, targets, conns)
}

func (c *compositeRequestRouter) ListConfigOptions(ctx context.Context, installationID int, capabilityID string, conn mediarequests.ResolvedRouterConnection) (map[string][]mediarequests.RouterOption, error) {
	if c.isVirtual(installationID, capabilityID) {
		if c.virtual == nil {
			return nil, nil
		}
		return c.virtual.ListConfigOptions(ctx, installationID, capabilityID, conn)
	}
	if c.plugin == nil {
		return nil, nil
	}
	return c.plugin.ListConfigOptions(ctx, installationID, capabilityID, conn)
}

func (c *compositeRequestRouter) TestConnection(ctx context.Context, installationID int, capabilityID string, conn mediarequests.ResolvedRouterConnection) (bool, string, error) {
	if c.isVirtual(installationID, capabilityID) {
		if c.virtual == nil {
			return false, "virtual library request router is unavailable", nil
		}
		return c.virtual.TestConnection(ctx, installationID, capabilityID, conn)
	}
	if c.plugin == nil {
		return false, "request router plugin service is not configured", nil
	}
	return c.plugin.TestConnection(ctx, installationID, capabilityID, conn)
}

func (c *compositeRequestRouter) Validate(ctx context.Context, installationID int, capabilityID string, conn mediarequests.ResolvedRouterConnection, siblings []mediarequests.ResolvedRouterConnection) (map[string]string, string, error) {
	if c.isVirtual(installationID, capabilityID) {
		if c.virtual == nil {
			return nil, "", nil
		}
		return c.virtual.Validate(ctx, installationID, capabilityID, conn, siblings)
	}
	if c.plugin == nil {
		return nil, "", nil
	}
	return c.plugin.Validate(ctx, installationID, capabilityID, conn, siblings)
}

// RouterFeatures forwards the optional request_router.v1 feature read to
// the provider that owns the capability. Without this the Service sees the
// composite wrapper — which declares nothing itself — and season- or
// progress-capable plugins read as incapable in mixed deployments. The
// core virtual router declares no optional features, matching the
// pre-composite behavior where its provider never satisfied the reader
// assertion.
func (c *compositeRequestRouter) RouterFeatures(ctx context.Context, installationID int, capabilityID string) (mediarequests.RouterFeatures, error) {
	if c.isVirtual(installationID, capabilityID) {
		return mediarequests.RouterFeatures{}, nil
	}
	if reader, ok := c.plugin.(mediarequests.RouterFeatureReader); ok {
		return reader.RouterFeatures(ctx, installationID, capabilityID)
	}
	return mediarequests.RouterFeatures{}, nil
}

// RouterFeatures reads the optional request_router.v1 features the
// capability's stored manifest declares, without launching the plugin.
func (a PluginRequestRouterAdapter) RouterFeatures(ctx context.Context, installationID int, capabilityID string) (mediarequests.RouterFeatures, error) {
	if a.Svc == nil {
		return mediarequests.RouterFeatures{}, errors.New("request router plugin service is not configured")
	}
	descriptor, err := a.Svc.RequestRouterDescriptor(ctx, installationID, capabilityID)
	if err != nil {
		return mediarequests.RouterFeatures{}, err
	}
	return mediarequests.RouterFeatures{
		SupportsSeasons:         descriptor.GetSupportsSeasons(),
		ReportsDownloadProgress: descriptor.GetReportsDownloadProgress(),
	}, nil
}

// AttachRequestRouter wires the router provider onto a requests service, combining
// core virtual library routing (when active) and external plugin routing.
func AttachRequestRouter(svc *mediarequests.Service, pluginService *plugins.Service, vlSvc ...*virtuallibrary.Service) {
	if svc == nil {
		return
	}
	var virtualRouter mediarequests.RequestRouterProvider
	var activeVL *virtuallibrary.Service
	if len(vlSvc) > 0 && vlSvc[0] != nil {
		activeVL = vlSvc[0]
		virtualRouter = &virtualLibraryRequestRouter{vlSvc: activeVL}
		svc.SetDefaultVirtualRouter(func() bool {
			return activeVL != nil
		})
	}
	var pluginRouter mediarequests.RequestRouterProvider
	if pluginService != nil {
		pluginRouter = mediarequests.NewPluginRouterProvider(PluginRequestRouterAdapter{pluginService})
	}

	if virtualRouter != nil && pluginRouter != nil {
		svc.SetRouterProvider(&compositeRequestRouter{virtual: virtualRouter, plugin: pluginRouter})
	} else if virtualRouter != nil {
		svc.SetRouterProvider(virtualRouter)
	} else if pluginRouter != nil {
		svc.SetRouterProvider(pluginRouter)
	}
}
