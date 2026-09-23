package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/nextlevelbuilder/goclaw/internal/mission"
	"github.com/nextlevelbuilder/goclaw/internal/mission/improve"
)

// improveCmd drives the evidence-gated improvement lifecycle for agent
// candidates on the offline mission benchmark.
func improveCmd() *cobra.Command {
	var bench, ledgerPath, executor, image, initial, incidents string
	load := func() (*improve.Benchmark, mission.Executor, error) {
		b, err := improve.LoadBenchmark(bench)
		if err != nil {
			return nil, nil, err
		}
		if incidents != "" {
			if err := b.AddTasks(incidents); err != nil {
				return nil, nil, err
			}
		}
		switch executor {
		case "host":
			return b, mission.HostExecutor{}, nil
		case "docker":
			if err := mission.CheckDockerImage(context.Background(), image); err != nil {
				return nil, nil, err
			}
			return b, mission.DockerExecutor{Image: image}, nil
		}
		return nil, nil, fmt.Errorf("--executor must be host or docker")
	}
	cmd := &cobra.Command{
		Use:   "improve",
		Short: "Evaluate, promote and roll back agent candidates on the mission benchmark",
		Long: "Candidates are promoted only when the benchmark shows no violations, no\n" +
			"regression, held-out not lower and a strict improvement; monitoring rolls a\n" +
			"promotion back when the champion does worse than the one it replaced.\n" +
			"Offline candidates are scripted; this does not show a live model improving.",
	}
	cmd.PersistentFlags().StringVar(&bench, "bench", "evals/improve", "benchmark directory (tasks/, candidates/, testdata/)")
	cmd.PersistentFlags().StringVar(&ledgerPath, "ledger", "improve-ledger.json", "ledger file (champion and decisions)")
	cmd.PersistentFlags().StringVar(&executor, "executor", "host", "verifier executor: host or docker")
	cmd.PersistentFlags().StringVar(&image, "image", "golang:1.26-bookworm", "verifier image for --executor docker")
	cmd.PersistentFlags().StringVar(&initial, "initial-champion", "", "champion to start a new ledger with")
	cmd.PersistentFlags().StringVar(&incidents, "incidents", "", "directory of extra tasks added after promotion")

	evaluate := &cobra.Command{
		Use: "evaluate <candidate>", Short: "Score one candidate", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, ex, err := load()
			if err != nil {
				return err
			}
			s, err := improve.Evaluate(cmd.Context(), b, args[0], nil, ex)
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "TASK\tSPLIT\tSTATUS\tVIOLATION")
			for _, r := range s.Results {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Task, r.Split, r.Status, r.Violation)
			}
			_ = tw.Flush()
			fmt.Printf("\n%s solves %d (dev %d/%d, held-out %d/%d), violations %d, evidence %s\n", s.Candidate, s.SolvedTotal(),
				s.Solved["dev"], s.Total["dev"], s.Solved["heldout"], s.Total["heldout"], s.Violations, s.Digest)
			return nil
		},
	}
	propose := &cobra.Command{
		Use: "propose <candidate>", Short: "Gate a candidate against the champion and record the decision", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, ex, err := load()
			if err != nil {
				return err
			}
			l, err := improve.OpenLedger(ledgerPath, initial)
			if err != nil {
				return err
			}
			d, err := l.Propose(cmd.Context(), b, args[0], ex)
			if err != nil {
				return err
			}
			if err := l.Save(); err != nil {
				return err
			}
			verdict := "REJECTED"
			if d.Promote {
				verdict = "PROMOTED"
			}
			fmt.Printf("%s: %s\n  %s\nchampion: %s\n", args[0], verdict, strings.Join(d.Reasons, "\n  "), l.Champion)
			return nil
		},
	}
	monitor := &cobra.Command{
		Use: "monitor", Short: "Compare the champion with its predecessor and roll back on regression",
		RunE: func(cmd *cobra.Command, _ []string) error {
			b, ex, err := load()
			if err != nil {
				return err
			}
			l, err := improve.OpenLedger(ledgerPath, initial)
			if err != nil {
				return err
			}
			rolled, err := l.Monitor(cmd.Context(), b, ex)
			if err != nil {
				return err
			}
			if err := l.Save(); err != nil {
				return err
			}
			last := l.Events[len(l.Events)-1]
			fmt.Printf("%s: %s\nchampion: %s (rolled back: %v)\n", last.Kind, strings.Join(last.Reasons, "; "), l.Champion, rolled)
			return nil
		},
	}
	status := &cobra.Command{
		Use: "status", Short: "Show the champion and decision history",
		RunE: func(cmd *cobra.Command, _ []string) error {
			l, err := improve.OpenLedger(ledgerPath, initial)
			if err != nil {
				return err
			}
			fmt.Printf("champion: %s (previous: %v)\n", l.Champion, l.Previous)
			for _, e := range l.Events {
				fmt.Printf("%s  %-11s %-4s champion=%s  %s\n", e.At.Format("2006-01-02 15:04"), e.Kind, e.Candidate, e.Champion, strings.Join(e.Reasons, "; "))
			}
			return nil
		},
	}
	cmd.AddCommand(evaluate, propose, monitor, status)
	return cmd
}
