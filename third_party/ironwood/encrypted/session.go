// Modified for YggChat (2026): session (re)keying after a node restarts with
// the same key, or under high latency, no longer livelocks. See
// "YggChat changes" below and THIRD_PARTY_NOTICES.txt. Licensed under the
// MPL-2.0 like the rest of this library.

package encrypted

import (
	"encoding/binary"
	"sync"
	"time"

	"github.com/Arceliar/phony"

	"github.com/Arceliar/ironwood/types"
)

/*

TODO:
  We either need to save the private keys for sent inits (so we can make a session when we receive an ack...) *or* we need to send an ack back when we receive an ack and we create a session because of it

*/

const (
	sessionTimeout            = time.Minute
	sessionTrafficOverheadMin = 1 + 1 + 1 + 1 + boxOverhead + boxPubSize // header, seq, seq, nonce
	sessionTrafficOverhead    = sessionTrafficOverheadMin + 9 + 9 + 9
	sessionInitSize           = 1 + boxPubSize + boxOverhead + edSigSize + boxPubSize + boxPubSize + 8 + 8
	sessionAckSize            = sessionInitSize
)

const (
	sessionTypeDummy = iota
	sessionTypeInit
	sessionTypeAck
	sessionTypeTraffic
)

/******************
 * sessionManager *
 ******************/

type sessionManager struct {
	phony.Inbox
	pc       *PacketConn
	sessions map[edPub]*sessionInfo
	buffers  map[edPub]*sessionBuffer
	strayAt  map[edPub]time.Time // last throwaway init per key (mgr actor only)
	seqMu    sync.Mutex
	lastSeq  map[edPub]uint64 // last init/ack seq sent per key (seqMu)
}

/*
YggChat changes (2026):

 1. The seq of an init/ack is the time in whole seconds, and a receiver drops
    one whose seq is not newer than the last it accepted. After a restart
    with the same key, the throwaway init (answering the peer's old-session
    traffic), the buffered init and the ack all went out in the same second,
    so the peer kept the throwaway keys and dropped the real ones: every
    datagram both ways was lost until a later second's init got through.
    Inits/acks we send now carry a seq that is strictly increasing per
    destination (the time, or the last one + 1 when that is not newer). A
    re-sent buffered init keeps its seq. Old peers compare seqs the same way.
 2. Every accepted init/ack rotates our keys, and only traffic sent to our
    current or previous key could be decrypted. Under latency above about a
    second (or with crossing inits), traffic in flight was always two or more
    rotations behind, so it failed, each failure sent another init, the peer
    accepted it and rotated again: a livelock that lasted as long as traffic
    kept flowing (minutes on a loaded host). We now keep the last few keys on
    both sides and decrypt traffic sent to any of them (with its own replay
    window) instead of asking for a new handshake.
 3. A failed decrypt sends at most one recovery init per second per session,
    and traffic from a key without a session one init per second.
 4. Traffic from a key without a session (a peer that kept its session while
    we restarted) is answered with an init whose keys we keep (see
    _handleTraffic), not with throwaway keys.
 5. An init that repeats the keys we already hold for the peer is acked
    without rotating our keys (see handleInit).
*/

// nextSeq returns the seq for an init/ack to dest (YggChat change 1).
func (mgr *sessionManager) nextSeq(dest *edPub) uint64 {
	now := uint64(time.Now().Unix())
	mgr.seqMu.Lock()
	defer mgr.seqMu.Unlock()
	if mgr.lastSeq == nil {
		mgr.lastSeq = make(map[edPub]uint64)
	}
	seq := now
	if last := mgr.lastSeq[*dest]; last >= seq {
		seq = last + 1
	}
	mgr.lastSeq[*dest] = seq
	if len(mgr.lastSeq) > 4096 {
		for k, v := range mgr.lastSeq {
			if v+2 < now { // only entries ahead of the clock matter
				delete(mgr.lastSeq, k)
			}
		}
	}
	return seq
}

func (mgr *sessionManager) init(pc *PacketConn) {
	mgr.pc = pc
	mgr.sessions = make(map[edPub]*sessionInfo)
	mgr.buffers = make(map[edPub]*sessionBuffer)
}

func (mgr *sessionManager) _newSession(ed *edPub, recv, send boxPub, seq uint64) *sessionInfo {
	info := newSession(ed, recv, send, seq)
	info.Act(mgr, func() {
		info.mgr = mgr
		info._resetTimer()
	})
	mgr.sessions[info.ed] = info
	return info
}

func (mgr *sessionManager) _sessionForInit(pub *edPub, init *sessionInit) (*sessionInfo, *sessionBuffer) {
	var info *sessionInfo
	var buf *sessionBuffer
	if info = mgr.sessions[*pub]; info == nil {
		info = mgr._newSession(pub, init.current, init.next, init.seq)
		if buf = mgr.buffers[*pub]; buf != nil {
			buf.timer.Stop()
			delete(mgr.buffers, *pub)
			info.sendPub, info.sendPriv = buf.init.current, buf.currentPriv
			info.nextPub, info.nextPriv = buf.init.next, buf.nextPriv
			info._fixShared(0, 0)
			// The caller is responsible for sending buf.data when ready
		}
	}
	return info, buf
}

func (mgr *sessionManager) handleData(from phony.Actor, pub *edPub, data []byte) {
	mgr.Act(from, func() {
		if len(data) == 0 {
			return
		}
		switch data[0] {
		case sessionTypeDummy:
		case sessionTypeInit:
			init := new(sessionInit)
			if init.decrypt(&mgr.pc.secretBox, pub, data, mgr.pc.groupAuth) {
				mgr._handleInit(pub, init)
			}
			freeBytes(data)
		case sessionTypeAck:
			ack := new(sessionAck)
			if ack.decrypt(&mgr.pc.secretBox, pub, data, mgr.pc.groupAuth) {
				mgr._handleAck(pub, ack)
			}
			freeBytes(data)
		case sessionTypeTraffic:
			mgr._handleTraffic(pub, data)
		default:
		}
	})
}

func (mgr *sessionManager) _handleInit(pub *edPub, init *sessionInit) {
	if info, buf := mgr._sessionForInit(pub, init); info != nil {
		info.handleInit(mgr, init)
		if buf != nil && buf.data != nil {
			info.doSend(mgr, buf.data)
		}
	}
}

func (mgr *sessionManager) _handleAck(pub *edPub, ack *sessionAck) {
	_, isOld := mgr.sessions[*pub]
	if info, buf := mgr._sessionForInit(pub, &ack.sessionInit); info != nil {
		if isOld {
			info.handleAck(mgr, ack)
		} else {
			info.handleInit(mgr, &ack.sessionInit)
		}
		if buf != nil && buf.data != nil {
			info.doSend(mgr, buf.data)
		}
	}
}

func (mgr *sessionManager) _handleTraffic(pub *edPub, msg []byte) {
	if info := mgr.sessions[*pub]; info != nil {
		info.doRecv(mgr, msg)
	} else {
		// We don't know that the node really exists, it could be spoofed/replay
		// So we don't want to save session or a buffer based on this node
		// So we send an init with keys we'll forget
		// If they ack, we'll set up a session and let it self-heal...
		//
		// YggChat change 4: a peer that kept its session while we restarted
		// accepts every such init (one per second) and switches to keys
		// nobody has, so everything it sent was lost until a later init
		// won. We keep the keys in a data-less buffer instead (bounded, it
		// expires like any buffer) and send the same init again, so the
		// peer's ack sets up a session with keys we hold; a repeat has the
		// same seq and doesn't make the peer rotate again. (change 3: at
		// most one per second per key)
		if mgr.strayAt == nil {
			mgr.strayAt = make(map[edPub]time.Time)
		}
		if at, ok := mgr.strayAt[*pub]; ok && time.Since(at) < time.Second {
			return
		}
		if len(mgr.strayAt) > 1024 {
			for k, at := range mgr.strayAt {
				if time.Since(at) > time.Second {
					delete(mgr.strayAt, k)
				}
			}
		}
		mgr.strayAt[*pub] = time.Now()
		if buf := mgr.buffers[*pub]; buf != nil {
			// They have a session, so they already took (or dropped) this
			// init and its ack didn't reach us: same keys, newer seq, so they
			// take it again and ack. Not more often than every few seconds:
			// each one they take makes them rotate their keys.
			if time.Since(buf.seqAt) >= sessionStrayRefresh {
				buf.init.seq = 0
				buf.seqAt = time.Now()
			}
			mgr.sendInit(pub, &buf.init)
			return
		}
		if len(mgr.buffers) < sessionStrayBuffers {
			mgr._newBuffer(*pub).seqAt = time.Now()
			mgr.sendInit(pub, &mgr.buffers[*pub].init)
			return
		}
		currentPub, _ := newBoxKeys()
		nextPub, _ := newBoxKeys()
		init := newSessionInit(&currentPub, &nextPub, 0)
		mgr.sendInit(pub, &init)
	}
}

func (mgr *sessionManager) writeTo(toKey edPub, msg []byte) {
	// WARNING: unsafe to call from within an actor, must only be exposed over the PacketConn functions (which are, themselves, unsafe for actors to call in most cases, since they may block)
	phony.Block(mgr, func() {
		if info := mgr.sessions[toKey]; info != nil {
			info.doSend(mgr, msg)
		} else {
			// Need to buffer the traffic
			mgr._bufferAndInit(toKey, msg)
		}
	})
}

func (mgr *sessionManager) _bufferAndInit(toKey edPub, msg []byte) {
	var buf *sessionBuffer
	if buf = mgr.buffers[toKey]; buf == nil {
		buf = mgr._newBuffer(toKey)
	}
	buf.data = msg
	mgr.sendInit(&toKey, &buf.init)
	mgr._bufferTimer(toKey, buf)
}

// sessionStrayBuffers bounds the data-less buffers made for traffic from
// keys without a session (YggChat change 4).
const sessionStrayBuffers = 256

// sessionStrayRefresh is how often such a buffer's init gets a newer seq.
const sessionStrayRefresh = 3 * time.Second

// _newBuffer creates the buffer (keys, init, expiry timer) for toKey.
func (mgr *sessionManager) _newBuffer(toKey edPub) *sessionBuffer {
	buf := new(sessionBuffer)
	currentPub, currentPriv := newBoxKeys()
	nextPub, nextPriv := newBoxKeys()
	buf.init = newSessionInit(&currentPub, &nextPub, 0)
	buf.currentPriv = currentPriv
	buf.nextPriv = nextPriv
	mgr.buffers[toKey] = buf
	mgr._bufferTimer(toKey, buf)
	return buf
}

func (mgr *sessionManager) _bufferTimer(toKey edPub, buf *sessionBuffer) {
	if buf.timer != nil {
		buf.timer.Stop()
	}
	buf.timer = time.AfterFunc(sessionTimeout, func() {
		mgr.Act(nil, func() {
			if b := mgr.buffers[toKey]; b == buf {
				b.timer.Stop()
				delete(mgr.buffers, toKey)
			}
		})
	})
}

func (mgr *sessionManager) sendInit(dest *edPub, init *sessionInit) {
	if init.seq == 0 {
		init.seq = mgr.nextSeq(dest) // a re-sent buffered init keeps its seq
	}
	if bs, err := init.encrypt(&mgr.pc.secretEd, dest, mgr.pc.groupAuth); err == nil {
		mgr.pc.PacketConn.WriteTo(bs, types.Addr(dest.asKey()))
	}
}

func (mgr *sessionManager) sendAck(dest *edPub, ack *sessionAck) {
	if ack.seq == 0 {
		ack.seq = mgr.nextSeq(dest)
	}
	if bs, err := ack.encrypt(&mgr.pc.secretEd, dest, mgr.pc.groupAuth); err == nil {
		mgr.pc.PacketConn.WriteTo(bs, types.Addr(dest.asKey()))
	}
}

/***************
 * sessionInfo *
 ***************/

type sessionInfo struct {
	phony.Inbox
	mgr            *sessionManager
	seq            uint64 // remote seq
	ed             edPub  // remote ed key
	remoteKeySeq   uint64 // signals rotation of current/next
	current        boxPub // send to this, expect to receive from it
	next           boxPub // if we receive from this, then rotate it to current
	localKeySeq    uint64 // signals rotation of recv/send/next
	recvPriv       boxPriv
	recvPub        boxPub
	recvShared     boxShared
	recvNonce      uint64
	sendPriv       boxPriv // becomes recvPriv when we ratchet forward
	sendPub        boxPub  // becomes recvPub
	sendShared     boxShared
	sendNonce      uint64
	nextPriv       boxPriv // becomes sendPriv
	nextPub        boxPub  // becomes sendPub
	timer          *time.Timer
	ack            *sessionAck
	since          time.Time
	rotated        time.Time // last time we rotated keys
	rx             uint64
	tx             uint64
	nextSendShared boxShared
	nextSendNonce  uint64
	nextRecvShared boxShared
	nextRecvNonce  uint64
	// YggChat change 2: recent keys by key seq, so traffic sent to a key a
	// few rotations old still decrypts. oldNonce is the replay window per
	// (remote seq, local seq) pair for those.
	oldLocal  []keyByPriv
	oldRemote []keyByPub
	oldNonce  map[boxShared]oldNonce
	initAt    time.Time // last recovery init (change 3)
}

const sessionKeyHistory = 8

type oldNonce struct {
	nonce, remoteSeq, localSeq uint64
}

type keyByPriv struct {
	seq  uint64
	priv boxPriv
}

type keyByPub struct {
	seq uint64
	pub boxPub
}

// _remember records the keys in use now (called whenever they change).
func (info *sessionInfo) _remember() {
	if info.localKeySeq > 0 {
		info._putPriv(info.localKeySeq-1, info.recvPriv)
	}
	info._putPriv(info.localKeySeq, info.sendPriv)
	info._putPub(info.remoteKeySeq, info.current)
	info._putPub(info.remoteKeySeq+1, info.next)
}

func (info *sessionInfo) _putPriv(seq uint64, priv boxPriv) {
	for i := range info.oldLocal {
		if info.oldLocal[i].seq == seq {
			info.oldLocal[i].priv = priv
			return
		}
	}
	info.oldLocal = append(info.oldLocal, keyByPriv{seq, priv})
	if len(info.oldLocal) > sessionKeyHistory {
		info.oldLocal = append(info.oldLocal[:0], info.oldLocal[1:]...)
	}
}

// _putPub records the peer's key for one of its key seqs. Besides the
// keys we use, we learn them from the next key inside every packet and from
// inits/acks we don't act on (stale seq), so traffic from a key the peer
// moved to before we caught up still decrypts.
// _addPub is _putPub that doesn't replace a key we have (for keys from a
// stale init/ack).
func (info *sessionInfo) _addPub(seq uint64, pub boxPub) {
	for i := range info.oldRemote {
		if info.oldRemote[i].seq == seq {
			return
		}
	}
	info._putPub(seq, pub)
}

func (info *sessionInfo) _putPub(seq uint64, pub boxPub) {
	for i := range info.oldRemote {
		if info.oldRemote[i].seq == seq {
			info.oldRemote[i].pub = pub
			return
		}
	}
	info.oldRemote = append(info.oldRemote, keyByPub{seq, pub})
	if len(info.oldRemote) > sessionKeyHistory {
		// drop the lowest seq
		low := 0
		for i := range info.oldRemote {
			if info.oldRemote[i].seq < info.oldRemote[low].seq {
				low = i
			}
		}
		info.oldRemote = append(info.oldRemote[:low], info.oldRemote[low+1:]...)
	}
}

// _recvOld tries to decrypt traffic sent to (or from) a key we rotated away
// from. It returns true when the packet was handled (delivered, or dropped
// as a replay), false when it can't be decrypted with any key we kept.
func (info *sessionInfo) _recvOld(remoteKeySeq, localKeySeq, nonce uint64, msg []byte) bool {
	var priv *boxPriv
	var pub *boxPub
	for i := range info.oldLocal {
		if info.oldLocal[i].seq == localKeySeq {
			priv = &info.oldLocal[i].priv
		}
	}
	for i := range info.oldRemote {
		if info.oldRemote[i].seq == remoteKeySeq {
			pub = &info.oldRemote[i].pub
		}
	}
	if priv == nil || pub == nil {
		return false
	}
	var shared boxShared
	getShared(&shared, pub, priv)
	unboxed, ok := boxOpen(allocBytes(0), msg, nonce, &shared)
	defer freeBytes(unboxed)
	if !ok || len(unboxed) < boxPubSize {
		return false
	}
	// The replay window is per shared key, so keys reused under the same
	// seqs (a peer that restarted) start a fresh window.
	if last, seen := info.oldNonce[shared]; seen && nonce <= last.nonce {
		return true // replay or reordered duplicate: drop quietly
	}
	info._noteNonce(&shared, remoteKeySeq, localKeySeq, nonce)
	var next boxPub
	copy(next[:], unboxed)
	info._putPub(remoteKeySeq+1, next)
	out := append(allocBytes(0), unboxed[boxPubSize:]...)
	info.mgr.pc.network.recv(info, out)
	info.rx += uint64(len(out))
	info._resetTimer()
	return true
}

// _noteNonce records the highest nonce received under a shared key.
func (info *sessionInfo) _noteNonce(shared *boxShared, remoteKeySeq, localKeySeq, nonce uint64) {
	if info.oldNonce == nil {
		info.oldNonce = make(map[boxShared]oldNonce)
	}
	if last, seen := info.oldNonce[*shared]; seen && last.nonce >= nonce {
		return
	}
	info.oldNonce[*shared] = oldNonce{nonce, remoteKeySeq, localKeySeq}
	if len(info.oldNonce) > 4*sessionKeyHistory {
		for k, v := range info.oldNonce {
			if v.remoteSeq+sessionKeyHistory < info.remoteKeySeq || v.localSeq+sessionKeyHistory < info.localKeySeq {
				delete(info.oldNonce, k)
			}
		}
	}
}

func newSession(ed *edPub, current, next boxPub, seq uint64) *sessionInfo {
	info := new(sessionInfo)
	info.seq = seq - 1 // so the first update works
	info.ed = *ed
	info.current, info.next = current, next
	info.recvPub, info.recvPriv = newBoxKeys()
	info.sendPub, info.sendPriv = newBoxKeys()
	info.nextPub, info.nextPriv = newBoxKeys()
	info.since = time.Now()
	info._fixShared(0, 0)
	return info
}

// happens at session creation or after receiving an init/ack
func (info *sessionInfo) _fixShared(recvNonce, sendNonce uint64) {
	getShared(&info.recvShared, &info.current, &info.recvPriv)
	getShared(&info.sendShared, &info.current, &info.sendPriv)
	getShared(&info.nextSendShared, &info.next, &info.sendPriv)
	getShared(&info.nextRecvShared, &info.next, &info.recvPriv)
	info.nextSendNonce, info.nextRecvNonce = 0, 0
	info.recvNonce, info.sendNonce = recvNonce, sendNonce
	info._remember()
}

func (info *sessionInfo) _resetTimer() {
	if info.timer != nil {
		info.timer.Stop()
	}
	info.timer = time.AfterFunc(sessionTimeout, func() {
		info.mgr.Act(nil, func() {
			if oldInfo := info.mgr.sessions[info.ed]; oldInfo == info {
				delete(info.mgr.sessions, info.ed)
			}
		})
	})
}

func (info *sessionInfo) handleInit(from phony.Actor, init *sessionInit) {
	info.Act(from, func() {
		if init.seq <= info.seq {
			info._addPub(init.keySeq, init.current) // still the peer's keys
			info._addPub(init.keySeq+1, init.next)
			return
		}
		if info.localKeySeq > 0 && init.keySeq == info.remoteKeySeq && init.current == info.current && init.next == info.next {
			// YggChat change 5: the peer asks again with the keys we already
			// have (it couldn't decrypt something of ours). Not for a session
			// that was just made from this init (localKeySeq 0): that one
			// needs the normal update. Our ack tells it
			// our keys; rotating ours as well would only put the traffic in
			// flight further behind and make it ask again.
			info.seq = init.seq
			info._resetTimer()
			info._sendAck()
			return
		}
		info._handleUpdate(init)
		// Send a sessionAck
		info._sendAck()
	})
}

func (info *sessionInfo) handleAck(from phony.Actor, ack *sessionAck) {
	info.Act(from, func() {
		if ack.seq <= info.seq {
			info._addPub(ack.keySeq, ack.current)
			info._addPub(ack.keySeq+1, ack.next)
			return
		}
		info._handleUpdate(&ack.sessionInit)
	})
}

// return true if everything looks OK and the session was updated
func (info *sessionInfo) _handleUpdate(init *sessionInit) {
	if init.keySeq < info.remoteKeySeq {
		info.oldRemote = nil // the peer restarted: its old keys are gone
	}
	info.current = init.current
	info.next = init.next
	info.seq = init.seq
	info.remoteKeySeq = init.keySeq
	// Advance our keys, since this counts as a response
	info.recvPub, info.recvPriv = info.sendPub, info.sendPriv
	info.sendPub, info.sendPriv = info.nextPub, info.nextPriv
	info.nextPub, info.nextPriv = newBoxKeys()
	info.localKeySeq++
	// Don't roll back sendNonce, just to be extra safe
	info._fixShared(0, info.sendNonce)
	info._resetTimer()
}

func (info *sessionInfo) doSend(from phony.Actor, msg []byte) {
	// TODO? some worker pool to multi-thread this
	info.Act(from, func() {
		defer freeBytes(msg)
		info.sendNonce += 1 // Advance the nonce before anything else
		if info.sendNonce == 0 {
			// Nonce overflowed, so rotate keys
			info.recvPub, info.recvPriv = info.sendPub, info.sendPriv
			info.sendPub, info.sendPriv = info.nextPub, info.nextPriv
			info.nextPub, info.nextPriv = newBoxKeys()
			info.localKeySeq++
			info._fixShared(0, 0)
		}
		bs := allocBytes(sessionTrafficOverhead + len(msg))
		defer freeBytes(bs)
		bs[0] = sessionTypeTraffic
		offset := 1
		offset += binary.PutUvarint(bs[offset:], info.localKeySeq)
		offset += binary.PutUvarint(bs[offset:], info.remoteKeySeq)
		offset += binary.PutUvarint(bs[offset:], info.sendNonce)
		bs = bs[:offset]
		// We need to include info.nextPub below the layer of encryption
		// That way the remote side knows it's us when we send from it later...
		tmp := allocBytes(len(info.nextPub) + len(msg))[:0]
		tmp = append(tmp, info.nextPub[:]...)
		tmp = append(tmp, msg...)
		bs = boxSeal(bs, tmp, info.sendNonce, &info.sendShared)
		freeBytes(tmp)
		// send
		info.mgr.pc.PacketConn.WriteTo(bs, types.Addr(info.ed[:]))
		info.tx += uint64(len(msg))
		info._resetTimer()
	})
}

func (info *sessionInfo) doRecv(from phony.Actor, msg []byte) {
	// TODO? some worker pool to multi-thread this
	info.Act(from, func() {
		orig := msg
		defer freeBytes(orig)
		if len(msg) < sessionTrafficOverheadMin || msg[0] != sessionTypeTraffic {
			return
		}
		offset := 1
		remoteKeySeq, rksLen := binary.Uvarint(msg[offset:])
		if rksLen <= 0 {
			return
		}
		offset += rksLen
		localKeySeq, lksLen := binary.Uvarint(msg[offset:])
		if lksLen <= 0 {
			return
		}
		offset += lksLen
		nonce, nonceLen := binary.Uvarint(msg[offset:])
		if nonceLen <= 0 {
			return
		}
		offset += nonceLen
		msg := msg[offset:]
		fromCurrent := remoteKeySeq == info.remoteKeySeq
		fromNext := remoteKeySeq == info.remoteKeySeq+1
		toRecv := localKeySeq+1 == info.localKeySeq
		toSend := localKeySeq == info.localKeySeq
		var sharedKey *boxShared
		var onSuccess func(boxPub)
		switch {
		case fromCurrent && toRecv:
			// The boring case, nothing to ratchet, just update nonce
			if !(info.recvNonce < nonce) {
				return
			}
			sharedKey = &info.recvShared
			onSuccess = func(_ boxPub) {
				info.recvNonce = nonce
			}
		case fromNext && toSend:
			// The remote side appears to have ratcheted forward
			if !(info.nextSendNonce < nonce) {
				return
			}
			sharedKey = &info.nextSendShared
			onSuccess = func(innerKey boxPub) {
				info.nextSendNonce = nonce
				if info.rotated.IsZero() || time.Since(info.rotated) > time.Minute {
					// Rotate their keys
					info.current = info.next
					info.next = innerKey
					info.remoteKeySeq++ // = remoteKeySeq
					// Rotate our own keys
					info.recvPub, info.recvPriv = info.sendPub, info.sendPriv
					info.sendPub, info.sendPriv = info.nextPub, info.nextPriv
					info.localKeySeq++
					// Generate new next keys
					info.nextPub, info.nextPriv = newBoxKeys()
					// Update nonces
					info._fixShared(nonce, 0)
					info.rotated = time.Now()
				}
			}
		case fromNext && toRecv:
			// The remote side appears to have ratcheted forward early
			// Technically there's no reason we can't handle this
			//panic("DEBUG") // TODO test this
			if !(info.nextRecvNonce < nonce) {
				return
			}
			sharedKey = &info.nextRecvShared
			onSuccess = func(innerKey boxPub) {
				info.nextRecvNonce = nonce
				if info.rotated.IsZero() || time.Since(info.rotated) > time.Minute {
					// Rotate their keys
					info.current = info.next
					info.next = innerKey
					info.remoteKeySeq++ // = remoteKeySeq
					// Rotate our own keys
					info.recvPub, info.recvPriv = info.sendPub, info.sendPriv
					info.sendPub, info.sendPriv = info.nextPub, info.nextPriv
					info.localKeySeq++
					// Generate new next keys
					info.nextPub, info.nextPriv = newBoxKeys()
					// Update nonces
					info._fixShared(nonce, 0)
					info.rotated = time.Now()
				}
			}
		default:
			// We can't make sense of their message
			// (YggChat change 2: unless it was sent to a key we kept)
			if info._recvOld(remoteKeySeq, localKeySeq, nonce, msg) {
				return
			}
			// Send a sessionInit and hope they ack so we can fix things
			info._sendInit()
			return
		}
		// Decrypt and handle packet
		unboxed, ok := allocBytes(0), false
		defer func() { freeBytes(unboxed) }()
		if unboxed, ok = boxOpen(unboxed, msg, nonce, sharedKey); ok {
			var key boxPub
			copy(key[:], unboxed)
			msg := append(allocBytes(0), unboxed[len(key):]...)
			info.mgr.pc.network.recv(info, msg)
			// Keep the replay window for when this key becomes an old one,
			// and the peer's next key for when it moves there
			info._noteNonce(sharedKey, remoteKeySeq, localKeySeq, nonce)
			info._putPub(remoteKeySeq+1, key)
			// Misc remaining followup work
			onSuccess(key)
			info.rx += uint64(len(msg))
			info._resetTimer()
		} else {
			// Keys somehow became out-of-sync
			// This seems to happen in some edge cases if a node restarts
			// Fix by sending a new init
			// (YggChat change 2: unless it was sent to a key we kept)
			if info._recvOld(remoteKeySeq, localKeySeq, nonce, msg) {
				return
			}
			info._sendInit()
		}
	})
}

func (info *sessionInfo) _sendInit() {
	if !info.initAt.IsZero() && time.Since(info.initAt) < time.Second {
		return // YggChat change 3: one recovery init per second
	}
	info.initAt = time.Now()
	init := newSessionInit(&info.sendPub, &info.nextPub, info.localKeySeq)
	info.mgr.sendInit(&info.ed, &init)
}

func (info *sessionInfo) _sendAck() {
	init := newSessionInit(&info.sendPub, &info.nextPub, info.localKeySeq)
	ack := sessionAck{init}
	info.mgr.sendAck(&info.ed, &ack)
}

/***************
 * sessionInit *
 ***************/

type sessionInit struct {
	current boxPub
	next    boxPub
	keySeq  uint64
	seq     uint64 // timestamp or similar
}

func newSessionInit(current, next *boxPub, keySeq uint64) sessionInit {
	var init sessionInit
	init.current = *current
	init.next = *next
	init.keySeq = keySeq
	// seq is set when it is sent (sessionManager.nextSeq, YggChat change 1)
	return init
}

func (init *sessionInit) encrypt(from *edPriv, to *edPub, auth groupAuth) ([]byte, error) {
	fromPub, fromPriv := newBoxKeys()
	var toBox *boxPub
	var err error
	if toBox, err = to.toBox(); err != nil {
		return nil, err
	}
	// Get sig bytes
	var sigBytes []byte // TODO initialize to correct size
	sigBytes = append(sigBytes, fromPub[:]...)
	sigBytes = append(sigBytes, init.current[:]...)
	sigBytes = append(sigBytes, init.next[:]...)
	offset := len(sigBytes)
	sigBytes = sigBytes[:offset+8]
	binary.BigEndian.PutUint64(sigBytes[offset:offset+8], init.keySeq)
	offset = len(sigBytes)
	sigBytes = sigBytes[:offset+8]
	binary.BigEndian.PutUint64(sigBytes[offset:offset+8], init.seq)
	// Sign
	sig := edSign(sigBytes, from, auth.preimage())
	// Prepare the payload (to be encrypted)
	var payload []byte // TODO initialize to correct size
	payload = append(payload, sig[:]...)
	payload = append(payload, sigBytes[boxPubSize:]...)
	// Encrypt
	var shared boxShared
	getShared(&shared, toBox, &fromPriv)
	bs := boxSeal(nil, payload, 0, &shared)
	// Assemble final message
	data := make([]byte, 1, sessionInitSize)
	data[0] = sessionTypeInit
	data = append(data, fromPub[:]...)
	data = append(data, bs...)
	if len(data) != sessionInitSize {
		panic("this should never happen")
	}
	return data, nil
}

func (init *sessionInit) decrypt(priv *boxPriv, from *edPub, data []byte, auth groupAuth) bool {
	if len(data) != sessionInitSize {
		return false
	}
	var fromBox boxPub
	offset := 1
	offset = bytesPop(fromBox[:], data, offset)
	bs := data[offset:]
	var shared boxShared
	getShared(&shared, &fromBox, priv)
	var payload []byte
	var ok bool
	if payload, ok = boxOpen(nil, bs, 0, &shared); !ok {
		return false
	}
	offset = 0
	var sig edSig
	offset = bytesPop(sig[:], payload, offset)
	tmp := payload[offset:] // Used in sigBytes
	offset = bytesPop(init.current[:], payload, offset)
	offset = bytesPop(init.next[:], payload, offset)
	init.keySeq = binary.BigEndian.Uint64(payload[offset : offset+8])
	offset += 8
	init.seq = binary.BigEndian.Uint64(payload[offset:])
	// Check signature
	var sigBytes []byte
	sigBytes = append(sigBytes, fromBox[:]...)
	sigBytes = append(sigBytes, tmp...)
	return edCheck(sigBytes, &sig, from, auth.preimage())
}

/**************
 * sessionAck *
 **************/

type sessionAck struct {
	sessionInit
}

func (ack *sessionAck) encrypt(from *edPriv, to *edPub, auth groupAuth) ([]byte, error) {
	data, err := ack.sessionInit.encrypt(from, to, auth)
	if err == nil {
		data[0] = sessionTypeAck
	}
	return data, err
}

/*****************
 * sessionBuffer *
 *****************/

type sessionBuffer struct {
	data        []byte
	init        sessionInit
	currentPriv boxPriv     // pairs with init.recv
	nextPriv    boxPriv     // pairs with init.send
	timer       *time.Timer // time.AfterFunc to clean up
	seqAt       time.Time   // when init.seq was last renewed (YggChat change 4)
}
