// fakemodel serves the scripted model over HTTP so the demo runs with no API key.
package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync/atomic"

	"github.com/NAMAN-VERMA-02/agentreplay/internal/fake/model"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8787", "listen address")
	flag.Parse()
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakemodel:", err)
		os.Exit(1)
	}
	fmt.Printf("fakemodel listening on http://%s (models: fake/<variant>, variants %v)\n", ln.Addr(), model.Variants)
	var hits atomic.Int64
	if err := http.Serve(ln, model.Handler(&hits)); err != nil {
		fmt.Fprintln(os.Stderr, "fakemodel:", err)
		os.Exit(1)
	}
}
