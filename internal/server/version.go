package server

import "runtime"

// Version is the build version stamped into responses. main sets it at start
// up from the linker flags.
var Version = "dev"

func goos() string   { return runtime.GOOS }
func goarch() string { return runtime.GOARCH }
