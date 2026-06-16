// po2mo - convert .po files to .mo using gotext (no system gettext needed).
//
// Usage: go run ./script/pomo <po-file> <mo-file>
package main

import (
	"fmt"
	"os"

	"github.com/leonelquinteros/gotext"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: po2mo <po-file> <mo-file>")
		os.Exit(2)
	}
	poPath, moPath := os.Args[1], os.Args[2]

	po := gotext.NewPo()
	po.ParseFile(poPath)

	data, err := po.MarshalBinary()
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshal: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(moPath, data, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "write: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s (%d bytes)\n", moPath, len(data))
}
