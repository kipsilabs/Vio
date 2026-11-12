package handlers

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/Silo-Server/silo-server/internal/diagnostics"
)

type AdminDiagnosticsService interface {
	ListForAdmin(ctx context.Context, filters diagnostics.ListFilters) (diagnostics.ListResult, error)
	GetReport(ctx context.Context, id string) (*diagnostics.Report, error)
	PresignReportDownload(ctx context.Context, report *diagnostics.Report, expiry time.Duration) (string, error)
	OpenReportDownload(ctx context.Context, report *diagnostics.Report) (io.ReadCloser, error)
	DeleteReport(ctx context.Context, id string) (*diagnostics.Report, error)
}

<<<<<<< HEAD
type adminDiagnosticsEffectiveTTL interface {
	EffectiveReportDownloadTTL(requested time.Duration) time.Duration
}

type diagnosticsDownloadURLResponse struct {
	DownloadURL string    `json:"download_url"`
	ExpiresAt   time.Time `json:"expires_at"`
}

func RegisterAdminDiagnosticsRoutes(r chi.Router, h *DiagnosticsHandler) {
	if h == nil {
		return
	}
	r.Route("/diagnostics/reports", func(r chi.Router) {
		r.Get("/", h.HandleAdminListReports)
		r.Get("/{id}", h.HandleAdminGetReport)
		r.Get("/{id}/download", h.HandleAdminDownloadReport)
		r.Delete("/{id}", h.HandleAdminDeleteReport)
	})
}

func (h *DiagnosticsHandler) HandleAdminListReports(w http.ResponseWriter, r *http.Request) {
	admin, ok := h.admin()
	if !ok {
		writeDiagnosticsAdminUnavailable(w)
		return
	}
	filters, err := parseDiagnosticsReportListFilters(r)
	if err != nil {
		writeDiagnosticsAdminError(w, err)
		return
	}
	result, err := admin.ListForAdmin(r.Context(), filters)
	if err != nil {
		writeDiagnosticsAdminError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *DiagnosticsHandler) HandleAdminGetReport(w http.ResponseWriter, r *http.Request) {
	admin, ok := h.admin()
	if !ok {
		writeDiagnosticsAdminUnavailable(w)
		return
	}
	report, err := admin.GetReport(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeDiagnosticsAdminError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func (h *DiagnosticsHandler) HandleAdminDownloadReport(w http.ResponseWriter, r *http.Request) {
	admin, ok := h.admin()
	if !ok {
		writeDiagnosticsAdminUnavailable(w)
		return
	}
	reportID := chi.URLParam(r, "id")
	report, err := admin.GetReport(r.Context(), reportID)
	if err != nil {
		writeDiagnosticsAdminError(w, err)
		return
	}
	if report.State != diagnostics.StateReady {
		writeDiagnosticsAdminError(w, diagnostics.ErrReportNotReady)
		return
	}

	ttl := diagnosticsEffectiveDownloadTTL(admin, diagnosticsDownloadExpiry)
	if !diagnosticsProxyDownloadRequested(r) {
		if url, err := admin.PresignReportDownload(r.Context(), report, ttl); err == nil && strings.TrimSpace(url) != "" {
			h.auditDownload(r, report.ID)
			writeJSON(w, http.StatusOK, diagnosticsDownloadURLResponse{
				DownloadURL: url,
				ExpiresAt:   time.Now().UTC().Add(ttl),
			})
			return
		}
	}

	body, err := admin.OpenReportDownload(r.Context(), report)
	if err != nil {
		writeDiagnosticsAdminError(w, err)
		return
	}
	defer func() { _ = body.Close() }()

	h.auditDownload(r, report.ID)
	// Without presigning this is the only delivery path, and a bundle can
	// outlast the API server's absolute WriteTimeout on a slow link.
	sw := httpstream.NewRollingDeadlineWriter(w)
	sw.Header().Set("Content-Type", diagnostics.ReportDownloadContentType)
	sw.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", diagnosticsReportFilename(report)))
	if report.BlobBytes != nil && *report.BlobBytes >= 0 {
		sw.Header().Set("Content-Length", strconv.FormatInt(*report.BlobBytes, 10))
	}
	sw.WriteHeader(http.StatusOK)
	if _, err := io.Copy(sw, body); err != nil {
		h.diagnosticsLogger().WarnContext(r.Context(), "diagnostic report download stream failed",
			"component", "diagnostics",
			"admin_user_id", currentAdminUserID(r),
			"report_id", report.ID,
			"error", err,
		)
	}
}

func (h *DiagnosticsHandler) HandleAdminDeleteReport(w http.ResponseWriter, r *http.Request) {
	admin, ok := h.admin()
	if !ok {
		writeDiagnosticsAdminUnavailable(w)
		return
	}
	reportID := chi.URLParam(r, "id")
	report, err := admin.DeleteReport(r.Context(), reportID)
	if err != nil {
		writeDiagnosticsAdminError(w, err)
		return
	}
	h.diagnosticsLogger().InfoContext(r.Context(), "diagnostic report deleted",
		"component", "diagnostics",
		"admin_user_id", currentAdminUserID(r),
		"report_id", report.ID,
	)
	w.WriteHeader(http.StatusNoContent)
}

=======
>>>>>>> upstream/main
func (h *DiagnosticsHandler) admin() (AdminDiagnosticsService, bool) {
	if h.adminService == nil {
		return nil, false
	}
	return h.adminService, true
}

func (h *DiagnosticsHandler) diagnosticsLogger() *slog.Logger {
	if h.logger == nil {
		return slog.Default()
	}
	return h.logger
}

func diagnosticsReportFilename(report *diagnostics.Report) string {
	name := strings.TrimSpace(report.ShortID)
	if name == "" {
		name = strings.TrimSpace(report.ID)
	}
	if name == "" {
		name = "report"
	}
	return "silo-diagnostics-" + name + ".tar.gz"
}
