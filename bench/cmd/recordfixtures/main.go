// Command recordfixtures runs realistic, host-shaped hook calls against every
// plugin under Sobek (the oracle) and writes the results as new-api
// pkg/jsplugin.Fixture JSON files into bench/testdata/fixtures.
//
//	go run ./cmd/recordfixtures [-plugin key] [-out dir]
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/MoeForge/moejs/bench"
	"github.com/MoeForge/moejs/bench/engines"
)

func main() {
	var (
		only = flag.String("plugin", "", "record only this plugin key")
		out  = flag.String("out", filepath.Join(bench.TestdataDir(), "fixtures"), "output directory")
	)
	flag.Parse()
	if err := os.MkdirAll(*out, 0o755); err != nil {
		log.Fatal(err)
	}
	oracle, err := bench.NewRunner(engines.NewSobekEngine())
	if err != nil {
		log.Fatal(err)
	}
	total := 0
	for _, key := range bench.PluginKeys {
		if *only != "" && key != *only {
			continue
		}
		list, ok := scenarios[key]
		if !ok {
			log.Fatalf("no scenarios for %s", key)
		}
		fixture, err := record(oracle, key, list)
		if err != nil {
			log.Fatalf("%s: %v", key, err)
		}
		data, err := json.MarshalIndent(fixture, "", "  ")
		if err != nil {
			log.Fatal(err)
		}
		path := filepath.Join(*out, key+".json")
		if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
			log.Fatal(err)
		}
		errCases := 0
		for _, c := range fixture.Cases {
			if c.ExpectedError != "" {
				errCases++
			}
		}
		fmt.Printf("%-10s %3d cases (%d throw) -> %s\n", key, len(fixture.Cases), errCases, path)
		total += len(fixture.Cases)
	}
	fmt.Printf("total %d cases\n", total)
}

func record(oracle *bench.Runner, key string, list []scenario) (*bench.Fixture, error) {
	rt, err := oracle.Runtime(key)
	if err != nil {
		return nil, err
	}
	defer rt.Close()
	unixNow := engines.DefaultHost.UnixNow
	fixture := &bench.Fixture{UnixNow: &unixNow}
	seen := map[string]bool{}
	for _, s := range list {
		if seen[s.Name] {
			return nil, fmt.Errorf("duplicate scenario name %q", s.Name)
		}
		seen[s.Name] = true
		c := bench.FixtureCase{Name: s.Name, Hook: s.Hook, Member: s.Member, Path: s.Path}
		for i, a := range s.Args {
			raw, err := json.Marshal(a)
			if err != nil {
				return nil, fmt.Errorf("%s: argument %d: %w", s.Name, i+1, err)
			}
			c.Args = append(c.Args, raw)
		}
		// Call with the JSON-decoded arguments, exactly as a replay will.
		result, err := bench.Replay(rt, c)
		switch {
		case err == nil:
			raw, err := json.Marshal(result)
			if err != nil {
				return nil, fmt.Errorf("%s: encode result: %w", s.Name, err)
			}
			c.Expected = raw
		case errors.Is(err, engines.ErrNotFound):
			return nil, fmt.Errorf("%s: hook %s not found (scenario bug)", s.Name, c.HookName())
		default:
			he, ok := engines.AsHookError(err)
			if !ok {
				return nil, fmt.Errorf("%s: %w", s.Name, err)
			}
			if he.Message == "" {
				return nil, fmt.Errorf("%s: hook threw without a message", s.Name)
			}
			c.ExpectedError = he.Message
		}
		fixture.Cases = append(fixture.Cases, c)
	}
	return fixture, nil
}
