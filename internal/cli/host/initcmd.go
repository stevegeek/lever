package host

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/cli"
	"github.com/stevegeek/lever/internal/config"
)

// newInitCmd scaffolds the framework operator skills into the instance tree
// (idempotent; safe to re-run after every lever upgrade or worker addition).
// Purely host-side file operations — never touches the jail, so it works
// before the first `lever up`.
func newInitCmd() *cobra.Command {
	var force, check, adopt bool
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
			stateDir := stateFor(path)
			if adopt {
				results, err := adoptSkills(app, stateDir)
				if err != nil {
					return err
				}
				for _, r := range results {
					switch r.Action {
					case skillAdopted:
						cmd.Printf("✓ %s — adopted\n", r.RelPath)
					case skillUnchanged:
						cmd.Printf("• %s — current (no adoption needed)\n", r.RelPath)
					case skillStale:
						cmd.Printf("✗ %s — stale scaffold, run `lever init` to refresh (not adopted)\n", r.RelPath)
					case skillMissing:
						cmd.Printf("✗ %s — missing (not adoptable)\n", r.RelPath)
					}
				}
				return nil
			}
			if !check {
				if err := ensureTreeDir(app.Tree); err != nil {
					return err
				}
			}
			results, err := syncSkills(app, stateDir, force, check)
			if err != nil {
				return err
			}
			blockAct, err := ensureClaudeMDBlock(app.Tree, stateDir, force, check)
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
				case skillAdopted:
					// Version-lag warning applies to SKILL.md scaffolds only —
					// the CLAUDE.md block entry carries no lever-version stamp.
					if strings.HasSuffix(r.RelPath, "SKILL.md") && r.AdoptedVersion != cli.Version {
						v := r.AdoptedVersion
						if v == "" {
							v = "unknown"
						}
						cmd.Printf("! %s — custom (adopted baseline %s; framework is %s — missing later framework guidance, see `lever doctor`)\n", r.RelPath, v, cli.Version)
					} else {
						cmd.Printf("• %s — custom (adopted baseline)\n", r.RelPath)
					}
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
	cmd.Flags().BoolVar(&adopt, "adopt", false, "record customized scaffolds as an accepted baseline (doctor then treats them as OK)")
	cmd.MarkFlagsMutuallyExclusive("adopt", "force")
	cmd.MarkFlagsMutuallyExclusive("adopt", "check")
	return cmd
}

// ensureTreeDir creates the instance tree when it does not exist yet: the
// documented order runs `lever init` before the first `lever up`, on a fresh
// lever.yaml whose tree: directory the operator has not made. Only the tree
// itself is created (0755), and only under a parent that already exists — a
// missing parent means a typo'd tree:, not a fresh instance. A path that
// exists is left alone, except a symlink that dangles: creating its target
// would let a planted link choose where the scaffold lands, so it is
// refused (an existing tree that is a link to a directory keeps working, as
// the tree-confined writes resolve it). Mkdir does not follow a final
// symlink, so a link planted between the Lstat and the Mkdir fails with
// EEXIST rather than being followed.
func ensureTreeDir(tree string) error {
	fi, err := os.Lstat(tree)
	switch {
	case err == nil:
		if fi.Mode()&fs.ModeSymlink == 0 {
			return nil
		}
		if _, err := os.Stat(tree); errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("tree %s is a dangling symbolic link — point it at an existing directory or remove it", tree)
		}
		return nil
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	parent := filepath.Dir(tree)
	if pfi, err := os.Stat(parent); err != nil {
		return fmt.Errorf("tree %s: parent %s: %w", tree, parent, err)
	} else if !pfi.IsDir() {
		return fmt.Errorf("tree %s: parent %s is not a directory", tree, parent)
	}
	if err := os.Mkdir(tree, 0o755); err != nil {
		return fmt.Errorf("create tree %s: %w", tree, err)
	}
	return nil
}
