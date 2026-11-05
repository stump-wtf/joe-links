// Command adr-check fails when one ADR number is claimed by more than one
// design record, which is what happens when two build lanes cut branches off a
// moving main and each takes the next number that looked free there. It runs as
// part of `make lint`; the same check runs inside `go test ./...` so the CI
// test job catches it too. Governing: ADR-0025, issue #345.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/joestump/joe-links/internal/adrguard"
)

func main() {
	dirs := flag.String("dirs", "docs/adrs", "comma-separated design-record directories to check")
	flag.Parse()

	if err := run(*dirs); err != nil {
		fmt.Fprintln(os.Stderr, "adr-check: "+err.Error())
		os.Exit(1)
	}
}

func run(list string) error {
	var claims []adrguard.Claim
	for _, dir := range strings.Split(list, ",") {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		found, err := adrguard.Scan(dir)
		switch {
		case errors.Is(err, os.ErrNotExist):
			// A repo without design records has nothing to collide.
			continue
		case err != nil:
			return fmt.Errorf("scanning %s: %w", dir, err)
		}
		claims = append(claims, found...)
	}

	collisions := adrguard.Collisions(claims)
	for _, collision := range collisions {
		fmt.Fprintln(os.Stderr, collision.Message())
	}
	if len(collisions) > 0 {
		return fmt.Errorf("%d ADR number(s) claimed twice: give the later record the next number that is free on main and update its references", len(collisions))
	}
	return nil
}
