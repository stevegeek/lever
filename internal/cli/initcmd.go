package cli

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/config"
)

// newInitCmd scaffolds the framework operator skills into the instance tree
// (idempotent; safe to re-run after every lever upgrade or grove addition).
// Purely host-side file operations — never touches the jail, so it works
// before the first `lever up`.
func newInitCmd() *cobra.Command {
	var force, check bool
	cmd := &cobra.Command{
		Use:          "init",
		Short:        "Scaffold/refresh the lever operator skills into the instance tree",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := resolveConfigPath("")
			if err != nil {
				return err
			}
			app, err := config.Load(path)
			if err != nil {
				return err
			}
			stateDir := filepath.Join(filepath.Dir(path), ".lever-state")
			results, err := syncSkills(app, stateDir, force, check)
			if err != nil {
				return err
			}
			blockAct, err := ensureClaudeMDBlock(app.Tree, check)
			if err != nil {
				return err
			}
			all := append(results, skillSyncResult{RelPath: "CLAUDE.md (lever:skills block)", Action: blockAct})
			for _, r := range all {
				switch r.Action {
				case skillCreated, skillRefreshed, skillForced:
					cmd.Printf("✓ %s — %s\n", r.RelPath, r.Action)
				case skillUnchanged:
					cmd.Printf("• %s — unchanged\n", r.RelPath)
				case skillSkipped:
					if check {
						cmd.Printf("✗ %s — locally modified\n", r.RelPath)
					} else {
						cmd.Printf("! %s — locally modified, left alone (re-run with --force to overwrite)\n", r.RelPath)
					}
				case skillMissing, skillStale:
					cmd.Printf("✗ %s — %s\n", r.RelPath, r.Action)
				}
			}
			if check && !skillsUpToDate(results, blockAct) {
				return fmt.Errorf("skills out of date — run `lever init`")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "overwrite locally-modified scaffolds")
	cmd.Flags().BoolVar(&check, "check", false, "report without writing; non-zero exit if anything is missing/stale/modified")
	return cmd
}
