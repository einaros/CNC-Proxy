// Command winpack appends a zip payload to a Windows installer stub.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/uwin/cnc-proxy/internal/installerpayload"
)

func main() {
	stub := flag.String("stub", "", "Windows installer stub executable")
	payload := flag.String("payload", "", "zip payload to append")
	out := flag.String("out", "", "output installer executable")
	flag.Parse()
	if *stub == "" || *payload == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "usage: winpack -stub installer-stub.exe -payload payload.zip -out cnc-proxy-installer.exe")
		os.Exit(2)
	}
	if err := installerpayload.Append(*stub, *payload, *out); err != nil {
		fmt.Fprintln(os.Stderr, "winpack:", err)
		os.Exit(1)
	}
}
