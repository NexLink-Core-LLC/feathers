package main

import (
	"math/rand"
	"time"

	// Embed the IANA timezone database into the binary. Windows has no
	// /usr/share/zoneinfo, so without this time.LoadLocation fails for any
	// non-UTC zone — which makes ConfigureTimezone return an error and the
	// daemon log.Fatal at boot. Embedding it makes timezone handling
	// self-contained and identical across OSes.
	_ "time/tzdata"

	"github.com/pterodactyl/wings/cmd"
)

func main() {
	// Since we make use of the math/rand package in the code, especially for generating
	// non-cryptographically secure random strings we need to seed the RNG. Just make use
	// of the current time for this.
	rand.Seed(time.Now().UnixNano())

	// Execute the main binary code.
	cmd.Execute()
}
