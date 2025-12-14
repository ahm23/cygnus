package main

import (
	"cygnus/cmd"
)

func main() {
	root := cmd.RootCmd()

	cmd.Execute(root)
}
