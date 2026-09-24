package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// missionView mirrors the /v1/missions response fields the CLI prints.
type missionView struct {
	ID              string          `json:"id"`
	Title           string          `json:"title"`
	AgentKey        string          `json:"agent_key"`
	Status          string          `json:"status"`
	StatusReason    string          `json:"status_reason"`
	Executor        string          `json:"executor"`
	OwnerID         string          `json:"owner_id"`
	ChangedFiles    []string        `json:"changed_files"`
	Diff            string          `json:"diff"`
	DiffTruncated   bool            `json:"diff_truncated"`
	Verification    json.RawMessage `json:"verification"`
	Contract        json.RawMessage `json:"contract"`
	Summary         string          `json:"summary"`
	InputTokens     int64           `json:"input_tokens"`
	OutputTokens    int64           `json:"output_tokens"`
	CostUSD         *float64        `json:"cost_usd"`
	UsageIncomplete bool            `json:"usage_incomplete"`
	Attempt         int             `json:"attempt"`
	MaxAttempts     int             `json:"max_attempts"`
	CreatedAt       time.Time       `json:"created_at"`
	FinishedAt      *time.Time      `json:"finished_at"`
}

type criterionView struct {
	ID             string `json:"id"`
	Kind           string `json:"kind"`
	Status         string `json:"status"`
	Detail         string `json:"detail"`
	BaselineStatus string `json:"baseline_status"`
	ProvesChange   bool   `json:"proves_change"`
	OutputTail     string `json:"output_tail"`
}

func missionCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "mission", Short: "Create and inspect verifiable missions (gateway must be running)"}

	var file string
	var wait bool
	var waitTimeout time.Duration
	create := &cobra.Command{
		Use:   "create -f contract.json",
		Short: "Create and start a mission from a contract file",
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			if !json.Valid(raw) {
				return fmt.Errorf("%s is not valid JSON", file)
			}
			requireRunningGatewayHTTP()
			m, err := gatewayHTTPPostTyped[missionView]("/v1/missions", json.RawMessage(raw))
			if err != nil {
				return err
			}
			fmt.Printf("Mission %s created (%s)\n", m.ID, m.Status)
			if !wait {
				return nil
			}
			return waitMission(m.ID, waitTimeout)
		},
	}
	create.Flags().StringVarP(&file, "file", "f", "", "contract JSON file")
	create.Flags().BoolVar(&wait, "wait", false, "wait for the mission to finish and print the result")
	create.Flags().DurationVar(&waitTimeout, "wait-timeout", 30*time.Minute, "maximum time to wait")
	_ = create.MarkFlagRequired("file")

	list := &cobra.Command{
		Use:   "list",
		Short: "List recent missions",
		RunE: func(cmd *cobra.Command, args []string) error {
			requireRunningGatewayHTTP()
			res, err := gatewayHTTPGetTyped[struct {
				Missions []missionView `json:"missions"`
				Enabled  bool          `json:"enabled"`
			}]("/v1/missions")
			if err != nil {
				return err
			}
			if !res.Enabled {
				fmt.Println("Note: missions are disabled on this gateway (GOCLAW_MISSIONS=1 enables them).")
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tSTATUS\tAGENT\tTITLE\tCREATED")
			for _, m := range res.Missions {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", m.ID, m.Status, m.AgentKey, termSafe(m.Title), m.CreatedAt.Local().Format(time.DateTime))
			}
			return tw.Flush()
		},
	}

	var showDiff bool
	show := &cobra.Command{
		Use:   "show <id>",
		Short: "Show a mission's verification evidence",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			requireRunningGatewayHTTP()
			m, err := gatewayHTTPGetTyped[missionView]("/v1/missions/" + args[0])
			if err != nil {
				return err
			}
			printMission(m, showDiff)
			if recs, err := gatewayHTTPGetTyped[[]store.MissionReceipt]("/v1/missions/" + args[0] + "/receipts"); err == nil && len(recs) > 0 {
				fmt.Println("\nTool calls (recorded before each call ran):")
				for _, r := range recs {
					fmt.Printf("  %d.%-3d %-12s %-16s %s\n", r.Attempt, r.Seq, r.Tool, r.ActionClass, r.Status)
				}
			}
			return nil
		},
	}
	show.Flags().BoolVar(&showDiff, "diff", false, "print the full diff")

	var exportSplit, exportID string
	exportTask := &cobra.Command{
		Use:   "export-task <id>",
		Short: "Turn a mission into a benchmark task (e.g. an incident) for `goclaw improve`",
		Long: "Prints the mission's pinned contract as a task JSON. Save it under the\n" +
			"benchmark's incidents/ directory so every future candidate is judged on it.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if exportSplit != "dev" && exportSplit != "heldout" {
				return fmt.Errorf("--split must be dev or heldout")
			}
			requireRunningGatewayHTTP()
			m, err := gatewayHTTPGetTyped[missionView]("/v1/missions/" + args[0])
			if err != nil {
				return err
			}
			if len(m.Contract) == 0 || string(m.Contract) == "null" {
				return fmt.Errorf("mission %s has no contract to export", m.ID)
			}
			id := exportID
			if id == "" {
				id = "incident-" + strings.SplitN(m.ID, "-", 2)[0]
			}
			b, err := json.MarshalIndent(map[string]any{"id": id, "split": exportSplit, "contract": m.Contract}, "", "  ")
			if err != nil {
				return err
			}
			fmt.Println(string(b))
			return nil
		},
	}
	exportTask.Flags().StringVar(&exportSplit, "split", "heldout", "benchmark split for the task (dev or heldout)")
	exportTask.Flags().StringVar(&exportID, "task-id", "", "task id (default incident-<mission id prefix>)")

	cancel := &cobra.Command{
		Use:   "cancel <id>",
		Short: "Cancel a running mission",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			requireRunningGatewayHTTP()
			m, err := gatewayHTTPPostTyped[missionView]("/v1/missions/"+args[0]+"/cancel", map[string]any{})
			if err != nil {
				return err
			}
			fmt.Printf("Mission %s: %s\n", m.ID, m.Status)
			return nil
		},
	}

	cmd.AddCommand(create, list, show, cancel, exportTask, missionEvalCmd())
	return cmd
}

func waitMission(id string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	last := ""
	for time.Now().Before(deadline) {
		m, err := gatewayHTTPGetTyped[missionView]("/v1/missions/" + id)
		if err != nil {
			return err
		}
		if m.Status != last {
			fmt.Printf("  … %s\n", m.Status)
			last = m.Status
		}
		if !slices.Contains(store.MissionActiveStatuses, m.Status) {
			printMission(m, false)
			if m.Status != store.MissionSucceeded {
				return fmt.Errorf("mission finished with status %s", m.Status)
			}
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("timed out waiting for mission %s", id)
}

func printMission(m missionView, withDiff bool) {
	fmt.Printf("Mission   %s\nTitle     %s\nAgent     %s\nStatus    %s\n", m.ID, termSafe(m.Title), m.AgentKey, strings.ToUpper(m.Status))
	if m.StatusReason != "" {
		fmt.Printf("Reason    %s\n", termSafe(m.StatusReason))
	}
	cost := "unknown"
	if m.CostUSD != nil {
		cost = fmt.Sprintf("$%.4f", *m.CostUSD)
	}
	usage := ""
	if m.UsageIncomplete {
		// An attempt ended without reporting usage: totals are a lower bound.
		usage, cost = " (incomplete: an attempt's usage was lost)", "at least "+cost
		if m.CostUSD == nil {
			cost = "unknown"
		}
	}
	fmt.Printf("Usage     %d in / %d out tokens, cost %s%s\nExecutor  %s\nAttempt   %d of %d\n",
		m.InputTokens, m.OutputTokens, cost, usage, m.Executor, m.Attempt, m.MaxAttempts)
	var crs []criterionView
	if len(m.Verification) > 0 && json.Unmarshal(m.Verification, &crs) == nil && len(crs) > 0 {
		fmt.Println("\nAcceptance criteria:")
		for _, c := range crs {
			mark := map[string]string{"pass": "PASS", "fail": "FAIL"}[c.Status]
			if mark == "" {
				mark = "ERROR"
			}
			extra := ""
			if c.ProvesChange {
				extra = " [proves change]"
			}
			if c.BaselineStatus != "" {
				extra += " baseline=" + c.BaselineStatus
			}
			fmt.Printf("  %-5s %s (%s)%s\n", mark, c.ID, c.Kind, extra)
			if c.Detail != "" {
				fmt.Printf("        %s\n", termSafe(c.Detail))
			}
		}
	}
	if len(m.ChangedFiles) > 0 {
		fmt.Printf("\nChanged files: %s\n", termSafe(strings.Join(m.ChangedFiles, ", ")))
	}
	if m.Summary != "" {
		fmt.Printf("\nAgent summary (narrative, not evidence):\n  %s\n", strings.ReplaceAll(strings.TrimSpace(termSafe(m.Summary)), "\n", "\n  "))
	}
	if withDiff && m.Diff != "" {
		fmt.Printf("\n%s\n", termSafe(m.Diff))
		if m.DiffTruncated {
			fmt.Println("[diff truncated]")
		}
	}
}

// termSafe removes control characters (ESC, CR, BEL, C1 controls such as
// CSI) from agent-controlled text before it is printed, keeping newlines and
// tabs, so the text cannot drive the operator's terminal.
func termSafe(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
			return -1
		}
		return r
	}, s)
}
