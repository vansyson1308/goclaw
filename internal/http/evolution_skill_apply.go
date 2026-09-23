package http

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/config"
	"github.com/nextlevelbuilder/goclaw/internal/skills"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// applySkillDraft creates a managed skill from a SuggestSkillAdd suggestion.
// Uses draftOverride if provided, otherwise falls back to the suggestion's parameters.skill_draft.
//
// The suggestion is claimed first (pending|approved → applying) so two
// reviewers cannot create the skill twice. Validation failures release the
// claim back to pending; a crash after the claim leaves a visible "applying"
// row for the reconciliation report instead of a silent half-applied state.
func (h *EvolutionHandler) applySkillDraft(ctx context.Context, sg store.EvolutionSuggestion, draftOverride, actor string) (*store.EvolutionSuggestion, error) {
	if h.skillStore == nil || h.skillLoader == nil {
		return nil, fmt.Errorf("skill creation not available")
	}
	if _, err := h.suggestions.TransitionSuggestion(ctx, store.SuggestionTransition{
		ID: sg.ID, From: []string{store.SuggestionPending, store.SuggestionApproved},
		To: store.SuggestionApplying, Actor: actor, Action: "claim",
	}); err != nil {
		return nil, err
	}
	skillID, slug, version, err := h.createSkillFromDraft(ctx, sg, draftOverride, actor)
	// Bookkeeping after the claim must survive a client disconnect, or the
	// row would stay "applying" although the outcome is known.
	bookCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err != nil {
		if _, relErr := h.suggestions.TransitionSuggestion(bookCtx, store.SuggestionTransition{
			ID: sg.ID, From: []string{store.SuggestionApplying}, To: store.SuggestionPending,
			Actor: actor, Action: "apply_failed", Detail: map[string]any{"error": err.Error()},
		}); relErr != nil {
			slog.Warn("evolution.skill_apply: release claim failed", "suggestion", sg.ID, "error", relErr)
		}
		return nil, err
	}
	updated, err := h.suggestions.TransitionSuggestion(bookCtx, store.SuggestionTransition{
		ID: sg.ID, From: []string{store.SuggestionApplying}, To: store.SuggestionApplied,
		Actor: actor, Action: "apply",
		Detail: map[string]any{"skill_id": skillID.String(), "slug": slug, "version": version},
	})
	if err != nil {
		// Skill exists but the suggestion stays "applying": surfaced by reconciliation.
		return nil, fmt.Errorf("skill %s created but suggestion status not recorded: %w", slug, err)
	}
	slog.Info("evolution.skill_apply: created", "skill_id", skillID, "slug", slug, "version", version, "suggestion", sg.ID)
	return updated, nil
}

func (h *EvolutionHandler) createSkillFromDraft(ctx context.Context, sg store.EvolutionSuggestion, draftOverride, owner string) (uuid.UUID, string, int, error) {

	// Resolve draft content: request override > suggestion parameters.
	draft := draftOverride
	if draft == "" {
		var params map[string]any
		if err := json.Unmarshal(sg.Parameters, &params); err == nil {
			draft, _ = params["skill_draft"].(string)
		}
	}
	if draft == "" {
		return uuid.Nil, "", 0, fmt.Errorf("no skill_draft content found")
	}

	// Security scan before any disk write.
	violations, safe := skills.GuardSkillContent(draft)
	if !safe {
		return uuid.Nil, "", 0, fmt.Errorf("skill draft failed security scan: %s", skills.FormatGuardViolations(violations))
	}

	// Parse frontmatter for metadata.
	name, description, slug, frontmatter := skills.ParseSkillFrontmatter(draft)
	if name == "" {
		return uuid.Nil, "", 0, fmt.Errorf("skill draft missing 'name' in frontmatter")
	}
	if slug == "" {
		slug = skills.Slugify(name)
	}

	// Resolve tenant-scoped destination directory.
	tenantID := store.TenantIDFromContext(ctx)
	tenantSlug := store.TenantSlugFromContext(ctx)
	baseDir := config.TenantSkillsStoreDir(h.dataDir, tenantID, tenantSlug)

	version := h.skillStore.GetNextVersion(ctx, slug)
	destDir := filepath.Join(baseDir, slug, fmt.Sprintf("%d", version))
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return uuid.Nil, "", 0, fmt.Errorf("create skill directory: %w", err)
	}

	// Write SKILL.md file.
	contentBytes := []byte(draft)
	if err := os.WriteFile(filepath.Join(destDir, "SKILL.md"), contentBytes, 0644); err != nil {
		return uuid.Nil, "", 0, fmt.Errorf("write SKILL.md: %w", err)
	}

	// DB insert.
	hasher := sha256.New()
	hasher.Write(contentBytes)
	fileHash := fmt.Sprintf("%x", hasher.Sum(nil))
	desc := description

	id, err := h.skillStore.CreateSkillManaged(ctx, store.SkillCreateParams{
		Name:        name,
		Slug:        slug,
		Description: &desc,
		OwnerID:     owner,
		Visibility:  "private",
		Version:     version,
		FilePath:    destDir,
		FileSize:    int64(len(contentBytes)),
		FileHash:    &fileHash,
		Frontmatter: frontmatter,
	})
	if err != nil {
		return uuid.Nil, "", 0, fmt.Errorf("register skill: %w", err)
	}

	// Bump loader to pick up new skill.
	h.skillLoader.BumpVersion()
	return id, slug, version, nil
}
