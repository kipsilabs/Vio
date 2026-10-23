package main

import "testing"

func TestParseEpisodeAliasCommand(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		env      string
		seriesID string
		dryRun   bool
		ok       bool
	}{
		{"apply", []string{"reconcile", "series-1"}, ".env", "series-1", false, true},
		{"leading dry run", []string{"reconcile", "--dry-run", "series-1"}, ".env", "series-1", true, true},
		{"trailing dry run", []string{"reconcile", "series-1", "--dry-run"}, ".env", "series-1", true, true},
		{"leading env and trailing dry run", []string{"reconcile", "-env", "/etc/silo.env", "series-1", "--dry-run"}, "/etc/silo.env", "series-1", true, true},
		{"env after the series", []string{"reconcile", "series-1", "-env", "/etc/silo.env", "--dry-run"}, "/etc/silo.env", "series-1", true, true},
		{"explicit false dry run", []string{"reconcile", "series-1", "--dry-run=false"}, ".env", "series-1", false, true},
		{"unknown flag after the series", []string{"reconcile", "series-1", "--force"}, "", "", false, false},
		{"unknown flag before the series", []string{"reconcile", "--force", "series-1"}, "", "", false, false},
		{"missing series id", []string{"reconcile", "--dry-run"}, "", "", false, false},
		{"two series ids", []string{"reconcile", "series-1", "series-2"}, "", "", false, false},
		{"missing subcommand", nil, "", "", false, false},
		{"unknown subcommand", []string{"reconcile-all", "series-1"}, "", "", false, false},
	} {
		env, seriesID, dryRun, err := parseEpisodeAliasCommand(tc.args)
		if tc.ok != (err == nil) || env != tc.env || seriesID != tc.seriesID || dryRun != tc.dryRun {
			t.Errorf("%s: env %q series %q dry-run %t err %v", tc.name, env, seriesID, dryRun, err)
		}
	}
}
