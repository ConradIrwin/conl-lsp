package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"runtime/debug"

	"github.com/ConradIrwin/conl-lsp/lsp"
)

var log *os.File

func logPanic() {
	if r := recover(); r != nil {
		log.WriteString(fmt.Sprintf("%#v", r))
		log.WriteString(string(debug.Stack()))
	}
}

func main() {
	logFile := flag.String("log", "", "a file to log to")
	verbose := flag.Bool("verbose", false, "whether to log raw messages")
	flag.Parse()

	if logFile != nil && *logFile != "" {
		var err error
		log, err = os.Create(*logFile)
		if err != nil {
			panic(err)
		}
		defer log.Close()
		if *verbose {
			lsp.FrameLogger = func(prefix string, data []byte) {
				log.WriteString(prefix + ": " + string(data) + "\n")
			}
		}
		defer func() {
			if r := recover(); r != nil {
				log.WriteString(fmt.Sprintf("%#v", r))
				log.WriteString(string(debug.Stack()))
				panic(r)
			}
		}()
	}

	c := lsp.NewConnection()
	err := NewServer(c).Serve(context.Background(), os.Stdin, os.Stdout)
	if err != nil {
		panic(err)
	}
}
