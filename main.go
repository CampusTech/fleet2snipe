package main

import "github.com/CampusTech/fleet2snipe/cmd"

var version = "dev"

func main() {
	cmd.Version = version
	cmd.Execute()
}
