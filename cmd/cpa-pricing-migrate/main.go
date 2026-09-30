// cpa-pricing-migrate reads a complete v2 pricing JSON from stdin and writes a
// reviewed replacement to stdout. It never edits a deployment or database.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/wsw/codex-gateway/internal/config"
)

const maxPricingBytes = 4 << 20

func main() {
	before := flag.String("rollback-before", "", "pre-upgrade pricing JSON snapshot")
	applied := flag.String("rollback-applied", "", "applied pricing JSON snapshot (required with -rollback-before)")
	flag.Parse()
	if flag.NArg() != 0 || (*before == "") != (*applied == "") {
		fail(fmt.Errorf("use stdin for pricing; rollback requires both snapshot paths"))
	}
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, maxPricingBytes+1))
	if err != nil || len(raw) > maxPricingBytes {
		fail(fmt.Errorf("read pricing: input unreadable or larger than 4 MiB"))
	}
	var result []byte
	if *before == "" {
		result, err = config.MigrateCPAV8Pricing(raw)
	} else {
		beforeRaw, readErr := readSnapshot(*before)
		if readErr != nil {
			fail(readErr)
		}
		appliedRaw, readErr := readSnapshot(*applied)
		if readErr != nil {
			fail(readErr)
		}
		result, err = config.RestoreCPAV8Pricing(raw, beforeRaw, appliedRaw)
	}
	if err != nil {
		fail(err)
	}
	if _, err := os.Stdout.Write(result); err != nil {
		fail(fmt.Errorf("write pricing: %w", err))
	}
}

func readSnapshot(path string) ([]byte, error) {
	// #nosec G304 -- The local operator selects rollback snapshot paths through CLI flags; this command does not accept remote requests.
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open pricing snapshot: %w", err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxPricingBytes+1))
	if err != nil || len(raw) > maxPricingBytes {
		return nil, fmt.Errorf("read pricing snapshot: unreadable or larger than 4 MiB")
	}
	return raw, nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "cpa-pricing-migrate:", err)
	os.Exit(1)
}
