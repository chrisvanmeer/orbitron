package logo

import "fmt"

// Banner displays the Milky Way unicode logo and Orbitron branding
const Banner = `
🌌 ORBITRON - Internal Ansible Galaxy Mirror Daemon
===================================================`

func PrintBanner() {
	fmt.Println(Banner)
}
