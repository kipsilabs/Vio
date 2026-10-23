package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/config"
	"github.com/Silo-Server/silo-server/internal/database"
)

const (
	episodeAliasReconcileCommand = "reconcile"
	episodeAliasEnvFlag          = "-env"
	episodeAliasCommandUsage     = "usage: silo episode-aliases " + episodeAliasReconcileCommand + " [" + episodeAliasEnvFlag + " .env] [--dry-run] <series-id>"
)

// runEpisodeAliasCommand is the supported, auditable activation path for
// episode aliases. It reads only the database and writes only a bounded set of
// title/air-date matches for one series; --dry-run leaves the table untouched.
func runEpisodeAliasCommand(ctx context.Context, args []string, out io.Writer) error {
	envFile, seriesID, dryRun, err := parseEpisodeAliasCommand(args)
	if err != nil {
		return err
	}
	dbURL, err := config.LoadDatabaseURL(envFile)
	if err != nil {
		return err
	}
	pool, err := database.NewPoolForRole(ctx, config.DatabaseConfig{URL: dbURL, MaxConnections: 2}, "application")
	if err != nil {
		return fmt.Errorf("database pool: %w", err)
	}
	defer database.ClosePool(pool)

	result, err := catalog.NewEpisodeAliasRepository(pool).ReconcileSeries(ctx, seriesID, dryRun)
	if err != nil {
		return err
	}
	writeEpisodeAliasReport(out, result)
	return nil
}

func parseEpisodeAliasCommand(args []string) (envFile, seriesID string, dryRun bool, err error) {
	if len(args) == 0 || args[0] != episodeAliasReconcileCommand {
		return "", "", false, errors.New(episodeAliasCommandUsage)
	}
	flags := flag.NewFlagSet("episode-aliases "+episodeAliasReconcileCommand, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	env := flags.String("env", ".env", "path to .env bootstrap file")
	dry := flags.Bool("dry-run", false, "report candidates without writing")
	// The flag package stops at the first positional argument, and the
	// documented form puts --dry-run after <series-id>. Move flags ahead of
	// the positional argument so both orders parse identically; an unknown
	// flag still reaches the flag set and fails there.
	flagArgs, positional := splitEpisodeAliasArgs(args[1:])
	if parseErr := flags.Parse(append(flagArgs, positional...)); parseErr != nil || flags.NArg() != 1 || flags.Arg(0) == "" {
		return "", "", false, errors.New(episodeAliasCommandUsage)
	}
	return *env, flags.Arg(0), *dry, nil
}

// splitEpisodeAliasArgs separates flags (and the separate value of -env) from
// positional arguments so the flag set can parse a trailing --dry-run. A "--"
// terminator stays in the flag list, so the flag set still treats everything
// after it as positional.
func splitEpisodeAliasArgs(args []string) (flagArgs, positional []string) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == episodeAliasEnvFlag || arg == "--env":
			flagArgs = append(flagArgs, arg)
			if i+1 < len(args) {
				i++
				flagArgs = append(flagArgs, args[i])
			}
		case strings.HasPrefix(arg, "-") && arg != "-":
			flagArgs = append(flagArgs, arg)
		default:
			positional = append(positional, arg)
		}
	}
	return flagArgs, positional
}

func writeEpisodeAliasReport(out io.Writer, result *catalog.EpisodeAliasReconcileResult) {
	mode := "applied"
	if result.DryRun {
		mode = "dry run"
	}
	_, _ = fmt.Fprintf(out, "series %s: %s, %d candidate(s), %d skipped\n",
		result.SeriesID, mode, len(result.Candidates), len(result.Skipped))
	for _, candidate := range result.Candidates {
		_, _ = fmt.Fprintf(out,
			"  %s (S%02dE%02d) -> %s (S%02dE%02d)  %s  %s\n",
			candidate.CanonicalEpisodeID,
			candidate.CanonicalSeasonNumber, candidate.CanonicalEpisodeNumber,
			candidate.AliasEpisodeID,
			candidate.AliasSeasonNumber, candidate.AliasEpisodeNumber,
			candidate.AirDate.Format("2006-01-02"),
			candidate.Title,
		)
	}
	for _, skipped := range result.Skipped {
		_, _ = fmt.Fprintf(out, "  skipped %s %q: %s (%v)\n",
			skipped.AirDate.Format("2006-01-02"), skipped.Title, skipped.Reason, skipped.EpisodeIDs)
	}
	if result.DryRun {
		_, _ = fmt.Fprintln(out, "dry run: no rows were written")
	}
}
