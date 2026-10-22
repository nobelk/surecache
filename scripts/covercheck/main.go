// SPDX-License-Identifier: Apache-2.0

// Command covercheck enforces statement coverage thresholds on a Go cover profile.
//
// It prints per-file and total statement coverage, fails when the total is below -min,
// and fails when any file named in -full is below 100%. With -report-only it prints the
// same report and always exits zero.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "covercheck:", err)
		os.Exit(1)
	}
}

// coverage counts the statements of one file.
type coverage struct {
	covered, total int
}

func (c coverage) percent() float64 {
	if c.total == 0 {
		return 100
	}
	return 100 * float64(c.covered) / float64(c.total)
}

type options struct {
	profile    string
	module     string
	min        float64
	full       []string
	exclude    []string
	reportOnly bool
}

func run(args []string, stdout io.Writer) error {
	opts, err := parseFlags(args)
	if err != nil {
		return err
	}
	f, err := os.Open(opts.profile)
	if err != nil {
		return err
	}
	defer f.Close()
	files, err := parseProfile(f)
	if err != nil {
		return err
	}
	files = relativize(files, opts.module, opts.exclude)

	failures := check(files, opts)
	report(stdout, files, failures, opts.reportOnly)
	if opts.reportOnly {
		return nil
	}
	return errors.Join(failures...)
}

func parseFlags(args []string) (options, error) {
	var opts options
	var full, exclude string
	fs := flag.NewFlagSet("covercheck", flag.ContinueOnError)
	fs.StringVar(&opts.profile, "profile", "coverage.out", "cover profile to read")
	fs.StringVar(&opts.module, "module", "", "module path (default: read from ./go.mod)")
	fs.Float64Var(&opts.min, "min", 0, "minimum total statement coverage in percent")
	fs.StringVar(&full, "full", "", "comma-separated module-relative files that must be at 100%")
	fs.StringVar(&exclude, "exclude", "", "comma-separated module-relative path prefixes to ignore")
	fs.BoolVar(&opts.reportOnly, "report-only", false, "print the report but never fail")
	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	opts.full = splitList(full)
	opts.exclude = splitList(exclude)
	if opts.module == "" {
		mod, err := moduleFromGoMod()
		if err != nil {
			return opts, err
		}
		opts.module = mod
	}
	return opts, nil
}

func splitList(s string) []string {
	var out []string
	for item := range strings.SplitSeq(s, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func moduleFromGoMod() (string, error) {
	const path = "go.mod"
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("-module not set and %w", err)
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if mod, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.Trim(strings.TrimSpace(mod), `"`), nil
		}
	}
	return "", fmt.Errorf("no module line in %s", path)
}

// parseProfile reads a cover profile and returns statement coverage per file. A block
// listed more than once (several test binaries) counts as covered if any listing hit it.
func parseProfile(r io.Reader) (map[string]coverage, error) {
	sc := bufio.NewScanner(r)
	if !sc.Scan() || !strings.HasPrefix(sc.Text(), "mode: ") {
		return nil, errors.New("profile does not start with a mode line")
	}
	type block struct {
		stmts int
		hit   bool
	}
	blocks := map[string]block{}
	for line := 2; sc.Scan(); line++ {
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		fields := strings.Fields(text)
		if len(fields) != 3 || !strings.Contains(fields[0], ":") {
			return nil, fmt.Errorf("line %d: malformed block %q", line, text)
		}
		stmts, err := strconv.Atoi(fields[1])
		if err != nil {
			return nil, fmt.Errorf("line %d: statement count: %w", line, err)
		}
		hits, err := strconv.Atoi(fields[2])
		if err != nil {
			return nil, fmt.Errorf("line %d: hit count: %w", line, err)
		}
		b := blocks[fields[0]]
		b.stmts = stmts
		b.hit = b.hit || hits > 0
		blocks[fields[0]] = b
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	files := map[string]coverage{}
	for pos, b := range blocks {
		name := pos[:strings.LastIndex(pos, ":")]
		c := files[name]
		c.total += b.stmts
		if b.hit {
			c.covered += b.stmts
		}
		files[name] = c
	}
	return files, nil
}

// relativize keys files by their module-relative path and drops excluded prefixes.
// Files outside the module are dropped.
func relativize(files map[string]coverage, module string, exclude []string) map[string]coverage {
	out := map[string]coverage{}
	for name, c := range files {
		rel, ok := strings.CutPrefix(name, module+"/")
		if !ok {
			continue
		}
		if slices.ContainsFunc(exclude, func(p string) bool { return strings.HasPrefix(rel, p) }) {
			continue
		}
		out[rel] = c
	}
	return out
}

func total(files map[string]coverage) coverage {
	var t coverage
	for _, c := range files {
		t.covered += c.covered
		t.total += c.total
	}
	return t
}

func check(files map[string]coverage, opts options) []error {
	var failures []error
	if pct := total(files).percent(); pct < opts.min {
		failures = append(failures, fmt.Errorf("total coverage %.1f%% is below %.1f%%", pct, opts.min))
	}
	for _, name := range opts.full {
		c, ok := files[name]
		switch {
		case !ok:
			failures = append(failures, fmt.Errorf("%s is not in the profile", name))
		case c.covered < c.total:
			failures = append(failures, fmt.Errorf("%s coverage %.1f%% is below 100%%", name, c.percent()))
		}
	}
	return failures
}

func report(w io.Writer, files map[string]coverage, failures []error, reportOnly bool) {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		c := files[name]
		fmt.Fprintf(w, "%6.1f%%  %4d/%-4d  %s\n", c.percent(), c.covered, c.total, name)
	}
	t := total(files)
	fmt.Fprintf(w, "total %.1f%% (%d/%d statements)\n", t.percent(), t.covered, t.total)
	for _, err := range failures {
		fmt.Fprintln(w, "FAIL", err)
	}
	if reportOnly && len(failures) > 0 {
		fmt.Fprintln(w, "report only: thresholds not enforced")
	}
}
