// Command kedr recommends Kubernetes container resource requests and limits.
package main

import (
	"os"

	"github.com/kedify/kedr/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
