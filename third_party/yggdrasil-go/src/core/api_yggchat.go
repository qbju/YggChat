// Added for YggChat (2026): a peer count that doesn't go through ironwood's
// Debug.GetPeers. That one reads each peer's ping times from outside the
// peer's actor (a data race seen with -race), and YggChat counts peers on
// every /api/state. Licensed under the LGPLv3 with the Yggdrasil special
// exception, like the rest of this library.

package core

import "github.com/Arceliar/phony"

// UpPeerCount returns how many peer links are connected now (the same as
// counting PeerInfo.Up in GetPeers).
func (c *Core) UpPeerCount() (n int) {
	phony.Block(&c.links, func() {
		for _, state := range c.links._links {
			if state._conn != nil {
				n++
			}
		}
	})
	return
}
