package http

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/mission"
	"github.com/nextlevelbuilder/goclaw/internal/permissions"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// MissionService is the subset of mission.Service the handler uses.
type MissionService interface {
	Create(ctx context.Context, raw []byte, owner string) (*store.Mission, error)
	Cancel(ctx context.Context, id uuid.UUID, actor, reason string) (*store.Mission, error)
}

// MissionsHandler serves /v1/missions. When missions are disabled (svc nil)
// reads still work and writes return 503 with a hint.
type MissionsHandler struct {
	store store.MissionStore
	svc   MissionService
}

func NewMissionsHandler(st store.MissionStore, svc MissionService) *MissionsHandler {
	return &MissionsHandler{store: st, svc: svc}
}

const maxContractBytes = 64 << 10

func (h *MissionsHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/missions", requireAuth(permissions.RoleViewer, h.handleList))
	// Creating a mission runs code on the gateway's executor: admin, and the
	// handler also requires the master scope. Cancelling is open to operators.
	mux.HandleFunc("POST /v1/missions", requireAuth(permissions.RoleAdmin, h.handleCreate))
	mux.HandleFunc("GET /v1/missions/{id}", requireAuth(permissions.RoleViewer, h.handleGet))
	mux.HandleFunc("GET /v1/missions/{id}/events", requireAuth(permissions.RoleViewer, h.handleEvents))
	mux.HandleFunc("GET /v1/missions/{id}/receipts", requireAuth(permissions.RoleViewer, h.handleReceipts))
	mux.HandleFunc("POST /v1/missions/{id}/cancel", requireAuth(permissions.RoleOperator, h.handleCancel))
}

func (h *MissionsHandler) handleList(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := h.store.ListMissions(r.Context(), limit)
	if err != nil {
		writeMissionInternalError(w, r, err)
		return
	}
	if list == nil {
		list = []store.Mission{}
	}
	for i := range list {
		list[i] = *redactMission(r.Context(), &list[i])
	}
	writeJSON(w, http.StatusOK, map[string]any{"missions": list, "enabled": h.svc != nil})
}

func (h *MissionsHandler) handleCreate(w http.ResponseWriter, r *http.Request) {
	// Verifier commands run on the gateway host until the container executor
	// exists, so creating a mission is a server-wide side effect (like shell
	// access): system owners / master tenant only.
	if !requireMasterScope(w, r) {
		return
	}
	if h.svc == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "missions are disabled on this gateway (set GOCLAW_MISSIONS=1 and restart)"})
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxContractBytes+1))
	if err != nil || len(raw) > maxContractBytes {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "contract body missing or larger than 64 KiB"})
		return
	}
	m, err := h.svc.Create(r.Context(), raw, evolutionActor(r.Context()))
	switch {
	case errors.Is(err, mission.ErrInvalidContract):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	case err != nil:
		writeMissionInternalError(w, r, err)
	default:
		writeJSON(w, http.StatusAccepted, redactMission(r.Context(), m))
	}
}

func (h *MissionsHandler) handleGet(w http.ResponseWriter, r *http.Request) {
	id, ok := missionID(w, r)
	if !ok {
		return
	}
	m, err := h.store.GetMission(r.Context(), id)
	if err != nil {
		writeMissionInternalError(w, r, err)
		return
	}
	if m == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "mission not found"})
		return
	}
	writeJSON(w, http.StatusOK, redactMission(r.Context(), m))
}

func (h *MissionsHandler) handleEvents(w http.ResponseWriter, r *http.Request) {
	id, ok := missionID(w, r)
	if !ok {
		return
	}
	m, err := h.store.GetMission(r.Context(), id)
	if err != nil || m == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "mission not found"})
		return
	}
	events, err := h.store.ListMissionEvents(r.Context(), id)
	if err != nil {
		writeMissionInternalError(w, r, err)
		return
	}
	if events == nil {
		events = []store.MissionEvent{}
	}
	if !store.IsMasterScope(r.Context()) {
		redacted := make([]store.MissionEvent, len(events))
		for i, ev := range events {
			ev.Message = redactHostPaths(m, ev.Message)
			if len(ev.Detail) > 0 {
				ev.Detail = json.RawMessage(redactHostPaths(m, string(ev.Detail)))
			}
			redacted[i] = ev
		}
		events = redacted
	}
	writeJSON(w, http.StatusOK, events)
}

// handleReceipts lists the tool calls of every attempt, in order. Arguments
// are only stored as digests.
func (h *MissionsHandler) handleReceipts(w http.ResponseWriter, r *http.Request) {
	id, ok := missionID(w, r)
	if !ok {
		return
	}
	if m, err := h.store.GetMission(r.Context(), id); err != nil || m == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "mission not found"})
		return
	}
	recs, err := h.store.ListMissionReceipts(r.Context(), id)
	if err != nil {
		writeMissionInternalError(w, r, err)
		return
	}
	if recs == nil {
		recs = []store.MissionReceipt{}
	}
	writeJSON(w, http.StatusOK, recs)
}

func (h *MissionsHandler) handleCancel(w http.ResponseWriter, r *http.Request) {
	if h.svc == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "missions are disabled on this gateway"})
		return
	}
	id, ok := missionID(w, r)
	if !ok {
		return
	}
	m, err := h.svc.Cancel(r.Context(), id, evolutionActor(r.Context()), "")
	switch {
	case errors.Is(err, store.ErrMissionStateConflict):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	case errors.Is(err, store.ErrMissionNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	case err != nil:
		writeMissionInternalError(w, r, err)
	default:
		writeJSON(w, http.StatusOK, redactMission(r.Context(), m))
	}
}

// redactMission hides gateway host paths and worker identity from
// tenant-scoped callers, including paths quoted in error texts.
func redactMission(ctx context.Context, m *store.Mission) *store.Mission {
	if m == nil || store.IsMasterScope(ctx) {
		return m
	}
	cp := *m
	cp.StatusReason = redactHostPaths(m, m.StatusReason)
	if len(m.Verification) > 0 {
		cp.Verification = json.RawMessage(redactHostPaths(m, string(m.Verification)))
	}
	cp.WorkspacePath, cp.LeaseOwner = "", ""
	return &cp
}

// redactHostPaths replaces the mission's directory on the gateway host (the
// parent of its attempt-N directories) in s.
func redactHostPaths(m *store.Mission, s string) string {
	if m.WorkspacePath == "" || s == "" {
		return s
	}
	dir := filepath.Dir(m.WorkspacePath)
	for d := m.WorkspacePath; d != filepath.Dir(d); d = filepath.Dir(d) {
		if strings.HasPrefix(filepath.Base(d), "attempt-") {
			dir = filepath.Dir(d)
			break
		}
	}
	if dir == "/" || dir == "." {
		return s
	}
	return strings.ReplaceAll(s, dir, "<mission-dir>")
}

// writeMissionInternalError logs a store/service failure and returns a
// generic message: error texts can carry SQL or host details.
func writeMissionInternalError(w http.ResponseWriter, r *http.Request, err error) {
	slog.Error("missions.internal_error", "method", r.Method, "path", r.URL.Path, "error", err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
}

func missionID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid mission ID"})
		return uuid.Nil, false
	}
	return id, true
}

// Test hooks (bypass auth middleware; inject tenant/user into the context).
func (h *MissionsHandler) HandleCreateForTest(w http.ResponseWriter, r *http.Request) {
	h.handleCreate(w, r)
}
func (h *MissionsHandler) HandleGetForTest(w http.ResponseWriter, r *http.Request) { h.handleGet(w, r) }
func (h *MissionsHandler) HandleCancelForTest(w http.ResponseWriter, r *http.Request) {
	h.handleCancel(w, r)
}
func (h *MissionsHandler) HandleListForTest(w http.ResponseWriter, r *http.Request) {
	h.handleList(w, r)
}
func (h *MissionsHandler) HandleEventsForTest(w http.ResponseWriter, r *http.Request) {
	h.handleEvents(w, r)
}
func (h *MissionsHandler) HandleReceiptsForTest(w http.ResponseWriter, r *http.Request) {
	h.handleReceipts(w, r)
}
