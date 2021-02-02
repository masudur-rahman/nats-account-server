// Copyright 2020-2021 The NATS Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io/ioutil"
	"math/rand"
	"os"
	"path"
	"sync"
	"sync/atomic"
	"time"
)

type RaftNode interface {
	Propose(entry []byte) error
	PausePropose()
	ResumePropose()
	ForwardProposal(entry []byte) error
	Snapshot(snap []byte) error
	Applied(index uint64)
	Compact(index uint64) error
	State() RaftState
	Size() (entries, bytes uint64)
	Leader() bool
	Quorum() bool
	Current() bool
	GroupLeader() string
	StepDown() error
	Campaign() error
	ID() string
	Group() string
	Peers() []*Peer
	ProposeAddPeer(peer string) error
	ProposeRemovePeer(peer string) error
	ApplyC() <-chan *CommittedEntry
	PauseApply()
	ResumeApply()
	LeadChangeC() <-chan bool
	QuitC() <-chan struct{}
	Stop()
	Delete()
}

type WAL interface {
	StoreMsg(subj string, hdr, msg []byte) (uint64, int64, error)
	LoadMsg(index uint64) (subj string, hdr, msg []byte, ts int64, err error)
	RemoveMsg(index uint64) (bool, error)
	Compact(index uint64) (uint64, error)
	State() StreamState
	Stop() error
	Delete() error
}

type LeadChange struct {
	Leader   bool
	Previous string
}

type Peer struct {
	ID      string
	Current bool
	Last    time.Time
	Index   uint64
}

type RaftState uint8

// Allowable states for a NATS Consensus Group.
const (
	Follower RaftState = iota
	Leader
	Candidate
	Observer
	Closed
)

func (state RaftState) String() string {
	switch state {
	case Follower:
		return "FOLLOWER"
	case Candidate:
		return "CANDIDATE"
	case Leader:
		return "LEADER"
	case Observer:
		return "OBSERVER"
	case Closed:
		return "CLOSED"
	}
	return "UNKNOWN"
}

type raft struct {
	sync.RWMutex
	group   string
	sd      string
	id      string
	wal     WAL
	state   RaftState
	csz     int
	qn      int
	peers   map[string]*lps
	acks    map[uint64]map[string]struct{}
	elect   *time.Timer
	active  time.Time
	term    uint64
	pterm   uint64
	pindex  uint64
	sindex  uint64
	commit  uint64
	applied uint64
	leader  string
	vote    string
	hash    string
	s       *Server
	c       *client
	dflag   bool

	// Subjects for votes, updates, replays.
	psubj  string
	vsubj  string
	vreply string
	asubj  string
	areply string

	// For when we need to catch up as a follower.
	catchup *catchupState

	// For leader or server catching up a follower.
	progress map[string]chan uint64

	// For when we have paused our applyC.
	paused  bool
	hcommit uint64

	// Channels
	propc    chan *Entry
	pausec   chan struct{}
	applyc   chan *CommittedEntry
	sendq    chan *pubMsg
	quit     chan struct{}
	reqs     chan *voteRequest
	votes    chan *voteResponse
	resp     chan *appendEntryResponse
	leadc    chan bool
	stepdown chan string
}

// cacthupState structure that holds our subscription, and catchup term and index
// as well as starting term and index and how many updates we have seen.
type catchupState struct {
	sub    *subscription
	cterm  uint64
	cindex uint64
	pterm  uint64
	pindex uint64
	hbs    int
}

// lps holds peer state of last time and last index replicated.
type lps struct {
	ts int64
	li uint64
}

const (
	minElectionTimeout = 300 * time.Millisecond
	maxElectionTimeout = 3 * minElectionTimeout
	minCampaignTimeout = 50 * time.Millisecond
	maxCampaignTimeout = 4 * minCampaignTimeout
	hbInterval         = 200 * time.Millisecond
	lostQuorumInterval = hbInterval * 3
)

type RaftConfig struct {
	Name  string
	Store string
	Log   WAL
}

var (
	errProposalFailed  = errors.New("raft: proposal failed")
	errProposalsPaused = errors.New("raft: proposals paused")
	errNotLeader       = errors.New("raft: not leader")
	errAlreadyLeader   = errors.New("raft: already leader")
	errNotCurrent      = errors.New("raft: not current")
	errNilCfg          = errors.New("raft: no config given")
	errUnknownPeer     = errors.New("raft: unknown peer")
	errCorruptPeers    = errors.New("raft: corrupt peer state")
	errStepdownFailed  = errors.New("raft: stepdown failed")
	errPeersNotCurrent = errors.New("raft: all peers are not current")
	errFailedToApply   = errors.New("raft: could not place apply entry")
	errEntryLoadFailed = errors.New("raft: could not load entry from WAL")
)

// This will bootstrap a raftNode by writing its config into the store directory.
func (s *Server) bootstrapRaftNode(cfg *RaftConfig, knownPeers []string, allPeersKnown bool) error {
	if cfg == nil {
		return errNilCfg
	}
	// Check validity of peers if presented.
	for _, p := range knownPeers {
		if len(p) != idLen {
			return fmt.Errorf("raft: illegal peer: %q", p)
		}
	}
	expected := len(knownPeers)
	// We need to adjust this is all peers are not known.
	if !allPeersKnown {
		if expected < 2 {
			expected = 2
		}
		if ncr := s.configuredRoutes(); expected < ncr {
			expected = ncr
		}
	}

	return writePeerState(cfg.Store, &peerState{knownPeers, expected})
}

// startRaftNode will start the raft node.
func (s *Server) startRaftNode(cfg *RaftConfig) (RaftNode, error) {
	if cfg == nil {
		return nil, errNilCfg
	}
	s.mu.Lock()
	if s.sys == nil || s.sys.sendq == nil {
		s.mu.Unlock()
		return nil, ErrNoSysAccount
	}
	sendq := s.sys.sendq
	sacc := s.sys.account
	hash := s.sys.shash
	s.mu.Unlock()

	ps, err := readPeerState(cfg.Store)
	if err != nil {
		return nil, err
	}
	if ps == nil || ps.clusterSize < 2 {
		return nil, errors.New("raft: cluster too small")
	}
	n := &raft{
		id:       hash[:idLen],
		group:    cfg.Name,
		sd:       cfg.Store,
		wal:      cfg.Log,
		state:    Follower,
		csz:      ps.clusterSize,
		qn:       ps.clusterSize/2 + 1,
		hash:     hash,
		peers:    make(map[string]*lps),
		acks:     make(map[uint64]map[string]struct{}),
		s:        s,
		c:        s.createInternalSystemClient(),
		sendq:    sendq,
		quit:     make(chan struct{}),
		reqs:     make(chan *voteRequest, 4),
		votes:    make(chan *voteResponse, 8),
		resp:     make(chan *appendEntryResponse, 256),
		propc:    make(chan *Entry, 256),
		applyc:   make(chan *CommittedEntry, 512),
		leadc:    make(chan bool, 4),
		stepdown: make(chan string, 4),
	}
	n.c.registerWithAccount(sacc)

	if atomic.LoadInt32(&s.logging.debug) > 0 {
		n.dflag = true
	}

	if term, vote, err := n.readTermVote(); err != nil && term > 0 {
		n.term = term
		n.vote = vote
	}

	if state := n.wal.State(); state.Msgs > 0 {
		// TODO(dlc) - Recover our state here.
		if first, err := n.loadFirstEntry(); err == nil {
			n.pterm, n.pindex = first.pterm, first.pindex
			if first.commit > 0 {
				n.commit = first.commit
			}
		}
		// Replay the log.
		// Since doing this in place we need to make sure we have enough room on the applyc.
		needed := state.Msgs + 1 // 1 is for nil to mark end of replay.
		if uint64(cap(n.applyc)) < needed {
			n.applyc = make(chan *CommittedEntry, needed)
		}

		for index := state.FirstSeq; index <= state.LastSeq; index++ {
			ae, err := n.loadEntry(index)
			if err != nil {
				panic("err loading entry from WAL")
			}
			n.processAppendEntry(ae, nil)
		}
	}

	// Send nil entry to signal the upper layers we are done doing replay/restore.
	n.applyc <- nil

	// Setup our internal subscriptions.
	if err := n.createInternalSubs(); err != nil {
		n.shutdown(true)
		return nil, err
	}

	// Make sure to track ourselves.
	n.trackPeer(n.id)
	// Track known peers
	for _, peer := range ps.knownPeers {
		// Set these to 0 to start.
		if peer != n.id {
			n.peers[peer] = &lps{0, 0}
		}
	}

	n.notice("Started")

	n.Lock()
	n.resetElectionTimeout()
	n.Unlock()

	s.registerRaftNode(n.group, n)
	s.startGoRoutine(n.run)

	return n, nil
}

// Maps node names back to server names.
func (s *Server) serverNameForNode(node string) string {
	s.mu.Lock()
	sn := s.nodeToName[node]
	s.mu.Unlock()
	return sn
}

// Server will track all raft nodes.
func (s *Server) registerRaftNode(group string, n RaftNode) {
	s.rnMu.Lock()
	defer s.rnMu.Unlock()
	if s.raftNodes == nil {
		s.raftNodes = make(map[string]RaftNode)
	}
	s.raftNodes[group] = n
}

func (s *Server) unregisterRaftNode(group string) {
	s.rnMu.Lock()
	defer s.rnMu.Unlock()
	if s.raftNodes != nil {
		delete(s.raftNodes, group)
	}
}

func (s *Server) lookupRaftNode(group string) RaftNode {
	s.rnMu.RLock()
	defer s.rnMu.RUnlock()
	var n RaftNode
	if s.raftNodes != nil {
		n = s.raftNodes[group]
	}
	return n
}

func (s *Server) transferRaftLeaders() bool {
	if s == nil {
		return false
	}

	var nodes []RaftNode
	s.rnMu.RLock()
	if len(s.raftNodes) > 0 {
		s.Noticef("Transferring any raft leaders")
	}
	for _, n := range s.raftNodes {
		nodes = append(nodes, n)
	}
	s.rnMu.RUnlock()

	var didTransfer bool
	for _, node := range nodes {
		if node.Leader() {
			node.StepDown()
			didTransfer = true
		}
	}
	return didTransfer
}

func (s *Server) shutdownRaftNodes() {
	if s == nil {
		return
	}

	var nodes []RaftNode
	s.rnMu.RLock()
	for _, n := range s.raftNodes {
		nodes = append(nodes, n)
	}
	s.rnMu.RUnlock()

	for _, node := range nodes {
		if node.Leader() {
			node.StepDown()
		}
		node.Stop()
	}
}

// Formal API

// Propose will propose a new entry to the group.
// This should only be called on the leader.
func (n *raft) Propose(data []byte) error {
	n.RLock()
	if n.state != Leader {
		n.RUnlock()
		n.debug("Proposal ignored, not leader")
		return errNotLeader
	}
	propc, paused, quit := n.propc, n.pausec, n.quit
	n.RUnlock()

	if paused != nil {
		n.debug("Proposals paused, will wait")
		select {
		case <-paused:
		case <-quit:
			return errProposalFailed
		case <-time.After(422 * time.Millisecond):
			return errProposalsPaused
		}
	}

	select {
	case propc <- &Entry{EntryNormal, data}:
	default:
		n.debug("Propose failed!")
		return errProposalFailed
	}
	return nil
}

// ForwardProposal will forward the proposal to the leader if known.
// If we are the leader this is the same as calling propose.
// FIXME(dlc) - We could have a reply subject and wait for a response
// for retries, but would need to not block and be in separate Go routine.
func (n *raft) ForwardProposal(entry []byte) error {
	if n.Leader() {
		return n.Propose(entry)
	}
	n.RLock()
	subj := n.psubj
	n.RUnlock()

	n.sendRPC(subj, _EMPTY_, entry)
	return nil
}

// PausePropose will pause new proposals.
func (n *raft) PausePropose() {
	n.Lock()
	if n.pausec == nil {
		n.pausec = make(chan struct{})
	}
	n.Unlock()
}

// ResumePropose will resum new proposals.
func (n *raft) ResumePropose() {
	n.Lock()
	paused := n.pausec
	n.pausec = nil
	n.Unlock()

	if paused != nil {
		close(paused)
	}
}

// ProposeAddPeer is called to add a peer to the group.
func (n *raft) ProposeAddPeer(peer string) error {
	n.RLock()
	if n.state != Leader {
		n.RUnlock()
		return errNotLeader
	}
	propc := n.propc
	n.RUnlock()

	select {
	case propc <- &Entry{EntryAddPeer, []byte(peer)}:
	default:
		return errProposalFailed
	}
	return nil
}

// ProposeRemovePeer is called to remove a peer from the group.
func (n *raft) ProposeRemovePeer(peer string) error {
	return errors.New("no impl")
}

// PauseApply will allow us to pause processing of append entries onto our
// external apply chan.
func (n *raft) PauseApply() {
	n.Lock()
	defer n.Unlock()

	n.paused = true
	n.hcommit = n.commit
}

func (n *raft) ResumeApply() {
	n.Lock()
	defer n.Unlock()

	// Run catchup..
	if n.hcommit > n.commit {
		for index := n.commit + 1; index <= n.hcommit; index++ {
			if err := n.applyCommit(index); err != nil {
				break
			}
		}
	}
	n.paused = false
	n.hcommit = 0
}

// Compact will compact our WAL. If this node is a leader we will want
// all our peers to be at least to the same index. Non-leaders just compact
// directly. This is for when we know we have our state on stable storage.
// E.g JS Consumers.
func (n *raft) Compact(index uint64) error {
	n.Lock()
	defer n.Unlock()
	// If we are not the leader compact at will.
	if n.state != Leader {
		_, err := n.wal.Compact(index)
		return err
	}
	// We are the leader so we need to make sure all peers are at least up to this index.
	for peer, ps := range n.peers {
		if peer != n.id && ps.li < index {
			return errPeersNotCurrent
		}
	}
	return nil
}

// Applied is to be called when the FSM has applied the committed entries.
func (n *raft) Applied(index uint64) {
	n.Lock()
	defer n.Unlock()

	// Ignore if already applied.
	if index <= n.applied {
		return
	}

	// FIXME(dlc) - Check spec on error conditions, storage
	n.applied = index
	if index == n.sindex {
		n.debug("Found snapshot entry: compacting log to index %d", index)
		n.wal.Compact(index)
	}
}

// Snapshot is used to snapshot the fsm. This can only be called from a leader.
// For now these are assumed to be small and will be placed into the log itself.
// TODO(dlc) - For meta and consumers this is straightforward, and for streams sans the messages this is as well.
func (n *raft) Snapshot(snap []byte) error {
	n.Lock()
	defer n.Unlock()

	n.debug("Snapshot called with %d bytes, applied is %d", len(snap), n.applied)

	if n.state != Leader {
		return errNotLeader
	}
	if !n.isCurrent() {
		return errNotCurrent
	}

	select {
	case n.propc <- &Entry{EntrySnapshot, snap}:
	default:
		return errProposalFailed
	}

	return nil
}

// Leader returns if we are the leader for our group.
func (n *raft) Leader() bool {
	if n == nil {
		return false
	}
	n.RLock()
	isLeader := n.state == Leader
	n.RUnlock()
	return isLeader
}

// Lock should be held.
func (n *raft) isCurrent() bool {
	// First check if we match commit and applied.
	if n.commit != n.applied {
		return false
	}
	// Make sure we are the leader or we know we have heard from the leader recently.
	if n.state == Leader {
		return true
	}

	// Check here on catchup status.
	if cs := n.catchup; cs != nil && n.pterm >= cs.cterm && n.pindex >= cs.cindex {
		n.cancelCatchup()
	}

	// Check to see that we have heard from the current leader lately.
	if n.leader != noLeader && n.leader != n.id && n.catchup == nil {
		const okInterval = int64(hbInterval) * 2
		ts := time.Now().UnixNano()
		if ps := n.peers[n.leader]; ps != nil && ps.ts > 0 && (ts-ps.ts) <= okInterval {
			return true
		}
	}
	return false
}

// Current returns if we are the leader for our group or an up to date follower.
func (n *raft) Current() bool {
	if n == nil {
		return false
	}
	n.RLock()
	defer n.RUnlock()

	return n.isCurrent()
}

// GroupLeader returns the current leader of the group.
func (n *raft) GroupLeader() string {
	if n == nil {
		return noLeader
	}
	n.RLock()
	defer n.RUnlock()
	return n.leader
}

// StepDown will have a leader stepdown and optionally do a leader transfer.
func (n *raft) StepDown() error {
	n.Lock()

	if n.state != Leader {
		n.Unlock()
		return errNotLeader
	}

	n.debug("Being asked to stepdown")

	// See if we have up to date followers.
	nowts := time.Now().UnixNano()
	maybeLeader := noLeader
	for peer, ps := range n.peers {
		// If not us and alive and caughtup.
		if peer != n.id && (nowts-ps.ts) < int64(hbInterval*2) {
			if n.s.getRouteByHash([]byte(peer)) != nil {
				n.debug("Looking at %q which is %v behind", peer, time.Duration(nowts-ps.ts))
				maybeLeader = peer
				break
			}
		}
	}
	stepdown := n.stepdown
	n.Unlock()

	if maybeLeader != noLeader {
		n.debug("Stepping down, selected %q for new leader", maybeLeader)
		n.sendAppendEntry([]*Entry{&Entry{EntryLeaderTransfer, []byte(maybeLeader)}})
	}
	// Force us to stepdown here.
	select {
	case stepdown <- noLeader:
	default:
		return errStepdownFailed
	}
	return nil
}

// Campaign will have our node start a leadership vote.
func (n *raft) Campaign() error {
	n.Lock()
	defer n.Unlock()
	return n.campaign()
}

func randCampaignTimeout() time.Duration {
	delta := rand.Int63n(int64(maxCampaignTimeout - minCampaignTimeout))
	return (minCampaignTimeout + time.Duration(delta))
}

// Campaign will have our node start a leadership vote.
// Lock should be held.
func (n *raft) campaign() error {
	n.debug("Starting campaign")
	if n.state == Leader {
		return errAlreadyLeader
	}
	n.resetElect(randCampaignTimeout())
	return nil
}

// State return the current state for this node.
func (n *raft) State() RaftState {
	n.RLock()
	state := n.state
	n.RUnlock()
	return state
}

// Size returns number of entries and total bytes for our WAL.
func (n *raft) Size() (uint64, uint64) {
	n.RLock()
	state := n.wal.State()
	n.RUnlock()
	return state.Msgs, state.Bytes
}

func (n *raft) ID() string {
	n.RLock()
	defer n.RUnlock()
	return n.id
}

func (n *raft) Group() string {
	n.RLock()
	defer n.RUnlock()
	return n.group
}

func (n *raft) Peers() []*Peer {
	n.RLock()
	defer n.RUnlock()

	var peers []*Peer
	for id, ps := range n.peers {
		p := &Peer{ID: id, Current: id == n.leader || ps.li >= n.applied, Last: time.Unix(0, ps.ts)}
		peers = append(peers, p)
	}
	return peers
}

func (n *raft) Stop() {
	n.shutdown(false)
}

func (n *raft) Delete() {
	n.shutdown(true)
}

func (n *raft) ApplyC() <-chan *CommittedEntry { return n.applyc }
func (n *raft) LeadChangeC() <-chan bool       { return n.leadc }
func (n *raft) QuitC() <-chan struct{}         { return n.quit }

func (n *raft) shutdown(shouldDelete bool) {
	n.Lock()
	if n.state == Closed {
		n.Unlock()
		return
	}
	close(n.quit)
	n.c.closeConnection(InternalClient)
	n.state = Closed
	s, g, wal := n.s, n.group, n.wal

	// Delete our peer state and vote state.
	if shouldDelete {
		os.Remove(path.Join(n.sd, peerStateFile))
		os.Remove(path.Join(n.sd, termVoteFile))
	}

	n.Unlock()

	s.unregisterRaftNode(g)
	if shouldDelete {
		n.notice("Deleted")
	} else {
		n.notice("Shutdown")
	}
	if wal != nil {
		if shouldDelete {
			wal.Delete()
		} else {
			wal.Stop()
		}
	}
}

func (n *raft) newInbox(cn string) string {
	var b [replySuffixLen]byte
	rn := rand.Int63()
	for i, l := 0, rn; i < len(b); i++ {
		b[i] = digits[l%base]
		l /= base
	}
	return fmt.Sprintf(raftReplySubj, b[:])
}

const (
	raftVoteSubj   = "$NRG.V.%s.%s"
	raftAppendSubj = "$NRG.E.%s.%s"
	raftPropSubj   = "$NRG.P.%s"
	raftReplySubj  = "$NRG.R.%s"
)

// Our internal subscribe.
// Lock should be held.
func (n *raft) subscribe(subject string, cb msgHandler) (*subscription, error) {
	return n.s.systemSubscribe(subject, _EMPTY_, false, n.c, cb)
}

func (n *raft) createInternalSubs() error {
	cn := n.s.ClusterName()
	n.vsubj, n.vreply = fmt.Sprintf(raftVoteSubj, cn, n.group), n.newInbox(cn)
	n.asubj, n.areply = fmt.Sprintf(raftAppendSubj, cn, n.group), n.newInbox(cn)
	n.psubj = fmt.Sprintf(raftPropSubj, n.group)

	// Votes
	if _, err := n.subscribe(n.vreply, n.handleVoteResponse); err != nil {
		return err
	}
	if _, err := n.subscribe(n.vsubj, n.handleVoteRequest); err != nil {
		return err
	}
	// AppendEntry
	if _, err := n.subscribe(n.areply, n.handleAppendEntryResponse); err != nil {
		return err
	}
	if _, err := n.subscribe(n.asubj, n.handleAppendEntry); err != nil {
		return err
	}

	// TODO(dlc) change events.
	return nil
}

func randElectionTimeout() time.Duration {
	delta := rand.Int63n(int64(maxElectionTimeout - minElectionTimeout))
	return (minElectionTimeout + time.Duration(delta))
}

// Lock should be held.
func (n *raft) resetElectionTimeout() {
	n.resetElect(randElectionTimeout())
}

// Lock should be held.
func (n *raft) resetElect(et time.Duration) {
	if n.elect == nil {
		n.elect = time.NewTimer(et)
	} else {
		if !n.elect.Stop() && len(n.elect.C) > 0 {
			<-n.elect.C
		}
		n.elect.Reset(et)
	}
}

func (n *raft) run() {
	s := n.s
	defer s.grWG.Done()

	for s.isRunning() {
		switch n.State() {
		case Follower:
			n.runAsFollower()
		case Candidate:
			n.runAsCandidate()
		case Leader:
			n.runAsLeader()
		case Observer:
			// TODO(dlc) - fix.
			n.runAsFollower()
		case Closed:
			return
		}
	}
}

func (n *raft) debug(format string, args ...interface{}) {
	if n.dflag {
		nf := fmt.Sprintf("RAFT [%s - %s] %s", n.id, n.group, format)
		n.s.Debugf(nf, args...)
	}
}

func (n *raft) warn(format string, args ...interface{}) {
	nf := fmt.Sprintf("RAFT [%s - %s] %s", n.id, n.group, format)
	n.s.Warnf(nf, args...)
}

func (n *raft) error(format string, args ...interface{}) {
	nf := fmt.Sprintf("RAFT [%s - %s] %s", n.id, n.group, format)
	n.s.Errorf(nf, args...)
}

func (n *raft) notice(format string, args ...interface{}) {
	nf := fmt.Sprintf("RAFT [%s - %s] %s", n.id, n.group, format)
	n.s.Noticef(nf, args...)
}

func (n *raft) electTimer() *time.Timer {
	n.RLock()
	elect := n.elect
	n.RUnlock()
	return elect
}

func (n *raft) runAsFollower() {
	for {
		elect := n.electTimer()
		select {
		case <-n.s.quitCh:
			return
		case <-n.quit:
			return
		case <-elect.C:
			n.switchToCandidate()
			return
		case vreq := <-n.reqs:
			n.processVoteRequest(vreq)
		case newLeader := <-n.stepdown:
			n.switchToFollower(newLeader)
			return
		}
	}
}

// CommitEntry is handed back to the user to apply a commit to their FSM.
type CommittedEntry struct {
	Index   uint64
	Entries []*Entry
}

type appendEntry struct {
	leader  string
	term    uint64
	commit  uint64
	pterm   uint64
	pindex  uint64
	entries []*Entry
	// internal use only.
	reply string
	buf   []byte
}

type EntryType uint8

const (
	EntryNormal EntryType = iota
	EntrySnapshot
	EntryPeerState
	EntryAddPeer
	EntryRemovePeer
	EntryLeaderTransfer
)

func (t EntryType) String() string {
	switch t {
	case EntryNormal:
		return "Normal"
	case EntrySnapshot:
		return "Snapshot"
	case EntryPeerState:
		return "PeerState"
	case EntryAddPeer:
		return "AddPeer"
	case EntryRemovePeer:
		return "RemovePeer"
	case EntryLeaderTransfer:
		return "LeaderTransfer"
	}
	return fmt.Sprintf("Unknown [%d]", uint8(t))
}

type Entry struct {
	Type EntryType
	Data []byte
}

func (ae *appendEntry) String() string {
	return fmt.Sprintf("&{leader:%s term:%d commit:%d pterm:%d pindex:%d entries: %d}",
		ae.leader, ae.term, ae.commit, ae.pterm, ae.pindex, len(ae.entries))
}

const appendEntryBaseLen = idLen + 4*8 + 2

func (ae *appendEntry) encode() []byte {
	var elen int
	for _, e := range ae.entries {
		elen += len(e.Data) + 1 + 4 // 1 is type, 4 is for size.
	}
	var le = binary.LittleEndian
	buf := make([]byte, appendEntryBaseLen+elen)
	copy(buf[:idLen], ae.leader)
	le.PutUint64(buf[8:], ae.term)
	le.PutUint64(buf[16:], ae.commit)
	le.PutUint64(buf[24:], ae.pterm)
	le.PutUint64(buf[32:], ae.pindex)
	le.PutUint16(buf[40:], uint16(len(ae.entries)))
	wi := 42
	for _, e := range ae.entries {
		le.PutUint32(buf[wi:], uint32(len(e.Data)+1))
		wi += 4
		buf[wi] = byte(e.Type)
		wi++
		copy(buf[wi:], e.Data)
		wi += len(e.Data)
	}
	return buf[:wi]
}

// This can not be used post the wire level callback since we do not copy.
func (n *raft) decodeAppendEntry(msg []byte, reply string) *appendEntry {
	if len(msg) < appendEntryBaseLen {
		return nil
	}

	var le = binary.LittleEndian
	ae := &appendEntry{
		leader: string(msg[:idLen]),
		term:   le.Uint64(msg[8:]),
		commit: le.Uint64(msg[16:]),
		pterm:  le.Uint64(msg[24:]),
		pindex: le.Uint64(msg[32:]),
	}
	// Decode Entries.
	ne, ri := int(le.Uint16(msg[40:])), 42
	for i := 0; i < ne; i++ {
		le := int(le.Uint32(msg[ri:]))
		ri += 4
		etype := EntryType(msg[ri])
		ae.entries = append(ae.entries, &Entry{etype, msg[ri+1 : ri+le]})
		ri += int(le)
	}
	ae.reply = reply
	ae.buf = msg
	return ae
}

// appendEntryResponse is our response to a received appendEntry.
type appendEntryResponse struct {
	term    uint64
	index   uint64
	peer    string
	success bool
	// internal
	reply string
}

// We want to make sure this does not change from system changing length of syshash.
const idLen = 8
const appendEntryResponseLen = 24 + 1

func (ar *appendEntryResponse) encode() []byte {
	var buf [appendEntryResponseLen]byte
	var le = binary.LittleEndian
	le.PutUint64(buf[0:], ar.term)
	le.PutUint64(buf[8:], ar.index)
	copy(buf[16:], ar.peer)

	if ar.success {
		buf[24] = 1
	} else {
		buf[24] = 0
	}
	return buf[:appendEntryResponseLen]
}

func (n *raft) decodeAppendEntryResponse(msg []byte) *appendEntryResponse {
	if len(msg) != appendEntryResponseLen {
		return nil
	}
	var le = binary.LittleEndian
	ar := &appendEntryResponse{
		term:  le.Uint64(msg[0:]),
		index: le.Uint64(msg[8:]),
		peer:  string(msg[16 : 16+idLen]),
	}
	ar.success = msg[24] == 1
	return ar
}

// Called when a peer has forwarded a proposal.
func (n *raft) handleForwardedProposal(sub *subscription, c *client, _, reply string, msg []byte) {
	if !n.Leader() {
		n.debug("Ignoring forwarded proposal, not leader")
		return
	}
	// Need to copy since this is underlying client/route buffer.
	msg = append(msg[:0:0], msg...)
	if err := n.Propose(msg); err != nil {
		n.warn("Got error processing forwarded proposal: %v", err)
	}
}

func (n *raft) runAsLeader() {
	n.Lock()
	// For forwarded proposals.
	fsub, err := n.subscribe(n.psubj, n.handleForwardedProposal)
	n.Unlock()

	if err != nil {
		panic(fmt.Sprintf("Error subscribing to forwarded proposals: %v", err))
	}

	// Cleanup our subscription when we leave.
	defer func() {
		n.Lock()
		if fsub != nil {
			n.s.sysUnsubscribe(fsub)
		}
		n.Unlock()
	}()

	n.sendPeerState()

	hb := time.NewTicker(hbInterval)
	defer hb.Stop()

	for {
		select {
		case <-n.s.quitCh:
			return
		case <-n.quit:
			return
		case b := <-n.propc:
			entries := []*Entry{b}
			if b.Type == EntryNormal {
				const maxBatch = 256 * 1024
			gather:
				for sz := 0; sz < maxBatch; {
					select {
					case e := <-n.propc:
						entries = append(entries, e)
						sz += len(e.Data) + 1
					default:
						break gather
					}
				}
			}
			n.sendAppendEntry(entries)
		case <-hb.C:
			if n.notActive() {
				n.sendHeartbeat()
			}
			if n.lostQuorum() {
				n.switchToFollower(noLeader)
				return
			}

		case vresp := <-n.votes:
			if vresp.term > n.currentTerm() {
				n.switchToFollower(noLeader)
				return
			}
			n.trackPeer(vresp.peer)
		case vreq := <-n.reqs:
			n.processVoteRequest(vreq)
		case newLeader := <-n.stepdown:
			n.switchToFollower(newLeader)
			return
		case ar := <-n.resp:
			n.trackPeer(ar.peer)
			if ar.success {
				n.trackResponse(ar)
			} else if ar.reply != _EMPTY_ {
				n.catchupFollower(ar)
			}
		}
	}
}

// Quorum reports the quorum status. Will be called on former leaders.
func (n *raft) Quorum() bool {
	n.RLock()
	defer n.RUnlock()

	now, nc := time.Now().UnixNano(), 1
	for _, peer := range n.peers {
		if now-peer.ts < int64(lostQuorumInterval) {
			nc++
			if nc >= n.qn {
				return true
			}
		}
	}
	return false
}

func (n *raft) lostQuorum() bool {
	n.RLock()
	defer n.RUnlock()
	return n.lostQuorumLocked()
}

func (n *raft) lostQuorumLocked() bool {
	now, nc := time.Now().UnixNano(), 1
	for _, peer := range n.peers {
		if now-peer.ts < int64(lostQuorumInterval) {
			nc++
			if nc >= n.qn {
				return false
			}
		}
	}
	return true
}

// Check for being not active in terms of sending entries.
// Used in determining if we need to send a heartbeat.
func (n *raft) notActive() bool {
	n.RLock()
	defer n.RUnlock()
	return time.Since(n.active) > hbInterval
}

// Return our current term.
func (n *raft) currentTerm() uint64 {
	n.RLock()
	defer n.RUnlock()
	return n.term
}

// Lock should be held.
func (n *raft) loadFirstEntry() (ae *appendEntry, err error) {
	return n.loadEntry(n.wal.State().FirstSeq)
}

func (n *raft) runCatchup(peer, subj string, indexUpdatesC <-chan uint64) {
	n.RLock()
	s, reply := n.s, n.areply
	n.RUnlock()

	defer s.grWG.Done()

	defer func() {
		n.Lock()
		delete(n.progress, peer)
		if len(n.progress) == 0 {
			n.progress = nil
		}
		// Check if this is a new peer and if so go ahead and propose adding them.
		_, ok := n.peers[peer]
		n.Unlock()
		if !ok {
			n.debug("Catchup done for %q, will add into peers", peer)
			n.ProposeAddPeer(peer)
		}
	}()

	n.debug("Running catchup for %q", peer)

	const maxOutstanding = 48 * 1024 * 1024 // 48MB for now.
	next, total, om := uint64(0), 0, make(map[uint64]int)

	sendNext := func() {
		for total <= maxOutstanding {
			next++
			ae, err := n.loadEntry(next)
			if err != nil {
				if err != ErrStoreEOF {
					n.debug("Got an error loading %d index: %v", next, err)
				}
				return
			}
			// Update our tracking total.
			om[next] = len(ae.buf)
			total += len(ae.buf)
			n.sendRPC(subj, reply, ae.buf)
		}
	}

	const activityInterval = 2 * time.Second
	timeout := time.NewTimer(activityInterval)
	defer timeout.Stop()

	stepCheck := time.NewTicker(100 * time.Millisecond)
	defer stepCheck.Stop()

	// Run as long as we are leader and still not caught up.
	for n.Leader() {
		select {
		case <-n.s.quitCh:
			return
		case <-n.quit:
			return
		case <-stepCheck.C:
			if !n.Leader() {
				n.debug("Catching up canceled, no longer leader")
				return
			}
		case <-timeout.C:
			n.debug("Catching up for %q stalled", peer)
			return
		case index := <-indexUpdatesC:
			// Update our activity timer.
			timeout.Reset(activityInterval)
			// Update outstanding total.
			total -= om[index]
			delete(om, index)
			n.RLock()
			finished := index >= n.pindex
			n.RUnlock()
			// Check if we are done.
			if finished {
				n.debug("Finished catching up")
				return
			}
			// Still have more catching up to do.
			if next < index {
				n.debug("Adjusting next to %d from %d", index, next)
				next = index
			}
			sendNext()
		}
	}
}

func (n *raft) catchupFollower(ar *appendEntryResponse) {
	n.debug("Being asked to catch up follower: %q", ar.peer)
	n.Lock()
	if n.progress == nil {
		n.progress = make(map[string]chan uint64)
	}
	if _, ok := n.progress[ar.peer]; ok {
		n.debug("Existing entry for catching up %q", ar.peer)
		n.Unlock()
		return
	}
	ae, err := n.loadEntry(ar.index + 1)
	if err != nil {
		ae, err = n.loadFirstEntry()
	}
	if err != nil || ae == nil {
		n.debug("Could not find a starting entry for us: %v", err)
		n.Unlock()
		return
	}
	if ae.pindex != ar.index || ae.pterm != ar.term {
		n.debug("Our first entry does not match")
	}
	// Create a chan for delivering updates from responses.
	indexUpdates := make(chan uint64, 1024)
	indexUpdates <- ae.pindex
	n.progress[ar.peer] = indexUpdates
	n.Unlock()

	n.s.startGoRoutine(func() { n.runCatchup(ar.peer, ar.reply, indexUpdates) })
}

func (n *raft) loadEntry(index uint64) (*appendEntry, error) {
	_, _, msg, _, err := n.wal.LoadMsg(index)
	if err != nil {
		return nil, err
	}
	return n.decodeAppendEntry(msg, _EMPTY_), nil
}

// applyCommit will update our commit index and apply the entry to the apply chan.
// lock should be held.
func (n *raft) applyCommit(index uint64) error {
	if index <= n.commit {
		n.debug("Ignoring apply commit for %d, already processed", index)
		return nil
	}
	original := n.commit
	n.commit = index

	if n.state == Leader {
		delete(n.acks, index)
	}

	// FIXME(dlc) - Can keep this in memory if this too slow.
	ae, err := n.loadEntry(index)
	if err != nil {
		n.debug("Got an error loading %d index: %v", index, err)
		n.commit = original
		return errEntryLoadFailed
	}
	ae.buf = nil

	var committed []*Entry
	for _, e := range ae.entries {
		switch e.Type {
		case EntryNormal:
			committed = append(committed, e)
		case EntrySnapshot:
			committed = append(committed, e)
		case EntryPeerState:
			if ps, err := decodePeerState(e.Data); err == nil {
				n.processPeerState(ps)
			}
		case EntryAddPeer:
			newPeer := string(e.Data)
			n.debug("Added peer %q", newPeer)
			if _, ok := n.peers[newPeer]; !ok {
				// We are not tracking this one automatically so we need to bump cluster size.
				n.debug("Expanding our clustersize: %d -> %d", n.csz, n.csz+1)
				n.csz++
				n.qn = n.csz/2 + 1
				n.peers[newPeer] = &lps{time.Now().UnixNano(), 0}
			}
			writePeerState(n.sd, &peerState{n.peerNames(), n.csz})
		}
	}
	// Pass to the upper layers if we have normal entries.
	if len(committed) > 0 {
		select {
		case n.applyc <- &CommittedEntry{index, committed}:
		default:
			n.debug("Failed to place committed entry onto our apply channel")
			n.commit = original
			return errFailedToApply
		}
	} else {
		// If we processed inline update our applied index.
		n.applied = index
	}
	return nil
}

// Used to track a success response and apply entries.
func (n *raft) trackResponse(ar *appendEntryResponse) {
	n.Lock()

	// Update peer's last index.
	if ps := n.peers[ar.peer]; ps != nil && ar.index > ps.li {
		ps.li = ar.index
	}

	// If we are tracking this peer as a catchup follower, update that here.
	if indexUpdateC := n.progress[ar.peer]; indexUpdateC != nil {
		select {
		case indexUpdateC <- ar.index:
		default:
			n.debug("Failed to place tracking response for catchup, will try again")
			n.Unlock()
			indexUpdateC <- ar.index
			n.Lock()
		}
	}

	// Ignore items already committed.
	if ar.index <= n.commit {
		n.Unlock()
		return
	}

	// See if we have items to apply.
	var sendHB bool

	if results := n.acks[ar.index]; results != nil {
		results[ar.peer] = struct{}{}
		if nr := len(results); nr >= n.qn {
			// We have a quorum.
			for index := n.commit + 1; index <= ar.index; index++ {
				if err := n.applyCommit(index); err != nil {
					break
				}
			}
			sendHB = len(n.propc) == 0
		}
	}
	n.Unlock()

	if sendHB {
		n.sendHeartbeat()
	}
}

// Track interactions with this peer.
func (n *raft) trackPeer(peer string) error {
	n.Lock()
	var needPeerUpdate bool
	if n.state == Leader {
		if _, ok := n.peers[peer]; !ok {
			// This is someone new, if we have registered all of the peers already
			// this is an error.
			if len(n.peers) >= n.csz {
				n.Unlock()
				n.debug("Leader detected a new peer! %q", peer)
				return errUnknownPeer
			}
			needPeerUpdate = true
		}
	}
	if ps := n.peers[peer]; ps != nil {
		ps.ts = time.Now().UnixNano()
	} else {
		n.peers[peer] = &lps{time.Now().UnixNano(), 0}
	}
	n.Unlock()

	if needPeerUpdate {
		n.sendPeerState()
	}
	return nil
}

func (n *raft) runAsCandidate() {
	n.Lock()
	// Drain old responses.
	for len(n.votes) > 0 {
		<-n.votes
	}
	n.Unlock()

	// Send out our request for votes.
	n.requestVote()

	// We vote for ourselves.
	votes := 1

	for {
		elect := n.electTimer()
		select {
		case <-n.s.quitCh:
			return
		case <-n.quit:
			return
		case <-elect.C:
			n.switchToCandidate()
			return
		case vresp := <-n.votes:
			n.trackPeer(vresp.peer)
			if vresp.granted && n.term >= vresp.term {
				votes++
				if n.wonElection(votes) {
					// Become LEADER if we have won.
					n.switchToLeader()
					return
				}
			}
		case vreq := <-n.reqs:
			n.processVoteRequest(vreq)
		case newLeader := <-n.stepdown:
			n.switchToFollower(newLeader)
			return
		}
	}
}

// handleAppendEntry handles an append entry from the wire. We can't rely on msg being available
// past this callback so will do a bunch of processing here to avoid copies, channels etc.
func (n *raft) handleAppendEntry(sub *subscription, c *client, subject, reply string, msg []byte) {
	ae := n.decodeAppendEntry(msg, reply)
	if ae == nil {
		return
	}
	n.processAppendEntry(ae, sub)
}

// Lock should be held.
func (n *raft) cancelCatchup() {
	n.debug("Canceling catchup subscription since we are now up to date")
	n.s.sysUnsubscribe(n.catchup.sub)
	n.catchup = nil
}

// catchupStalled will try to determine if we are stalled. This is called
// on a new entry from our leader.
// Lock should be held.
func (n *raft) catchupStalled() bool {
	if n.catchup == nil {
		return false
	}
	const maxHBs = 3
	if n.catchup.pindex == n.pindex {
		n.catchup.hbs++
	} else {
		n.catchup.pindex = n.pindex
		n.catchup.hbs = 0
	}
	return n.catchup.hbs >= maxHBs
}

// Lock should be held.
func (n *raft) createCatchup(ae *appendEntry) string {
	// Cleanup any old ones.
	if n.catchup != nil && n.catchup.sub != nil {
		n.s.sysUnsubscribe(n.catchup.sub)
	}
	// Snapshot term and index.
	n.catchup = &catchupState{
		cterm:  ae.pterm,
		cindex: ae.pindex,
		pterm:  n.pterm,
		pindex: n.pindex,
	}
	inbox := n.newInbox(n.s.ClusterName())
	sub, _ := n.subscribe(inbox, n.handleAppendEntry)
	n.catchup.sub = sub
	return inbox
}

// Attempt to stepdown, lock should be held.
func (n *raft) attemptStepDown(newLeader string) {
	select {
	case n.stepdown <- newLeader:
	default:
		n.debug("Failed to place stepdown for new leader %q for %q", newLeader, n.group)
	}
}

// processAppendEntry will process an appendEntry.
func (n *raft) processAppendEntry(ae *appendEntry, sub *subscription) {
	n.Lock()
	// Just return if closed.
	if n.state == Closed {
		n.Unlock()
		return
	}

	// If we received an append entry as a candidate we should convert to a follower.
	if n.state == Candidate {
		n.debug("Received append entry in candidate state from %q, converting to follower", ae.leader)
		n.term = ae.term
		n.vote = noVote
		n.writeTermVote()
		n.attemptStepDown(ae.leader)
	}

	// Catching up state.
	catchingUp := n.catchup != nil
	// Is this a new entry or a replay on startup?
	isNew := sub != nil && (!catchingUp || sub != n.catchup.sub)

	if isNew {
		n.resetElectionTimeout()
		// Track leader directly
		if ae.leader != noLeader {
			if ps := n.peers[ae.leader]; ps != nil {
				ps.ts = time.Now().UnixNano()
			} else {
				n.peers[ae.leader] = &lps{time.Now().UnixNano(), 0}
			}
		}
	}

	// Ignore old terms.
	if isNew && ae.term < n.term {
		n.Unlock()
		n.debug("AppendEntry ignoring old term")
		return
	}

	// Check state if we are catching up.
	if catchingUp && isNew {
		if cs := n.catchup; cs != nil && n.pterm >= cs.cterm && n.pindex >= cs.cindex {
			// If we are here we are good, so if we have a catchup pending we can cancel.
			n.cancelCatchup()
			catchingUp = false
		} else {
			var ar *appendEntryResponse
			var inbox string
			// Check to see if we are stalled. If so recreate our catchup state and resend response.
			if n.catchupStalled() {
				n.debug("Catchup may be stalled, will request again")
				inbox = n.createCatchup(ae)
				ar = &appendEntryResponse{n.pterm, n.pindex, n.id, false, _EMPTY_}
			}
			// Ignore new while catching up or replaying.
			n.Unlock()
			if ar != nil {
				n.sendRPC(ae.reply, inbox, ar.encode())
			}
			return
		}
	}

	// If this term is greater than ours.
	if ae.term > n.term {
		n.term = ae.term
		n.vote = noVote
		n.writeTermVote()
		if n.state != Follower {
			n.debug("Term higher than ours and we are not a follower: %v, stepping down to %q", n.state, ae.leader)
			n.attemptStepDown(ae.leader)
		}
	}

	if n.leader != ae.leader && n.state == Follower {
		n.debug("AppendEntry updating leader to %q", ae.leader)
		n.leader = ae.leader
		n.vote = noVote
		n.writeTermVote()
		if isNew {
			n.resetElectionTimeout()
			n.updateLeadChange(false)
		}
	}

	// TODO(dlc) - Do both catchup and delete new behaviors from spec.
	if ae.pterm != n.pterm || ae.pindex != n.pindex {
		// Check if we are catching up and this is a snapshot, if so reset our wal's index.
		// Snapshots will always be by themselves.
		if catchingUp && len(ae.entries) > 0 && ae.entries[0].Type == EntrySnapshot {
			n.debug("Should reset index for wal to %d", ae.pindex+1)
			n.wal.Compact(ae.pindex + 1)
			n.pindex = ae.pindex
			n.commit = ae.pindex
		} else {
			n.debug("AppendEntry did not match %d %d with %d %d", ae.pterm, ae.pindex, n.pterm, n.pindex)
			// Reset our term.
			n.term = n.pterm
			// Setup our state for catching up.
			inbox := n.createCatchup(ae)
			ar := appendEntryResponse{n.pterm, n.pindex, n.id, false, _EMPTY_}
			n.Unlock()
			n.sendRPC(ae.reply, inbox, ar.encode())
			return
		}
	}

	// Save to our WAL if we have entries.
	if len(ae.entries) > 0 {
		// Only store if an original which will have sub != nil
		if sub != nil {
			if err := n.storeToWAL(ae); err != nil {
				n.debug("Error storing to WAL: %v", err)
				if err == ErrStoreClosed {
					n.Unlock()
					return
				}
			}
		} else {
			// This is a replay on startup so just take the appendEntry version.
			n.pterm = ae.term
			n.pindex = ae.pindex + 1
		}

		// Check to see if we have any related entries to process here.
		for _, e := range ae.entries {
			switch e.Type {
			case EntryLeaderTransfer:
				if isNew {
					maybeLeader := string(e.Data)
					if maybeLeader == n.id {
						n.campaign()
					}
				}
			case EntryAddPeer:
				if newPeer := string(e.Data); len(newPeer) == idLen {
					// Track directly
					if ps := n.peers[newPeer]; ps != nil {
						ps.ts = time.Now().UnixNano()
					} else {
						n.peers[newPeer] = &lps{time.Now().UnixNano(), 0}
					}
				}
			case EntrySnapshot:
				if ae.pindex+1 > n.sindex {
					n.sindex = ae.pindex + 1
				}
			}
		}
	}

	// Apply anything we need here.
	if ae.commit > n.commit {
		if n.paused {
			n.hcommit = ae.commit
			n.debug("Paused, not applying %d", ae.commit)
		} else {
			for index := n.commit + 1; index <= ae.commit; index++ {
				if err := n.applyCommit(index); err != nil {
					break
				}
			}
		}
	}

	ar := appendEntryResponse{n.pterm, n.pindex, n.id, true, _EMPTY_}
	n.Unlock()

	// Success. Send our response.
	n.sendRPC(ae.reply, _EMPTY_, ar.encode())
}

// Lock should be held.
func (n *raft) processPeerState(ps *peerState) {
	// Update our version of peers to that of the leader.
	n.csz = ps.clusterSize
	n.peers = make(map[string]*lps)
	for _, peer := range ps.knownPeers {
		n.peers[peer] = &lps{0, 0}
	}
	n.debug("Update peers from leader to %+v", n.peers)
	writePeerState(n.sd, ps)
}

// handleAppendEntryResponse just places the decoded response on the appropriate channel.
func (n *raft) handleAppendEntryResponse(sub *subscription, c *client, subject, reply string, msg []byte) {
	aer := n.decodeAppendEntryResponse(msg)
	if reply != _EMPTY_ {
		aer.reply = reply
	}
	select {
	case n.resp <- aer:
	default:
		n.error("Failed to place add entry response on chan for %q", n.group)
	}
}

func (n *raft) buildAppendEntry(entries []*Entry) *appendEntry {
	return &appendEntry{n.id, n.term, n.commit, n.pterm, n.pindex, entries, _EMPTY_, nil}
}

// lock should be held.
func (n *raft) storeToWAL(ae *appendEntry) error {
	if ae.buf == nil {
		panic("nil buffer for appendEntry!")
	}
	seq, _, err := n.wal.StoreMsg(_EMPTY_, nil, ae.buf)
	if err != nil {
		return err
	}

	// Sanity checking for now.
	if ae.pindex != seq-1 {
		panic(fmt.Sprintf("[%s] Placed an entry at the wrong index, ae is %+v, index is %d\n\n", n.s, ae, seq))
	}

	n.pterm = ae.term
	n.pindex = seq
	return nil
}

func (n *raft) sendAppendEntry(entries []*Entry) {
	n.Lock()
	defer n.Unlock()
	ae := n.buildAppendEntry(entries)
	ae.buf = ae.encode()
	// If we have entries store this in our wal.
	if len(entries) > 0 {
		if err := n.storeToWAL(ae); err != nil {
			panic("Error storing!")
		}
		// We count ourselves.
		n.acks[n.pindex] = map[string]struct{}{n.id: struct{}{}}
		// Check for snapshot
		for _, e := range entries {
			if e.Type == EntrySnapshot {
				n.sindex = n.pindex
			}
		}
		n.active = time.Now()
	}
	n.sendRPC(n.asubj, n.areply, ae.buf)
}

type peerState struct {
	knownPeers  []string
	clusterSize int
}

func encodePeerState(ps *peerState) []byte {
	var le = binary.LittleEndian
	buf := make([]byte, 4+4+(8*len(ps.knownPeers)))
	le.PutUint32(buf[0:], uint32(ps.clusterSize))
	le.PutUint32(buf[4:], uint32(len(ps.knownPeers)))
	wi := 8
	for _, peer := range ps.knownPeers {
		copy(buf[wi:], peer)
		wi += idLen
	}
	return buf
}

func decodePeerState(buf []byte) (*peerState, error) {
	if len(buf) < 8 {
		return nil, errCorruptPeers
	}
	var le = binary.LittleEndian
	ps := &peerState{clusterSize: int(le.Uint32(buf[0:]))}
	expectedPeers := int(le.Uint32(buf[4:]))
	buf = buf[8:]
	for i, ri, n := 0, 0, expectedPeers; i < n && ri < len(buf); i++ {
		ps.knownPeers = append(ps.knownPeers, string(buf[ri:ri+idLen]))
		ri += idLen
	}
	if len(ps.knownPeers) != expectedPeers {
		return nil, errCorruptPeers
	}
	return ps, nil
}

// Lock should be held.
func (n *raft) peerNames() []string {
	var peers []string
	for peer := range n.peers {
		peers = append(peers, peer)
	}
	return peers
}

func (n *raft) currentPeerState() *peerState {
	n.RLock()
	ps := &peerState{n.peerNames(), n.csz}
	n.RUnlock()
	return ps
}

// sendPeerState will send our current peer state to the cluster.
func (n *raft) sendPeerState() {
	n.sendAppendEntry([]*Entry{&Entry{EntryPeerState, encodePeerState(n.currentPeerState())}})
}

func (n *raft) sendHeartbeat() {
	n.sendAppendEntry(nil)
}

type voteRequest struct {
	term      uint64
	lastTerm  uint64
	lastIndex uint64
	candidate string
	// internal only.
	reply string
}

const voteRequestLen = 24 + idLen

func (vr *voteRequest) encode() []byte {
	var buf [voteRequestLen]byte
	var le = binary.LittleEndian
	le.PutUint64(buf[0:], vr.term)
	le.PutUint64(buf[8:], vr.lastTerm)
	le.PutUint64(buf[16:], vr.lastIndex)
	copy(buf[24:24+idLen], vr.candidate)

	return buf[:voteRequestLen]
}

func (n *raft) decodeVoteRequest(msg []byte, reply string) *voteRequest {
	if len(msg) != voteRequestLen {
		return nil
	}
	// Need to copy for now b/c of candidate.
	msg = append(msg[:0:0], msg...)

	var le = binary.LittleEndian
	return &voteRequest{
		term:      le.Uint64(msg[0:]),
		lastTerm:  le.Uint64(msg[8:]),
		lastIndex: le.Uint64(msg[16:]),
		candidate: string(msg[24 : 24+idLen]),
		reply:     reply,
	}
}

const peerStateFile = "peers.idx"

// Writes out our peer state.
func writePeerState(sd string, ps *peerState) error {
	psf := path.Join(sd, peerStateFile)
	if _, err := os.Stat(psf); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := ioutil.WriteFile(psf, encodePeerState(ps), 0644); err != nil {
		return err
	}
	return nil
}

func readPeerState(sd string) (ps *peerState, err error) {
	buf, err := ioutil.ReadFile(path.Join(sd, peerStateFile))
	if err != nil {
		return nil, err
	}
	return decodePeerState(buf)
}

const termVoteFile = "tav.idx"
const termVoteLen = idLen + 8

// readTermVote will read the largest term and who we voted from to stable storage.
// Lock should be held.
func (n *raft) readTermVote() (term uint64, voted string, err error) {
	buf, err := ioutil.ReadFile(path.Join(n.sd, termVoteFile))
	if err != nil {
		return 0, noVote, err
	}
	if len(buf) < termVoteLen {
		return 0, noVote, nil
	}
	var le = binary.LittleEndian
	term = le.Uint64(buf[0:])
	voted = string(buf[8:])
	return term, voted, nil
}

// writeTermVote will record the largest term and who we voted for to stable storage.
// Lock should be held.
func (n *raft) writeTermVote() error {
	tvf := path.Join(n.sd, termVoteFile)
	if _, err := os.Stat(tvf); err != nil && !os.IsNotExist(err) {
		return err
	}
	var buf [termVoteLen]byte
	var le = binary.LittleEndian
	le.PutUint64(buf[0:], n.term)
	// FIXME(dlc) - NoVote
	copy(buf[8:], n.vote)
	if err := ioutil.WriteFile(tvf, buf[:8+len(n.vote)], 0644); err != nil {
		return err
	}
	return nil
}

// voteResponse is a response to a vote request.
type voteResponse struct {
	term    uint64
	peer    string
	granted bool
}

const voteResponseLen = 8 + 8 + 1

func (vr *voteResponse) encode() []byte {
	var buf [voteResponseLen]byte
	var le = binary.LittleEndian
	le.PutUint64(buf[0:], vr.term)
	copy(buf[8:], vr.peer)
	if vr.granted {
		buf[16] = 1
	} else {
		buf[16] = 0
	}
	return buf[:voteResponseLen]
}

func (n *raft) decodeVoteResponse(msg []byte) *voteResponse {
	if len(msg) != voteResponseLen {
		return nil
	}
	var le = binary.LittleEndian
	vr := &voteResponse{term: le.Uint64(msg[0:]), peer: string(msg[8:16])}
	vr.granted = msg[16] == 1
	return vr
}

func (n *raft) handleVoteResponse(sub *subscription, c *client, _, reply string, msg []byte) {
	vr := n.decodeVoteResponse(msg)
	n.debug("Received a voteResponse %+v", vr)
	if vr == nil {
		n.error("Received malformed vote response for %q", n.group)
		return
	}
	select {
	case n.votes <- vr:
	default:
		// FIXME(dlc)
		n.error("Failed to place vote response on chan for %q", n.group)
	}
}

func (n *raft) processVoteRequest(vr *voteRequest) error {
	n.RLock()
	vresp := voteResponse{n.term, n.id, false}
	n.RUnlock()

	n.debug("Received a voteRequest %+v", vr)
	defer n.debug("Sending a voteResponse %+v -> %q", &vresp, vr.reply)

	if err := n.trackPeer(vr.candidate); err != nil {
		n.sendReply(vr.reply, vresp.encode())
		return err
	}

	n.Lock()

	// Ignore if we are newer.
	if vr.term < n.term {
		n.Unlock()
		n.sendReply(vr.reply, vresp.encode())
		return nil
	}

	// If this is a higher term go ahead and stepdown.
	if vr.term > n.term {
		n.term = vr.term
		n.vote = noVote
		n.writeTermVote()
		if n.state == Candidate {
			n.debug("Stepping down from candidate, detected higher term: %d vs %d", vr.term, n.term)
			n.attemptStepDown(noLeader)
		}
	}

	// Only way we get to yes is through here.
	if vr.lastIndex >= n.pindex && n.vote == noVote || n.vote == vr.candidate {
		vresp.granted = true
		n.vote = vr.candidate
		n.writeTermVote()
		n.resetElectionTimeout()
	}
	n.Unlock()

	n.sendReply(vr.reply, vresp.encode())

	return nil
}

func (n *raft) handleVoteRequest(sub *subscription, c *client, subject, reply string, msg []byte) {
	vr := n.decodeVoteRequest(msg, reply)
	if vr == nil {
		n.error("Received malformed vote request for %q", n.group)
		return
	}
	select {
	case n.reqs <- vr:
	default:
		n.error("Failed to place vote request on chan for %q", n.group)
	}
}

func (n *raft) requestVote() {
	n.Lock()
	if n.state != Candidate {
		n.Unlock()
		panic("raft requestVote not from candidate")
	}
	n.vote = n.id
	n.writeTermVote()
	vr := voteRequest{n.term, n.pterm, n.pindex, n.id, _EMPTY_}
	subj, reply := n.vsubj, n.vreply
	n.Unlock()

	n.debug("Sending out voteRequest %+v", vr)

	// Now send it out.
	n.sendRPC(subj, reply, vr.encode())
}

func (n *raft) sendRPC(subject, reply string, msg []byte) {
	n.sendq <- &pubMsg{n.c, subject, reply, nil, msg, false}
}

func (n *raft) sendReply(subject string, msg []byte) {
	n.sendq <- &pubMsg{n.c, subject, _EMPTY_, nil, msg, false}
}

func (n *raft) wonElection(votes int) bool {
	return votes >= n.quorumNeeded()
}

// Return the quorum size for a given cluster config.
func (n *raft) quorumNeeded() int {
	n.RLock()
	qn := n.qn
	n.RUnlock()
	return qn
}

// Lock should be held.
func (n *raft) updateLeadChange(isLeader bool) {
	select {
	case n.leadc <- isLeader:
	case <-n.leadc:
		// We had an old value not consumed.
		select {
		case n.leadc <- isLeader:
		default:
			n.error("Failed to post lead change to %v for %q", isLeader, n.group)
		}
	}
}

// Lock should be held.
func (n *raft) switchState(state RaftState) {
	if n.state == Closed {
		return
	}

	// Reset the election timer.
	n.resetElectionTimeout()

	if n.state == Leader && state != Leader {
		n.updateLeadChange(false)
	} else if state == Leader && n.state != Leader {
		n.updateLeadChange(true)
	}

	n.state = state
	n.vote = noVote
	n.writeTermVote()
}

const (
	noLeader = _EMPTY_
	noVote   = _EMPTY_
)

func (n *raft) switchToFollower(leader string) {
	n.notice("Switching to follower")
	n.Lock()
	defer n.Unlock()
	n.leader = leader
	n.switchState(Follower)
}

func (n *raft) switchToCandidate() {
	n.Lock()
	defer n.Unlock()
	if n.state != Candidate {
		n.notice("Switching to candidate")
	} else if n.lostQuorumLocked() {
		// We signal to the upper layers such that can alert on quorum lost.
		n.updateLeadChange(false)
	}
	// Increment the term.
	n.term++
	// Clear current Leader.
	n.leader = noLeader
	n.switchState(Candidate)
}

func (n *raft) switchToLeader() {
	n.notice("Switching to leader")
	n.Lock()
	defer n.Unlock()
	n.leader = n.id
	n.switchState(Leader)
}
