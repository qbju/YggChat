// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

package main

import "time"

// Undelivered hidden group copies used to be kept (and retried) forever. A
// member who stopped using YggChat without leaving made every later post add
// one more queued copy for it to chat.json: in a big group with a few such
// members the file (and every scan of the history) grew without end (r2).
//
// A member coming back asks the others for the recent posts (ghq catch-up),
// so very old or very many copies for one member are not worth keeping:
//   - a copy of a post that reached someone is dropped after copyKeep, or
//     when the member has copyMaxPer newer ones of that group waiting;
//   - a copy that failed for good (we left the group) goes after copyKeep.
//
// Copies of posts nobody has got yet are kept (bounded by our own posting).
const (
	copyKeep   = 14 * 86400
	copyMaxPer = 100
)

// pruneStaleCopiesLocked drops such copies; reports how many. Caller holds s.mu.
func (s *Store) pruneStaleCopiesLocked(now int64) int {
	reached := map[string]bool{} // gid/post id -> delivered to someone
	for _, m := range s.Messages {
		if m.Out && !m.Hid && isGroupKey(m.Peer) && (m.Status == "sent" || m.Dlv > 0) {
			reached[m.Peer+"/"+m.ID] = true
		}
	}
	per := map[string]int{} // member|gid -> copies kept (newest first)
	drop := make([]bool, len(s.Messages))
	n := 0
	for i := len(s.Messages) - 1; i >= 0; i-- {
		m := s.Messages[i]
		if !m.Hid || !m.Out || m.Status == "sent" {
			continue
		}
		old := m.TS > 0 && now-m.TS > copyKeep
		if m.Status == "failed" {
			if old {
				drop[i], n = true, n+1
			}
			continue
		}
		if !reached[gKey(m.Grp)+"/"+m.Of] {
			continue
		}
		k := m.Peer + "|" + m.Grp
		if old || per[k] >= copyMaxPer {
			drop[i], n = true, n+1
			continue
		}
		per[k]++
	}
	if n == 0 {
		return 0
	}
	kept := s.Messages[:0]
	for i, m := range s.Messages {
		if !drop[i] {
			kept = append(kept, m)
		}
	}
	for i := len(kept); i < len(s.Messages); i++ {
		s.Messages[i] = Message{} // let the dropped ones be collected
	}
	s.Messages = kept
	return n
}

// pruneCopies runs pruneStaleCopiesLocked and saves when it dropped any.
func (a *App) pruneCopies() {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	if a.store.locked {
		return
	}
	if a.store.pruneStaleCopiesLocked(time.Now().Unix()) > 0 {
		a.store.save()
	}
}
