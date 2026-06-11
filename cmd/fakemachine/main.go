// Command fakemachine runs the carveratest FakeMachine as a standalone process
// for manual end-to-end testing of the proxy without real hardware.
package main

import (
	"flag"
	"log"

	"github.com/uwin/cnc-proxy/internal/carveratest"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:2222", "listen address")
	status := flag.String("status", "<Idle|MPos:0,0,0|WPos:0,0,0>", "status payload returned for ? queries")
	flag.Parse()

	m, err := carveratest.NewOn(*addr)
	if err != nil {
		log.Fatal(err)
	}
	m.SetStatus(*status)
	log.Printf("fakemachine listening on %s (status %q)", m.Addr(), *status)
	select {} // run forever
}
