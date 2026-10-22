// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"strings"
	"testing"
)

const fixture = "testdata/profile.out"

func TestParseProfile_MergesDuplicateBlocks(t *testing.T) {
	r := strings.NewReader(`mode: atomic
example.com/m/a.go:1.1,2.2 2 0
example.com/m/a.go:1.1,2.2 2 3
example.com/m/a.go:4.1,5.2 1 0
`)
	files, err := parseProfile(r)
	if err != nil {
		t.Fatal(err)
	}
	got := files["example.com/m/a.go"]
	if got.covered != 2 || got.total != 3 {
		t.Fatalf("a.go = %d/%d, want 2/3", got.covered, got.total)
	}
}

func TestParseProfile_RejectsMalformed(t *testing.T) {
	cases := map[string]string{
		"no mode line":     "example.com/m/a.go:1.1,2.2 2 0\n",
		"missing fields":   "mode: set\nexample.com/m/a.go:1.1,2.2 2\n",
		"no colon":         "mode: set\nexample.com/m/a.go 2 0\n",
		"bad stmt count":   "mode: set\nexample.com/m/a.go:1.1,2.2 x 0\n",
		"bad hit count":    "mode: set\nexample.com/m/a.go:1.1,2.2 2 y\n",
		"empty profile":    "",
		"mode line broken": "mod: set\n",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseProfile(strings.NewReader(in)); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestRun(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantErr string
		wantOut []string
	}{
		{
			name:    "passes above minimum",
			args:    []string{"-profile", fixture, "-module", "github.com/nobelk/surecache", "-min", "70"},
			wantOut: []string{"total 70.0%", "read.go", "mutate.go"},
		},
		{
			name:    "fails below minimum",
			args:    []string{"-profile", fixture, "-module", "github.com/nobelk/surecache", "-min", "70.1"},
			wantErr: "total coverage 70.0% is below 70.1%",
		},
		{
			name: "full file at 100 passes",
			args: []string{"-profile", fixture, "-module", "github.com/nobelk/surecache", "-full", "read.go"},
		},
		{
			name:    "full file below 100 fails",
			args:    []string{"-profile", fixture, "-module", "github.com/nobelk/surecache", "-full", "read.go,mutate.go"},
			wantErr: "mutate.go coverage 75.0% is below 100%",
		},
		{
			name:    "full file missing from profile fails",
			args:    []string{"-profile", fixture, "-module", "github.com/nobelk/surecache", "-full", "rebuild.go"},
			wantErr: "rebuild.go is not in the profile",
		},
		{
			name:    "report only never fails",
			args:    []string{"-profile", fixture, "-module", "github.com/nobelk/surecache", "-min", "99", "-full", "mutate.go,rebuild.go", "-report-only"},
			wantOut: []string{"FAIL", "report only"},
		},
		{
			name:    "exclude drops a path prefix",
			args:    []string{"-profile", fixture, "-module", "github.com/nobelk/surecache", "-exclude", "internal/", "-min", "87.5"},
			wantOut: []string{"total 87.5%"},
		},
		{
			name:    "missing profile",
			args:    []string{"-profile", "testdata/absent.out", "-module", "m"},
			wantErr: "absent.out",
		},
		{
			name:    "bad flag",
			args:    []string{"-nope"},
			wantErr: "flag provided but not defined",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := run(tc.args, &out)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("error = %v, want containing %q", err, tc.wantErr)
			}
			for _, w := range tc.wantOut {
				if !strings.Contains(out.String(), w) {
					t.Errorf("output missing %q:\n%s", w, out.String())
				}
			}
		})
	}
}

func TestRun_ModuleFromGoMod(t *testing.T) {
	t.Chdir("testdata/mod")
	var out bytes.Buffer
	if err := run([]string{"-profile", "../profile.out", "-full", "read.go"}, &out); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
}
