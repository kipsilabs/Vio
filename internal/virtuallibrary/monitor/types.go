package monitor

// Narrow core replacements for the plugin-SDK protobuf boundary the ported
// monitor previously used. Only the fields the monitor actually reads are
// modeled; field names mirror the old shape so the ported logic stays
// verbatim, and getters are nil-safe like the generated Get* helpers.

type Library struct {
	ID        string
	Name      string
	MediaType string
}

func (x *Library) GetId() string {
	if x != nil {
		return x.ID
	}
	return ""
}

func (x *Library) GetName() string {
	if x != nil {
		return x.Name
	}
	return ""
}

func (x *Library) GetMediaType() string {
	if x != nil {
		return x.MediaType
	}
	return ""
}

type RequestDescriptor struct {
	MediaType    string
	Title        string
	Year         int32
	ExternalIds  map[string]string
	PosterPath   string
	BackdropPath string
}

func (x *RequestDescriptor) GetMediaType() string {
	if x != nil {
		return x.MediaType
	}
	return ""
}

func (x *RequestDescriptor) GetTitle() string {
	if x != nil {
		return x.Title
	}
	return ""
}

func (x *RequestDescriptor) GetYear() int32 {
	if x != nil {
		return x.Year
	}
	return 0
}

func (x *RequestDescriptor) GetExternalIds() map[string]string {
	if x != nil {
		return x.ExternalIds
	}
	return nil
}

type RouterConnection struct {
	ID     string
	Config map[string]any
}

func (x *RouterConnection) GetId() string {
	if x != nil {
		return x.ID
	}
	return ""
}

func (x *RouterConnection) GetConfig() map[string]any {
	if x != nil {
		return x.Config
	}
	return nil
}

type RequestedQuality struct {
	ID   string
	Is4K bool
}

func (x *RequestedQuality) GetId() string {
	if x != nil {
		return x.ID
	}
	return ""
}

type FulfillRequest struct {
	Request     *RequestDescriptor
	Qualities   []*RequestedQuality
	Connections []*RouterConnection
}

func (x *FulfillRequest) GetRequest() *RequestDescriptor {
	if x != nil {
		return x.Request
	}
	return nil
}

func (x *FulfillRequest) GetQualities() []*RequestedQuality {
	if x != nil {
		return x.Qualities
	}
	return nil
}

func (x *FulfillRequest) GetConnections() []*RouterConnection {
	if x != nil {
		return x.Connections
	}
	return nil
}

type FulfillmentTarget struct {
	Quality        string
	ConnectionId   string
	ExternalId     string
	ExternalStatus string
	Status         string
	Message        string
}

type FulfillResponse struct {
	Targets []*FulfillmentTarget
	Message string
}

type TargetRef struct {
	Quality      string
	ConnectionId string
	ExternalId   string
}

func (x *TargetRef) GetQuality() string {
	if x != nil {
		return x.Quality
	}
	return ""
}

func (x *TargetRef) GetConnectionId() string {
	if x != nil {
		return x.ConnectionId
	}
	return ""
}

func (x *TargetRef) GetExternalId() string {
	if x != nil {
		return x.ExternalId
	}
	return ""
}

type CheckStatusRequest struct {
	Request     *RequestDescriptor
	Targets     []*TargetRef
	Connections []*RouterConnection
}

func (x *CheckStatusRequest) GetRequest() *RequestDescriptor {
	if x != nil {
		return x.Request
	}
	return nil
}

func (x *CheckStatusRequest) GetTargets() []*TargetRef {
	if x != nil {
		return x.Targets
	}
	return nil
}

type TargetStatus struct {
	Quality        string
	ConnectionId   string
	Status         string
	ExternalStatus string
	Message        string
}

type CheckStatusResponse struct {
	Statuses []*TargetStatus
}

type ConfigOption struct {
	Value string
	Label string
}

type ConfigOptionList struct {
	Options []*ConfigOption
}

type ListConfigOptionsRequest struct{}

type ListConfigOptionsResponse struct {
	OptionsByField map[string]*ConfigOptionList
}

type ValidateRequest struct{}

type ValidateResponse struct {
	FieldErrors map[string]string
	FormError   string
}

type TestConnectionRequest struct{}

type TestConnectionResponse struct {
	Ok      bool
	Message string
}

type RunScheduledTaskRequest struct {
	TaskKey string
	Input   map[string]any
}

func (x *RunScheduledTaskRequest) GetTaskKey() string {
	if x != nil {
		return x.TaskKey
	}
	return ""
}

type RunScheduledTaskResponse struct {
	Output map[string]any
}
