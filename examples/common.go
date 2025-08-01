package examples

import (
	"flag"
	"io"
	"os"
)

// DemoPathFromArgs returns the value of the -demo command line flag.
// Panics if an error occurs.
func DemoPathFromArgs() string {
	fl := new(flag.FlagSet)

	demPathPtr := fl.String("demo", "", "Demo file `path`")

	err := fl.Parse(os.Args[1:])
	if err != nil {
		panic(err)
	}

	demPath := *demPathPtr

	return demPath
}

func checkError(err error) {
	if err != nil {
		panic(err)
	}
}

// RedirectStdout redirects standard output to dev null.
// Panics if an error occurs.
func RedirectStdout(f func()) {
	// Redirect stdout, the resulting image is written to this
	old := os.Stdout

	r, w, err := os.Pipe()
	checkError(err)

	os.Stdout = w

	// Discard the output in a separate goroutine so writing to stdout can't block indefinitely
	go func() {
		for err := error(nil); err == nil; _, err = io.Copy(io.Discard, r) {
		}
	}()

	f()

	os.Stdout = old
}
