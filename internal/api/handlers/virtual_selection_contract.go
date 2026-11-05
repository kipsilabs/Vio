package handlers

import "github.com/Silo-Server/silo-server/internal/catalog"

// VirtualSelectionProber schedules pre-play work for an explicitly selected
// virtual version. Implementations must retain their normal generation and
// authority checks.
type VirtualSelectionProber interface {
	ScheduleSelectedFileProbe(contentID string, fileID int, filter catalog.AccessFilter)
	ScheduleSelectedFileWarm(contentID string, fileID int, filter catalog.AccessFilter)
}

// SetVirtualSelectionProber wires probe-on-select for watch-detail reads.
func (h *ItemsHandler) SetVirtualSelectionProber(p VirtualSelectionProber) {
	if h != nil {
		h.virtualSelectionProber = p
	}
}
