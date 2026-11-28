package netguard

// Values several tests share.
const (
	netTCP         = "tcp"
	netTCP4        = "tcp4"
	netTCP6        = "tcp6"
	pub4           = "8.8.8.8"
	doc6           = "2001:db8::1"
	relay6a44      = "192.88.99.2"
	zonedLinkLocal = "fe80::1%eth0"
	nat64Public    = "64:ff9b::808:808"
	unspec4        = "0.0.0.0"
	loop6HTTPS     = "[::1]:443"
	mappedPublic   = "::ffff:8.8.8.8"
	mapped1234     = "::ffff:1.2.3.4"
	loop6          = "::1"
	pub4HTTPS      = pub4 + ":443"
	pub4DNS        = pub4 + ":53"
)
