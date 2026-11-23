// Command savevault is the SaveVault command-line interface.
//
// `savevault` is the historical name and is still built and shipped: scripts
// and documentation written before the rename keep working, and both binaries
// run exactly the same code.
package main

import (
	"os"

	"github.com/jun9100/savevault/internal/cliapp"
)

func main() {
	os.Exit(cliapp.Run(os.Args[1:]))
}
