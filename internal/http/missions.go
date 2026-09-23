package http

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"

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
	mux.HandleFunc("POST /v1/missions", requireAuth(permissions.RoleOperator, h.handleCreate))
	mux.HandleFunc("GET /v1/missions/{id}", requireAuth(permissions.RoleViewer, h.handleGet))
	mux.HandleFunc("GET /v1/missions/{id}/events", requireAuth(permissions.RoleViewer, h.handleEvents))
	mux.HandleFunc("POST /v1/missions/{id}/cancel", requireAuth(permissions.RoleOperator, h.handleCancel))
}

func (h *MissionsHandler) handleList(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := h.store.ListMissions(r.Context(), limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if list == nil {
		list = []store.Mission{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"missions": list, "enabled": h.svc != nil})
}

func (h *MissionsHandler) handleCreate(w http.ResponseWriter, r *http.Request) {
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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
	default:
		writeJSON(w, http.StatusAccepted, m)
	}
}

func (h *MissionsHandler) handleGet(w http.ResponseWriter, r *http.Request) {
	id, ok := missionID(w, r)
	if !ok {
		return
	}
	m, err := h.store.GetMission(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if m == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "mission not found"})
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (h *MissionsHandler) handleEvents(w http.ResponseWriter, r *http.Request) {
	id, ok := missionID(w, r)
	if !ok {
		return
	}
	if m, err := h.store.GetMission(r.Context(), id); err != nil || m == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "mission not found"})
		return
	}
	events, err := h.store.ListMissionEvents(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if events == nil {
		events = []store.MissionEvent{}
	}
	writeJSON(w, http.StatusOK, events)
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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
	default:
		writeJSON(w, http.StatusOK, m)
	}
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
