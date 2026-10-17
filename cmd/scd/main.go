// Command scd is the single binary for the scd, sfc and gtl services.
package main

import (
	"os"

	"github.com/a1ekseev/scd/internal/cli"
	"github.com/a1ekseev/scd/internal/common/buildinfo"
)

var version = "dev"

func main() {
	buildinfo.Version = version
	os.Exit(cli.Execute(os.Args[1:], os.Stdout, os.Stderr))
}
