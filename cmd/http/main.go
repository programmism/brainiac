// Command brainiac-http serves the REST API and WebUI — a thin adapter over
// internal/core.
//
// The router (net/http + chi) and endpoints are built in issue #19; this
// scaffold exists so the binary and its wiring are in place.
package main

import (
	"fmt"

	"github.com/programmism/brainiac/internal/core"
)

func main() {
	c := core.New()
	_ = c
	fmt.Printf("brainiac-http %s — REST API server (endpoints land in #19)\n", core.Version)
}
