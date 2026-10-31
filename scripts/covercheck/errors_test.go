// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
)

func TestRun_RejectsMalformedProfileBeforeReporting(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "coverage.out")
	if err := os.WriteFile(profile, []byte("mode: atomic\nm/a.go:1.1,2.1 invalid 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, reportOnly := range []bool{false, true} {
		args := []string{"-module", "m", "-profile", profile}
		if reportOnly {
			args = append(args, "-report-only")
		}
		var out bytes.Buffer
		err := run(args, &out)
		if err == nil || !strings.Contains(err.Error(), "line 2: statement count:") {
			t.Fatalf("report-only=%t: error = %v, want malformed profile error", reportOnly, err)
		}
		if out.Len() != 0 {
			t.Fatalf("report-only=%t: reported invalid coverage: %s", reportOnly, &out)
		}
	}
}

func TestRun_ModuleDiscovery(t *testing.T) {
	for _, tc := range []struct {
		name    string
		goMod   string
		wantErr string
	}{
		{name: "missing go.mod", wantErr: "-module not set and"},
		{name: "missing module directive", goMod: "go 1.25\n", wantErr: "no module line in go.mod"},
		{name: "quoted module directive", goMod: "// comment\n\n  module \"example.com/m\"  \n\ngo 1.25\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			if tc.goMod != "" {
				if err := os.WriteFile("go.mod", []byte(tc.goMod), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile("coverage.out", []byte("mode: set\nexample.com/m/a.go:1.1,2.1 1 1\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			err := run([]string{"-full", "a.go"}, &out)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tc.wantErr)
				}
				if tc.goMod == "" && !errors.Is(err, os.ErrNotExist) {
					t.Errorf("error = %v, want wrapped os.ErrNotExist", err)
				}
				if out.Len() != 0 {
					t.Fatalf("unexpected report: %s", &out)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "a.go\n") || !strings.Contains(out.String(), "total 100.0% (1/1 statements)") {
				t.Fatalf("unexpected report: %s", &out)
			}
		})
	}
}

func TestParseProfile_ReaderErrors(t *testing.T) {
	readErr := errors.New("profile read failed")
	files, err := parseProfile(io.MultiReader(strings.NewReader("mode: atomic\nm/a.go:1.1,2.1 1 1\n"), iotest.ErrReader(readErr)))
	if !errors.Is(err, readErr) {
		t.Fatalf("error = %v, want underlying read error", err)
	}
	if files != nil {
		t.Fatalf("returned partial coverage on read failure: %v", files)
	}
}

func TestRun_EmptyCoverage(t *testing.T) {
	for _, tc := range []struct {
		name    string
		profile string
	}{
		{name: "no blocks", profile: "mode: atomic\n\n  \n"},
		{name: "zero statements", profile: "mode: set\nm/empty.go:1.1,1.2 0 0\n"},
		{name: "outside module", profile: "mode: count\nother/a.go:1.1,2.1 3 0\nm-extra/b.go:1.1,2.1 2 0\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile := filepath.Join(t.TempDir(), "coverage.out")
			if err := os.WriteFile(profile, []byte(tc.profile), 0o600); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			if err := run([]string{"-module", "m", "-profile", profile, "-min", "100"}, &out); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "total 100.0% (0/0 statements)\n") {
				t.Fatalf("unexpected empty-coverage report: %s", &out)
			}
			if strings.Contains(out.String(), "other/") || strings.Contains(out.String(), "m-extra/") {
				t.Fatalf("report contains files outside the module: %s", &out)
			}
		})
	}
}
