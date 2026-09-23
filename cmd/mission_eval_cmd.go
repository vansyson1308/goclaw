package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/nextlevelbuilder/goclaw/internal/mission"
	"github.com/nextlevelbuilder/goclaw/internal/mission/evalsuite"
)

// missionEvalCmd runs the offline mission evaluation suite locally (no
// gateway, database or model needed).
func missionEvalCmd() *cobra.Command {
	var suite, splits, only, executor, image, out, md string
	cmd := &cobra.Command{
		Use:          "eval",
		SilenceUsage: true,
		Short:        "Run the offline mission evaluation suite (scripted agents, real verifiers)",
		Long: "Replays each case's scripted agent through the real tool guard, verifiers and\n" +
			"outcome rules, and checks the verdict. Measures how missions judge agent\n" +
			"behaviour; it does not measure a live model.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cases, err := evalsuite.LoadCases(suite)
			if err != nil {
				return err
			}
			var want []string
			if splits != "" {
				want = strings.Split(splits, ",")
			}
			cases = evalsuite.Filter(cases, want)
			if only != "" {
				ids := strings.Split(only, ",")
				var sel []evalsuite.Case
				for _, c := range cases {
					for _, id := range ids {
						if c.ID == id {
							sel = append(sel, c)
						}
					}
				}
				if len(sel) != len(ids) {
					return fmt.Errorf("--case: some ids were not found in the selected splits")
				}
				cases = sel
			}
			var exec mission.Executor = mission.HostExecutor{}
			if executor == "docker" {
				if err := mission.CheckDockerImage(cmd.Context(), image); err != nil {
					return err
				}
				exec = mission.DockerExecutor{Image: image}
			} else if executor != "host" {
				return fmt.Errorf("--executor must be host or docker")
			}
			fmt.Fprintf(os.Stderr, "running %d case(s) with the %s executor…\n", len(cases), exec.Name())
			rep, err := evalsuite.Run(context.WithoutCancel(cmd.Context()), cases, evalsuite.Options{SourceRoot: filepath.Join(suite, "testdata"), Executor: exec})
			if err != nil {
				return err
			}
			printEvalReport(rep)
			if out != "" {
				b, _ := json.MarshalIndent(rep, "", "  ")
				if err := os.WriteFile(out, append(b, '\n'), 0o644); err != nil {
					return err
				}
			}
			if md != "" {
				if err := os.WriteFile(md, []byte(evalsuite.Markdown(rep)), 0o644); err != nil {
					return err
				}
			}
			if !rep.OK() {
				return fmt.Errorf("evaluation failed: %d/%d passed, %d false success(es)", rep.Passed, rep.Total-rep.Skipped, rep.FalseSuccess)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&suite, "suite", "evals/missions", "suite directory (cases/ and testdata/)")
	cmd.Flags().StringVar(&splits, "split", "", "comma-separated splits: dev,regression,heldout (default all)")
	cmd.Flags().StringVar(&only, "case", "", "comma-separated case ids to run")
	cmd.Flags().StringVar(&executor, "executor", "host", "verifier executor: host or docker")
	cmd.Flags().StringVar(&image, "image", "golang:1.26-bookworm", "verifier image for --executor docker")
	cmd.Flags().StringVar(&out, "out", "", "write the JSON report to this file")
	cmd.Flags().StringVar(&md, "md", "", "write a Markdown report to this file")
	return cmd
}

func printEvalReport(rep *evalsuite.Report) {
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "CASE\tSPLIT\tEXPECTED\tACTUAL\tRESULT")
	for _, c := range rep.Cases {
		result := "PASS"
		switch {
		case c.Skipped != "":
			result = "SKIP (" + c.Skipped + ")"
		case !c.Pass:
			result = "FAIL: " + strings.Join(c.Mismatches, "; ")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", c.ID, c.Split, c.Expected, c.Actual, result)
	}
	_ = tw.Flush()
	fmt.Printf("\n%d/%d passed (%d skipped); false successes: %d; false failures: %d\n",
		rep.Passed, rep.Total-rep.Skipped, rep.Skipped, rep.FalseSuccess, rep.FalseFailure)
	for _, split := range []string{evalsuite.SplitDev, evalsuite.SplitRegression, evalsuite.SplitHeldOut} {
		if sc, ok := rep.BySplit[split]; ok {
			fmt.Printf("  %-10s %d/%d\n", split, sc.Passed, sc.Total)
		}
	}
}
