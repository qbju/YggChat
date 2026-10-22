// Group delivery fixes (v0.19.2, found by the chaos test):
//
//   - One post, one message: member copies carry "<post>g<i>" ids, so a
//     receiver dedupes on sender + postID and stores the bare post id; the
//     catch-up relay ("ghs") sends the bare id too.
//   - Leaving: our pending copies get one last push before the gl; what is
//     still undelivered then is marked failed (never retried for a group we
//     are out of), instead of cycling sending/stored forever.
//   - Every node remembers when a member dropped out (GoneAt, local only):
//     a post made before that is still taken (late copy or relay), a later
//     one is acked and dropped so the sender stops retrying.
//   - The host answers a recently removed member that still posts or asks
//     with the current info, so it learns it is out.
package main

import (
	"sort"
	"sync"
	"time"
)

const goneKeep = 14 * 86400 // how long GoneAt remembers a member who dropped out

// samePost reports whether two group message ids are the same post.
func samePost(a, b string) bool { return postID(a) == postID(b) }

// noteGone records when members of before dropped out of g (at: when they
// left by their own clock, from gl; 0 = now) and forgets members who are back
// or long gone. Every node keeps it; it never goes on the wire.
func noteGone(g *Group, before []string, at int64) {
	now := time.Now().Unix()
	if at <= 0 || at > now {
		at = now
	}
	for _, m := range before {
		if hasMember(*g, m) {
			continue
		}
		if g.GoneAt == nil {
			g.GoneAt = map[string]int64{}
		}
		if t, ok := g.GoneAt[m]; !ok || at < t {
			g.GoneAt[m] = at
		}
	}
	for m, t := range g.GoneAt {
		if hasMember(*g, m) || now-t > goneKeep {
			delete(g.GoneAt, m)
		}
	}
	if len(g.GoneAt) > maxGone {
		type e struct {
			m string
			t int64
		}
		var es []e
		for m, t := range g.GoneAt {
			es = append(es, e{m, t})
		}
		sort.Slice(es, func(i, j int) bool { return es[i].t > es[j].t })
		g.GoneAt = map[string]int64{}
		for _, x := range es[:maxGone] {
			g.GoneAt[x.m] = x.t
		}
	}
	if len(g.GoneAt) == 0 {
		g.GoneAt = nil
	}
	noteAdded(g, before, 0) // and when the new ones came (history without sharing, v0.19.3)
	if len(g.Ban) > 0 {
		var keep []string // someone added back (by any manager) is no longer barred
		for _, m := range g.Ban {
			if !hasMember(*g, m) {
				keep = append(keep, m)
			}
		}
		g.Ban = keep
	}
}

// goneAt returns when addr dropped out of g (0 = not known as a former member).
func goneAt(g Group, addr string) int64 {
	if t, ok := g.GoneAt[addr]; ok {
		return t
	}
	if g.Owner != "" && hasMember(Group{Members: g.Gone}, addr) {
		return -1 // the host knows they are gone but not when (pre-v0.19.3 store)
	}
	return 0
}

var goneNoticeMu sync.Mutex
var goneNoticeAt = map[string]time.Time{}

// goneNotice tells a recently removed member that still posts or asks about
// the group the current state (the host's gi without them), at most once a
// 15 s per member and group. Only the host's list is taken by members.
func (a *App) goneNotice(peer string, g Group) {
	if g.Owner != a.addr || g.Left || hasMember(g, peer) || goneAt(g, peer) == 0 {
		return
	}
	goneNoticeMu.Lock()
	k := g.ID + "|" + peer
	if time.Since(goneNoticeAt[k]) < 15*time.Second {
		goneNoticeMu.Unlock()
		return
	}
	if len(goneNoticeAt) > 4096 {
		goneNoticeAt = map[string]time.Time{}
	}
	goneNoticeAt[k] = time.Now()
	goneNoticeMu.Unlock()
	for i, p := range a.infoPackets(g) {
		if i == 0 {
			// say it is them who are out ("gr"): a list at the version they
			// hold from an admin is then not taken as a collision (v0.19.3)
			x := p
			x.GR = packAddrs([]string{peer})
			if wireLen(x) <= dgramMax {
				p = x
			}
		}
		a.sendWire(peer, p)
	}
}

// groupOut reports whether we are out of group gid (left or removed). Caller holds a.store.mu.
func (a *App) groupOutLocked(gid string) bool {
	g, ok := a.store.Groups[gid]
	return ok && g.Left
}

// leavingState marks groups whose pending copies get their last push while leaving.
var leaving = struct {
	sync.Mutex
	g map[string]bool
}{g: map[string]bool{}}

func isLeaving(gid string) bool {
	leaving.Lock()
	defer leaving.Unlock()
	return leaving.g[gid]
}

// pendingCopiesLocked returns our undelivered copies of posts to group gid. Caller holds a.store.mu.
func (a *App) pendingCopiesLocked(gid string) []Message {
	var out []Message
	for _, m := range a.store.Messages {
		if m.Hid && m.Out && m.Grp == gid && m.Status != "sent" && m.Status != "failed" {
			out = append(out, m)
		}
	}
	return out
}

// flushBeforeLeave gives our pending copies of gid one more try (bounded by
// wait), so posts made before leaving still reach the members.
func (a *App) flushBeforeLeave(gid string, cs []Message, wait time.Duration) {
	if len(cs) == 0 {
		return
	}
	leaving.Lock()
	leaving.g[gid] = true
	leaving.Unlock()
	defer func() { leaving.Lock(); delete(leaving.g, gid); leaving.Unlock() }()
	done := make(chan struct{}, len(cs))
	for _, c := range cs {
		c := c
		go func() {
			// wait for a retry already running for this copy
			for i := 0; i < 20; i++ {
				a.flMu.Lock()
				busy := a.inflight[c.ID]
				a.flMu.Unlock()
				if !busy {
					break
				}
				time.Sleep(250 * time.Millisecond)
			}
			if a.copyPending(c.Peer, c.ID) {
				a.deliverN(c, 2)
			}
			done <- struct{}{}
		}()
	}
	t := time.After(wait)
	for range cs {
		select {
		case <-done:
		case <-t:
			return
		}
	}
}

func (a *App) copyPending(peer, id string) bool {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	for i := len(a.store.Messages) - 1; i >= 0; i-- {
		if m := a.store.Messages[i]; m.Hid && m.Peer == peer && m.ID == id {
			return m.Status != "sent" && m.Status != "failed"
		}
	}
	return false
}

// failLeftCopies marks our still-undelivered copies of posts to a group we
// are out of as failed (they are never retried) and updates their posts.
// Copies already parked on Nostr keep "stored": that delivery is out of our hands.
func (a *App) failLeftCopies(gid string) {
	a.store.mu.Lock()
	ofs := map[string]bool{}
	for i := range a.store.Messages {
		m := &a.store.Messages[i]
		if m.Hid && m.Out && m.Grp == gid && m.Status != "sent" && m.Status != "failed" && !m.Nostr {
			// (a copy parked on Nostr stays "stored": the member may still fetch it)
			m.Status = "failed"
			ofs[m.Of] = true
		}
	}
	if len(ofs) > 0 {
		a.store.save()
	}
	a.store.mu.Unlock()
	for of := range ofs {
		a.groupStatus(gid, of)
	}
}

// dedupeGroupMsgs drops received group messages that are a second copy of
// the same post (same group, sender and postID), keeping the first (older
// versions stored per-member "<id>g<i>" copy ids, so relays doubled them). Caller holds s.mu.
func (s *Store) dedupeGroupMsgs() {
	seen := map[string]bool{}
	kept := s.Messages[:0]
	for _, m := range s.Messages {
		if isGroupKey(m.Peer) && !m.Out && m.Sys == "" && m.From != "" {
			k := m.Peer + "|" + m.From + "|" + postID(m.ID)
			if seen[k] {
				continue
			}
			seen[k] = true
		}
		kept = append(kept, m)
	}
	s.Messages = kept
}

// requeueAfterRestart: messages left "sending"/"failed" by a previous run
// never got an ack and go back to the queue, except copies (and posts) of
// groups we are out of, which stay failed. Caller holds s.mu.
func (s *Store) requeueAfterRestart() {
	for i := range s.Messages {
		m := &s.Messages[i]
		if m.Status != "sending" && m.Status != "failed" {
			continue
		}
		gid := m.Grp
		if gid == "" && isGroupKey(m.Peer) {
			gid = m.Peer[2:]
		}
		if gid != "" {
			if g, ok := s.Groups[gid]; ok && g.Left {
				if m.Status == "sending" {
					m.Status = "failed"
				}
				continue
			}
		}
		m.Status = "queued"
	}
}
