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
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"path"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/s2"
	"github.com/nats-io/nuid"
)

// jetStreamCluster holds information about the meta group and stream assignments.
type jetStreamCluster struct {
	// The metacontroller raftNode.
	meta RaftNode
	// For stream and consumer assignments. All servers will have this be the same.
	// ACC -> STREAM -> Stream Assignment -> Consumers
	streams map[string]map[string]*streamAssignment
	// Server.
	s *Server
	// Internal client.
	c *client
	// Processing assignment results.
	streamResults   *subscription
	consumerResults *subscription
}

// Define types of the entry.
type entryOp uint8

const (
	// Meta ops.
	assignStreamOp entryOp = iota
	assignConsumerOp
	removeStreamOp
	removeConsumerOp
	// Stream ops.
	streamMsgOp
	purgeStreamOp
	deleteMsgOp
	// Consumer ops
	updateDeliveredOp
	updateAcksOp
	// Compressed consumer assignments.
	assignCompressedConsumerOp
)

// raftGroups are controlled by the metagroup controller.
// The raftGroups will house streams and consumers.
type raftGroup struct {
	Name      string      `json:"name"`
	Peers     []string    `json:"peers"`
	Storage   StorageType `json:"store"`
	Preferred string      `json:"preferred,omitempty"`
	// Internal
	node RaftNode
}

// streamAssignment is what the meta controller uses to assign streams to peers.
type streamAssignment struct {
	Client  *ClientInfo   `json:"client,omitempty"`
	Created time.Time     `json:"created"`
	Config  *StreamConfig `json:"stream"`
	Group   *raftGroup    `json:"group"`
	Sync    string        `json:"sync"`
	Reply   string        `json:"reply"`
	Restore *StreamState  `json:"restore_state,omitempty"`
	// Internal
	consumers map[string]*consumerAssignment
	responded bool
	err       error
}

// consumerAssignment is what the meta controller uses to assign consumers to streams.
type consumerAssignment struct {
	Client  *ClientInfo     `json:"client,omitempty"`
	Created time.Time       `json:"created"`
	Name    string          `json:"name"`
	Stream  string          `json:"stream"`
	Config  *ConsumerConfig `json:"consumer"`
	Group   *raftGroup      `json:"group"`
	Reply   string          `json:"reply"`
	State   *ConsumerState  `json:"state,omitempty"`
	// Internal
	responded bool
	err       error
}

// streamPurge is what the stream leader will replicate when purging a stream.
type streamPurge struct {
	Client *ClientInfo `json:"client,omitempty"`
	Stream string      `json:"stream"`
	Reply  string      `json:"reply"`
}

// streamMsgDelete is what the stream leader will replicate when deleting a message.
type streamMsgDelete struct {
	Client *ClientInfo `json:"client,omitempty"`
	Stream string      `json:"stream"`
	Seq    uint64      `json:"seq"`
	Reply  string      `json:"reply"`
}

const (
	defaultStoreDirName  = "_js_"
	defaultMetaGroupName = "_meta_"
	defaultMetaFSBlkSize = 64 * 1024
)

// For validating clusters.
func validateJetStreamOptions(o *Options) error {
	// If not clustered no checks.
	if !o.JetStream || o.Cluster.Port == 0 {
		return nil
	}
	if o.ServerName == _EMPTY_ {
		return fmt.Errorf("jetstream cluster requires `server_name` to be set")
	}
	if o.Cluster.Name == _EMPTY_ {
		return fmt.Errorf("jetstream cluster requires `cluster.name` to be set")
	}
	return nil
}

func (s *Server) getJetStreamCluster() (*jetStream, *jetStreamCluster) {
	s.mu.Lock()
	shutdown := s.shutdown
	js := s.js
	s.mu.Unlock()

	if shutdown || js == nil {
		return nil, nil
	}

	js.mu.RLock()
	cc := js.cluster
	js.mu.RUnlock()
	if cc == nil {
		return nil, nil
	}
	return js, cc
}

func (s *Server) JetStreamIsClustered() bool {
	js := s.getJetStream()
	if js == nil {
		return false
	}
	js.mu.RLock()
	isClustered := js.cluster != nil
	js.mu.RUnlock()
	return isClustered
}

func (s *Server) JetStreamIsLeader() bool {
	js := s.getJetStream()
	if js == nil {
		return false
	}
	js.mu.RLock()
	defer js.mu.RUnlock()
	return js.cluster.isLeader()
}

func (s *Server) JetStreamIsCurrent() bool {
	js := s.getJetStream()
	if js == nil {
		return false
	}
	js.mu.RLock()
	defer js.mu.RUnlock()
	return js.cluster.isCurrent()
}

func (s *Server) JetStreamSnapshotMeta() error {
	js := s.getJetStream()
	if js == nil {
		return ErrJetStreamNotEnabled
	}
	js.mu.RLock()
	defer js.mu.RUnlock()
	cc := js.cluster
	if !cc.isLeader() {
		return errNotLeader
	}
	return cc.meta.Snapshot(js.metaSnapshot())
}

func (s *Server) JetStreamStepdownStream(account, stream string) error {
	js, cc := s.getJetStreamCluster()
	if js == nil {
		return ErrJetStreamNotEnabled
	}
	if cc == nil {
		return ErrJetStreamNotClustered
	}
	// Grab account
	acc, err := s.LookupAccount(account)
	if err != nil {
		return err
	}
	// Grab stream
	mset, err := acc.LookupStream(stream)
	if err != nil {
		return err
	}

	if node := mset.raftNode(); node != nil && node.Leader() {
		node.StepDown()
	}

	return nil
}

func (s *Server) JetStreamSnapshotStream(account, stream string) error {
	js, cc := s.getJetStreamCluster()
	if js == nil {
		return ErrJetStreamNotEnabled
	}
	if cc == nil {
		return ErrJetStreamNotClustered
	}
	// Grab account
	acc, err := s.LookupAccount(account)
	if err != nil {
		return err
	}
	// Grab stream
	mset, err := acc.LookupStream(stream)
	if err != nil {
		return err
	}

	mset.mu.RLock()
	if !mset.node.Leader() {
		mset.mu.RUnlock()
		return ErrJetStreamNotLeader
	}
	n := mset.node
	mset.mu.RUnlock()

	n.PausePropose()
	err = n.Snapshot(mset.snapshot())
	n.ResumePropose()

	return err
}

func (s *Server) JetStreamClusterPeers() []string {
	js := s.getJetStream()
	if js == nil {
		return nil
	}
	js.mu.RLock()
	defer js.mu.RUnlock()

	cc := js.cluster
	if !cc.isLeader() {
		return nil
	}
	peers := cc.meta.Peers()
	var nodes []string
	for _, p := range peers {
		nodes = append(nodes, p.ID)
	}
	return nodes
}

// Read lock should be held.
func (cc *jetStreamCluster) isLeader() bool {
	if cc == nil {
		// Non-clustered mode
		return true
	}
	return cc.meta.Leader()
}

// isCurrent will determine if this node is a leader or an up to date follower.
// Read lock should be held.
func (cc *jetStreamCluster) isCurrent() bool {
	if cc == nil {
		// Non-clustered mode
		return true
	}
	return cc.meta.Current()
}

// isStreamCurrent will determine if this node is a participant for the stream and if its up to date.
// Read lock should be held.
func (cc *jetStreamCluster) isStreamCurrent(account, stream string) bool {
	if cc == nil {
		// Non-clustered mode
		return true
	}
	as := cc.streams[account]
	if as == nil {
		return false
	}
	sa := as[stream]
	if sa == nil {
		return false
	}
	rg := sa.Group
	if rg == nil || rg.node == nil {
		return false
	}

	isCurrent := rg.node.Current()
	if isCurrent {
		// Check if we are processing a snapshot and are catching up.
		acc, err := cc.s.LookupAccount(account)
		if err != nil {
			return false
		}
		mset, err := acc.LookupStream(stream)
		if err != nil {
			return false
		}
		if mset.isCatchingUp() {
			return false
		}
	}

	return isCurrent
}

func (a *Account) getJetStreamFromAccount() (*Server, *jetStream, *jsAccount) {
	a.mu.RLock()
	jsa := a.js
	a.mu.RUnlock()
	if jsa == nil {
		return nil, nil, nil
	}
	jsa.mu.RLock()
	js := jsa.js
	jsa.mu.RUnlock()
	if js == nil {
		return nil, nil, nil
	}
	js.mu.RLock()
	s := js.srv
	js.mu.RUnlock()
	return s, js, jsa
}

func (s *Server) JetStreamIsStreamLeader(account, stream string) bool {
	js, cc := s.getJetStreamCluster()
	if js == nil || cc == nil {
		return false
	}
	js.mu.RLock()
	defer js.mu.RUnlock()
	return cc.isStreamLeader(account, stream)
}

func (a *Account) JetStreamIsStreamLeader(stream string) bool {
	s, js, jsa := a.getJetStreamFromAccount()
	if s == nil || js == nil || jsa == nil {
		return false
	}
	js.mu.RLock()
	defer js.mu.RUnlock()
	return js.cluster.isStreamLeader(a.Name, stream)
}

func (s *Server) JetStreamIsStreamCurrent(account, stream string) bool {
	js, cc := s.getJetStreamCluster()
	if js == nil {
		return false
	}
	js.mu.RLock()
	defer js.mu.RUnlock()
	return cc.isStreamCurrent(account, stream)
}

func (a *Account) JetStreamIsConsumerLeader(stream, consumer string) bool {
	s, js, jsa := a.getJetStreamFromAccount()
	if s == nil || js == nil || jsa == nil {
		return false
	}
	js.mu.RLock()
	defer js.mu.RUnlock()
	return js.cluster.isConsumerLeader(a.Name, stream, consumer)
}

func (s *Server) JetStreamIsConsumerLeader(account, stream, consumer string) bool {
	js, cc := s.getJetStreamCluster()
	if js == nil || cc == nil {
		return false
	}
	js.mu.RLock()
	defer js.mu.RUnlock()
	return cc.isConsumerLeader(account, stream, consumer)
}

func (s *Server) enableJetStreamClustering() error {
	if !s.isRunning() {
		return nil
	}
	js := s.getJetStream()
	if js == nil {
		return ErrJetStreamNotEnabled
	}
	// Already set.
	if js.cluster != nil {
		return nil
	}

	s.Noticef("Starting JetStream cluster")
	// We need to determine if we have a stable cluster name and expected number of servers.
	s.Debugf("JetStream cluster checking for stable cluster name and peers")
	if s.isClusterNameDynamic() || s.configuredRoutes() == 0 {
		return errors.New("JetStream cluster requires cluster name and explicit routes")
	}

	return js.setupMetaGroup()
}

func (js *jetStream) setupMetaGroup() error {
	s := js.srv
	s.Noticef("Creating JetStream metadata controller")

	// Setup our WAL for the metagroup.
	sysAcc := s.SystemAccount()
	stateDir := path.Join(js.config.StoreDir, sysAcc.Name, defaultStoreDirName, defaultMetaGroupName)
	fs, bootstrap, err := newFileStore(
		FileStoreConfig{StoreDir: stateDir, BlockSize: defaultMetaFSBlkSize},
		StreamConfig{Name: defaultMetaGroupName, Storage: FileStorage},
	)
	if err != nil {
		s.Errorf("Error creating filestore: %v", err)
		return err
	}

	cfg := &RaftConfig{Name: defaultMetaGroupName, Store: stateDir, Log: fs}

	if bootstrap {
		s.Noticef("JetStream cluster bootstrapping")
		// FIXME(dlc) - Make this real.
		peers := s.activePeers()
		s.Debugf("JetStream cluster initial peers: %+v", peers)
		s.bootstrapRaftNode(cfg, peers, false)
	} else {
		s.Noticef("JetStream cluster recovering state")
	}
	// Start up our meta node.
	n, err := s.startRaftNode(cfg)
	if err != nil {
		s.Warnf("Could not start metadata controller: %v", err)
		return err
	}

	c := s.createInternalJetStreamClient()
	sacc := s.SystemAccount()

	js.mu.Lock()
	defer js.mu.Unlock()
	js.cluster = &jetStreamCluster{
		meta:    n,
		streams: make(map[string]map[string]*streamAssignment),
		s:       s,
		c:       c,
	}
	c.registerWithAccount(sacc)

	js.srv.startGoRoutine(js.monitorCluster)
	return nil
}

func (js *jetStream) getMetaGroup() RaftNode {
	js.mu.RLock()
	defer js.mu.RUnlock()
	if js.cluster == nil {
		return nil
	}
	return js.cluster.meta
}

func (js *jetStream) server() *Server {
	js.mu.RLock()
	s := js.srv
	js.mu.RUnlock()
	return s
}

// Will respond iff we are a member and we know we have no leader.
func (js *jetStream) isGroupLeaderless(rg *raftGroup) bool {
	if rg == nil {
		return false
	}
	js.mu.RLock()
	defer js.mu.RUnlock()

	cc := js.cluster

	// If we are not a member we can not say..
	if !rg.isMember(cc.meta.ID()) {
		return false
	}
	// Single peer groups always have a leader if we are here.
	if rg.node == nil {
		return false
	}
	return rg.node.GroupLeader() == _EMPTY_
}

func (s *Server) JetStreamIsStreamAssigned(account, stream string) bool {
	js, cc := s.getJetStreamCluster()
	if js == nil || cc == nil {
		return false
	}
	acc, _ := s.LookupAccount(account)
	if acc == nil {
		return false
	}
	return cc.isStreamAssigned(acc, stream)
}

// streamAssigned informs us if this server has this stream assigned.
func (jsa *jsAccount) streamAssigned(stream string) bool {
	jsa.mu.RLock()
	js, acc := jsa.js, jsa.account
	jsa.mu.RUnlock()

	if js == nil {
		return false
	}
	js.mu.RLock()
	assigned := js.cluster.isStreamAssigned(acc, stream)
	js.mu.RUnlock()
	return assigned
}

// Read lock should be held.
func (cc *jetStreamCluster) isStreamAssigned(a *Account, stream string) bool {
	// Non-clustered mode always return true.
	if cc == nil {
		return true
	}
	as := cc.streams[a.Name]
	if as == nil {
		return false
	}
	sa := as[stream]
	if sa == nil {
		return false
	}
	rg := sa.Group
	if rg == nil {
		return false
	}
	// Check if we are the leader of this raftGroup assigned to the stream.
	ourID := cc.meta.ID()
	for _, peer := range rg.Peers {
		if peer == ourID {
			return true
		}
	}
	return false
}

// Read lock should be held.
func (cc *jetStreamCluster) isStreamLeader(account, stream string) bool {
	// Non-clustered mode always return true.
	if cc == nil {
		return true
	}
	var sa *streamAssignment
	if as := cc.streams[account]; as != nil {
		sa = as[stream]
	}
	if sa == nil {
		return false
	}
	rg := sa.Group
	if rg == nil {
		return false
	}
	// Check if we are the leader of this raftGroup assigned to the stream.
	ourID := cc.meta.ID()
	for _, peer := range rg.Peers {
		if peer == ourID {
			if len(rg.Peers) == 1 || rg.node != nil && rg.node.Leader() {
				return true
			}
		}
	}
	return false
}

// Read lock should be held.
func (cc *jetStreamCluster) isConsumerLeader(account, stream, consumer string) bool {
	// Non-clustered mode always return true.
	if cc == nil {
		return true
	}
	var sa *streamAssignment
	if as := cc.streams[account]; as != nil {
		sa = as[stream]
	}
	if sa == nil {
		return false
	}
	// Check if we are the leader of this raftGroup assigned to this consumer.
	ourID := cc.meta.ID()
	for _, ca := range sa.consumers {
		rg := ca.Group
		for _, peer := range rg.Peers {
			if peer == ourID {
				if len(rg.Peers) == 1 || (rg.node != nil && rg.node.Leader()) {
					return true
				}
			}
		}
	}
	return false
}

func (js *jetStream) monitorCluster() {
	const (
		compactInterval  = 5 * time.Minute
		compactSizeLimit = 64 * 1024
	)

	s, cc, n := js.server(), js.cluster, js.getMetaGroup()
	qch, lch, ach := n.QuitC(), n.LeadChangeC(), n.ApplyC()

	defer s.grWG.Done()

	s.Debugf("Starting metadata monitor")
	defer s.Debugf("Exiting metadata monitor")

	t := time.NewTicker(compactInterval)
	defer t.Stop()

	isLeader := cc.isLeader()

	var lastSnap []byte
	var snapout bool

	// Only to be called from leader.
	attemptSnapshot := func() {
		if snapout {
			return
		}
		n.PausePropose()
		defer n.ResumePropose()
		if snap := js.metaSnapshot(); !bytes.Equal(lastSnap, snap) {
			if err := n.Snapshot(snap); err != nil {
			} else {
				lastSnap = snap
				snapout = true
			}
		}
	}

	isRecovering := true

	for {
		select {
		case <-s.quitCh:
			return
		case <-qch:
			return
		case ce := <-ach:
			if ce == nil {
				// Signals we have replayed all of our metadata.
				isRecovering = false
				s.Debugf("Recovered JetStream cluster metadata")
				continue
			}
			// FIXME(dlc) - Deal with errors.
			if hadSnapshot, err := js.applyMetaEntries(ce.Entries, isRecovering); err == nil {
				n.Applied(ce.Index)
				if hadSnapshot {
					snapout = false
				}
			}
			if isLeader && !snapout {
				_, b := n.Size()
				if b > compactSizeLimit {
					attemptSnapshot()
				}
			}
		case isLeader = <-lch:
			js.processLeaderChange(isLeader)
		case <-t.C:
			if isLeader && !snapout {
				attemptSnapshot()
			}
		}
	}
}

// Represents our stable meta state that we can write out.
type writeableStreamAssignment struct {
	Client    *ClientInfo   `json:"client,omitempty"`
	Created   time.Time     `json:"created"`
	Config    *StreamConfig `json:"stream"`
	Group     *raftGroup    `json:"group"`
	Sync      string        `json:"sync"`
	Consumers []*consumerAssignment
}

func (js *jetStream) metaSnapshot() []byte {
	var streams []writeableStreamAssignment
	js.mu.RLock()
	cc := js.cluster
	for _, asa := range cc.streams {
		for _, sa := range asa {
			wsa := writeableStreamAssignment{
				Client:  sa.Client,
				Created: sa.Created,
				Config:  sa.Config,
				Group:   sa.Group,
				Sync:    sa.Sync,
			}
			for _, ca := range sa.consumers {
				wsa.Consumers = append(wsa.Consumers, ca)
			}
			streams = append(streams, wsa)
		}
	}
	js.mu.RUnlock()

	if len(streams) == 0 {
		return nil
	}

	b, _ := json.Marshal(streams)
	return s2.EncodeBetter(nil, b)
}

func (js *jetStream) applyMetaSnapshot(buf []byte, isRecovering bool) error {
	jse, err := s2.Decode(nil, buf)
	if err != nil {
		return err
	}
	var wsas []writeableStreamAssignment
	if err = json.Unmarshal(jse, &wsas); err != nil {
		return err
	}
	// Build our new version here outside of js.
	streams := make(map[string]map[string]*streamAssignment)
	for _, wsa := range wsas {
		as := streams[wsa.Client.Account]
		if as == nil {
			as = make(map[string]*streamAssignment)
			streams[wsa.Client.Account] = as
		}
		sa := &streamAssignment{Client: wsa.Client, Created: wsa.Created, Config: wsa.Config, Group: wsa.Group, Sync: wsa.Sync}
		if len(wsa.Consumers) > 0 {
			sa.consumers = make(map[string]*consumerAssignment)
			for _, ca := range wsa.Consumers {
				sa.consumers[ca.Name] = ca
			}
		}
		as[wsa.Config.Name] = sa
	}

	js.mu.Lock()
	cc := js.cluster

	var saAdd, saDel, saChk []*streamAssignment
	// Walk through the old list to generate the delete list.
	for account, asa := range cc.streams {
		nasa := streams[account]
		for sn, sa := range asa {
			if nsa := nasa[sn]; nsa == nil {
				saDel = append(saDel, sa)
			} else {
				saChk = append(saChk, nsa)
			}
		}
	}
	// Walk through the new list to generate the add list.
	for account, nasa := range streams {
		asa := cc.streams[account]
		for sn, sa := range nasa {
			if asa[sn] == nil {
				saAdd = append(saAdd, sa)
			}
		}
	}
	// Now walk the ones to check and process consumers.
	var caAdd, caDel []*consumerAssignment
	for _, sa := range saChk {
		if osa := js.streamAssignment(sa.Client.Account, sa.Config.Name); osa != nil {
			for _, ca := range osa.consumers {
				if sa.consumers[ca.Name] == nil {
					caDel = append(caDel, ca)
				} else {
					caAdd = append(caAdd, ca)
				}
			}
		}
	}
	js.mu.Unlock()

	// Do removals first.
	for _, sa := range saDel {
		if isRecovering {
			js.setStreamAssignmentResponded(sa)
		}
		js.processStreamRemoval(sa)
	}
	// Now do add for the streams. Also add in all consumers.
	for _, sa := range saAdd {
		if isRecovering {
			js.setStreamAssignmentResponded(sa)
		}
		js.processStreamAssignment(sa)
		// We can simply add the consumers.
		for _, ca := range sa.consumers {
			if isRecovering {
				js.setConsumerAssignmentResponded(ca)
			}
			js.processConsumerAssignment(ca)
		}
	}
	// Now do the deltas for existing stream's consumers.
	for _, ca := range caDel {
		if isRecovering {
			js.setConsumerAssignmentResponded(ca)
		}
		js.processConsumerRemoval(ca)
	}
	for _, ca := range caAdd {
		if isRecovering {
			js.setConsumerAssignmentResponded(ca)
		}
		js.processConsumerAssignment(ca)
	}

	return nil
}

// Called on recovery to make sure we do not process like original
func (js *jetStream) setStreamAssignmentResponded(sa *streamAssignment) {
	js.mu.Lock()
	defer js.mu.Unlock()
	sa.responded = true
	sa.Restore = nil
}

// Called on recovery to make sure we do not process like original
func (js *jetStream) setConsumerAssignmentResponded(ca *consumerAssignment) {
	js.mu.Lock()
	defer js.mu.Unlock()
	ca.responded = true
}

func (js *jetStream) applyMetaEntries(entries []*Entry, isRecovering bool) (bool, error) {
	var didSnap bool
	for _, e := range entries {
		if e.Type == EntrySnapshot {
			js.applyMetaSnapshot(e.Data, isRecovering)
			didSnap = true
		} else {
			buf := e.Data
			switch entryOp(buf[0]) {
			case assignStreamOp:
				sa, err := decodeStreamAssignment(buf[1:])
				if err != nil {
					js.srv.Errorf("JetStream cluster failed to decode stream assignment: %q", buf[1:])
					return didSnap, err
				}
				if isRecovering {
					js.setStreamAssignmentResponded(sa)
				}
				js.processStreamAssignment(sa)
			case removeStreamOp:
				sa, err := decodeStreamAssignment(buf[1:])
				if err != nil {
					js.srv.Errorf("JetStream cluster failed to decode stream assignment: %q", buf[1:])
					return didSnap, err
				}
				if isRecovering {
					js.setStreamAssignmentResponded(sa)
				}
				js.processStreamRemoval(sa)
			case assignConsumerOp:
				ca, err := decodeConsumerAssignment(buf[1:])
				if err != nil {
					js.srv.Errorf("JetStream cluster failed to decode consumer assigment: %q", buf[1:])
					return didSnap, err
				}
				if isRecovering {
					js.setConsumerAssignmentResponded(ca)
				}
				js.processConsumerAssignment(ca)
			case assignCompressedConsumerOp:
				ca, err := decodeConsumerAssignmentCompressed(buf[1:])
				if err != nil {
					js.srv.Errorf("JetStream cluster failed to decode compressed consumer assigment: %q", buf[1:])
					return didSnap, err
				}
				if isRecovering {
					js.setConsumerAssignmentResponded(ca)
				}
				js.processConsumerAssignment(ca)
			case removeConsumerOp:
				ca, err := decodeConsumerAssignment(buf[1:])
				if err != nil {
					js.srv.Errorf("JetStream cluster failed to decode consumer assigment: %q", buf[1:])
					return didSnap, err
				}
				if isRecovering {
					js.setConsumerAssignmentResponded(ca)
				}
				js.processConsumerRemoval(ca)
			default:
				panic("JetStream Cluster Unknown meta entry op type")
			}
		}
	}
	return didSnap, nil
}

func (rg *raftGroup) isMember(id string) bool {
	if rg == nil {
		return false
	}
	for _, peer := range rg.Peers {
		if peer == id {
			return true
		}
	}
	return false
}

func (rg *raftGroup) setPreferred() {
	if rg == nil || len(rg.Peers) == 0 {
		return
	}
	if len(rg.Peers) == 1 {
		rg.Preferred = rg.Peers[0]
	} else {
		// For now just randomly select a peer for the preferred.
		pi := rand.Int31n(int32(len(rg.Peers)))
		rg.Preferred = rg.Peers[pi]
	}
}

// createRaftGroup is called to spin up this raft group if needed.
func (js *jetStream) createRaftGroup(rg *raftGroup) error {
	js.mu.Lock()
	defer js.mu.Unlock()

	s, cc := js.srv, js.cluster

	// If this is a single peer raft group or we are not a member return.
	if len(rg.Peers) <= 1 || !rg.isMember(cc.meta.ID()) {
		// Nothing to do here.
		return nil
	}

	// We already have this assigned.
	if node := s.lookupRaftNode(rg.Name); node != nil {
		s.Debugf("JetStream cluster already has raft group %q assigned", rg.Name)
		return nil
	}

	s.Debugf("JetStream cluster creating raft group:%+v", rg)

	sysAcc := s.SystemAccount()
	if sysAcc == nil {
		s.Debugf("JetStream cluster detected shutdown processing raft group: %+v", rg)
		return errors.New("shutting down")
	}

	stateDir := path.Join(js.config.StoreDir, sysAcc.Name, defaultStoreDirName, rg.Name)
	fs, bootstrap, err := newFileStore(
		FileStoreConfig{StoreDir: stateDir, BlockSize: 32 * 1024 * 1024},
		StreamConfig{Name: rg.Name, Storage: FileStorage},
	)
	if err != nil {
		s.Errorf("Error creating filestore: %v", err)
		return err
	}

	cfg := &RaftConfig{Name: rg.Name, Store: stateDir, Log: fs}

	if bootstrap {
		s.bootstrapRaftNode(cfg, rg.Peers, true)
	}
	n, err := s.startRaftNode(cfg)
	if err != nil {
		s.Debugf("Error creating raft group: %v", err)
		return err
	}
	rg.node = n

	// See if we are preferred and should start campaign immediately.
	if n.ID() == rg.Preferred {
		n.Campaign()
	}

	return nil
}

func (mset *Stream) raftNode() RaftNode {
	if mset == nil {
		return nil
	}
	mset.mu.RLock()
	defer mset.mu.RUnlock()
	return mset.node
}

// Monitor our stream node for this stream.
func (js *jetStream) monitorStream(mset *Stream, sa *streamAssignment) {
	s, cc, n := js.server(), js.cluster, sa.Group.node
	defer s.grWG.Done()

	if n == nil {
		s.Warnf("No RAFT group for '%s > %s", sa.Client.Account, sa.Config.Name)
		return
	}

	qch, lch, ach := n.QuitC(), n.LeadChangeC(), n.ApplyC()

	const (
		compactInterval  = 10 * time.Minute
		compactSizeLimit = 64 * 1024 * 1024
		compactMinWait   = 5 * time.Second
	)

	s.Debugf("Starting stream monitor for '%s > %s'", sa.Client.Account, sa.Config.Name)
	defer s.Debugf("Exiting stream monitor for '%s > %s'", sa.Client.Account, sa.Config.Name)

	t := time.NewTicker(compactInterval)
	defer t.Stop()

	js.mu.RLock()
	isLeader := cc.isStreamLeader(sa.Client.Account, sa.Config.Name)
	isRestore := sa.Restore != nil
	js.mu.RUnlock()

	acc, err := s.LookupAccount(sa.Client.Account)
	if err != nil {
		s.Warnf("Could not retrieve account for stream '%s > %s", sa.Client.Account, sa.Config.Name)
		return
	}

	var (
		lastSnap   []byte
		snapout    bool
		lastFailed time.Time
	)

	// Only to be called from leader.
	attemptSnapshot := func() {
		if mset == nil || isRestore || snapout {
			return
		}
		n.PausePropose()
		defer n.ResumePropose()
		if snap := mset.snapshot(); !bytes.Equal(lastSnap, snap) {
			if !lastFailed.IsZero() && time.Since(lastFailed) <= compactMinWait {
				s.Debugf("Stream compaction delayed")
				return
			}
			if err := n.Snapshot(snap); err != nil {
				lastFailed = time.Now()
			} else {
				lastSnap = snap
				snapout = true
				lastFailed = time.Time{}
			}
		}
	}

	// We will establish a restoreDoneCh no matter what. Will never be triggered unless
	// we replace with the restore chan.
	restoreDoneCh := make(<-chan error)

	for {
		select {
		case err := <-restoreDoneCh:
			// We have completed a restore from snapshot on this server. The stream assignment has
			// already been assigned but the replicas will need to catch up out of band. Consumers
			// will need to be assigned by forwarding the proposal and stamping the initial state.
			s.Debugf("Stream restore for '%s > %s' completed", sa.Client.Account, sa.Config.Name)
			if err != nil {
				s.Debugf("Stream restore failed: %v", err)
			}
			isRestore = false
			sa.Restore = nil
			// If we were successful lookup up our stream now.
			if err == nil {
				mset, err = acc.LookupStream(sa.Config.Name)
				if mset != nil {
					mset.setStreamAssignment(sa)
				}
			}
			if err != nil {
				if mset != nil {
					mset.Delete()
				}
				js.mu.Lock()
				sa.err = err
				sa.responded = true
				if n != nil {
					n.Delete()
				}
				result := &streamAssignmentResult{
					Account: sa.Client.Account,
					Stream:  sa.Config.Name,
					Restore: &JSApiStreamRestoreResponse{ApiResponse: ApiResponse{Type: JSApiStreamRestoreResponseType}},
				}
				result.Restore.Error = jsError(sa.err)
				js.mu.Unlock()
				// Send response to the metadata leader. They will forward to the user as needed.
				b, _ := json.Marshal(result) // Avoids auto-processing and doing fancy json with newlines.
				s.sendInternalMsgLocked(streamAssignmentSubj, _EMPTY_, nil, b)
				return
			}

			if !isLeader {
				panic("Finished restore but not leader")
			}
			js.processStreamLeaderChange(mset, sa, isLeader)
			attemptSnapshot()

			// Check to see if we have restored consumers here.
			// These are not currently assigned so we will need to do so here.
			if consumers := mset.Consumers(); len(consumers) > 0 {
				for _, o := range mset.Consumers() {
					rg := cc.createGroupForConsumer(sa)
					// Pick a preferred leader.
					rg.setPreferred()
					name, cfg := o.Name(), o.Config()
					// Place our initial state here as well for assignment distribution.
					ca := &consumerAssignment{
						Group:   rg,
						Stream:  sa.Config.Name,
						Name:    name,
						Config:  &cfg,
						Client:  sa.Client,
						Created: o.Created(),
						State:   o.readStoreState(),
					}

					// We make these compressed in case state is complex.
					addEntry := encodeAddConsumerAssignmentCompressed(ca)
					cc.meta.ForwardProposal(addEntry)

					// Check to make sure we see the assignment.
					go func() {
						ticker := time.NewTicker(time.Second)
						defer ticker.Stop()
						for range ticker.C {
							js.mu.RLock()
							ca, meta := js.consumerAssignment(ca.Client.Account, sa.Config.Name, name), cc.meta
							js.mu.RUnlock()
							if ca == nil {
								s.Warnf("Consumer assignment has not been assigned, retrying")
								meta.ForwardProposal(addEntry)
							} else {
								return
							}
						}
					}()
				}
			}
		case <-s.quitCh:
			return
		case <-qch:
			return
		case ce := <-ach:
			// No special processing needed for when we are caught up on restart.
			if ce == nil {
				continue
			}
			if mset == nil && isRestore {
				isRestore = false
				sa.Restore = nil
				if mset, err = acc.addStream(sa.Config, nil, sa); err != nil {
					s.Warnf("Could not add stream after restore '%s > %s': %v", sa.Client.Account, sa.Config.Name, err)
					return
				}
			}
			// Apply our entries.
			if hadSnapshot, err := js.applyStreamEntries(mset, ce); err == nil {
				n.Applied(ce.Index)
				if hadSnapshot {
					snapout = false
				}
			} else {
				s.Warnf("Error applying entries to '%s > %s'", sa.Client.Account, sa.Config.Name)
			}
			if isLeader && !snapout {
				if _, b := n.Size(); b > compactSizeLimit {
					attemptSnapshot()
				}
			}
		case isLeader = <-lch:
			if isLeader && isRestore {
				acc, _ := s.LookupAccount(sa.Client.Account)
				restoreDoneCh = s.processStreamRestore(sa.Client, acc, sa.Config.Name, _EMPTY_, sa.Reply, _EMPTY_)
			} else {
				if !isLeader && n.GroupLeader() != noLeader {
					js.setStreamAssignmentResponded(sa)
				}
				js.processStreamLeaderChange(mset, sa, isLeader)
			}
		case <-t.C:
			if isLeader {
				attemptSnapshot()
			}
		}
	}
}

func (js *jetStream) applyStreamEntries(mset *Stream, ce *CommittedEntry) (bool, error) {
	var didSnap bool
	for _, e := range ce.Entries {
		if e.Type == EntrySnapshot {
			mset.processSnapshot(e.Data)
			didSnap = true
		} else {
			buf := e.Data
			switch entryOp(buf[0]) {
			case streamMsgOp:
				subject, reply, hdr, msg, lseq, ts, err := decodeStreamMsg(buf[1:])
				if err != nil {
					panic(err.Error())
				}
				// Skip by hand here since first msg special case.
				// Reason is sequence is unsigned and for lseq being 0
				// the lseq under stream would have be -1.
				if lseq == 0 && mset.lastSeq() != 0 {
					continue
				}
				if err := mset.processJetStreamMsg(subject, reply, hdr, msg, lseq, ts); err != nil {
					js.srv.Debugf("Got error processing JetStream msg: %v", err)
				}
			case deleteMsgOp:
				md, err := decodeMsgDelete(buf[1:])
				if err != nil {
					panic(err.Error())
				}
				s, cc := js.server(), js.cluster
				removed, err := mset.EraseMsg(md.Seq)
				if err != nil {
					s.Warnf("JetStream cluster failed to delete msg %d from stream %q for account %q: %v", md.Seq, md.Stream, md.Client.Account, err)
				}
				js.mu.RLock()
				isLeader := cc.isStreamLeader(md.Client.Account, md.Stream)
				js.mu.RUnlock()
				if isLeader {
					var resp = JSApiMsgDeleteResponse{ApiResponse: ApiResponse{Type: JSApiMsgDeleteResponseType}}
					if err != nil {
						resp.Error = jsError(err)
						s.sendAPIErrResponse(md.Client, mset.account(), _EMPTY_, md.Reply, _EMPTY_, s.jsonResponse(resp))
					} else if !removed {
						resp.Error = &ApiError{Code: 400, Description: fmt.Sprintf("sequence [%d] not found", md.Seq)}
						s.sendAPIErrResponse(md.Client, mset.account(), _EMPTY_, md.Reply, _EMPTY_, s.jsonResponse(resp))
					} else {
						resp.Success = true
						s.sendAPIResponse(md.Client, mset.account(), _EMPTY_, md.Reply, _EMPTY_, s.jsonResponse(resp))
					}
				}
			case purgeStreamOp:
				sp, err := decodeStreamPurge(buf[1:])
				if err != nil {
					panic(err.Error())
				}
				s := js.server()
				purged, err := mset.Purge()
				if err != nil {
					s.Warnf("JetStream cluster failed to purge stream %q for account %q: %v", sp.Stream, sp.Client.Account, err)
				}
				js.mu.RLock()
				isLeader := js.cluster.isStreamLeader(sp.Client.Account, sp.Stream)
				js.mu.RUnlock()
				if isLeader {
					var resp = JSApiStreamPurgeResponse{ApiResponse: ApiResponse{Type: JSApiStreamPurgeResponseType}}
					if err != nil {
						resp.Error = jsError(err)
						s.sendAPIErrResponse(sp.Client, mset.account(), _EMPTY_, sp.Reply, _EMPTY_, s.jsonResponse(resp))
					} else {
						resp.Purged = purged
						resp.Success = true
						s.sendAPIResponse(sp.Client, mset.account(), _EMPTY_, sp.Reply, _EMPTY_, s.jsonResponse(resp))
					}
				}
			default:
				panic("JetStream Cluster Unknown group entry op type!")
			}
		}
	}
	return didSnap, nil
}

// Returns the PeerInfo for all replicas of a raft node. This is different than node.Peers()
// and is used for external facing advisories.
func (s *Server) replicas(node RaftNode) []*PeerInfo {
	now := time.Now()
	var replicas []*PeerInfo
	for _, rp := range node.Peers() {
		pi := &PeerInfo{Name: s.serverNameForNode(rp.ID), Current: rp.Current, Active: now.Sub(rp.Last)}
		replicas = append(replicas, pi)
	}
	return replicas
}

func (js *jetStream) processStreamLeaderChange(mset *Stream, sa *streamAssignment, isLeader bool) {
	js.mu.Lock()
	s, account, err := js.srv, sa.Client.Account, sa.err
	client, reply := sa.Client, sa.Reply
	hasResponded := sa.responded
	sa.responded = true
	js.mu.Unlock()

	stream := mset.Name()

	if isLeader {
		s.Noticef("JetStream cluster new stream leader for '%s > %s'", sa.Client.Account, stream)
		s.sendStreamLeaderElectAdvisory(mset)
	} else {
		// We are stepping down.
		// Make sure if we are doing so because we have lost quorum that we send the appropriate advisories.
		if node := mset.raftNode(); node != nil && !node.Quorum() {
			s.sendStreamLostQuorumAdvisory(mset)
		}
	}

	// Tell stream to switch leader status.
	mset.setLeader(isLeader)

	if !isLeader || hasResponded {
		return
	}

	acc, _ := s.LookupAccount(account)
	if acc == nil {
		return
	}

	// Send our response.
	var resp = JSApiStreamCreateResponse{ApiResponse: ApiResponse{Type: JSApiStreamCreateResponseType}}
	if err != nil {
		resp.Error = jsError(err)
		s.sendAPIErrResponse(client, acc, _EMPTY_, reply, _EMPTY_, s.jsonResponse(&resp))
	} else {
		resp.StreamInfo = &StreamInfo{Created: mset.Created(), State: mset.State(), Config: mset.Config(), Cluster: s.clusterInfo(mset.raftNode())}
		s.sendAPIResponse(client, acc, _EMPTY_, reply, _EMPTY_, s.jsonResponse(&resp))
		if node := mset.raftNode(); node != nil {
			mset.sendCreateAdvisory()
		}
	}
}

// Fixed value ok for now.
const lostQuorumAdvInterval = 10 * time.Second

// Determines if we should send lost quorum advisory. We throttle these after first one.
func (mset *Stream) shouldSendLostQuorum() bool {
	mset.mu.Lock()
	defer mset.mu.Unlock()
	if time.Since(mset.lqsent) >= lostQuorumAdvInterval {
		mset.lqsent = time.Now()
		return true
	}
	return false
}

func (s *Server) sendStreamLostQuorumAdvisory(mset *Stream) {
	if mset == nil {
		return
	}
	node, stream, acc := mset.raftNode(), mset.Name(), mset.account()
	if node == nil {
		return
	}
	if !mset.shouldSendLostQuorum() {
		return
	}

	s.Warnf("JetStream cluster stream '%s > %s' has NO quorum, stalled.", acc.GetName(), stream)

	subj := JSAdvisoryStreamQuorumLostPre + "." + stream
	adv := &JSStreamQuorumLostAdvisory{
		TypedEvent: TypedEvent{
			Type: JSStreamQuorumLostAdvisoryType,
			ID:   nuid.Next(),
			Time: time.Now().UTC(),
		},
		Stream:   stream,
		Replicas: s.replicas(node),
	}

	// Send to the user's account if not the system account.
	if acc != s.SystemAccount() {
		s.publishAdvisory(acc, subj, adv)
	}
	// Now do system level one. Place account info in adv, and nil account means system.
	adv.Account = acc.GetName()
	s.publishAdvisory(nil, subj, adv)
}

func (s *Server) sendStreamLeaderElectAdvisory(mset *Stream) {
	if mset == nil {
		return
	}
	node, stream, acc := mset.raftNode(), mset.Name(), mset.account()
	if node == nil {
		return
	}
	subj := JSAdvisoryStreamLeaderElectedPre + "." + stream
	adv := &JSStreamLeaderElectedAdvisory{
		TypedEvent: TypedEvent{
			Type: JSStreamLeaderElectedAdvisoryType,
			ID:   nuid.Next(),
			Time: time.Now().UTC(),
		},
		Stream:   stream,
		Leader:   s.serverNameForNode(node.GroupLeader()),
		Replicas: s.replicas(node),
	}

	// Send to the user's account if not the system account.
	if acc != s.SystemAccount() {
		s.publishAdvisory(acc, subj, adv)
	}
	// Now do system level one. Place account info in adv, and nil account means system.
	adv.Account = acc.GetName()
	s.publishAdvisory(nil, subj, adv)
}

// Will lookup a stream assignment.
// Lock should be held.
func (js *jetStream) streamAssignment(account, stream string) (sa *streamAssignment) {
	cc := js.cluster
	if cc == nil {
		return nil
	}

	if as := cc.streams[account]; as != nil {
		sa = as[stream]
	}
	return sa
}

// processStreamAssignment is called when followers have replicated an assignment.
func (js *jetStream) processStreamAssignment(sa *streamAssignment) {
	js.mu.RLock()
	s, cc := js.srv, js.cluster
	js.mu.RUnlock()
	if s == nil || cc == nil {
		// TODO(dlc) - debug at least
		return
	}

	acc, err := s.LookupAccount(sa.Client.Account)
	if err != nil {
		// TODO(dlc) - log error
		return
	}
	stream := sa.Config.Name

	js.mu.Lock()
	// Check if we already have this assigned.
	accStreams := cc.streams[acc.Name]
	if accStreams != nil && accStreams[stream] != nil {
		// TODO(dlc) - Debug?
		// We already have this assignment, should we check they are the same?
		js.mu.Unlock()
		return
	}
	if accStreams == nil {
		accStreams = make(map[string]*streamAssignment)
	}
	// Update our state.
	accStreams[stream] = sa
	cc.streams[acc.Name] = accStreams

	var isMember bool
	if sa.Group != nil && cc.meta != nil {
		isMember = sa.Group.isMember(cc.meta.ID())
	}
	js.mu.Unlock()

	// Check if this is for us..
	if isMember {
		js.processClusterCreateStream(acc, sa)
	}
}

// processClusterCreateStream is called when we have a stream assignment that
// has been committed and this server is a member of the peer group.
func (js *jetStream) processClusterCreateStream(acc *Account, sa *streamAssignment) {
	if sa == nil {
		return
	}

	js.mu.RLock()
	s, rg := js.srv, sa.Group
	js.mu.RUnlock()

	// Process the raft group and make sure it's running if needed.
	err := js.createRaftGroup(rg)

	// If we are restoring, create the stream if we are R>1 and not the preferred who handles the
	// receipt of the snapshot itself.
	shouldCreate := true
	if sa.Restore != nil {
		if len(rg.Peers) == 1 || rg.node != nil && rg.node.ID() == rg.Preferred {
			shouldCreate = false
		}
	}

	var mset *Stream

	// Process here if not restoring or not the leader.
	if shouldCreate && err == nil {
		// Go ahead and create or update the stream.
		mset, err = acc.LookupStream(sa.Config.Name)
		if err == nil && mset != nil {
			mset.setStreamAssignment(sa)
			if err := mset.Update(sa.Config); err != nil {
				s.Warnf("JetStream cluster error updating stream %q for account %q: %v", sa.Config.Name, acc.Name, err)
			}
		} else if err == ErrJetStreamStreamNotFound {
			// Add in the stream here.
			mset, err = acc.addStream(sa.Config, nil, sa)
		}
		if mset != nil {
			mset.setCreated(sa.Created)
		}
	}

	// This is an error condition.
	if err != nil {
		s.Debugf("Stream create failed for '%s > %s': %v", sa.Client.Account, sa.Config.Name, err)
		js.mu.Lock()
		sa.err = err
		sa.responded = true
		if rg.node != nil {
			rg.node.Delete()
		}
		result := &streamAssignmentResult{
			Account:  sa.Client.Account,
			Stream:   sa.Config.Name,
			Response: &JSApiStreamCreateResponse{ApiResponse: ApiResponse{Type: JSApiStreamCreateResponseType}},
		}
		result.Response.Error = jsError(err)
		js.mu.Unlock()

		// Send response to the metadata leader. They will forward to the user as needed.
		b, _ := json.Marshal(result) // Avoids auto-processing and doing fancy json with newlines.
		s.sendInternalMsgLocked(streamAssignmentSubj, _EMPTY_, nil, b)
		return
	}

	// Start our monitoring routine.
	if rg.node != nil {
		s.startGoRoutine(func() { js.monitorStream(mset, sa) })
	} else {
		// Single replica stream, process manually here.
		// If we are restoring, process that first.
		if sa.Restore != nil {
			// We are restoring a stream here.
			restoreDoneCh := s.processStreamRestore(sa.Client, acc, sa.Config.Name, _EMPTY_, sa.Reply, _EMPTY_)
			s.startGoRoutine(func() {
				defer s.grWG.Done()
				select {
				case err := <-restoreDoneCh:
					if err == nil {
						mset, err = acc.LookupStream(sa.Config.Name)
						if mset != nil {
							mset.setStreamAssignment(sa)
							mset.setCreated(sa.Created)
						}
					}
					if err != nil {
						if mset != nil {
							mset.Delete()
						}
						js.mu.Lock()
						sa.err = err
						sa.responded = true
						result := &streamAssignmentResult{
							Account: sa.Client.Account,
							Stream:  sa.Config.Name,
							Restore: &JSApiStreamRestoreResponse{ApiResponse: ApiResponse{Type: JSApiStreamRestoreResponseType}},
						}
						result.Restore.Error = jsError(sa.err)
						js.mu.Unlock()
						// Send response to the metadata leader. They will forward to the user as needed.
						b, _ := json.Marshal(result) // Avoids auto-processing and doing fancy json with newlines.
						s.sendInternalMsgLocked(streamAssignmentSubj, _EMPTY_, nil, b)
						return
					}
					js.processStreamLeaderChange(mset, sa, true)

					// Check to see if we have restored consumers here.
					// These are not currently assigned so we will need to do so here.
					if consumers := mset.Consumers(); len(consumers) > 0 {
						js.mu.RLock()
						cc := js.cluster
						js.mu.RUnlock()

						for _, o := range mset.Consumers() {
							rg := cc.createGroupForConsumer(sa)
							name, cfg := o.Name(), o.Config()
							// Place our initial state here as well for assignment distribution.
							ca := &consumerAssignment{
								Group:   rg,
								Stream:  sa.Config.Name,
								Name:    name,
								Config:  &cfg,
								Client:  sa.Client,
								Created: o.Created(),
							}

							addEntry := encodeAddConsumerAssignment(ca)
							cc.meta.ForwardProposal(addEntry)

							// Check to make sure we see the assignment.
							go func() {
								ticker := time.NewTicker(time.Second)
								defer ticker.Stop()
								for range ticker.C {
									js.mu.RLock()
									ca, meta := js.consumerAssignment(ca.Client.Account, sa.Config.Name, name), cc.meta
									js.mu.RUnlock()
									if ca == nil {
										s.Warnf("Consumer assignment has not been assigned, retrying")
										meta.ForwardProposal(addEntry)
									} else {
										return
									}
								}
							}()
						}
					}

				case <-s.quitCh:
					return
				}
			})
		} else {
			js.processStreamLeaderChange(mset, sa, true)
		}
	}
}

// processStreamRemoval is called when followers have replicated an assignment.
func (js *jetStream) processStreamRemoval(sa *streamAssignment) {
	js.mu.Lock()
	s, cc := js.srv, js.cluster
	if s == nil || cc == nil {
		// TODO(dlc) - debug at least
		js.mu.Unlock()
		return
	}
	stream := sa.Config.Name
	isMember := sa.Group.isMember(cc.meta.ID())
	wasLeader := cc.isStreamLeader(sa.Client.Account, stream)

	// Check if we already have this assigned.
	accStreams := cc.streams[sa.Client.Account]
	needDelete := accStreams != nil && accStreams[stream] != nil
	if needDelete {
		delete(accStreams, stream)
		if len(accStreams) == 0 {
			delete(cc.streams, sa.Client.Account)
		}
	}
	js.mu.Unlock()

	if needDelete {
		js.processClusterDeleteStream(sa, isMember, wasLeader)
	}
}

func (js *jetStream) processClusterDeleteStream(sa *streamAssignment, isMember, wasLeader bool) {
	if sa == nil {
		return
	}
	js.mu.RLock()
	s := js.srv
	js.mu.RUnlock()

	acc, err := s.LookupAccount(sa.Client.Account)
	if err != nil {
		s.Debugf("JetStream cluster failed to lookup account %q: %v", sa.Client.Account, err)
		return
	}

	var resp = JSApiStreamDeleteResponse{ApiResponse: ApiResponse{Type: JSApiStreamDeleteResponseType}}

	// Go ahead and delete the stream.
	mset, err := acc.LookupStream(sa.Config.Name)
	if err != nil {
		resp.Error = jsNotFoundError(err)
	} else if mset != nil {
		if mset.Config().internal {
			err = errors.New("not allowed to delete internal stream")
		} else {
			err = mset.stop(true, wasLeader)
		}
	}

	if sa.Group.node != nil {
		sa.Group.node.Delete()
	}

	if !isMember || !wasLeader && sa.Group.node != nil && sa.Group.node.GroupLeader() != noLeader {
		return
	}

	if err != nil {
		if resp.Error == nil {
			resp.Error = jsError(err)
		}
		s.sendAPIErrResponse(sa.Client, acc, _EMPTY_, sa.Reply, _EMPTY_, s.jsonResponse(resp))
	} else {
		resp.Success = true
		s.sendAPIResponse(sa.Client, acc, _EMPTY_, sa.Reply, _EMPTY_, s.jsonResponse(resp))
	}
}

// processConsumerAssignment is called when followers have replicated an assignment for a consumer.
func (js *jetStream) processConsumerAssignment(ca *consumerAssignment) {
	js.mu.Lock()
	s, cc := js.srv, js.cluster
	if s == nil || cc == nil {
		// TODO(dlc) - debug at least
		js.mu.Unlock()
		return
	}

	sa := js.streamAssignment(ca.Client.Account, ca.Stream)
	if sa == nil {
		s.Debugf("Consumer create failed, could not locate stream '%s > %s'", ca.Client.Account, ca.Stream)
		ca.err = ErrJetStreamStreamNotFound
		result := &consumerAssignmentResult{
			Account:  ca.Client.Account,
			Stream:   ca.Stream,
			Consumer: ca.Name,
			Response: &JSApiConsumerCreateResponse{ApiResponse: ApiResponse{Type: JSApiConsumerCreateResponseType}},
		}
		result.Response.Error = jsNotFoundError(ErrJetStreamStreamNotFound)
		// Send response to the metadata leader. They will forward to the user as needed.
		b, _ := json.Marshal(result) // Avoids auto-processing and doing fancy json with newlines.
		s.sendInternalMsgLocked(consumerAssignmentSubj, _EMPTY_, nil, b)
		js.mu.Unlock()
		return
	}

	if sa.consumers == nil {
		sa.consumers = make(map[string]*consumerAssignment)
	}

	// Place into our internal map under the stream assignment.
	// Ok to replace an existing one, we check on process call below.
	sa.consumers[ca.Name] = ca
	// See if we are a member
	isMember := ca.Group.isMember(cc.meta.ID())
	js.mu.Unlock()

	// Check if this is for us..
	if isMember {
		js.processClusterCreateConsumer(ca)
	}
}

func (js *jetStream) processConsumerRemoval(ca *consumerAssignment) {
	js.mu.Lock()
	s, cc := js.srv, js.cluster
	if s == nil || cc == nil {
		// TODO(dlc) - debug at least
		js.mu.Unlock()
		return
	}
	isMember := ca.Group.isMember(cc.meta.ID())
	wasLeader := cc.isConsumerLeader(ca.Client.Account, ca.Stream, ca.Name)

	// Delete from our state.
	var needDelete bool
	if accStreams := cc.streams[ca.Client.Account]; accStreams != nil {
		if sa := accStreams[ca.Stream]; sa != nil && sa.consumers != nil && sa.consumers[ca.Name] != nil {
			needDelete = true
			delete(sa.consumers, ca.Name)
		}
	}
	js.mu.Unlock()

	if needDelete {
		js.processClusterDeleteConsumer(ca, isMember, wasLeader)
	}
}

type consumerAssignmentResult struct {
	Account  string                       `json:"account"`
	Stream   string                       `json:"stream"`
	Consumer string                       `json:"consumer"`
	Response *JSApiConsumerCreateResponse `json:"response,omitempty"`
}

// processClusterCreateConsumer is when we are a member fo the group and need to create the consumer.
func (js *jetStream) processClusterCreateConsumer(ca *consumerAssignment) {
	if ca == nil {
		return
	}
	js.mu.RLock()
	s := js.srv
	acc, err := s.LookupAccount(ca.Client.Account)
	if err != nil {
		s.Warnf("JetStream cluster failed to lookup account %q: %v", ca.Client.Account, err)
		js.mu.RUnlock()
		return
	}
	rg := ca.Group
	js.mu.RUnlock()

	// Go ahead and create or update the consumer.
	mset, err := acc.LookupStream(ca.Stream)
	if err != nil {
		js.mu.Lock()
		s.Debugf("Consumer create failed, could not locate stream '%s > %s'", ca.Client.Account, ca.Stream)
		ca.err = ErrJetStreamStreamNotFound
		result := &consumerAssignmentResult{
			Account:  ca.Client.Account,
			Stream:   ca.Stream,
			Consumer: ca.Name,
			Response: &JSApiConsumerCreateResponse{ApiResponse: ApiResponse{Type: JSApiConsumerCreateResponseType}},
		}
		result.Response.Error = jsNotFoundError(ErrJetStreamStreamNotFound)
		// Send response to the metadata leader. They will forward to the user as needed.
		b, _ := json.Marshal(result) // Avoids auto-processing and doing fancy json with newlines.
		s.sendInternalMsgLocked(consumerAssignmentSubj, _EMPTY_, nil, b)
		js.mu.Unlock()
		return
	}

	// Process the raft group and make sure its running if needed.
	js.createRaftGroup(rg)

	// Check if we already have this consumer running.
	o := mset.LookupConsumer(ca.Name)
	if o != nil {
		if o.isDurable() && o.isPushMode() {
			ocfg := o.Config()
			if configsEqualSansDelivery(ocfg, *ca.Config) && (ocfg.allowNoInterest || o.hasNoLocalInterest()) {
				o.updateDeliverSubject(ca.Config.DeliverSubject)
			}
		}
		o.setConsumerAssignment(ca)
		s.Debugf("JetStream cluster, consumer was already running")
	}

	// Add in the consumer if needed.
	if o == nil {
		o, err = mset.addConsumer(ca.Config, ca.Name, ca)
	}

	// If we have an initial state set apply that now.
	if ca.State != nil && o != nil {
		err = o.setStoreState(ca.State)
	}

	if err != nil {
		js.srv.Debugf("Consumer create failed for '%s > %s > %s': %v\n", ca.Client.Account, ca.Stream, ca.Name, err)
		ca.err = err
		if rg.node != nil {
			rg.node.Delete()
		}
		result := &consumerAssignmentResult{
			Account:  ca.Client.Account,
			Stream:   ca.Stream,
			Consumer: ca.Name,
			Response: &JSApiConsumerCreateResponse{ApiResponse: ApiResponse{Type: JSApiConsumerCreateResponseType}},
		}
		result.Response.Error = jsError(err)

		// Send response to the metadata leader. They will forward to the user as needed.
		b, _ := json.Marshal(result) // Avoids auto-processing and doing fancy json with newlines.
		s.sendInternalMsgLocked(consumerAssignmentSubj, _EMPTY_, nil, b)
	} else {
		o.setCreated(ca.Created)
		// Start our monitoring routine.
		if rg.node != nil {
			s.startGoRoutine(func() { js.monitorConsumer(o, ca) })
		} else {
			// Single replica consumer, process manually here.
			js.processConsumerLeaderChange(o, ca, true)
		}
	}
}

func (js *jetStream) processClusterDeleteConsumer(ca *consumerAssignment, isMember, wasLeader bool) {
	if ca == nil {
		return
	}
	js.mu.RLock()
	s := js.srv
	js.mu.RUnlock()

	acc, err := s.LookupAccount(ca.Client.Account)
	if err != nil {
		s.Warnf("JetStream cluster failed to lookup account %q: %v", ca.Client.Account, err)
		return
	}

	var resp = JSApiConsumerDeleteResponse{ApiResponse: ApiResponse{Type: JSApiConsumerDeleteResponseType}}

	// Go ahead and delete the consumer.
	mset, err := acc.LookupStream(ca.Stream)
	if err != nil {
		resp.Error = jsNotFoundError(err)
	} else if mset != nil {
		if mset.Config().internal {
			err = errors.New("not allowed to delete internal consumer")
		} else if o := mset.LookupConsumer(ca.Name); o != nil {
			err = o.stop(true, true, wasLeader)
		} else {
			resp.Error = jsNoConsumerErr
		}
	}

	if ca.Group.node != nil {
		ca.Group.node.Delete()
	}

	if !wasLeader || ca.Reply == _EMPTY_ {
		return
	}

	if err != nil {
		if resp.Error == nil {
			resp.Error = jsError(err)
		}
		s.sendAPIErrResponse(ca.Client, acc, _EMPTY_, ca.Reply, _EMPTY_, s.jsonResponse(resp))
	} else {
		resp.Success = true
		s.sendAPIResponse(ca.Client, acc, _EMPTY_, ca.Reply, _EMPTY_, s.jsonResponse(resp))
	}
}

// Returns the consumer assignment, or nil if not present.
// Lock should be held.
func (js *jetStream) consumerAssignment(account, stream, consumer string) *consumerAssignment {
	if sa := js.streamAssignment(account, stream); sa != nil {
		return sa.consumers[consumer]
	}
	return nil
}

// consumerAssigned informs us if this server has this consumer assigned.
func (jsa *jsAccount) consumerAssigned(stream, consumer string) bool {
	jsa.mu.RLock()
	defer jsa.mu.RUnlock()

	js, acc := jsa.js, jsa.account
	if js == nil {
		return false
	}
	return js.cluster.isConsumerAssigned(acc, stream, consumer)
}

// Read lock should be held.
func (cc *jetStreamCluster) isConsumerAssigned(a *Account, stream, consumer string) bool {
	// Non-clustered mode always return true.
	if cc == nil {
		return true
	}
	var sa *streamAssignment
	accStreams := cc.streams[a.Name]
	if accStreams != nil {
		sa = accStreams[stream]
	}
	if sa == nil {
		// TODO(dlc) - This should not happen.
		return false
	}
	ca := sa.consumers[consumer]
	if ca == nil {
		return false
	}
	rg := ca.Group
	// Check if we are the leader of this raftGroup assigned to the stream.
	ourID := cc.meta.ID()
	for _, peer := range rg.Peers {
		if peer == ourID {
			return true
		}
	}
	return false
}

func (o *Consumer) raftNode() RaftNode {
	if o == nil {
		return nil
	}
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.node
}

func (js *jetStream) monitorConsumer(o *Consumer, ca *consumerAssignment) {
	s, n := js.server(), o.raftNode()
	defer s.grWG.Done()

	if n == nil {
		s.Warnf("No RAFT group for consumer")
		return
	}

	qch, lch, ach := n.QuitC(), n.LeadChangeC(), n.ApplyC()

	const (
		compactInterval  = 1 * time.Minute
		compactSizeLimit = 8 * 1024 * 1024
	)

	s.Debugf("Starting consumer monitor for '%s > %s > %s", o.acc.Name, ca.Stream, ca.Name)
	defer s.Debugf("Exiting consumer monitor for '%s > %s > %s'", o.acc.Name, ca.Stream, ca.Name)

	t := time.NewTicker(compactInterval)
	defer t.Stop()

	// Our last applied.
	last := uint64(0)

	for {
		select {
		case <-s.quitCh:
			return
		case <-qch:
			return
		case ce := <-ach:
			// No special processing needed for when we are caught up on restart.
			if ce == nil {
				continue
			}
			if _, err := js.applyConsumerEntries(o, ce); err == nil {
				n.Applied(ce.Index)
				last = ce.Index
				if _, b := n.Size(); b > compactSizeLimit {
					n.Compact(last)
				}
			}
		case isLeader := <-lch:
			if !isLeader && n.GroupLeader() != noLeader {
				js.setConsumerAssignmentResponded(ca)
			}
			js.processConsumerLeaderChange(o, ca, isLeader)
		case <-t.C:
			// TODO(dlc) - We should have this delayed a bit to not race the invariants.
			if last != 0 {
				n.Compact(last)
			}
		}
	}
}

func (js *jetStream) applyConsumerEntries(o *Consumer, ce *CommittedEntry) (bool, error) {
	var didSnap bool
	for _, e := range ce.Entries {
		if e.Type == EntrySnapshot {
			// No-op needed?
			state, err := decodeConsumerState(e.Data)
			if err != nil {
				panic(err.Error())
			}
			o.store.Update(state)
			didSnap = true
		} else {
			buf := e.Data
			switch entryOp(buf[0]) {
			case updateDeliveredOp:
				dseq, sseq, dc, ts, err := decodeDeliveredUpdate(buf[1:])
				if err != nil {
					panic(err.Error())
				}
				if err := o.store.UpdateDelivered(dseq, sseq, dc, ts); err != nil {
					panic(err.Error())
				}
			case updateAcksOp:
				dseq, sseq, err := decodeAckUpdate(buf[1:])
				if err != nil {
					panic(err.Error())
				}
				o.store.UpdateAcks(dseq, sseq)
			default:
				panic("JetStream Cluster Unknown group entry op type!")
			}
		}
	}
	return didSnap, nil
}

var errBadAckUpdate = errors.New("jetstream cluster bad replicated ack update")
var errBadDeliveredUpdate = errors.New("jetstream cluster bad replicated delivered update")

func decodeAckUpdate(buf []byte) (dseq, sseq uint64, err error) {
	var bi, n int
	if dseq, n = binary.Uvarint(buf); n < 0 {
		return 0, 0, errBadAckUpdate
	}
	bi += n
	if sseq, n = binary.Uvarint(buf[bi:]); n < 0 {
		return 0, 0, errBadAckUpdate
	}
	return dseq, sseq, nil
}

func decodeDeliveredUpdate(buf []byte) (dseq, sseq, dc uint64, ts int64, err error) {
	var bi, n int
	if dseq, n = binary.Uvarint(buf); n < 0 {
		return 0, 0, 0, 0, errBadDeliveredUpdate
	}
	bi += n
	if sseq, n = binary.Uvarint(buf[bi:]); n < 0 {
		return 0, 0, 0, 0, errBadDeliveredUpdate
	}
	bi += n
	if dc, n = binary.Uvarint(buf[bi:]); n < 0 {
		return 0, 0, 0, 0, errBadDeliveredUpdate
	}
	bi += n
	if ts, n = binary.Varint(buf[bi:]); n < 0 {
		return 0, 0, 0, 0, errBadDeliveredUpdate
	}
	return dseq, sseq, dc, ts, nil
}

func (js *jetStream) processConsumerLeaderChange(o *Consumer, ca *consumerAssignment, isLeader bool) {
	js.mu.Lock()
	s, account, err := js.srv, ca.Client.Account, ca.err
	client, reply := ca.Client, ca.Reply
	hasResponded := ca.responded
	ca.responded = true
	js.mu.Unlock()

	stream := o.Stream()
	consumer := o.Name()
	acc, _ := s.LookupAccount(account)
	if acc == nil {
		return
	}

	if isLeader {
		s.Noticef("JetStream cluster new consumer leader for '%s > %s > %s'", ca.Client.Account, stream, consumer)
		s.sendConsumerLeaderElectAdvisory(o)
	} else {
		// We are stepping down.
		// Make sure if we are doing so because we have lost quorum that we send the appropriate advisories.
		if node := o.raftNode(); node != nil && !node.Quorum() {
			s.sendConsumerLostQuorumAdvisory(o)
		}
	}

	// Tell consumer to switch leader status.
	o.setLeader(isLeader)

	if !isLeader || hasResponded {
		return
	}

	var resp = JSApiConsumerCreateResponse{ApiResponse: ApiResponse{Type: JSApiConsumerCreateResponseType}}
	if err != nil {
		resp.Error = jsError(err)
		s.sendAPIErrResponse(client, acc, _EMPTY_, reply, _EMPTY_, s.jsonResponse(&resp))
	} else {
		resp.ConsumerInfo = o.Info()
		s.sendAPIResponse(client, acc, _EMPTY_, reply, _EMPTY_, s.jsonResponse(&resp))
		if node := o.raftNode(); node != nil {
			o.sendCreateAdvisory()
		}
	}
}

// Determines if we should send lost quorum advisory. We throttle these after first one.
func (o *Consumer) shouldSendLostQuorum() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if time.Since(o.lqsent) >= lostQuorumAdvInterval {
		o.lqsent = time.Now()
		return true
	}
	return false
}

func (s *Server) sendConsumerLostQuorumAdvisory(o *Consumer) {
	if o == nil {
		return
	}
	node, stream, consumer, acc := o.raftNode(), o.Stream(), o.Name(), o.account()
	if node == nil {
		return
	}
	if !o.shouldSendLostQuorum() {
		return
	}

	s.Warnf("JetStream cluster consumer '%s > %s >%s' has NO quorum, stalled.", acc.GetName(), stream, consumer)

	subj := JSAdvisoryConsumerQuorumLostPre + "." + stream + "." + consumer
	adv := &JSConsumerQuorumLostAdvisory{
		TypedEvent: TypedEvent{
			Type: JSConsumerQuorumLostAdvisoryType,
			ID:   nuid.Next(),
			Time: time.Now().UTC(),
		},
		Stream:   stream,
		Consumer: consumer,
		Replicas: s.replicas(node),
	}

	// Send to the user's account if not the system account.
	if acc != s.SystemAccount() {
		s.publishAdvisory(acc, subj, adv)
	}
	// Now do system level one. Place account info in adv, and nil account means system.
	adv.Account = acc.GetName()
	s.publishAdvisory(nil, subj, adv)
}

func (s *Server) sendConsumerLeaderElectAdvisory(o *Consumer) {
	if o == nil {
		return
	}
	node, stream, consumer, acc := o.raftNode(), o.Stream(), o.Name(), o.account()
	if node == nil {
		return
	}

	subj := JSAdvisoryConsumerLeaderElectedPre + "." + stream + "." + consumer
	adv := &JSConsumerLeaderElectedAdvisory{
		TypedEvent: TypedEvent{
			Type: JSConsumerLeaderElectedAdvisoryType,
			ID:   nuid.Next(),
			Time: time.Now().UTC(),
		},
		Stream:   stream,
		Consumer: consumer,
		Leader:   s.serverNameForNode(node.GroupLeader()),
		Replicas: s.replicas(node),
	}

	// Send to the user's account if not the system account.
	if acc != s.SystemAccount() {
		s.publishAdvisory(acc, subj, adv)
	}
	// Now do system level one. Place account info in adv, and nil account means system.
	adv.Account = acc.GetName()
	s.publishAdvisory(nil, subj, adv)
}

type streamAssignmentResult struct {
	Account  string                      `json:"account"`
	Stream   string                      `json:"stream"`
	Response *JSApiStreamCreateResponse  `json:"create_response,omitempty"`
	Restore  *JSApiStreamRestoreResponse `json:"restore_response,omitempty"`
}

// Process error results of stream and consumer assignments.
// Success will be handled by stream leader.
func (js *jetStream) processStreamAssignmentResults(sub *subscription, c *client, subject, reply string, msg []byte) {
	var result streamAssignmentResult
	if err := json.Unmarshal(msg, &result); err != nil {
		// TODO(dlc) - log
		return
	}
	acc, _ := js.srv.LookupAccount(result.Account)
	if acc == nil {
		// TODO(dlc) - log
		return
	}

	js.mu.Lock()
	defer js.mu.Unlock()

	s, cc := js.srv, js.cluster

	// FIXME(dlc) - suppress duplicates?
	if sa := js.streamAssignment(result.Account, result.Stream); sa != nil {
		var resp string
		if result.Response != nil {
			resp = s.jsonResponse(result.Response)
		} else if result.Restore != nil {
			resp = s.jsonResponse(result.Restore)
		}
		js.srv.sendAPIErrResponse(sa.Client, acc, _EMPTY_, sa.Reply, _EMPTY_, resp)
		sa.responded = true
		// TODO(dlc) - Could have mixed results, should track per peer.
		// Set sa.err while we are deleting so we will not respond to list/names requests.
		sa.err = ErrJetStreamNotAssigned
		cc.meta.Propose(encodeDeleteStreamAssignment(sa))
	}
}

func (js *jetStream) processConsumerAssignmentResults(sub *subscription, c *client, subject, reply string, msg []byte) {
	var result consumerAssignmentResult
	if err := json.Unmarshal(msg, &result); err != nil {
		// TODO(dlc) - log
		return
	}
	acc, _ := js.srv.LookupAccount(result.Account)
	if acc == nil {
		// TODO(dlc) - log
		return
	}

	js.mu.Lock()
	defer js.mu.Unlock()

	s, cc := js.srv, js.cluster

	if sa := js.streamAssignment(result.Account, result.Stream); sa != nil && sa.consumers != nil {
		if ca := sa.consumers[result.Consumer]; ca != nil && !ca.responded {
			js.srv.sendAPIErrResponse(ca.Client, acc, _EMPTY_, ca.Reply, _EMPTY_, s.jsonResponse(result.Response))
			ca.responded = true
			// Check if this failed.
			// TODO(dlc) - Could have mixed results, should track per peer.
			if result.Response.Error != nil {
				// So while we are delting we will not respond to list/names requests.
				ca.err = ErrJetStreamNotAssigned
				cc.meta.Propose(encodeDeleteConsumerAssignment(ca))
			}
		}
	}
}

const (
	streamAssignmentSubj   = "$SYS.JSC.STREAM.ASSIGNMENT.RESULT"
	consumerAssignmentSubj = "$SYS.JSC.CONSUMER.ASSIGNMENT.RESULT"
)

// Lock should be held.
func (js *jetStream) startUpdatesSub() {
	cc, s, c := js.cluster, js.srv, js.cluster.c
	if cc.streamResults == nil {
		cc.streamResults, _ = s.systemSubscribe(streamAssignmentSubj, _EMPTY_, false, c, js.processStreamAssignmentResults)
	}
	if cc.consumerResults == nil {
		cc.consumerResults, _ = s.systemSubscribe(consumerAssignmentSubj, _EMPTY_, false, c, js.processConsumerAssignmentResults)
	}
}

// Lock should be held.
func (js *jetStream) stopUpdatesSub() {
	cc := js.cluster
	if cc.streamResults != nil {
		cc.s.sysUnsubscribe(cc.streamResults)
		cc.streamResults = nil
	}
	if cc.consumerResults != nil {
		cc.s.sysUnsubscribe(cc.consumerResults)
		cc.consumerResults = nil
	}
}

func (js *jetStream) processLeaderChange(isLeader bool) {
	if isLeader {
		js.srv.Noticef("JetStream cluster new metadata leader")
	}

	js.mu.Lock()
	defer js.mu.Unlock()

	if isLeader {
		js.startUpdatesSub()
	} else {
		js.stopUpdatesSub()
		// TODO(dlc) - stepdown.
	}
}

// selectPeerGroup will select a group of peers to start a raft group.
// TODO(dlc) - For now randomly select. Can be way smarter.
func (cc *jetStreamCluster) selectPeerGroup(r int) []string {
	var nodes []string
	peers := cc.meta.Peers()
	// Make sure they are active
	s := cc.s
	ourID := cc.meta.ID()
	for _, p := range peers {
		if p.ID == ourID || s.getRouteByHash([]byte(p.ID)) != nil {
			nodes = append(nodes, p.ID)
		}
	}
	if len(nodes) < r {
		return nil
	}
	// Don't depend on range.
	rand.Shuffle(len(nodes), func(i, j int) { nodes[i], nodes[j] = nodes[j], nodes[i] })
	return nodes[:r]
}

func groupNameForStream(peers []string, storage StorageType) string {
	return groupName("S", peers, storage)
}

func groupNameForConsumer(peers []string, storage StorageType) string {
	return groupName("C", peers, storage)
}

func groupName(prefix string, peers []string, storage StorageType) string {
	var gns string
	if len(peers) == 1 {
		gns = peers[0]
	} else {
		gns = string(getHash(nuid.Next()))
	}
	return fmt.Sprintf("%s-R%d%s-%s", prefix, len(peers), storage.String()[:1], gns)
}

// createGroupForStream will create a group for assignment for the stream.
// Lock should be held.
func (cc *jetStreamCluster) createGroupForStream(cfg *StreamConfig) *raftGroup {
	replicas := cfg.Replicas
	if replicas == 0 {
		replicas = 1
	}

	// Need to create a group here.
	// TODO(dlc) - Can be way smarter here.
	peers := cc.selectPeerGroup(replicas)
	if len(peers) == 0 {
		return nil
	}
	return &raftGroup{Name: groupNameForStream(peers, cfg.Storage), Storage: cfg.Storage, Peers: peers}
}

func (s *Server) jsClusteredStreamRequest(ci *ClientInfo, subject, reply string, rmsg []byte, cfg *StreamConfig) {
	js, cc := s.getJetStreamCluster()
	if js == nil || cc == nil {
		return
	}

	var resp = JSApiStreamCreateResponse{ApiResponse: ApiResponse{Type: JSApiStreamCreateResponseType}}
	acc, err := s.LookupAccount(ci.Account)
	if err != nil {
		resp.Error = jsError(err)
		s.sendAPIErrResponse(ci, acc, subject, reply, string(rmsg), s.jsonResponse(&resp))
		return
	}

	js.mu.RLock()
	numStreams := len(cc.streams[ci.Account])
	js.mu.RUnlock()

	// Grab our jetstream account info.
	acc.mu.RLock()
	jsa := acc.js
	acc.mu.RUnlock()

	// Check for stream limits here before proposing.
	jsa.mu.RLock()
	exceeded := jsa.limits.MaxStreams > 0 && numStreams >= jsa.limits.MaxStreams
	jsa.mu.RUnlock()

	if exceeded {
		resp.Error = jsError(fmt.Errorf("maximum number of streams reached"))
		s.sendAPIErrResponse(ci, acc, subject, reply, string(rmsg), s.jsonResponse(&resp))
		return
	}

	// Now process the request and proposal.
	js.mu.Lock()
	defer js.mu.Unlock()

	if sa := js.streamAssignment(ci.Account, cfg.Name); sa != nil {
		resp.Error = jsError(ErrJetStreamStreamAlreadyUsed)
		s.sendAPIErrResponse(ci, acc, subject, reply, string(rmsg), s.jsonResponse(&resp))
		return
	}

	// Raft group selection and placement.
	rg := cc.createGroupForStream(cfg)
	if rg == nil {
		resp.Error = jsInsufficientErr
		s.sendAPIErrResponse(ci, acc, subject, reply, string(rmsg), s.jsonResponse(&resp))
		return
	}
	// Pick a preferred leader.
	rg.setPreferred()
	// Sync subject for post snapshot sync.
	sa := &streamAssignment{Group: rg, Sync: syncSubjForStream(), Config: cfg, Reply: reply, Client: ci, Created: time.Now()}
	cc.meta.Propose(encodeAddStreamAssignment(sa))
}

func (s *Server) jsClusteredStreamDeleteRequest(ci *ClientInfo, stream, reply string, rmsg []byte) {
	js, cc := s.getJetStreamCluster()
	if js == nil || cc == nil {
		return
	}

	js.mu.Lock()
	defer js.mu.Unlock()

	osa := js.streamAssignment(ci.Account, stream)
	if osa == nil {
		acc, err := s.LookupAccount(ci.Account)
		if err == nil {
			var resp = JSApiStreamDeleteResponse{ApiResponse: ApiResponse{Type: JSApiStreamDeleteResponseType}}
			resp.Error = jsNotFoundError(ErrJetStreamStreamNotFound)
			s.sendAPIErrResponse(ci, acc, _EMPTY_, reply, string(rmsg), s.jsonResponse(&resp))
		}
		return
	}
	// Remove any remaining consumers as well.
	for _, ca := range osa.consumers {
		ca.Reply, ca.State = _EMPTY_, nil
		cc.meta.Propose(encodeDeleteConsumerAssignment(ca))
	}

	sa := &streamAssignment{Group: osa.Group, Config: osa.Config, Reply: reply, Client: ci}
	cc.meta.Propose(encodeDeleteStreamAssignment(sa))
}

func (s *Server) jsClusteredStreamPurgeRequest(ci *ClientInfo, stream, subject, reply string, rmsg []byte) {
	js, cc := s.getJetStreamCluster()
	if js == nil || cc == nil {
		return
	}

	js.mu.Lock()
	defer js.mu.Unlock()

	sa := js.streamAssignment(ci.Account, stream)
	if sa == nil || sa.Group == nil || sa.Group.node == nil {
		resp := JSApiStreamPurgeResponse{ApiResponse: ApiResponse{Type: JSApiStreamPurgeResponseType}}
		acc, err := s.LookupAccount(ci.Account)
		if err != nil {
			resp.Error = jsError(err)
		} else {
			resp.Error = jsNotFoundError(ErrJetStreamStreamNotFound)
		}
		s.sendAPIErrResponse(ci, acc, subject, reply, string(rmsg), s.jsonResponse(&resp))
		return
	}

	n := sa.Group.node
	sp := &streamPurge{Stream: stream, Reply: reply, Client: ci}
	n.Propose(encodeStreamPurge(sp))
}

func (s *Server) jsClusteredStreamRestoreRequest(ci *ClientInfo, acc *Account, req *JSApiStreamRestoreRequest, stream, subject, reply string, rmsg []byte) {
	js, cc := s.getJetStreamCluster()
	if js == nil || cc == nil {
		return
	}

	js.mu.Lock()
	defer js.mu.Unlock()

	cfg := &req.Config
	resp := JSApiStreamRestoreResponse{ApiResponse: ApiResponse{Type: JSApiStreamRestoreResponseType}}

	if sa := js.streamAssignment(ci.Account, cfg.Name); sa != nil {
		resp.Error = jsError(ErrJetStreamStreamAlreadyUsed)
		s.sendAPIErrResponse(ci, acc, subject, reply, string(rmsg), s.jsonResponse(&resp))
		return
	}

	// Raft group selection and placement.
	rg := cc.createGroupForStream(cfg)
	if rg == nil {
		resp.Error = jsInsufficientErr
		s.sendAPIErrResponse(ci, acc, subject, reply, string(rmsg), s.jsonResponse(&resp))
		return
	}
	// Pick a preferred leader.
	rg.setPreferred()
	sa := &streamAssignment{Group: rg, Sync: syncSubjForStream(), Config: cfg, Reply: reply, Client: ci, Created: time.Now()}
	// Now add in our restore state and pre-select a peer to handle the actual receipt of the snapshot.
	sa.Restore = &req.State
	cc.meta.Propose(encodeAddStreamAssignment(sa))
}

// This will do a scatter and gather operation for all streams for this account.
// This will be running in a separate Go routine.
func (s *Server) jsClusteredStreamListRequest(acc *Account, ci *ClientInfo, offset int, subject, reply string, rmsg []byte) {
	defer s.grWG.Done()

	js, cc := s.getJetStreamCluster()
	if js == nil || cc == nil {
		return
	}

	js.mu.Lock()
	defer js.mu.Unlock()

	var streams []*streamAssignment
	for _, sa := range cc.streams[acc.Name] {
		streams = append(streams, sa)
	}
	// Needs to be sorted.
	if len(streams) > 1 {
		sort.Slice(streams, func(i, j int) bool {
			return strings.Compare(streams[i].Config.Name, streams[j].Config.Name) < 0
		})
	}

	scnt := len(streams)
	if offset > scnt {
		offset = scnt
	}
	if offset > 0 {
		streams = streams[offset:]
	}
	if len(streams) > JSApiListLimit {
		streams = streams[:JSApiListLimit]
	}

	rc := make(chan *StreamInfo, len(streams))

	// Create an inbox for our responses and send out requests.
	inbox := infoReplySubject()
	rsub, _ := s.systemSubscribe(inbox, _EMPTY_, false, cc.c, func(_ *subscription, _ *client, _, reply string, msg []byte) {
		var si StreamInfo
		if err := json.Unmarshal(msg, &si); err != nil {
			s.Warnf("Error unmarshaling clustered stream info response:%v", err)
			return
		}
		select {
		case rc <- &si:
		default:
			s.Warnf("Failed placing stream info result on internal chan")
		}
	})
	defer s.sysUnsubscribe(rsub)

	// Send out our requests here.
	for _, sa := range streams {
		isubj := fmt.Sprintf(clusterStreamInfoT, sa.Client.Account, sa.Config.Name)
		s.sendInternalMsgLocked(isubj, inbox, nil, nil)
	}

	const timeout = 2 * time.Second
	notActive := time.NewTimer(timeout)
	defer notActive.Stop()

	var resp = JSApiStreamListResponse{
		ApiResponse: ApiResponse{Type: JSApiStreamListResponseType},
		Streams:     make([]*StreamInfo, 0, len(streams)),
	}

LOOP:
	for {
		select {
		case <-s.quitCh:
			return
		case <-notActive.C:
			s.Warnf("Did not receive all stream info results for %q", acc)
			break LOOP
		case si := <-rc:
			resp.Streams = append(resp.Streams, si)
			// Check to see if we are done.
			if len(resp.Streams) == len(streams) {
				break LOOP
			}
		}
	}

	// Needs to be sorted as well.
	if len(resp.Streams) > 1 {
		sort.Slice(resp.Streams, func(i, j int) bool {
			return strings.Compare(resp.Streams[i].Config.Name, resp.Streams[j].Config.Name) < 0
		})
	}

	resp.Total = len(resp.Streams)
	resp.Limit = JSApiListLimit
	resp.Offset = offset
	s.sendAPIResponse(ci, acc, subject, reply, string(rmsg), s.jsonResponse(resp))
}

// This will do a scatter and gather operation for all consumers for this stream and account.
// This will be running in a separate Go routine.
func (s *Server) jsClusteredConsumerListRequest(acc *Account, ci *ClientInfo, offset int, stream, subject, reply string, rmsg []byte) {
	defer s.grWG.Done()

	js, cc := s.getJetStreamCluster()
	if js == nil || cc == nil {
		return
	}

	js.mu.Lock()
	defer js.mu.Unlock()

	var consumers []*consumerAssignment
	if sas := cc.streams[acc.Name]; sas != nil {
		if sa := sas[stream]; sa != nil {
			// Copy over since we need to sort etc.
			for _, ca := range sa.consumers {
				consumers = append(consumers, ca)
			}
		}
	}
	// Needs to be sorted.
	if len(consumers) > 1 {
		sort.Slice(consumers, func(i, j int) bool {
			return strings.Compare(consumers[i].Name, consumers[j].Name) < 0
		})
	}

	ocnt := len(consumers)
	if offset > ocnt {
		offset = ocnt
	}
	if offset > 0 {
		consumers = consumers[offset:]
	}
	if len(consumers) > JSApiListLimit {
		consumers = consumers[:JSApiListLimit]
	}

	rc := make(chan *ConsumerInfo, len(consumers))

	// Create an inbox for our responses and send out requests.
	inbox := infoReplySubject()
	rsub, _ := s.systemSubscribe(inbox, _EMPTY_, false, cc.c, func(_ *subscription, _ *client, _, reply string, msg []byte) {
		var ci ConsumerInfo
		if err := json.Unmarshal(msg, &ci); err != nil {
			s.Warnf("Error unmarshaling clustered consumer info response:%v", err)
			return
		}
		select {
		case rc <- &ci:
		default:
			s.Warnf("Failed placing consumer info result on internal chan")
		}
	})
	defer s.sysUnsubscribe(rsub)

	// Send out our requests here.
	var resp = JSApiConsumerListResponse{
		ApiResponse: ApiResponse{Type: JSApiConsumerListResponseType},
		Consumers:   []*ConsumerInfo{},
	}

	if len(consumers) == 0 {
		resp.Limit = JSApiListLimit
		resp.Offset = offset
		s.sendAPIResponse(ci, acc, subject, reply, string(rmsg), s.jsonResponse(resp))
		return
	}

	for _, ca := range consumers {
		isubj := fmt.Sprintf(clusterConsumerInfoT, ca.Client.Account, stream, ca.Name)
		s.sendInternalMsgLocked(isubj, inbox, nil, nil)
	}

	const timeout = 2 * time.Second
	notActive := time.NewTimer(timeout)
	defer notActive.Stop()

LOOP:
	for {
		select {
		case <-s.quitCh:
			return
		case <-notActive.C:
			s.Warnf("Did not receive all stream info results for %q", acc)
			break LOOP
		case ci := <-rc:
			resp.Consumers = append(resp.Consumers, ci)
			// Check to see if we are done.
			if len(resp.Consumers) == len(consumers) {
				break LOOP
			}
		}
	}

	// Needs to be sorted as well.
	if len(resp.Consumers) > 1 {
		sort.Slice(resp.Consumers, func(i, j int) bool {
			return strings.Compare(resp.Consumers[i].Name, resp.Consumers[j].Name) < 0
		})
	}

	resp.Total = len(resp.Consumers)
	resp.Limit = JSApiListLimit
	resp.Offset = offset
	s.sendAPIResponse(ci, acc, subject, reply, string(rmsg), s.jsonResponse(resp))
}

func encodeStreamPurge(sp *streamPurge) []byte {
	var bb bytes.Buffer
	bb.WriteByte(byte(purgeStreamOp))
	json.NewEncoder(&bb).Encode(sp)
	return bb.Bytes()
}

func decodeStreamPurge(buf []byte) (*streamPurge, error) {
	var sp streamPurge
	err := json.Unmarshal(buf, &sp)
	return &sp, err
}

func (s *Server) jsClusteredConsumerDeleteRequest(ci *ClientInfo, stream, consumer, subject, reply string, rmsg []byte) {
	js, cc := s.getJetStreamCluster()
	if js == nil || cc == nil {
		return
	}

	js.mu.Lock()
	defer js.mu.Unlock()

	sa := js.streamAssignment(ci.Account, stream)
	if sa == nil || sa.consumers == nil {
		// TODO(dlc) - Should respond? Log?
		return
	}
	oca := sa.consumers[consumer]
	if oca == nil {
		acc, err := s.LookupAccount(ci.Account)
		if err == nil {
			var resp = JSApiConsumerDeleteResponse{ApiResponse: ApiResponse{Type: JSApiConsumerDeleteResponseType}}
			resp.Error = jsNoAccountErr
			s.sendAPIErrResponse(ci, acc, subject, reply, string(rmsg), s.jsonResponse(&resp))
		}
		return
	}
	ca := &consumerAssignment{Group: oca.Group, Stream: stream, Name: consumer, Config: oca.Config, Reply: reply, Client: ci}
	cc.meta.Propose(encodeDeleteConsumerAssignment(ca))
}

func encodeMsgDelete(md *streamMsgDelete) []byte {
	var bb bytes.Buffer
	bb.WriteByte(byte(deleteMsgOp))
	json.NewEncoder(&bb).Encode(md)
	return bb.Bytes()
}

func decodeMsgDelete(buf []byte) (*streamMsgDelete, error) {
	var md streamMsgDelete
	err := json.Unmarshal(buf, &md)
	return &md, err
}

func (s *Server) jsClusteredMsgDeleteRequest(ci *ClientInfo, stream, subject, reply string, seq uint64, rmsg []byte) {
	js, cc := s.getJetStreamCluster()
	if js == nil || cc == nil {
		return
	}

	js.mu.Lock()
	defer js.mu.Unlock()

	sa := js.streamAssignment(ci.Account, stream)
	if sa == nil || sa.Group == nil || sa.Group.node == nil {
		// TODO(dlc) - Should respond? Log?
		return
	}
	n := sa.Group.node
	md := &streamMsgDelete{Seq: seq, Stream: stream, Reply: reply, Client: ci}
	n.Propose(encodeMsgDelete(md))
}

func encodeAddStreamAssignment(sa *streamAssignment) []byte {
	var bb bytes.Buffer
	bb.WriteByte(byte(assignStreamOp))
	json.NewEncoder(&bb).Encode(sa)
	return bb.Bytes()
}

func encodeDeleteStreamAssignment(sa *streamAssignment) []byte {
	var bb bytes.Buffer
	bb.WriteByte(byte(removeStreamOp))
	json.NewEncoder(&bb).Encode(sa)
	return bb.Bytes()
}

func decodeStreamAssignment(buf []byte) (*streamAssignment, error) {
	var sa streamAssignment
	err := json.Unmarshal(buf, &sa)
	return &sa, err
}

// createGroupForConsumer will create a new group with same peer set as the stream.
func (cc *jetStreamCluster) createGroupForConsumer(sa *streamAssignment) *raftGroup {
	peers := sa.Group.Peers
	if len(peers) == 0 {
		return nil
	}
	return &raftGroup{Name: groupNameForConsumer(peers, sa.Config.Storage), Storage: sa.Config.Storage, Peers: peers}
}

func (s *Server) jsClusteredConsumerRequest(ci *ClientInfo, subject, reply string, rmsg []byte, stream string, cfg *ConsumerConfig) {
	js, cc := s.getJetStreamCluster()
	if js == nil || cc == nil {
		return
	}

	js.mu.Lock()
	defer js.mu.Unlock()

	var resp = JSApiConsumerCreateResponse{ApiResponse: ApiResponse{Type: JSApiConsumerCreateResponseType}}
	acc, err := s.LookupAccount(ci.Account)
	if err != nil {
		resp.Error = jsError(err)
		s.sendAPIErrResponse(ci, acc, subject, reply, string(rmsg), s.jsonResponse(&resp))
		return
	}

	// Lookup the stream assignment.
	sa := js.streamAssignment(ci.Account, stream)
	if sa == nil {
		resp.Error = jsError(ErrJetStreamStreamNotFound)
		s.sendAPIErrResponse(ci, acc, subject, reply, string(rmsg), s.jsonResponse(&resp))
		return
	}

	rg := cc.createGroupForConsumer(sa)
	if rg == nil {
		resp.Error = jsInsufficientErr
		s.sendAPIErrResponse(ci, acc, subject, reply, string(rmsg), s.jsonResponse(&resp))
		return
	}
	// Pick a preferred leader.
	rg.setPreferred()

	// We need to set the ephemeral here before replicating.
	var oname string
	if !isDurableConsumer(cfg) {
		for {
			oname = createConsumerName()
			if sa.consumers != nil {
				if sa.consumers[oname] != nil {
					continue
				}
			}
			break
		}
	} else {
		oname = cfg.Durable
		if sa.consumers[oname] != nil {
			resp.Error = jsError(ErrJetStreamConsumerAlreadyUsed)
			s.sendAPIErrResponse(ci, acc, subject, reply, string(rmsg), s.jsonResponse(&resp))
			return
		}
	}

	ca := &consumerAssignment{Group: rg, Stream: stream, Name: oname, Config: cfg, Reply: reply, Client: ci, Created: time.Now()}
	cc.meta.Propose(encodeAddConsumerAssignment(ca))
}

func encodeAddConsumerAssignment(ca *consumerAssignment) []byte {
	var bb bytes.Buffer
	bb.WriteByte(byte(assignConsumerOp))
	json.NewEncoder(&bb).Encode(ca)
	return bb.Bytes()
}

func encodeDeleteConsumerAssignment(ca *consumerAssignment) []byte {
	var bb bytes.Buffer
	bb.WriteByte(byte(removeConsumerOp))
	json.NewEncoder(&bb).Encode(ca)
	return bb.Bytes()
}

func decodeConsumerAssignment(buf []byte) (*consumerAssignment, error) {
	var ca consumerAssignment
	err := json.Unmarshal(buf, &ca)
	return &ca, err
}

func encodeAddConsumerAssignmentCompressed(ca *consumerAssignment) []byte {
	b, err := json.Marshal(ca)
	if err != nil {
		return nil
	}
	// TODO(dlc) - Streaming better approach here probably.
	var bb bytes.Buffer
	bb.WriteByte(byte(assignCompressedConsumerOp))
	bb.Write(s2.Encode(nil, b))
	return bb.Bytes()
}

func decodeConsumerAssignmentCompressed(buf []byte) (*consumerAssignment, error) {
	var ca consumerAssignment
	js, err := s2.Decode(nil, buf)
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(js, &ca)
	return &ca, err
}

var errBadStreamMsg = errors.New("jetstream cluster bad replicated stream msg")

func decodeStreamMsg(buf []byte) (subject, reply string, hdr, msg []byte, lseq uint64, ts int64, err error) {
	var le = binary.LittleEndian
	if len(buf) < 26 {
		return _EMPTY_, _EMPTY_, nil, nil, 0, 0, errBadStreamMsg
	}
	lseq = le.Uint64(buf)
	buf = buf[8:]
	ts = int64(le.Uint64(buf))
	buf = buf[8:]
	sl := int(le.Uint16(buf))
	buf = buf[2:]
	if len(buf) < sl {
		return _EMPTY_, _EMPTY_, nil, nil, 0, 0, errBadStreamMsg
	}
	subject = string(buf[:sl])
	buf = buf[sl:]
	if len(buf) < 2 {
		return _EMPTY_, _EMPTY_, nil, nil, 0, 0, errBadStreamMsg
	}
	rl := int(le.Uint16(buf))
	buf = buf[2:]
	if len(buf) < rl {
		return _EMPTY_, _EMPTY_, nil, nil, 0, 0, errBadStreamMsg
	}
	reply = string(buf[:rl])
	buf = buf[rl:]
	if len(buf) < 2 {
		return _EMPTY_, _EMPTY_, nil, nil, 0, 0, errBadStreamMsg
	}
	hl := int(le.Uint16(buf))
	buf = buf[2:]
	if len(buf) < hl {
		return _EMPTY_, _EMPTY_, nil, nil, 0, 0, errBadStreamMsg
	}
	hdr = buf[:hl]
	buf = buf[hl:]
	if len(buf) < 4 {
		return _EMPTY_, _EMPTY_, nil, nil, 0, 0, errBadStreamMsg
	}
	ml := int(le.Uint32(buf))
	buf = buf[4:]
	if len(buf) < ml {
		return _EMPTY_, _EMPTY_, nil, nil, 0, 0, errBadStreamMsg
	}
	msg = buf[:ml]
	return subject, reply, hdr, msg, lseq, ts, nil
}

func encodeStreamMsg(subject, reply string, hdr, msg []byte, lseq uint64, ts int64) []byte {
	elen := 1 + 8 + 8 + len(subject) + len(reply) + len(hdr) + len(msg)
	elen += (2 + 2 + 2 + 4) // Encoded lengths, 4bytes
	// TODO(dlc) - check sizes of subject, reply and hdr, make sure uint16 ok.
	buf := make([]byte, elen)
	buf[0] = byte(streamMsgOp)
	var le = binary.LittleEndian
	wi := 1
	le.PutUint64(buf[wi:], lseq)
	wi += 8
	le.PutUint64(buf[wi:], uint64(ts))
	wi += 8
	le.PutUint16(buf[wi:], uint16(len(subject)))
	wi += 2
	copy(buf[wi:], subject)
	wi += len(subject)
	le.PutUint16(buf[wi:], uint16(len(reply)))
	wi += 2
	copy(buf[wi:], reply)
	wi += len(reply)
	le.PutUint16(buf[wi:], uint16(len(hdr)))
	wi += 2
	if len(hdr) > 0 {
		copy(buf[wi:], hdr)
		wi += len(hdr)
	}
	le.PutUint32(buf[wi:], uint32(len(msg)))
	wi += 4
	if len(msg) > 0 {
		copy(buf[wi:], msg)
		wi += len(msg)
	}
	return buf[:wi]
}

// StreamSnapshot is used for snapshotting and out of band catch up in clustered mode.
type streamSnapshot struct {
	Msgs     uint64   `json:"messages"`
	Bytes    uint64   `json:"bytes"`
	FirstSeq uint64   `json:"first_seq"`
	LastSeq  uint64   `json:"last_seq"`
	Deleted  []uint64 `json:"deleted,omitempty"`
}

// Grab a snapshot of a stream for clustered mode.
func (mset *Stream) snapshot() []byte {
	mset.mu.RLock()
	defer mset.mu.RUnlock()

	state := mset.store.State()
	snap := &streamSnapshot{
		Msgs:     state.Msgs,
		Bytes:    state.Bytes,
		FirstSeq: state.FirstSeq,
		LastSeq:  state.LastSeq,
		Deleted:  state.Deleted,
	}
	b, _ := json.Marshal(snap)
	return b
}

// processClusteredMsg will propose the inbound message to the underlying raft group.
func (mset *Stream) processClusteredInboundMsg(subject, reply string, hdr, msg []byte) error {
	// For possible error response.
	var response []byte

	mset.mu.RLock()
	canRespond := !mset.config.NoAck && len(reply) > 0
	s, jsa, st, rf, sendq := mset.srv, mset.jsa, mset.config.Storage, mset.config.Replicas, mset.sendq
	maxMsgSize := int(mset.config.MaxMsgSize)
	mset.mu.RUnlock()

	// Check here pre-emptively if we have exceeded our account limits.
	var exceeded bool
	jsa.mu.RLock()
	if st == MemoryStorage {
		total := jsa.storeTotal + int64(memStoreMsgSize(subject, hdr, msg)*uint64(rf))
		if jsa.limits.MaxMemory > 0 && total > jsa.limits.MaxMemory {
			exceeded = true
		}
	} else {
		total := jsa.storeTotal + int64(fileStoreMsgSize(subject, hdr, msg)*uint64(rf))
		if jsa.limits.MaxStore > 0 && total > jsa.limits.MaxStore {
			exceeded = true
		}
	}
	jsa.mu.RUnlock()

	// If we have exceeded our account limits go ahead and return.
	if exceeded {
		err := fmt.Errorf("JetStream resource limits exceeded for account: %q", jsa.acc().Name)
		s.Warnf(err.Error())
		if canRespond {
			var resp = &JSPubAckResponse{PubAck: &PubAck{Stream: mset.Name()}}
			resp.Error = &ApiError{Code: 400, Description: "resource limits exceeded for account"}
			response, _ = json.Marshal(resp)
			sendq <- &jsPubMsg{reply, _EMPTY_, _EMPTY_, nil, response, nil, 0}
		}
		return err
	}

	// Check msgSize if we have a limit set there. Again this works if it goes through but better to be pre-emptive.
	if maxMsgSize >= 0 && (len(hdr)+len(msg)) > maxMsgSize {
		err := fmt.Errorf("JetStream message size exceeds limits for '%s > %s'", jsa.acc().Name, mset.config.Name)
		s.Warnf(err.Error())
		if canRespond {
			var resp = &JSPubAckResponse{PubAck: &PubAck{Stream: mset.Name()}}
			resp.Error = &ApiError{Code: 400, Description: "message size exceeds maximum allowed"}
			response, _ = json.Marshal(resp)
			sendq <- &jsPubMsg{reply, _EMPTY_, _EMPTY_, nil, response, nil, 0}
		}
		return err
	}

	// Proceed with proposing this message.
	mset.mu.Lock()

	// We only use mset.clseq for clustering and in case we run ahead of actual commits.
	// Check if we need to set initial value here
	if mset.clseq < mset.lseq {
		mset.clseq = mset.lseq
	}

	// Do proposal.
	err := mset.node.Propose(encodeStreamMsg(subject, reply, hdr, msg, mset.clseq, time.Now().UnixNano()))
	if err != nil {
		if canRespond {
			var resp = &JSPubAckResponse{PubAck: &PubAck{Stream: mset.config.Name}}
			resp.Error = &ApiError{Code: 503, Description: err.Error()}
			response, _ = json.Marshal(resp)
		}
	} else {
		mset.clseq++
	}
	mset.mu.Unlock()

	// If we errored out respond here.
	if err != nil && canRespond {
		sendq <- &jsPubMsg{reply, _EMPTY_, _EMPTY_, nil, response, nil, 0}
	}

	return err
}

// For requesting messages post raft snapshot to catch up streams post server restart.
// Any deleted msgs etc will be handled inline on catchup.
type streamSyncRequest struct {
	FirstSeq uint64 `json:"first_seq"`
	LastSeq  uint64 `json:"last_seq"`
}

// Given a stream state that represents a snapshot, calculate the sync request based on our current state.
func (mset *Stream) calculateSyncRequest(state *StreamState, snap *streamSnapshot) *streamSyncRequest {
	// Quick check if we are already caught up.
	if state.LastSeq >= snap.LastSeq {
		return nil
	}
	return &streamSyncRequest{FirstSeq: state.LastSeq + 1, LastSeq: snap.LastSeq}
}

// processSnapshotDeletes will update our current store based on the snapshot
// but only processing deletes and new FirstSeq / purges.
func (mset *Stream) processSnapshotDeletes(snap *streamSnapshot) {
	state := mset.store.State()

	// Adjust if FirstSeq has moved.
	if snap.FirstSeq > state.FirstSeq {
		mset.store.Compact(snap.FirstSeq)
		state = mset.store.State()
	}
	// Range the deleted and delete if applicable.
	for _, dseq := range snap.Deleted {
		if dseq <= state.LastSeq {
			mset.store.RemoveMsg(dseq)
		}
	}
}

func (mset *Stream) setCatchingUp() {
	mset.mu.Lock()
	mset.catchup = true
	mset.mu.Unlock()
}

func (mset *Stream) clearCatchingUp() {
	mset.mu.Lock()
	mset.catchup = false
	mset.mu.Unlock()
}

func (mset *Stream) isCatchingUp() bool {
	mset.mu.RLock()
	defer mset.mu.RUnlock()
	return mset.catchup
}

// Process a stream snapshot.
func (mset *Stream) processSnapshot(buf []byte) {
	var snap streamSnapshot
	if err := json.Unmarshal(buf, &snap); err != nil {
		// Log error.
		return
	}

	// Update any deletes, etc.
	mset.processSnapshotDeletes(&snap)

	mset.mu.Lock()
	state := mset.store.State()
	sreq := mset.calculateSyncRequest(&state, &snap)
	s, subject, n := mset.srv, mset.sa.Sync, mset.node
	mset.mu.Unlock()

	// Just return if up to date..
	if sreq == nil {
		return
	}

	// Pause the apply channel for our raft group while we catch up.
	n.PauseApply()
	defer n.ResumeApply()

	// Set our catchup state.
	mset.setCatchingUp()
	defer mset.clearCatchingUp()

	js := s.getJetStream()

RETRY:

	// Grab sync request again on failures.
	if sreq == nil {
		mset.mu.Lock()
		state := mset.store.State()
		sreq = mset.calculateSyncRequest(&state, &snap)
		mset.mu.Unlock()
		if sreq == nil {
			return
		}
	}

	msgsC := make(chan []byte, 1024)

	// Send our catchup request here.
	reply := syncReplySubject()
	sub, err := s.sysSubscribe(reply, func(_ *subscription, _ *client, _, reply string, msg []byte) {
		// Make copies - https://github.com/go101/go101/wiki
		// TODO(dlc) - Since we are using a buffer from the inbound client/route.
		if len(msg) > 0 {
			msg = append(msg[:0:0], msg...)
		}
		msgsC <- msg
		if reply != _EMPTY_ {
			s.sendInternalMsgLocked(reply, _EMPTY_, nil, nil)
		}
	})
	if err != nil {
		return
	}
	defer s.sysUnsubscribe(sub)

	b, _ := json.Marshal(sreq)
	s.sendInternalMsgLocked(subject, reply, nil, b)

	// Clear our sync request and capture last.
	last := sreq.LastSeq
	sreq = nil

	const activityInterval = 5 * time.Second
	notActive := time.NewTimer(activityInterval)
	defer notActive.Stop()

	// Run our own select loop here.
	for qch, lch := n.QuitC(), n.LeadChangeC(); ; {
		select {
		case msg := <-msgsC:
			notActive.Reset(activityInterval)
			// Check eof signaling.
			if len(msg) == 0 {
				goto RETRY
			}

			if lseq, err := mset.processCatchupMsg(msg); err == nil {
				if lseq >= last {
					return
				}
			} else {
				goto RETRY
			}
		case <-notActive.C:
			s.Warnf("Catchup for stream '%s > %s' stalled", mset.account(), mset.Name())
			goto RETRY
		case <-s.quitCh:
			return
		case <-qch:
			return
		case isLeader := <-lch:
			sa := js.streamAssignment(mset.account().Name, mset.Name())
			js.processStreamLeaderChange(mset, sa, isLeader)
		}
	}
}

// processCatchupMsg will be called to process out of band catchup msgs from a sync request.
func (mset *Stream) processCatchupMsg(msg []byte) (uint64, error) {
	if len(msg) == 0 || entryOp(msg[0]) != streamMsgOp {
		// TODO(dlc) - This is error condition, log.
		return 0, errors.New("bad catchup msg")
	}

	subj, _, hdr, msg, seq, ts, err := decodeStreamMsg(msg[1:])
	if err != nil {
		return 0, errors.New("bad catchup msg")
	}
	// Put into our store
	// Messages to be skipped have no subject or timestamp.
	// TODO(dlc) - formalize witrh skipMsgOp
	if subj == _EMPTY_ && ts == 0 {
		lseq := mset.store.SkipMsg()
		if lseq != seq {
			return 0, errors.New("wrong sequence for skipped msg")
		}
	} else if err := mset.store.StoreRawMsg(subj, hdr, msg, seq, ts); err != nil {
		return 0, err
	}
	// Update our lseq.
	mset.setLastSeq(seq)

	return seq, nil
}

func (mset *Stream) handleClusterSyncRequest(sub *subscription, c *client, subject, reply string, msg []byte) {
	var sreq streamSyncRequest
	if err := json.Unmarshal(msg, &sreq); err != nil {
		// Log error.
		return
	}
	mset.srv.startGoRoutine(func() { mset.runCatchup(reply, &sreq) })
}

// clusterInfo will report on the status of the raft group.
func (s *Server) clusterInfo(n RaftNode) *ClusterInfo {
	if n == nil {
		return &ClusterInfo{
			Name:   s.ClusterName(),
			Leader: s.Name(),
		}
	}

	ci := &ClusterInfo{
		Name:   s.ClusterName(),
		Leader: s.serverNameForNode(n.GroupLeader()),
	}

	now := time.Now()

	id, peers := n.ID(), n.Peers()
	for _, rp := range peers {
		if rp.ID != id {
			lastSeen := now.Sub(rp.Last)
			current := rp.Current
			if current && lastSeen > lostQuorumInterval {
				current = false
			}
			pi := &PeerInfo{Name: s.serverNameForNode(rp.ID), Current: current, Active: lastSeen}
			ci.Replicas = append(ci.Replicas, pi)
		}
	}
	return ci
}

func (mset *Stream) handleClusterStreamInfoRequest(sub *subscription, c *client, subject, reply string, msg []byte) {
	mset.mu.RLock()
	if mset.client == nil {
		mset.mu.RUnlock()
		return
	}
	s, config, node := mset.srv, mset.config, mset.node
	mset.mu.RUnlock()

	si := &StreamInfo{Created: mset.Created(), State: mset.State(), Config: config, Cluster: s.clusterInfo(node)}
	b, _ := json.Marshal(si)
	s.sendInternalMsgLocked(reply, _EMPTY_, nil, b)
}

func (mset *Stream) runCatchup(sendSubject string, sreq *streamSyncRequest) {
	s := mset.srv
	defer s.grWG.Done()

	const maxOut = int64(48 * 1024 * 1024) // 48MB for now.
	out := int64(0)

	// Flow control processing.
	ackReplySize := func(subj string) int64 {
		if li := strings.LastIndexByte(subj, btsep); li > 0 && li < len(subj) {
			return parseAckReplyNum(subj[li+1:])
		}
		return 0
	}

	nextBatchC := make(chan struct{}, 1)
	nextBatchC <- struct{}{}

	// Setup ackReply for flow control.
	ackReply := syncAckSubject()
	ackSub, _ := s.sysSubscribe(ackReply, func(sub *subscription, c *client, subject, reply string, msg []byte) {
		sz := ackReplySize(subject)
		atomic.AddInt64(&out, -sz)
		select {
		case nextBatchC <- struct{}{}:
		default:
		}
	})
	defer s.sysUnsubscribe(ackSub)
	ackReplyT := strings.ReplaceAll(ackReply, ".*", ".%d")

	// EOF
	defer s.sendInternalMsgLocked(sendSubject, _EMPTY_, nil, nil)

	const activityInterval = 5 * time.Second
	notActive := time.NewTimer(activityInterval)
	defer notActive.Stop()

	// Setup sequences to walk through.
	seq, last := sreq.FirstSeq, sreq.LastSeq

	sendNextBatch := func() {
		for ; seq <= last && atomic.LoadInt64(&out) <= maxOut; seq++ {
			subj, hdr, msg, ts, err := mset.store.LoadMsg(seq)
			// if this is not a deleted msg, bail out.
			if err != nil && err != ErrStoreMsgNotFound && err != errDeletedMsg {
				// break, something changed.
				seq = last + 1
				return
			}
			// S2?
			em := encodeStreamMsg(subj, _EMPTY_, hdr, msg, seq, ts)
			// Place size in reply subject for flow control.
			reply := fmt.Sprintf(ackReplyT, len(em))
			atomic.AddInt64(&out, int64(len(em)))
			s.sendInternalMsgLocked(sendSubject, reply, nil, em)
		}
	}

	// Grab stream quit channel.
	mset.mu.RLock()
	qch := mset.qch
	mset.mu.RUnlock()
	if qch == nil {
		return
	}

	// Run as long as we are still active and need catchup.
	// FIXME(dlc) - Purge event? Stream delete?
	for {
		select {
		case <-s.quitCh:
			return
		case <-qch:
			return
		case <-notActive.C:
			s.Warnf("Catchup for stream '%s > %s' stalled", mset.account(), mset.Name())
			return
		case <-nextBatchC:
			// Update our activity timer.
			notActive.Reset(activityInterval)
			sendNextBatch()
			// Check if we are finished.
			if seq >= last {
				s.Debugf("Done resync for stream '%s > %s'", mset.account(), mset.Name())
				return
			}
		}
	}
}

func syncSubjForStream() string {
	return syncSubject("$JSC.SYNC")
}

func syncReplySubject() string {
	return syncSubject("$JSC.R")
}

func infoReplySubject() string {
	return syncSubject("$JSC.R")
}

func syncAckSubject() string {
	return syncSubject("$JSC.ACK") + ".*"
}

func syncSubject(pre string) string {
	var sb strings.Builder
	sb.WriteString(pre)
	sb.WriteByte(btsep)

	var b [replySuffixLen]byte
	rn := rand.Int63()
	for i, l := 0, rn; i < len(b); i++ {
		b[i] = digits[l%base]
		l /= base
	}

	sb.Write(b[:])
	return sb.String()
}

const (
	clusterStreamInfoT   = "$JSC.SI.%s.%s"
	clusterConsumerInfoT = "$JSC.CI.%s.%s.%s"
	jsaUpdatesSubT       = "$JSC.ARU.%s.*"
	jsaUpdatesPubT       = "$JSC.ARU.%s.%s"
)
