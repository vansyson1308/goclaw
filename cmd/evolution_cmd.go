package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/spf13/cobra"

	"github.com/nextlevelbuilder/goclaw/internal/config"
	"github.com/nextlevelbuilder/goclaw/internal/store/pg"
)

func evolutionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "evolution",
		Short: "Agent/skill evolution maintenance",
	}
	var asJSON bool
	reconcile := &cobra.Command{
		Use:   "reconcile",
		Short: "Report inconsistent or legacy evolution records (read-only)",
		Long: "Scans all tenants for evolution records left inconsistent by earlier versions or partial failures " +
			"(legacy threshold applies, tenant-wide tool disables, stuck claims, half-applied skill patches). " +
			"Nothing is modified; each finding names the recommended action.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEvolutionReconcile(asJSON)
		},
	}
	reconcile.Flags().BoolVar(&asJSON, "json", false, "print findings as JSON")
	cmd.AddCommand(reconcile)
	return cmd
}

func runEvolutionReconcile(asJSON bool) error {
	cfg, err := config.Load(resolveConfigPath())
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if cfg.Database.PostgresDSN == "" {
		return fmt.Errorf("database not configured (set GOCLAW_POSTGRES_DSN); the Lite edition is not covered by this report")
	}
	db, err := sql.Open("pgx", cfg.Database.PostgresDSN)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	findings, err := pg.EvolutionReconcile(ctx, db)
	if err != nil {
		return err
	}
	if asJSON {
		if findings == nil {
			findings = []pg.ReconcileFinding{}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(findings)
	}
	if len(findings) == 0 {
		fmt.Println("No inconsistent evolution records found.")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "KIND\tTENANT\tTARGET\tSUGGESTION\tDETAIL")
	for _, f := range findings {
		target := "-"
		if f.AgentID != nil {
			target = "agent:" + f.AgentID.String()
		} else if f.SkillID != nil {
			target = "skill:" + f.SkillID.String()
		}
		sg := "-"
		if f.SuggestionID != nil {
			sg = f.SuggestionID.String()
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", f.Kind, f.TenantID, target, sg, f.Detail)
	}
	tw.Flush()
	fmt.Printf("\n%d finding(s). Recommended actions:\n", len(findings))
	seen := map[string]bool{}
	for _, f := range findings {
		if !seen[f.Kind] {
			seen[f.Kind] = true
			fmt.Printf("  %s: %s\n", f.Kind, f.Action)
		}
	}
	return nil
}
