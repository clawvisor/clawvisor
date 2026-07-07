// Command terraform-provider-clawvisor is the Terraform provider server for
// Clawvisor. Build it with `go build ./...` from the provider/ directory; run
// it under Terraform via a dev override or the registry mirror (v1.1).
package main

import (
	"context"
	"flag"
	"log"

	"github.com/clawvisor/clawvisor/provider/internal/provider"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
)

// version is set at build time via -ldflags; defaults to "dev".
var version = "dev"

func main() {
	var debug bool
	flag.BoolVar(&debug, "debug", false, "set to run the provider with support for debuggers like delve")
	flag.Parse()

	opts := providerserver.ServeOpts{
		Address: "registry.terraform.io/clawvisor/clawvisor",
		Debug:   debug,
	}

	if err := providerserver.Serve(context.Background(), provider.New(version), opts); err != nil {
		log.Fatal(err.Error())
	}
}
