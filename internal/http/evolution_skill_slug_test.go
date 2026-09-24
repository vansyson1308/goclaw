package http

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// A skill draft's frontmatter slug becomes a directory name under the tenant's
// skills store; it must be a plain slug, like every other skill writer
// requires, or an approver could write SKILL.md into another tenant's (or the
// master tenant's) skills.
func TestSkillDraftSlugCannotEscapeTenantSkillsStore(t *testing.T) {
	dataDir := t.TempDir()
	h := &EvolutionHandler{dataDir: dataDir}
	ctx := store.WithTenantID(context.Background(), uuid.New())
	for _, slug := range []string{"../.././../skills-store/victim", "../victim", "a/b", "/abs", "UPPER", "-x"} {
		draft := "---\nname: Helper\nslug: " + slug + "\ndescription: d\n---\nbody\n"
		_, _, _, err := h.createSkillFromDraft(ctx, store.EvolutionSuggestion{ID: uuid.New()}, draft, "u")
		if err == nil || !strings.Contains(err.Error(), "slug") {
			t.Errorf("slug %q: want a slug error, got %v", slug, err)
		}
	}
	var created []string
	_ = filepath.Walk(dataDir, func(p string, fi os.FileInfo, _ error) error {
		if fi != nil && !fi.IsDir() {
			created = append(created, p)
		}
		return nil
	})
	if len(created) > 0 {
		t.Fatalf("rejected drafts wrote files: %v", created)
	}
}
