//go:generate go get -u github.com/valyala/quicktemplate/qtc
//go:generate qtc -dir=internal

package main

import (
	"gitlab.com/kabes/widdler-ex/internal"
	_ "modernc.org/sqlite"
)

// -------------------------------------------------------------------

func main() {
	internal.Main()
}

// -------------------------------------------------------------------
