// Command stamped prints buildstamp.Describe for the build test.
package main

import (
	"fmt"

	"github.com/stevegeek/lever/internal/buildstamp"
)

func main() { fmt.Println(buildstamp.Describe("1.2.3")) }
