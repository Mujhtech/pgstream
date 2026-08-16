package database

import (
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/mujhtech/pgstream/config"
)

func TestJoinOptionsSkipsEmptyParts(t *testing.T) {
	cases := []struct {
		parts []string
		want  string
	}{
		{[]string{"", "sslmode=require"}, "sslmode=require"},
		{[]string{"a=1", "b=2"}, "a=1&b=2"},
		{[]string{"", ""}, ""},
		{[]string{"a=1", "", "c=3"}, "a=1&c=3"},
	}
	for _, testCase := range cases {
		if got := joinOptions(testCase.parts...); got != testCase.want {
			t.Fatalf("joinOptions(%v) = %q, want %q", testCase.parts, got, testCase.want)
		}
	}
}

func TestTargetTuningOptionsSurviveDsnEncoding(t *testing.T) {
	cfg := config.Database{
		Driver:   config.DatabaseDriverPostgres,
		Host:     "localhost",
		Port:     5432,
		User:     "postgres",
		Password: "secret",
		Database: "target",
		Options:  joinOptions("options="+url.QueryEscape(targetTuningParameters), "sslmode=require&connect_timeout=30"),
	}
	dsn := cfg.BuildDsn()
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	if got := parsed.Query().Get("options"); got != targetTuningParameters {
		t.Fatalf("options runtime parameter = %q, want %q", got, targetTuningParameters)
	}
	if got := parsed.Query().Get("sslmode"); got != "require" {
		t.Fatalf("sslmode = %q, want require", got)
	}
}

func TestIsStartupParameterUnsupported(t *testing.T) {
	if !isStartupParameterUnsupported(errors.New(`pq: unsupported startup parameter: options`)) {
		t.Fatal("PgBouncer-style rejection not recognized")
	}
	if isStartupParameterUnsupported(errors.New("connection refused")) {
		t.Fatal("unrelated error misclassified as startup parameter rejection")
	}
	if isStartupParameterUnsupported(nil) {
		t.Fatal("nil error misclassified")
	}
}

func TestTargetTuningExcludesDurabilityTradeoffs(t *testing.T) {
	// Resume checkpoints assume acknowledged target commits survive a crash;
	// this guards against someone adding synchronous_commit=off later
	// without revisiting that invariant.
	if strings.Contains(targetTuningParameters, "synchronous_commit") {
		t.Fatal("targetTuningParameters must not trade durability for speed")
	}
}
