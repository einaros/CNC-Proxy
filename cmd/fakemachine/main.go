// Command fakemachine runs the carveratest FakeMachine as a standalone process
// for manual end-to-end testing of the proxy without real hardware.
package main

import (
	"flag"
	"log"
	"os"
	"path/filepath"

	"github.com/uwin/cnc-proxy/internal/carveratest"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:0", "listen address")
	sidecar := flag.String("sidecar", "127.0.0.1:0", "sidecar web UI listen address; empty disables it")
	status := flag.String("status", "<Idle|MPos:0,0,0|WPos:0,0,0|C:2,4,0,1|T:0,0.000>", "status payload returned for ? queries")
	model := flag.String("model", "", "optional STL/GLB/glTF probe collision model to load at startup")
	flag.Parse()

	m, err := carveratest.NewOn(*addr)
	if err != nil {
		log.Fatal(err)
	}
	m.SetStatus(*status)
	if *model != "" {
		data, err := os.ReadFile(*model)
		if err != nil {
			log.Fatal(err)
		}
		if err := m.LoadProbeModel(filepath.Base(*model), data); err != nil {
			log.Fatal(err)
		}
		log.Printf("loaded probe model %s", *model)
	}
	log.Printf("fakemachine listening on %s (status %q)", m.Addr(), *status)
	if *sidecar != "" {
		_, ln, err := startSidecar(*sidecar, m)
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("fakemachine sidecar listening on %s", sidecarURL(ln.Addr().String()))
	}
	select {} // run forever
}
