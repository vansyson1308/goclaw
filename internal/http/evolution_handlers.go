package http

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/agent"
	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/permissions"
	"github.com/nextlevelbuilder/goclaw/internal/skills"
	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/pkg/protocol"
)

// EvolutionHandler serves evolution metrics and suggestion endpoints.
type EvolutionHandler struct {
	metrics     store.EvolutionMetricsStore
	suggestions store.EvolutionSuggestionStore

	// Optional: skill creation on SuggestSkillAdd approval.
	// Nil-safe — skill creation disabled if any is nil.
	skillStore  store.SkillManageStore
	skillLoader *skills.Loader
	dataDir     string

	// Optional: agent store for applying agent-scoped config suggestions.
	agentStore store.AgentStore

	// Optional: broadcasts agent cache invalidation after a config change.
	msgBus *bus.MessageBus

	// Tenant membership lookup for the tenant-admin gate on writes.
	tenantStore store.TenantStore
}

// EvolutionHandlerOpt configures optional EvolutionHandler dependencies.
type EvolutionHandlerOpt func(*EvolutionHandler)

// WithSkillCreation enables skill creation when approving skill_add suggestions.
func WithSkillCreation(ss store.SkillManageStore, loader *skills.Loader, dataDir string) EvolutionHandlerOpt {
	return func(h *EvolutionHandler) {
		h.skillStore = ss
		h.skillLoader = loader
		h.dataDir = dataDir
	}
}

// WithAgentStore enables applying agent-scoped config suggestions (tool_order).
func WithAgentStore(as store.AgentStore) EvolutionHandlerOpt {
	return func(h *EvolutionHandler) { h.agentStore = as }
}

// WithTenantStore enables the tenant owner/admin check on suggestion writes
// for tenant-scoped callers (without it they are refused).
func WithTenantStore(ts store.TenantStore) EvolutionHandlerOpt {
	return func(h *EvolutionHandler) { h.tenantStore = ts }
}

// WithMessageBus enables agent cache invalidation after applied/rolled-back changes.
func WithMessageBus(mb *bus.MessageBus) EvolutionHandlerOpt {
	return func(h *EvolutionHandler) { h.msgBus = mb }
}

func NewEvolutionHandler(m store.EvolutionMetricsStore, s store.EvolutionSuggestionStore, opts ...EvolutionHandlerOpt) *EvolutionHandler {
	h := &EvolutionHandler{metrics: m, suggestions: s}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

func (h *EvolutionHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/agents/{agentID}/evolution/metrics", h.auth(h.handleGetMetrics))
	mux.HandleFunc("GET /v1/agents/{agentID}/evolution/suggestions", h.auth(h.handleListSuggestions))
	mux.HandleFunc("PATCH /v1/agents/{agentID}/evolution/suggestions/{suggestionID}", h.tenantAdmin(h.handleUpdateSuggestion))
	mux.HandleFunc("GET /v1/agents/{agentID}/evolution/suggestions/{suggestionID}/events", h.auth(h.handleListEvents))
}

func (h *EvolutionHandler) auth(next http.HandlerFunc) http.HandlerFunc {
	return requireAuth("", next)
}

// tenantAdmin gates suggestion writes like the direct agent/skill writes they
// stand in for (apply changes agent config or creates tenant skills): admin
// role, and a tenant owner/admin when the caller is tenant-scoped.
func (h *EvolutionHandler) tenantAdmin(next http.HandlerFunc) http.HandlerFunc {
	return requireAuth(permissions.RoleAdmin, func(w http.ResponseWriter, r *http.Request) {
		if !store.IsMasterScope(r.Context()) && !requireTenantAdmin(w, r, h.tenantStore) {
			return
		}
		next(w, r)
	})
}

// handleGetMetrics returns raw or aggregated evolution metrics for an agent.
// Query params: type (tool|retrieval|feedback), since (ISO timestamp), aggregate (true/false).
func (h *EvolutionHandler) handleGetMetrics(w http.ResponseWriter, r *http.Request) {
	agentID, err := uuid.Parse(r.PathValue("agentID"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid agent ID"})
		return
	}

	metricType := store.MetricType(r.URL.Query().Get("type"))
	aggregate := r.URL.Query().Get("aggregate") == "true"

	since := time.Now().AddDate(0, 0, -7) // default 7 days
	if s := r.URL.Query().Get("since"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			since = t
		}
	}

	ctx := r.Context()

	// Aggregated response: tool + retrieval aggregates combined.
	if aggregate {
		toolAggs, err := h.metrics.AggregateToolMetrics(ctx, agentID, since)
		if err != nil {
			slog.Warn("evolution.aggregate_tool failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		retrievalAggs, err := h.metrics.AggregateRetrievalMetrics(ctx, agentID, since)
		if err != nil {
			slog.Warn("evolution.aggregate_retrieval failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if toolAggs == nil {
			toolAggs = []store.ToolAggregate{}
		}
		if retrievalAggs == nil {
			retrievalAggs = []store.RetrievalAggregate{}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"tool_aggregates":      toolAggs,
			"retrieval_aggregates": retrievalAggs,
		})
		return
	}

	// Raw metrics query.
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	metrics, err := h.metrics.QueryMetrics(ctx, agentID, metricType, since, limit)
	if err != nil {
		slog.Warn("evolution.query_metrics failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if metrics == nil {
		metrics = []store.EvolutionMetric{}
	}
	writeJSON(w, http.StatusOK, metrics)
}

// handleListSuggestions returns evolution suggestions for an agent.
// Query params: status (pending|approved|applied|rejected|rolled_back), limit.
func (h *EvolutionHandler) handleListSuggestions(w http.ResponseWriter, r *http.Request) {
	agentID, err := uuid.Parse(r.PathValue("agentID"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid agent ID"})
		return
	}

	status := r.URL.Query().Get("status")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}

	suggestions, err := h.suggestions.ListSuggestions(r.Context(), agentID, status, limit)
	if err != nil {
		slog.Warn("evolution.list_suggestions failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if suggestions == nil {
		suggestions = []store.EvolutionSuggestion{}
	}
	writeJSON(w, http.StatusOK, suggestions)
}

// handleUpdateSuggestion reviews a suggestion: approve (apply when the type
// has a reversible config change), reject, or roll back an applied one.
// The audit actor is always the authenticated identity; a client-supplied
// reviewed_by is ignored.
func (h *EvolutionHandler) handleUpdateSuggestion(w http.ResponseWriter, r *http.Request) {
	locale := extractLocale(r)
	ctx := r.Context()
	agentID, err := uuid.Parse(r.PathValue("agentID"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid agent ID"})
		return
	}

	suggestionID, err := uuid.Parse(r.PathValue("suggestionID"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid suggestion ID"})
		return
	}

	// Verify suggestion belongs to the agent in the URL path.
	existing, err := h.suggestions.GetSuggestion(ctx, suggestionID)
	if err != nil || existing == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "suggestion not found"})
		return
	}
	if existing.AgentID != agentID {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "suggestion does not belong to this agent"})
		return
	}

	var body struct {
		Status     string `json:"status"`
		SkillDraft string `json:"skill_draft,omitempty"` // override draft content for skill_add approval
		Reason     string `json:"reason,omitempty"`
	}
	if !bindJSON(w, r, locale, &body) {
		return
	}
	actor := evolutionActor(ctx)

	switch body.Status {
	case store.SuggestionApproved:
		h.approveSuggestion(w, r, *existing, body.SkillDraft, actor)
	case store.SuggestionRejected:
		updated, err := h.suggestions.TransitionSuggestion(ctx, store.SuggestionTransition{
			// "applying" is included so an operator can resolve a claim left by
			// a crash (see `goclaw evolution reconcile`); any side effect that
			// did complete (e.g. a created skill) is left for them to handle.
			ID: suggestionID, From: []string{store.SuggestionPending, store.SuggestionApproved, store.SuggestionApplying},
			To: store.SuggestionRejected, Actor: actor, Action: "reject",
			Detail: map[string]any{"reason": body.Reason},
		})
		writeTransitionResult(w, updated, "rejected", err)
	case store.SuggestionRolledBack:
		if existing.SuggestionType == store.SuggestSkillAdd {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "skill_add cannot be rolled back here; archive or delete the created skill instead"})
			return
		}
		updated, err := agent.RollbackSuggestion(ctx, h.suggestions, *existing, actor, body.Reason)
		if err == nil {
			h.invalidateAgent(ctx, agentID)
		}
		writeTransitionResult(w, updated, "rolled_back", err)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "status must be approved, rejected, or rolled_back"})
	}
}

func (h *EvolutionHandler) approveSuggestion(w http.ResponseWriter, r *http.Request, sg store.EvolutionSuggestion, skillDraft, actor string) {
	ctx := r.Context()
	switch sg.SuggestionType {
	case store.SuggestSkillAdd:
		updated, err := h.applySkillDraft(ctx, sg, skillDraft, actor)
		writeTransitionResult(w, updated, "skill_created", err)

	case store.SuggestToolOrder:
		if h.agentStore == nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "tool_order apply not available"})
			return
		}
		ag, err := h.agentStore.GetByID(ctx, sg.AgentID)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "agent not found"})
			return
		}
		g := agent.GuardrailsForAgent(ag)
		if err := agent.CheckLockedTarget(g, "tools_config", []string{"deny"}); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		dataPoints, err := h.toolCallCount(ctx, sg)
		if err != nil {
			slog.Warn("evolution.query_metrics_for_guardrail failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to query metrics for guardrail check"})
			return
		}
		if err := agent.CheckGuardrails(g, sg, dataPoints); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		updated, err := agent.ApplyToolOrder(ctx, h.suggestions, sg, actor)
		if err == nil {
			h.invalidateAgent(ctx, sg.AgentID)
		}
		writeTransitionResult(w, updated, "tool_denied_for_agent", err)

	default:
		// threshold (and unknown types): no safe automatic change exists.
		updated, err := agent.AcknowledgeAdvisory(ctx, h.suggestions, sg, actor,
			"advisory suggestion: review manually; no automatic config change is applied")
		writeTransitionResult(w, updated, "acknowledged", err)
	}
}

// toolCallCount counts the tool's recorded calls over the last 7 days — the
// data behind a tool_order suggestion — for the MinDataPoints guardrail.
func (h *EvolutionHandler) toolCallCount(ctx context.Context, sg store.EvolutionSuggestion) (int, error) {
	var params struct {
		Tool string `json:"tool"`
	}
	_ = json.Unmarshal(sg.Parameters, &params)
	aggs, err := h.metrics.AggregateToolMetrics(ctx, sg.AgentID, time.Now().AddDate(0, 0, -7))
	if err != nil {
		return 0, err
	}
	for _, a := range aggs {
		if a.ToolName == params.Tool {
			return a.CallCount, nil
		}
	}
	return 0, nil
}

// handleListEvents returns the audit trail of a suggestion.
func (h *EvolutionHandler) handleListEvents(w http.ResponseWriter, r *http.Request) {
	agentID, err1 := uuid.Parse(r.PathValue("agentID"))
	suggestionID, err2 := uuid.Parse(r.PathValue("suggestionID"))
	if err1 != nil || err2 != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid ID"})
		return
	}
	sg, err := h.suggestions.GetSuggestion(r.Context(), suggestionID)
	if err != nil || sg == nil || sg.AgentID != agentID {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "suggestion not found"})
		return
	}
	events, err := h.suggestions.ListSuggestionEvents(r.Context(), suggestionID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if events == nil {
		events = []store.EvolutionEvent{}
	}
	writeJSON(w, http.StatusOK, events)
}

func (h *EvolutionHandler) invalidateAgent(ctx context.Context, agentID uuid.UUID) {
	if h.msgBus == nil || h.agentStore == nil {
		return
	}
	ag, err := h.agentStore.GetByID(ctx, agentID)
	if err != nil {
		return
	}
	h.msgBus.Broadcast(bus.Event{
		Name:    protocol.EventCacheInvalidate,
		Payload: bus.CacheInvalidatePayload{Kind: bus.CacheKindAgent, Key: ag.AgentKey},
	})
}

// evolutionActor is the authenticated identity recorded in the audit trail.
func evolutionActor(ctx context.Context) string {
	if id := store.ActorIDFromContext(ctx); id != "" {
		return id
	}
	if role := store.RoleFromContext(ctx); role != "" {
		return "token:" + role
	}
	return "unknown"
}

func writeTransitionResult(w http.ResponseWriter, sg *store.EvolutionSuggestion, action string, err error) {
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "action": action, "suggestion": sg})
	case errors.Is(err, store.ErrSuggestionStateConflict), errors.Is(err, agent.ErrRollbackConflict):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	case errors.Is(err, agent.ErrNotApplicable):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	default:
		slog.Warn("evolution.transition failed", "action", action, "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
}

// HandleUpdateSuggestionForTest invokes the review handler without auth
// middleware. Integration tests must inject tenant and user into the request
// context. Production code MUST go through RegisterRoutes.
func (h *EvolutionHandler) HandleUpdateSuggestionForTest(w http.ResponseWriter, r *http.Request) {
	h.handleUpdateSuggestion(w, r)
}

// HandleListEventsForTest invokes the audit-trail handler without auth middleware.
func (h *EvolutionHandler) HandleListEventsForTest(w http.ResponseWriter, r *http.Request) {
	h.handleListEvents(w, r)
}
