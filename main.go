package main

import (
	"os"

	"github.com/ConradIrwin/conl-lsp/protocol"
	"github.com/ConradIrwin/dbg"
)

func main() {
	// 	input := &bytes.Buffer{}
	// 	requests := make(chan []byte)
	// 	wg := sync.WaitGroup{}
	// 	wg.Add(1)

	// 	go func() {
	// 		protocol.WriteFrames(input, requests)
	// 		defer wg.Done()
	// 	}()
	// 	requests <- []byte(`{"jsonrpc":"2.0","method":"workspace/configuration","params":{"items":[{"section":"gopls"}]},"id":21}
	// `)
	// 	close(requests)
	// 	wg.Wait()

	err := protocol.Serve(os.Stdin, os.Stdout)
	dbg.Dbg(err)

}
