// Copyright (c) 2013-2017 The btcsuite developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package netsync

import (
	"container/list"
	"fmt"
	"math/rand"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/database"
	"github.com/btcsuite/btcd/mempool"
	peerpkg "github.com/btcsuite/btcd/peer"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
)

const (
	// minInFlightBlocks is the minimum number of blocks that should be
	// in the request queue for headers-first mode before requesting
	// more.
	minInFlightBlocks = 10

	// maxRejectedTxns is the maximum number of rejected transactions
	// hashes to store in memory.
	maxRejectedTxns = 1000

	// maxRequestedBlocks is the maximum number of requested block
	// hashes to store in memory.
	maxRequestedBlocks = wire.MaxInvPerMsg

	// maxRequestedTxns is the maximum number of requested transactions
	// hashes to store in memory.
	maxRequestedTxns = wire.MaxInvPerMsg

	// maxStallDuration is the time after which we will disconnect our
	// current sync peer if we haven't made progress.
	maxStallDuration = 3 * time.Minute

	// stallSampleInterval the interval at which we will check to see if our
	// sync has stalled.
	stallSampleInterval = 30 * time.Second
)

// zeroHash is the zero value hash (all zeros).  It is defined as a convenience.
var zeroHash chainhash.Hash

// newPeerMsg signifies a newly connected peer to the block handler.
type newPeerMsg struct {
	peer *peerpkg.Peer
}

// blockMsg packages a bitcoin block message and the peer it came from together
// so the block handler has access to that information.
type blockMsg struct {
	block *btcutil.Block
	peer  *peerpkg.Peer
	reply chan struct{}
}

type ublockMsg struct {
	ublock *btcutil.UBlock
	peer   *peerpkg.Peer
	reply  chan struct{}
}

// invMsg packages a bitcoin inv message and the peer it came from together
// so the block handler has access to that information.
type invMsg struct {
	inv  *wire.MsgInv
	peer *peerpkg.Peer
}

// headersMsg packages a bitcoin headers message and the peer it came from
// together so the block handler has access to that information.
type headersMsg struct {
	headers *wire.MsgHeaders
	peer    *peerpkg.Peer
}

// notFoundMsg packages a bitcoin notfound message and the peer it came from
// together so the block handler has access to that information.
type notFoundMsg struct {
	notFound *wire.MsgNotFound
	peer     *peerpkg.Peer
}

// donePeerMsg signifies a newly disconnected peer to the block handler.
type donePeerMsg struct {
	peer *peerpkg.Peer
}

// txMsg packages a bitcoin tx message and the peer it came from together
// so the block handler has access to that information.
type txMsg struct {
	tx    *btcutil.Tx
	peer  *peerpkg.Peer
	reply chan struct{}
}

// getSyncPeerMsg is a message type to be sent across the message channel for
// retrieving the current sync peer.
type getSyncPeerMsg struct {
	reply chan int32
}

// processBlockResponse is a response sent to the reply channel of a
// processBlockMsg.
type processBlockResponse struct {
	isOrphan bool
	err      error
}

// processBlockMsg is a message type to be sent across the message channel
// for requested a block is processed.  Note this call differs from blockMsg
// above in that blockMsg is intended for blocks that came from peers and have
// extra handling whereas this message essentially is just a concurrent safe
// way to call ProcessBlock on the internal block chain instance.
type processBlockMsg struct {
	block *btcutil.Block
	flags blockchain.BehaviorFlags
	reply chan processBlockResponse
}

// processUBlockMsg is a message type to be sent across the message channel
// for requested a block is processed.  Note this call differs from blockMsg
// above in that blockMsg is intended for blocks that came from peers and have
// extra handling whereas this message essentially is just a concurrent safe
// way to call ProcessBlock on the internal block chain instance.
type processUBlockMsg struct {
	ublock *btcutil.UBlock
	flags  blockchain.BehaviorFlags
	reply  chan processBlockResponse
}

// isCurrentMsg is a message type to be sent across the message channel for
// requesting whether or not the sync manager believes it is synced with the
// currently connected peers.
type isCurrentMsg struct {
	reply chan bool
}

// pauseMsg is a message type to be sent across the message channel for
// pausing the sync manager.  This effectively provides the caller with
// exclusive access over the manager until a receive is performed on the
// unpause channel.
type pauseMsg struct {
	unpause <-chan struct{}
}

// HeaderNode is used as a node in a list of headers that are linked together
// between checkpoints.
type HeaderNode struct {
	Height int32
	Hash   *chainhash.Hash
}

type uRootHintMsg struct {
	uRootHint *chaincfg.UtreexoRootHint
	done      <-chan struct{}
}

// peerSyncState stores additional information that the SyncManager tracks
// about a peer.
type peerSyncState struct {
	syncCandidate       bool
	requestQueue        []*wire.InvVect
	requestedTxns       map[chainhash.Hash]struct{}
	requestedBlocks     map[chainhash.Hash]struct{}
	requestedBlocksLock sync.RWMutex
}

// limitAdd is a helper function for maps that require a maximum limit by
// evicting a random value if adding the new value would cause it to
// overflow the maximum allowed.
func limitAdd(m map[chainhash.Hash]struct{}, hash chainhash.Hash, limit int) {
	if len(m)+1 > limit {
		// Remove a random entry from the map.  For most compilers, Go's
		// range statement iterates starting at a random item although
		// that is not 100% guaranteed by the spec.  The iteration order
		// is not important here because an adversary would have to be
		// able to pull off preimage attacks on the hashing function in
		// order to target eviction of specific entries anyways.
		for txHash := range m {
			delete(m, txHash)
			break
		}
	}
	m[hash] = struct{}{}
}

// ValidateParallelUtreexoRoot validates the given utreexo root
func (sm *SyncManager) ValidateParallelUtreexoRoot(startHeight, endHeight int32) error {
	// Eh whatever just say segwitisAcitve and only ask for segwit peers
	segwitActive := true

	var higherPeers, equalPeers []*peerpkg.Peer
	for peer, state := range sm.peerStates {
		if !state.syncCandidate {
			continue
		}

		if segwitActive && !peer.IsWitnessEnabled() {
			log.Debugf("peer %v not witness enabled, skipping", peer)
			continue
		}

		// Remove sync candidate peers that are no longer candidates due
		// to passing their latest known block.  NOTE: The < is
		// intentional as opposed to <=.  While technically the peer
		// doesn't have a later block when it's equal, it will likely
		// have one soon so it is a reasonable choice.  It also allows
		// the case where both are at 0 such as during regression test.
		if peer.LastBlock() < endHeight {
			state.syncCandidate = false
			continue
		}

		// If the peer is at the same height as us, we'll add it a set
		// of backup peers in case we do not find one with a higher
		// height. If we are synced up with all of our peers, all of
		// them will be in this set.
		if peer.LastBlock() == endHeight {
			equalPeers = append(equalPeers, peer)
			continue
		}

		// This peer has a height greater than our own, we'll consider
		// it in the set of better peers from which we'll randomly
		// select.
		higherPeers = append(higherPeers, peer)
	}

	// Pick randomly from the set of peers greater than our block height,
	// falling back to a random peer of the same height if none are greater.
	//
	// TODO(conner): Use a better algorithm to ranking peers based on
	// observed metrics and/or sync in parallel.
	var bestPeer *peerpkg.Peer
	switch {
	case len(higherPeers) > 0:
		bestPeer = higherPeers[rand.Intn(len(higherPeers))]

	case len(equalPeers) > 0:
		bestPeer = equalPeers[rand.Intn(len(equalPeers))]
	}

	// Start syncing from the best peer if one was selected.
	if bestPeer != nil {
		sm.utreexoRootVerifyMode = true
		sm.headersFirstMode = true

		if sm.chainParams != &chaincfg.RegressionNetParams {
			sm.progressLogger.SetLastLogTime(time.Now())
			sm.syncPeer = bestPeer

			sm.fetchParallelVerifyUBlocks(startHeight, endHeight)
		}

		sm.syncPeer = bestPeer

		// Reset the last progress time now that we have a non-nil
		// syncPeer to avoid instantly detecting it as stalled in the
		// event the progress time hasn't been updated recently.
		sm.lastProgressTime = time.Now()
	} else {
		log.Warnf("No sync peer candidates available")
	}

	return nil
}

// ValidateUtreexoRoot validates the given utreexo root
func (sm *SyncManager) ValidateUtreexoRoot() error {
	rootToVerify := sm.utreexoRootToVerify

	// The block height that we wanna verify to
	endHeight := rootToVerify.Height

	// Eh whatever just say segwitisAcitve and only ask for segwit peers
	segwitActive := true

	var higherPeers, equalPeers []*peerpkg.Peer
	for peer, state := range sm.peerStates {
		if !state.syncCandidate {
			continue
		}

		if segwitActive && !peer.IsWitnessEnabled() {
			log.Debugf("peer %v not witness enabled, skipping", peer)
			continue
		}

		// Remove sync candidate peers that are no longer candidates due
		// to passing their latest known block.  NOTE: The < is
		// intentional as opposed to <=.  While technically the peer
		// doesn't have a later block when it's equal, it will likely
		// have one soon so it is a reasonable choice.  It also allows
		// the case where both are at 0 such as during regression test.
		if peer.LastBlock() < endHeight {
			state.syncCandidate = false
			continue
		}

		// If the peer is at the same height as us, we'll add it a set
		// of backup peers in case we do not find one with a higher
		// height. If we are synced up with all of our peers, all of
		// them will be in this set.
		if peer.LastBlock() == endHeight {
			equalPeers = append(equalPeers, peer)
			continue
		}

		// This peer has a height greater than our own, we'll consider
		// it in the set of better peers from which we'll randomly
		// select.
		higherPeers = append(higherPeers, peer)
	}

	// Pick randomly from the set of peers greater than our block height,
	// falling back to a random peer of the same height if none are greater.
	//
	// TODO(conner): Use a better algorithm to ranking peers based on
	// observed metrics and/or sync in parallel.
	var bestPeer *peerpkg.Peer
	switch {
	case len(higherPeers) > 0:
		bestPeer = higherPeers[rand.Intn(len(higherPeers))]

	case len(equalPeers) > 0:
		bestPeer = equalPeers[rand.Intn(len(equalPeers))]
	}

	// Start syncing from the best peer if one was selected.
	if bestPeer != nil {
		sm.utreexoRootVerifyMode = true
		sm.headersFirstMode = true

		locator, err := sm.chain.LatestBlockLocator()
		if err != nil {
			log.Errorf("Failed to get block locator for the "+
				"latest block: %v", err)
			return err
		}

		if sm.chainParams != &chaincfg.RegressionNetParams {
			prevNodeEl := sm.headerList.Back()
			prevNode := prevNodeEl.Value.(*HeaderNode)
			if prevNode.Height >= rootToVerify.Height {
				sm.progressLogger.SetLastLogTime(time.Now())
				sm.syncPeer = bestPeer

				sm.fetchHeaderVerifyUBlocks()
			} else {
				bestPeer.PushGetHeadersMsg(locator, &chainhash.Hash{})
				sm.headersFirstMode = true
				best := sm.chain.BestSnapshot()
				log.Infof("Downloading headers for blocks %d to "+
					"%d from peer %s", best.Height+1,
					rootToVerify.Height, bestPeer.Addr())
			}
		}

		sm.syncPeer = bestPeer

		// Reset the last progress time now that we have a non-nil
		// syncPeer to avoid instantly detecting it as stalled in the
		// event the progress time hasn't been updated recently.
		sm.lastProgressTime = time.Now()
	} else {
		log.Warnf("No sync peer candidates available")
	}

	return nil
}

type uTreeState struct {
	uView        *blockchain.UtreexoViewpoint
	startRoot    *chaincfg.UtreexoRootHint
	rootToVerify *chaincfg.UtreexoRootHint
}

// SyncManager is used to communicate block related messages with peers. The
// SyncManager is started as by executing Start() in a goroutine. Once started,
// it selects peers to sync from and starts the initial block download. Once the
// chain is in sync, the SyncManager handles incoming block and header
// notifications and relays announcements of new blocks to peers.
type SyncManager struct {
	peerNotifier   PeerNotifier
	started        int32
	shutdown       int32
	chain          *blockchain.BlockChain
	txMemPool      *mempool.TxPool
	chainParams    *chaincfg.Params
	progressLogger *blockProgressLogger
	msgChan        chan interface{}
	wg             sync.WaitGroup
	quit           chan struct{}

	// These fields should only be accessed from the blockHandler thread
	rejectedTxns        map[chainhash.Hash]struct{}
	requestedTxns       map[chainhash.Hash]struct{}
	requestedBlocks     map[chainhash.Hash]struct{}
	requestedBlocksLock sync.RWMutex
	syncPeer            *peerpkg.Peer
	peerStates          map[*peerpkg.Peer]*peerSyncState
	peerStatesLock      sync.RWMutex
	lastProgressTime    time.Time

	// The following fields are used for headers-first mode.
	headersFirstMode bool
	headerList       *list.List
	startHeader      *list.Element
	nextCheckpoint   *chaincfg.Checkpoint

	utreexoCSN            bool
	utreexoMN             bool
	utreexoWN             bool
	utreexoRootVerifyMode bool
	utreexoRootToVerify   *chaincfg.UtreexoRootHint
	utreexoStartRoot      *chaincfg.UtreexoRootHint
	newSyncPeer           chan struct{}
	newSyncNum            int8
	uTreeMap              map[int32]*uTreeState
	uTreeMapLock          sync.RWMutex

	// An optional fee estimator.
	feeEstimator *mempool.FeeEstimator
}

func (sm *SyncManager) SetStartHeader() {
	e := sm.headerList.Front()
	sm.startHeader = e
}

func (sm *SyncManager) SetHeaderList(headers *list.List) {
	//if sm.headerList != nil {
	//	return
	//}

	sm.headerList = headers

	e := sm.headerList.Front()
	sm.startHeader = e
}

// resetHeaderState sets the headers-first mode state to values appropriate for
// syncing from a new peer.
func (sm *SyncManager) resetHeaderState(newestHash *chainhash.Hash, newestHeight int32) {
	if sm.utreexoWN {
		log.Infof("resetHeaderState not reseting as this node is a worker node")
		return
	}
	sm.headersFirstMode = false
	sm.headerList.Init()
	sm.startHeader = nil

	// When there is a next checkpoint, add an entry for the latest known
	// block into the header pool.  This allows the next downloaded header
	// to prove it links to the chain properly.
	if sm.nextCheckpoint != nil {
		node := HeaderNode{Height: newestHeight, Hash: newestHash}
		sm.headerList.PushBack(&node)
	}

	// if utreexo mainnode, reset headers
	if sm.utreexoMN {
		// Push back the genesis header to the headerList
		best := sm.chain.BestSnapshot()
		node := HeaderNode{Height: best.Height, Hash: &best.Hash}
		sm.headerList = list.New()
		sm.headerList.PushBack(&node)
	}
}

// findNextHeaderCheckpoint returns the next checkpoint after the passed height.
// It returns nil when there is not one either because the height is already
// later than the final checkpoint or some other reason such as disabled
// checkpoints.
func (sm *SyncManager) findNextHeaderCheckpoint(height int32) *chaincfg.Checkpoint {
	checkpoints := sm.chain.Checkpoints()
	if len(checkpoints) == 0 {
		return nil
	}

	// There is no next checkpoint if the height is already after the final
	// checkpoint.
	finalCheckpoint := &checkpoints[len(checkpoints)-1]
	if height >= finalCheckpoint.Height {
		return nil
	}

	// Find the next checkpoint.
	nextCheckpoint := finalCheckpoint
	for i := len(checkpoints) - 2; i >= 0; i-- {
		if height >= checkpoints[i].Height {
			break
		}
		nextCheckpoint = &checkpoints[i]
	}
	return nextCheckpoint
}

// startSync will choose the best peer among the available candidate peers to
// download/sync the blockchain from.  When syncing is already running, it
// simply returns.  It also examines the candidates for any which are no longer
// candidates and removes them as needed.
func (sm *SyncManager) startSync() {
	// Return now if we're already syncing.
	if sm.syncPeer != nil {
		return
	}

	// If we are verifying a utreexo root range, then call ValidateUtreexoRoot()
	// and return. We keep a separate process for the root range verify
	if sm.utreexoRootVerifyMode {
		log.Infof("node in utreexoRootVerifyMode")
		if sm.utreexoMN {
			sm.ValidateUtreexoRoot()
			return
		}

		// if there's something we were verifying, continue verifying this
		if len(sm.uTreeMap) > 0 {
			for _, uRootHint := range sm.uTreeMap {
				log.Infof("re-queuing uRootHint height at %d",
					uRootHint.rootToVerify.Height)
				sm.QueueURootHint(uRootHint.rootToVerify)
			}
		}
		return
	}

	// Once the segwit soft-fork package has activated, we only
	// want to sync from peers which are witness enabled to ensure
	// that we fully validate all blockchain data.
	segwitActive, err := sm.chain.IsDeploymentActive(chaincfg.DeploymentSegwit)
	if err != nil {
		log.Errorf("Unable to query for segwit soft-fork state: %v", err)
		return
	}

	best := sm.chain.BestSnapshot()
	var higherPeers, equalPeers []*peerpkg.Peer
	for peer, state := range sm.peerStates {
		if !state.syncCandidate {
			continue
		}

		if segwitActive && !peer.IsWitnessEnabled() {
			log.Debugf("peer %v not witness enabled, skipping", peer)
			continue
		}

		// Remove sync candidate peers that are no longer candidates due
		// to passing their latest known block.  NOTE: The < is
		// intentional as opposed to <=.  While technically the peer
		// doesn't have a later block when it's equal, it will likely
		// have one soon so it is a reasonable choice.  It also allows
		// the case where both are at 0 such as during regression test.
		if peer.LastBlock() < best.Height {
			state.syncCandidate = false
			continue
		}

		// If the peer is at the same height as us, we'll add it a set
		// of backup peers in case we do not find one with a higher
		// height. If we are synced up with all of our peers, all of
		// them will be in this set.
		if peer.LastBlock() == best.Height {
			equalPeers = append(equalPeers, peer)
			continue
		}

		// This peer has a height greater than our own, we'll consider
		// it in the set of better peers from which we'll randomly
		// select.
		higherPeers = append(higherPeers, peer)
	}

	// Pick randomly from the set of peers greater than our block height,
	// falling back to a random peer of the same height if none are greater.
	//
	// TODO(conner): Use a better algorithm to ranking peers based on
	// observed metrics and/or sync in parallel.
	var bestPeer *peerpkg.Peer
	switch {
	case len(higherPeers) > 0:
		bestPeer = higherPeers[rand.Intn(len(higherPeers))]

	case len(equalPeers) > 0:
		bestPeer = equalPeers[rand.Intn(len(equalPeers))]
	}

	// Start syncing from the best peer if one was selected.
	if bestPeer != nil {
		// Clear the requestedBlocks if the sync peer changes, otherwise
		// we may ignore blocks we need that the last sync peer failed
		// to send.
		sm.requestedBlocks = make(map[chainhash.Hash]struct{})

		locator, err := sm.chain.LatestBlockLocator()
		if err != nil {
			log.Errorf("Failed to get block locator for the "+
				"latest block: %v", err)
			return
		}

		log.Infof("Syncing to block height %d from peer %v",
			bestPeer.LastBlock(), bestPeer.Addr())

		// When the current height is less than a known checkpoint we
		// can use block headers to learn about which blocks comprise
		// the chain up to the checkpoint and perform less validation
		// for them.  This is possible since each header contains the
		// hash of the previous header and a merkle root.  Therefore if
		// we validate all of the received headers link together
		// properly and the checkpoint hashes match, we can be sure the
		// hashes for the blocks in between are accurate.  Further, once
		// the full blocks are downloaded, the merkle root is computed
		// and compared against the value in the header which proves the
		// full block hasn't been tampered with.
		//
		// Once we have passed the final checkpoint, or checkpoints are
		// disabled, use standard inv messages learn about the blocks
		// and fully validate them.  Finally, regression test mode does
		// not support the headers-first approach so do normal block
		// downloads when in regression test mode.
		if sm.nextCheckpoint != nil &&
			best.Height < sm.nextCheckpoint.Height &&
			sm.chainParams != &chaincfg.RegressionNetParams {

			bestPeer.PushGetHeadersMsg(locator, sm.nextCheckpoint.Hash)
			sm.headersFirstMode = true
			log.Infof("Downloading headers for blocks %d to "+
				"%d from peer %s", best.Height+1,
				sm.nextCheckpoint.Height, bestPeer.Addr())
		} else {
			if sm.utreexoCSN {
				bestPeer.PushGetUBlocksMsg(locator, &zeroHash)
			} else {
				bestPeer.PushGetBlocksMsg(locator, &zeroHash)
			}
		}
		sm.syncPeer = bestPeer

		// Reset the last progress time now that we have a non-nil
		// syncPeer to avoid instantly detecting it as stalled in the
		// event the progress time hasn't been updated recently.
		sm.lastProgressTime = time.Now()
	} else {
		log.Warnf("No sync peer candidates available")
	}
}

// isSyncCandidate returns whether or not the peer is a candidate to consider
// syncing from.
func (sm *SyncManager) isSyncCandidate(peer *peerpkg.Peer) bool {
	// Typically a peer is not a candidate for sync if it's not a full node,
	// however regression test is special in that the regression tool is
	// not a full node and still needs to be considered a sync candidate.
	if sm.chainParams == &chaincfg.RegressionNetParams {
		// The peer is not a candidate if it's not coming from localhost
		// or the hostname can't be determined for some reason.
		host, _, err := net.SplitHostPort(peer.Addr())
		if err != nil {
			return false
		}

		if host != "127.0.0.1" && host != "localhost" {
			return false
		}
	} else {
		// The peer is not a candidate for sync if it's not a full
		// node. Additionally, if the segwit soft-fork package has
		// activated, then the peer must also be upgraded.
		segwitActive, err := sm.chain.IsDeploymentActive(chaincfg.DeploymentSegwit)
		if err != nil {
			log.Errorf("Unable to query for segwit "+
				"soft-fork state: %v", err)
		}
		nodeServices := peer.Services()
		if sm.utreexoCSN {
			if nodeServices&wire.SFNodeUtreexo != wire.SFNodeUtreexo {
				log.Debugf("Peer is not a Utreexo node. Not a sync candidate")
				return false
			}
		} else {
			if nodeServices&wire.SFNodeNetwork != wire.SFNodeNetwork ||
				(segwitActive && !peer.IsWitnessEnabled()) {
				return false
			}
		}
	}

	// Candidate if all checks passed.
	return true
}

// handleNewPeerMsg deals with new peers that have signalled they may
// be considered as a sync peer (they have already successfully negotiated).  It
// also starts syncing if needed.  It is invoked from the syncHandler goroutine.
func (sm *SyncManager) handleNewPeerMsg(peer *peerpkg.Peer) {
	// Ignore if in the process of shutting down.
	if atomic.LoadInt32(&sm.shutdown) != 0 {
		return
	}

	log.Infof("New valid peer %s (%s)", peer, peer.UserAgent())

	// Initialize the peer state
	isSyncCandidate := sm.isSyncCandidate(peer)
	sm.peerStates[peer] = &peerSyncState{
		syncCandidate:   isSyncCandidate,
		requestedTxns:   make(map[chainhash.Hash]struct{}),
		requestedBlocks: make(map[chainhash.Hash]struct{}),
	}

	// Start syncing by choosing the best candidate if needed.
	if isSyncCandidate && sm.syncPeer == nil {
		if sm.newSyncNum == 0 {
			close(sm.newSyncPeer)
			sm.newSyncNum++
		}
		sm.startSync()
	}
}

// handleStallSample will switch to a new sync peer if the current one has
// stalled. This is detected when by comparing the last progress timestamp with
// the current time, and disconnecting the peer if we stalled before reaching
// their highest advertised block.
func (sm *SyncManager) handleStallSample() {
	if atomic.LoadInt32(&sm.shutdown) != 0 {
		return
	}

	// If we don't have an active sync peer, exit early.
	if sm.syncPeer == nil {
		return
	}

	// If the stall timeout has not elapsed, exit early.
	if time.Since(sm.lastProgressTime) <= maxStallDuration {
		return
	}

	// Check to see that the peer's sync state exists.
	state, exists := sm.peerStates[sm.syncPeer]
	if !exists {
		return
	}

	sm.clearRequestedState(state)

	disconnectSyncPeer := sm.shouldDCStalledSyncPeer()
	sm.updateSyncPeer(disconnectSyncPeer)
}

// shouldDCStalledSyncPeer determines whether or not we should disconnect a
// stalled sync peer. If the peer has stalled and its reported height is greater
// than our own best height, we will disconnect it. Otherwise, we will keep the
// peer connected in case we are already at tip.
func (sm *SyncManager) shouldDCStalledSyncPeer() bool {
	lastBlock := sm.syncPeer.LastBlock()
	startHeight := sm.syncPeer.StartingHeight()

	var peerHeight int32
	if lastBlock > startHeight {
		peerHeight = lastBlock
	} else {
		peerHeight = startHeight
	}

	// If we've stalled out yet the sync peer reports having more blocks for
	// us we will disconnect them. This allows us at tip to not disconnect
	// peers when we are equal or they temporarily lag behind us.
	best := sm.chain.BestSnapshot()
	return peerHeight > best.Height
}

// handleDonePeerMsg deals with peers that have signalled they are done.  It
// removes the peer as a candidate for syncing and in the case where it was
// the current sync peer, attempts to select a new best peer to sync from.  It
// is invoked from the syncHandler goroutine.
func (sm *SyncManager) handleDonePeerMsg(peer *peerpkg.Peer) {
	state, exists := sm.peerStates[peer]
	if !exists {
		log.Warnf("Received done peer message for unknown peer %s", peer)
		return
	}

	// Remove the peer from the list of candidate peers.
	delete(sm.peerStates, peer)

	log.Infof("Lost peer %s", peer)

	sm.clearRequestedState(state)

	if peer == sm.syncPeer {
		// Update the sync peer. The server has already disconnected the
		// peer before signaling to the sync manager.
		sm.updateSyncPeer(false)
	}
}

// clearRequestedState wipes all expected transactions and blocks from the sync
// manager's requested maps that were requested under a peer's sync state, This
// allows them to be rerequested by a subsequent sync peer.
func (sm *SyncManager) clearRequestedState(state *peerSyncState) {
	// Remove requested transactions from the global map so that they will
	// be fetched from elsewhere next time we get an inv.
	for txHash := range state.requestedTxns {
		delete(sm.requestedTxns, txHash)
	}

	// Remove requested blocks from the global map so that they will be
	// fetched from elsewhere next time we get an inv.
	// TODO: we could possibly here check which peers have these blocks
	// and request them now to speed things up a little.
	for blockHash := range state.requestedBlocks {
		delete(sm.requestedBlocks, blockHash)
	}
}

// updateSyncPeer choose a new sync peer to replace the current one. If
// dcSyncPeer is true, this method will also disconnect the current sync peer.
// If we are in header first mode, any header state related to prefetching is
// also reset in preparation for the next sync peer.
func (sm *SyncManager) updateSyncPeer(dcSyncPeer bool) {
	log.Debugf("Updating sync peer, no progress for: %v",
		time.Since(sm.lastProgressTime))

	// First, disconnect the current sync peer if requested.
	if dcSyncPeer {
		log.Tracef("updateSyncPeer. disconnect")
		sm.syncPeer.Disconnect()
	}

	// Reset any header state before we choose our next active sync peer.
	if sm.headersFirstMode {
		best := sm.chain.BestSnapshot()
		sm.resetHeaderState(&best.Hash, best.Height)
	}

	sm.syncPeer = nil
	sm.startSync()
}

// handleTxMsg handles transaction messages from all peers.
func (sm *SyncManager) handleTxMsg(tmsg *txMsg) {
	peer := tmsg.peer
	state, exists := sm.peerStates[peer]
	if !exists {
		log.Warnf("Received tx message from unknown peer %s", peer)
		return
	}

	// NOTE:  BitcoinJ, and possibly other wallets, don't follow the spec of
	// sending an inventory message and allowing the remote peer to decide
	// whether or not they want to request the transaction via a getdata
	// message.  Unfortunately, the reference implementation permits
	// unrequested data, so it has allowed wallets that don't follow the
	// spec to proliferate.  While this is not ideal, there is no check here
	// to disconnect peers for sending unsolicited transactions to provide
	// interoperability.
	txHash := tmsg.tx.Hash()

	// Ignore transactions that we have already rejected.  Do not
	// send a reject message here because if the transaction was already
	// rejected, the transaction was unsolicited.
	if _, exists = sm.rejectedTxns[*txHash]; exists {
		log.Debugf("Ignoring unsolicited previously rejected "+
			"transaction %v from %s", txHash, peer)
		return
	}

	// Process the transaction to include validation, insertion in the
	// memory pool, orphan handling, etc.
	acceptedTxs, err := sm.txMemPool.ProcessTransaction(tmsg.tx,
		true, true, mempool.Tag(peer.ID()))

	// Remove transaction from request maps. Either the mempool/chain
	// already knows about it and as such we shouldn't have any more
	// instances of trying to fetch it, or we failed to insert and thus
	// we'll retry next time we get an inv.
	delete(state.requestedTxns, *txHash)
	delete(sm.requestedTxns, *txHash)

	if err != nil {
		// Do not request this transaction again until a new block
		// has been processed.
		limitAdd(sm.rejectedTxns, *txHash, maxRejectedTxns)

		// When the error is a rule error, it means the transaction was
		// simply rejected as opposed to something actually going wrong,
		// so log it as such.  Otherwise, something really did go wrong,
		// so log it as an actual error.
		if _, ok := err.(mempool.RuleError); ok {
			log.Debugf("Rejected transaction %v from %s: %v",
				txHash, peer, err)
		} else {
			log.Errorf("Failed to process transaction %v: %v",
				txHash, err)
		}

		// Convert the error into an appropriate reject message and
		// send it.
		code, reason := mempool.ErrToRejectErr(err)
		peer.PushRejectMsg(wire.CmdTx, code, reason, txHash, false)
		return
	}

	sm.peerNotifier.AnnounceNewTransactions(acceptedTxs)
}

// current returns true if we believe we are synced with our peers, false if we
// still have blocks to check
func (sm *SyncManager) current() bool {
	if !sm.chain.IsCurrent() {
		return false
	}

	// if blockChain thinks we are current and we have no syncPeer it
	// is probably right.
	if sm.syncPeer == nil {
		return true
	}

	// No matter what chain thinks, if we are below the block we are syncing
	// to we are not current.
	if sm.chain.BestSnapshot().Height < sm.syncPeer.LastBlock() {
		return false
	}
	return true
}

// handleBlockMsg handles block messages from all peers.
func (sm *SyncManager) handleBlockMsg(bmsg *blockMsg) {
	peer := bmsg.peer
	state, exists := sm.peerStates[peer]
	if !exists {
		log.Warnf("Received block message from unknown peer %s", peer)
		return
	}

	// If we didn't ask for this block then the peer is misbehaving.
	blockHash := bmsg.block.Hash()
	if _, exists = state.requestedBlocks[*blockHash]; !exists {
		// The regression test intentionally sends some blocks twice
		// to test duplicate block insertion fails.  Don't disconnect
		// the peer or ignore the block when we're in regression test
		// mode in this case so the chain code is actually fed the
		// duplicate blocks.
		if sm.chainParams != &chaincfg.RegressionNetParams {
			log.Warnf("Got unrequested block %v from %s -- "+
				"disconnecting", blockHash, peer.Addr())
			peer.Disconnect()
			return
		}
	}

	if sm.utreexoCSN {
		log.Warnf("Got unrequested block (not a ublock) %v from %s -- "+
			"ignoring block", blockHash, peer.Addr())
		return
	}

	// When in headers-first mode, if the block matches the hash of the
	// first header in the list of headers that are being fetched, it's
	// eligible for less validation since the headers have already been
	// verified to link together and are valid up to the next checkpoint.
	// Also, remove the list entry for all blocks except the checkpoint
	// since it is needed to verify the next round of headers links
	// properly.
	isCheckpointBlock := false
	behaviorFlags := blockchain.BFNone
	if sm.headersFirstMode {
		firstNodeEl := sm.headerList.Front()
		if firstNodeEl != nil {
			firstNode := firstNodeEl.Value.(*HeaderNode)
			if blockHash.IsEqual(firstNode.Hash) {
				behaviorFlags |= blockchain.BFFastAdd
				if firstNode.Hash.IsEqual(sm.nextCheckpoint.Hash) {
					isCheckpointBlock = true
				} else {
					sm.headerList.Remove(firstNodeEl)
				}
			}
		}
	}
	// Remove block from request maps. Either chain will know about it and
	// so we shouldn't have any more instances of trying to fetch it, or we
	// will fail the insert and thus we'll retry next time we get an inv.
	delete(state.requestedBlocks, *blockHash)
	delete(sm.requestedBlocks, *blockHash)

	// Process the block to include validation, best chain selection, orphan
	// handling, etc.
	_, isOrphan, err := sm.chain.ProcessBlock(bmsg.block, behaviorFlags)
	if err != nil {
		// When the error is a rule error, it means the block was simply
		// rejected as opposed to something actually going wrong, so log
		// it as such.  Otherwise, something really did go wrong, so log
		// it as an actual error.
		if _, ok := err.(blockchain.RuleError); ok {
			log.Infof("Rejected block %v from %s: %v", blockHash,
				peer, err)
		} else {
			log.Errorf("Failed to process block %v: %v",
				blockHash, err)
		}
		if dbErr, ok := err.(database.Error); ok && dbErr.ErrorCode ==
			database.ErrCorruption {
			panic(dbErr)
		}

		// Convert the error into an appropriate reject message and
		// send it.
		code, reason := mempool.ErrToRejectErr(err)
		peer.PushRejectMsg(wire.CmdBlock, code, reason, blockHash, false)
		return
	}

	// Meta-data about the new block this peer is reporting. We use this
	// below to update this peer's latest block height and the heights of
	// other peers based on their last announced block hash. This allows us
	// to dynamically update the block heights of peers, avoiding stale
	// heights when looking for a new sync peer. Upon acceptance of a block
	// or recognition of an orphan, we also use this information to update
	// the block heights over other peers who's invs may have been ignored
	// if we are actively syncing while the chain is not yet current or
	// who may have lost the lock announcement race.
	var heightUpdate int32
	var blkHashUpdate *chainhash.Hash

	// Request the parents for the orphan block from the peer that sent it.
	if isOrphan {
		// We've just received an orphan block from a peer. In order
		// to update the height of the peer, we try to extract the
		// block height from the scriptSig of the coinbase transaction.
		// Extraction is only attempted if the block's version is
		// high enough (ver 2+).
		header := &bmsg.block.MsgBlock().Header
		if blockchain.ShouldHaveSerializedBlockHeight(header) {
			coinbaseTx := bmsg.block.Transactions()[0]
			cbHeight, err := blockchain.ExtractCoinbaseHeight(coinbaseTx)
			if err != nil {
				log.Warnf("Unable to extract height from "+
					"coinbase tx: %v", err)
			} else {
				log.Debugf("Extracted height of %v from "+
					"orphan block", cbHeight)
				heightUpdate = cbHeight
				blkHashUpdate = blockHash
			}
		}

		orphanRoot := sm.chain.GetOrphanRoot(blockHash, false)
		locator, err := sm.chain.LatestBlockLocator()
		if err != nil {
			log.Warnf("Failed to get block locator for the "+
				"latest block: %v", err)
		} else {
			peer.PushGetBlocksMsg(locator, orphanRoot)
		}
	} else {
		if peer == sm.syncPeer {
			sm.lastProgressTime = time.Now()
		}

		// When the block is not an orphan, log information about it and
		// update the chain state.
		sm.progressLogger.LogBlockHeight(bmsg.block, sm.chain)

		// Update this peer's latest block height, for future
		// potential sync node candidacy.
		best := sm.chain.BestSnapshot()
		heightUpdate = best.Height
		blkHashUpdate = &best.Hash

		// Clear the rejected transactions.
		sm.rejectedTxns = make(map[chainhash.Hash]struct{})
	}

	// Update the block height for this peer. But only send a message to
	// the server for updating peer heights if this is an orphan or our
	// chain is "current". This avoids sending a spammy amount of messages
	// if we're syncing the chain from scratch.
	if blkHashUpdate != nil && heightUpdate != 0 {
		peer.UpdateLastBlockHeight(heightUpdate)
		if isOrphan || sm.current() {
			go sm.peerNotifier.UpdatePeerHeights(blkHashUpdate, heightUpdate,
				peer)
		}
	}

	// Nothing more to do if we aren't in headers-first mode.
	if !sm.headersFirstMode {
		return
	}

	// This is headers-first mode, so if the block is not a checkpoint
	// request more blocks using the header list when the request queue is
	// getting short.
	if !isCheckpointBlock {
		if sm.startHeader != nil &&
			len(state.requestedBlocks) < minInFlightBlocks {
			sm.fetchHeaderBlocks()
		}
		return
	}

	// This is headers-first mode and the block is a checkpoint.  When
	// there is a next checkpoint, get the next round of headers by asking
	// for headers starting from the block after this one up to the next
	// checkpoint.
	prevHeight := sm.nextCheckpoint.Height
	prevHash := sm.nextCheckpoint.Hash
	sm.nextCheckpoint = sm.findNextHeaderCheckpoint(prevHeight)
	if sm.nextCheckpoint != nil {
		locator := blockchain.BlockLocator([]*chainhash.Hash{prevHash})
		err := peer.PushGetHeadersMsg(locator, sm.nextCheckpoint.Hash)
		if err != nil {
			log.Warnf("Failed to send getheaders message to "+
				"peer %s: %v", peer.Addr(), err)
			return
		}
		log.Infof("Downloading headers for blocks %d to %d from "+
			"peer %s", prevHeight+1, sm.nextCheckpoint.Height,
			sm.syncPeer.Addr())
		return
	}

	// This is headers-first mode, the block is a checkpoint, and there are
	// no more checkpoints, so switch to normal mode by requesting blocks
	// from the block after this one up to the end of the chain (zero hash).
	sm.headersFirstMode = false
	sm.headerList.Init()
	log.Infof("Reached the final checkpoint -- switching to normal mode")
	locator := blockchain.BlockLocator([]*chainhash.Hash{blockHash})
	err = peer.PushGetBlocksMsg(locator, &zeroHash)
	if err != nil {
		log.Warnf("Failed to send getblocks message to peer %s: %v",
			peer.Addr(), err)
		return
	}
}

// TODO kcalvinalvin: It's really mostly the same procedure with a regular block
// This isn't the prettiest way
func (sm *SyncManager) handleUBlockMsg(ubmsg *ublockMsg) {
	peer := ubmsg.peer
	state, exists := sm.peerStates[peer]
	if !exists {
		log.Warnf("Received ublock message from unknown peer %s", peer)
		return
	}

	// If we didn't ask for this block then the peer is misbehaving.
	blockHash := ubmsg.ublock.Hash()
	if _, exists = state.requestedBlocks[*blockHash]; !exists {
		// The regression test intentionally sends some blocks twice
		// to test duplicate block insertion fails.  Don't disconnect
		// the peer or ignore the block when we're in regression test
		// mode in this case so the chain code is actually fed the
		// duplicate blocks.
		if sm.chainParams != &chaincfg.RegressionNetParams {
			log.Warnf("Got unrequested ublock %v from %s -- "+
				"disconnecting", blockHash, peer.Addr())
			peer.Disconnect()
			return
		}
	}

	// When in headers-first mode, if the block matches the hash of the
	// first header in the list of headers that are being fetched, it's
	// eligible for less validation since the headers have already been
	// verified to link together and are valid up to the next checkpoint.
	// Also, remove the list entry for all blocks except the checkpoint
	// since it is needed to verify the next round of headers links
	// properly.
	isCheckpointBlock := false
	behaviorFlags := blockchain.BFNone
	if sm.headersFirstMode {
		firstNodeEl := sm.headerList.Front()
		if firstNodeEl != nil {
			firstNode := firstNodeEl.Value.(*HeaderNode)
			if blockHash.IsEqual(firstNode.Hash) {
				behaviorFlags |= blockchain.BFFastAdd
				if firstNode.Hash.IsEqual(sm.nextCheckpoint.Hash) {
					isCheckpointBlock = true
				} else {
					sm.headerList.Remove(firstNodeEl)
				}
			}
		}
	}
	// Remove block from request maps. Either chain will know about it and
	// so we shouldn't have any more instances of trying to fetch it, or we
	// will fail the insert and thus we'll retry next time we get an inv.
	delete(state.requestedBlocks, *blockHash)
	delete(sm.requestedBlocks, *blockHash)

	// Process the block to include validation, best chain selection, orphan
	// handling, etc.
	_, isOrphan, err := sm.chain.ProcessUBlock(ubmsg.ublock, behaviorFlags)
	if err != nil {
		// When the error is a rule error, it means the block was simply
		// rejected as opposed to something actually going wrong, so log
		// it as such.  Otherwise, something really did go wrong, so log
		// it as an actual error.
		if _, ok := err.(blockchain.RuleError); ok {
			log.Infof("Rejected ublock %v from %s: %v", blockHash,
				peer, err)
		} else {
			log.Errorf("Failed to process ublock %v: %v",
				blockHash, err)
		}
		if dbErr, ok := err.(database.Error); ok && dbErr.ErrorCode ==
			database.ErrCorruption {
			panic(dbErr)
		}

		// Convert the error into an appropriate reject message and
		// send it.
		code, reason := mempool.ErrToRejectErr(err)
		peer.PushRejectMsg(wire.CmdUBlock, code, reason, blockHash, false)
		return
	}

	// These two if statements are for logging the time for when these blocks are verified
	if *ubmsg.ublock.Hash() == [32]byte{
		0xdd, 0x2c, 0xe8, 0xb0, 0x29, 0x3b, 0xc1, 0x66,
		0x29, 0x88, 0x86, 0x54, 0xdd, 0x3a, 0xed, 0x5b,
		0x64, 0xaa, 0x1f, 0xdd, 0x4a, 0xfc, 0xb, 0x0,
		0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0} {
		log.Infof("PROCESSED BLOCK 0000000000000000000bfc4add1faa645bed3add5486882966c13b29b0e82cdd" +
			"at height 667000 on mainnet")
	}

	if *ubmsg.ublock.Hash() == [32]byte{
		0xd0, 0x87, 0x87, 0xa3, 0x5f, 0x1a, 0x4, 0xba,
		0x5, 0x7b, 0x6c, 0xc7, 0xf2, 0xcf, 0xfc, 0xd5,
		0x73, 0x64, 0x23, 0xfd, 0x98, 0x5b, 0x68, 0xb0,
		0xb, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0,
	} {
		log.Infof("PROCESSED BLOCK 000000000000000bb0685b98fd236473d5fccff2c76c7b05ba041a5fa38787d0 at height 1906000 on testnet3")
	}

	// Meta-data about the new block this peer is reporting. We use this
	// below to update this peer's latest block height and the heights of
	// other peers based on their last announced block hash. This allows us
	// to dynamically update the block heights of peers, avoiding stale
	// heights when looking for a new sync peer. Upon acceptance of a block
	// or recognition of an orphan, we also use this information to update
	// the block heights over other peers who's invs may have been ignored
	// if we are actively syncing while the chain is not yet current or
	// who may have lost the lock announcement race.
	var heightUpdate int32
	var blkHashUpdate *chainhash.Hash

	// Request the parents for the orphan block from the peer that sent it.
	if isOrphan {
		// We've just received an orphan block from a peer. In order
		// to update the height of the peer, we try to extract the
		// block height from the scriptSig of the coinbase transaction.
		// Extraction is only attempted if the block's version is
		// high enough (ver 2+).
		header := &ubmsg.ublock.Block().MsgBlock().Header
		if blockchain.ShouldHaveSerializedBlockHeight(header) {
			coinbaseTx := ubmsg.ublock.Block().Transactions()[0]
			cbHeight, err := blockchain.ExtractCoinbaseHeight(coinbaseTx)
			if err != nil {
				log.Warnf("Unable to extract height from "+
					"coinbase tx: %v", err)
			} else {
				log.Debugf("Extracted height of %v from "+
					"orphan block", cbHeight)
				heightUpdate = cbHeight
				blkHashUpdate = blockHash
			}
		}

		orphanRoot := sm.chain.GetOrphanRoot(blockHash, true)
		locator, err := sm.chain.LatestBlockLocator()
		if err != nil {
			log.Warnf("Failed to get block locator for the "+
				"latest block: %v", err)
		} else {
			peer.PushGetUBlocksMsg(locator, orphanRoot)
		}
	} else {
		if peer == sm.syncPeer {
			sm.lastProgressTime = time.Now()
		}

		// Something for compatibility with the existing LogBlockHeight method
		block := ubmsg.ublock.Block()

		// When the block is not an orphan, log information about it and
		// update the chain state.
		sm.progressLogger.LogBlockHeight(block, sm.chain)

		// Update this peer's latest block height, for future
		// potential sync node candidacy.
		best := sm.chain.BestSnapshot()
		heightUpdate = best.Height
		blkHashUpdate = &best.Hash

		// Clear the rejected transactions.
		sm.rejectedTxns = make(map[chainhash.Hash]struct{})
	}

	// Update the block height for this peer. But only send a message to
	// the server for updating peer heights if this is an orphan or our
	// chain is "current". This avoids sending a spammy amount of messages
	// if we're syncing the chain from scratch.
	if blkHashUpdate != nil && heightUpdate != 0 {
		peer.UpdateLastBlockHeight(heightUpdate)
		if isOrphan || sm.current() {
			go sm.peerNotifier.UpdatePeerHeights(blkHashUpdate, heightUpdate,
				peer)
		}
	}

	// If we are not in headers first mode, it's a good time to periodically
	// flush the blockchain cache because we don't expect new blocks immediately.
	// After that, there is nothing more to do.
	if !sm.headersFirstMode {
		if err := sm.chain.FlushCachedState(blockchain.FlushPeriodic); err != nil {
			log.Errorf("Error while flushing the blockchain cache: %v", err)
		}
		return
	}

	// This is headers-first mode, so if the block is not a checkpoint
	// request more blocks using the header list when the request queue is
	// getting short.
	if !isCheckpointBlock {
		if sm.startHeader != nil &&
			len(state.requestedBlocks) < minInFlightBlocks {
			sm.fetchHeaderUBlocks()
		}
		return
	}

	// This is headers-first mode and the block is a checkpoint.  When
	// there is a next checkpoint, get the next round of headers by asking
	// for headers starting from the block after this one up to the next
	// checkpoint.
	prevHeight := sm.nextCheckpoint.Height
	prevHash := sm.nextCheckpoint.Hash
	sm.nextCheckpoint = sm.findNextHeaderCheckpoint(prevHeight)
	if sm.nextCheckpoint != nil {
		locator := blockchain.BlockLocator([]*chainhash.Hash{prevHash})
		err := peer.PushGetHeadersMsg(locator, sm.nextCheckpoint.Hash)
		if err != nil {
			log.Warnf("Failed to send getheaders message to "+
				"peer %s: %v", peer.Addr(), err)
			return
		}
		log.Infof("Downloading headers for ublocks %d to %d from "+
			"peer %s", prevHeight+1, sm.nextCheckpoint.Height,
			sm.syncPeer.Addr())
		return
	}

	// This is headers-first mode, the block is a checkpoint, and there are
	// no more checkpoints, so switch to normal mode by requesting blocks
	// from the block after this one up to the end of the chain (zero hash).
	sm.headersFirstMode = false
	sm.headerList.Init()
	log.Infof("Reached the final checkpoint -- switching to normal mode")
	locator := blockchain.BlockLocator([]*chainhash.Hash{blockHash})
	err = peer.PushGetUBlocksMsg(locator, &zeroHash)
	if err != nil {
		log.Warnf("Failed to send getublocks message to peer %s: %v",
			peer.Addr(), err)
		return
	}

}

// fetchHeaderBlocks creates and sends a request to the syncPeer for the next
// list of blocks to be downloaded based on the current list of headers.
func (sm *SyncManager) fetchHeaderBlocks() {
	// Nothing to do if there is no start header.
	if sm.startHeader == nil {
		log.Warnf("fetchHeaderBlocks called with no start header")
		return
	}

	// Build up a getdata request for the list of blocks the headers
	// describe.  The size hint will be limited to wire.MaxInvPerMsg by
	// the function, so no need to double check it here.
	gdmsg := wire.NewMsgGetDataSizeHint(uint(sm.headerList.Len()))
	numRequested := 0
	for e := sm.startHeader; e != nil; e = e.Next() {
		node, ok := e.Value.(*HeaderNode)
		if !ok {
			log.Warn("Header list node type is not a headerNode")
			continue
		}

		iv := wire.NewInvVect(wire.InvTypeBlock, node.Hash)
		haveInv, err := sm.haveInventory(iv)
		if err != nil {
			log.Warnf("Unexpected failure when checking for "+
				"existing inventory during header block "+
				"fetch: %v", err)
		}
		if !haveInv {
			syncPeerState := sm.peerStates[sm.syncPeer]

			sm.requestedBlocks[*node.Hash] = struct{}{}
			syncPeerState.requestedBlocks[*node.Hash] = struct{}{}

			// If we're fetching from a witness enabled peer
			// post-fork, then ensure that we receive all the
			// witness data in the blocks.
			if sm.syncPeer.IsWitnessEnabled() {
				iv.Type = wire.InvTypeWitnessBlock
			}

			gdmsg.AddInvVect(iv)
			numRequested++
		}
		sm.startHeader = e.Next()
		if numRequested >= wire.MaxInvPerMsg {
			break
		}
	}
	if len(gdmsg.InvList) > 0 {
		sm.syncPeer.QueueMessage(gdmsg, nil)
	}
}

// fetchParallelUBlocks creates and sends a request to the syncPeer for the next
// list of blocks to be downloaded based on the current list of headers.
func (sm *SyncManager) fetchParallelVerifyUBlocks(start, end int32) {
	startHeader := sm.headerList.Front()

	// Build up a getdata request for the list of blocks the headers
	// describe.  The size hint will be limited to wire.MaxInvPerMsg by
	// the function, so no need to double check it here.
	gdmsg := wire.NewMsgGetDataSizeHint(uint(sm.headerList.Len()))
	numRequested := 0
	for e := startHeader; e != nil; e = e.Next() {
		node, ok := e.Value.(*HeaderNode)
		if !ok {
			log.Warn("Header list node type is not a headerNode")
			continue
		}

		// skip all the blocks that are less or greater than the height
		if node.Height <= start {
			continue
		}
		if node.Height > end {
			break
		}
		iv := wire.NewInvVect(wire.InvTypeUBlock, node.Hash)

		sm.peerStatesLock.RLock()
		syncPeerState := sm.peerStates[sm.syncPeer]
		sm.peerStatesLock.RUnlock()

		sm.requestedBlocksLock.Lock()
		sm.requestedBlocks[*node.Hash] = struct{}{}
		sm.requestedBlocksLock.Unlock()

		syncPeerState.requestedBlocksLock.Lock()
		syncPeerState.requestedBlocks[*node.Hash] = struct{}{}
		syncPeerState.requestedBlocksLock.Unlock()

		// If we're fetching from a witness enabled peer
		// post-fork, then ensure that we receive all the
		// witness data in the blocks.
		if sm.syncPeer.IsWitnessEnabled() {
			if sm.utreexoCSN {
				iv.Type = wire.InvTypeWitnessUBlock
			} else {
				iv.Type = wire.InvTypeWitnessBlock
			}
		}

		gdmsg.AddInvVect(iv)
		numRequested++

		startHeader = e.Next()
		if numRequested >= wire.MaxInvPerMsg {
			break
		}
	}
	if len(gdmsg.InvList) > 0 {
		sm.syncPeer.QueueMessage(gdmsg, nil)
	}
}

// fetchHeaderUBlocks creates and sends a request to the syncPeer for the next
// list of blocks to be downloaded based on the current list of headers.
func (sm *SyncManager) fetchHeaderVerifyUBlocks() {
	// Nothing to do if there is no start header.
	if sm.startHeader == nil {
		log.Warnf("fetchHeaderUBlocks called with no start header")
		return
	}

	prevURoot := sm.chain.FindPreviousUtreexoRootHint(sm.utreexoRootToVerify.Height)

	// Build up a getdata request for the list of blocks the headers
	// describe.  The size hint will be limited to wire.MaxInvPerMsg by
	// the function, so no need to double check it here.
	gdmsg := wire.NewMsgGetDataSizeHint(uint(sm.headerList.Len()))
	numRequested := 0
	for e := sm.startHeader; e != nil; e = e.Next() {
		node, ok := e.Value.(*HeaderNode)
		if !ok {
			log.Warn("Header list node type is not a headerNode")
			continue
		}

		// If we have a root that we're doing the verify from, then
		// skip all the blocks that are less than the height for the
		// root.
		if sm.utreexoRootToVerify != nil {
			// prevURoot is nil if we're verifying the very first
			// utreexo root hint. This is because we'll be starting from genesis block.
			if prevURoot == nil {
				if node.Height > sm.utreexoRootToVerify.Height {
					break
				}
			} else {
				if node.Height <= prevURoot.Height {
					continue
				}
				if node.Height > sm.utreexoRootToVerify.Height {
					break
				}
			}
		}
		iv := wire.NewInvVect(wire.InvTypeUBlock, node.Hash)
		syncPeerState := sm.peerStates[sm.syncPeer]

		sm.requestedBlocks[*node.Hash] = struct{}{}
		syncPeerState.requestedBlocks[*node.Hash] = struct{}{}

		// If we're fetching from a witness enabled peer
		// post-fork, then ensure that we receive all the
		// witness data in the blocks.
		if sm.syncPeer.IsWitnessEnabled() {
			if sm.utreexoCSN {
				iv.Type = wire.InvTypeWitnessUBlock
			} else {
				iv.Type = wire.InvTypeWitnessBlock
			}
		}

		gdmsg.AddInvVect(iv)
		numRequested++

		sm.startHeader = e.Next()
		if numRequested >= wire.MaxInvPerMsg {
			break
		}
	}
	if len(gdmsg.InvList) > 0 {
		sm.syncPeer.QueueMessage(gdmsg, nil)
	}
}

// fetchHeaderUBlocks creates and sends a request to the syncPeer for the next
// list of blocks to be downloaded based on the current list of headers.
func (sm *SyncManager) fetchHeaderUBlocks() {
	// Nothing to do if there is no start header.
	if sm.startHeader == nil {
		log.Warnf("fetchHeaderUBlocks called with no start header")
		return
	}

	// Build up a getdata request for the list of blocks the headers
	// describe.  The size hint will be limited to wire.MaxInvPerMsg by
	// the function, so no need to double check it here.
	gdmsg := wire.NewMsgGetDataSizeHint(uint(sm.headerList.Len()))
	numRequested := 0
	for e := sm.startHeader; e != nil; e = e.Next() {
		node, ok := e.Value.(*HeaderNode)
		if !ok {
			log.Warn("Header list node type is not a headerNode")
			continue
		}

		iv := wire.NewInvVect(wire.InvTypeUBlock, node.Hash)
		haveInv, err := sm.haveInventory(iv)
		if err != nil {
			log.Warnf("Unexpected failure when checking for "+
				"existing inventory during header block "+
				"fetch: %v", err)
		}
		if !haveInv {
			syncPeerState := sm.peerStates[sm.syncPeer]

			sm.requestedBlocks[*node.Hash] = struct{}{}
			syncPeerState.requestedBlocks[*node.Hash] = struct{}{}

			// If we're fetching from a witness enabled peer
			// post-fork, then ensure that we receive all the
			// witness data in the blocks.
			if sm.syncPeer.IsWitnessEnabled() {
				if sm.utreexoCSN {
					iv.Type = wire.InvTypeWitnessUBlock
				} else {
					iv.Type = wire.InvTypeWitnessBlock
				}
			}

			gdmsg.AddInvVect(iv)
			numRequested++
		}
		sm.startHeader = e.Next()
		if numRequested >= wire.MaxInvPerMsg {
			break
		}
	}
	if len(gdmsg.InvList) > 0 {
		sm.syncPeer.QueueMessage(gdmsg, nil)
	}
}

// handleOnlyHeadersMsg handles block header messages from all peers.  Headers are
// requested when performing a headers-first sync.
func (sm *SyncManager) handleOnlyHeadersMsg(hmsg *headersMsg) bool {
	peer := hmsg.peer
	_, exists := sm.peerStates[peer]
	if !exists {
		log.Warnf("Received headers message from unknown peer %s", peer)
		return false
	}

	var flags blockchain.BehaviorFlags = blockchain.BFNone
	err := sm.chain.ProcessHeaders(hmsg.headers, sm.utreexoStartRoot, flags)
	if err != nil {
		log.Warnf("Got invalid headers from %s -- "+
			"disconnecting", peer.Addr())
		peer.Disconnect()
		return false
	}

	var finalHash *chainhash.Hash
	receivedAllHeaders := false
	for _, blockHeader := range hmsg.headers.Headers {
		blockHash := blockHeader.BlockHash()
		finalHash = &blockHash

		// Ensure there is a previous header to compare against.
		prevNodeEl := sm.headerList.Back()
		if prevNodeEl == nil {
			log.Warnf("Header list does not contain a previous" +
				"element as expected -- disconnecting peer")
			peer.Disconnect()
			return false
		}
		// Ensure the header properly connects to the previous one and
		// add it to the list of headers.
		node := HeaderNode{Hash: &blockHash}
		prevNode := prevNodeEl.Value.(*HeaderNode)
		if prevNode.Hash.IsEqual(&blockHeader.PrevBlock) {
			node.Height = prevNode.Height + 1
			e := sm.headerList.PushBack(&node)
			if sm.startHeader == nil {
				sm.startHeader = e
			}
		} else {
			log.Warnf("Received block header that does not "+
				"properly connect to the chain from peer %s "+
				"-- disconnecting", peer.Addr())
			peer.Disconnect()
			return false
		}

		if node.Height == sm.utreexoRootToVerify.Height {
			receivedAllHeaders = true
			log.Infof("Downloaded all headers "+
				"to root being verified at height "+
				"%d/hash %s", node.Height, node.Hash)
		}
	}

	if receivedAllHeaders {
		// Since the first entry of the list is always the final block
		// that is already in the database and is only used to ensure
		// the next header links properly, it must be removed before
		// fetching the blocks.
		//sm.headerList.Remove(sm.headerList.Front())
		//log.Infof("Received %v block headers: Fetching blocks",
		//	sm.headerList.Len())
		//sm.progressLogger.SetLastLogTime(time.Now())
		//sm.fetchHeaderVerifyUBlocks()
		return true
	}

	locator := blockchain.BlockLocator([]*chainhash.Hash{finalHash})
	err = peer.PushGetHeadersMsg(locator, &chainhash.Hash{})
	if err != nil {
		log.Warnf("Failed to send getheaders message to "+
			"peer %s: %v", peer.Addr(), err)
		return false

	}

	return false
}

// handleHeadersMsg handles block header messages from all peers.  Headers are
// requested when performing a headers-first sync.
func (sm *SyncManager) handleHeadersMsg(hmsg *headersMsg) {
	peer := hmsg.peer
	_, exists := sm.peerStates[peer]
	if !exists {
		log.Warnf("Received headers message from unknown peer %s", peer)
		return
	}

	// The remote peer is misbehaving if we didn't request headers.
	msg := hmsg.headers
	numHeaders := len(msg.Headers)
	if !sm.headersFirstMode {
		log.Warnf("Got %d unrequested headers from %s -- "+
			"disconnecting", numHeaders, peer.Addr())
		peer.Disconnect()
		return
	}

	// Nothing to do for an empty headers message.
	if numHeaders == 0 {
		return
	}

	// Process all of the received headers ensuring each one connects to the
	// previous and that checkpoints match.
	receivedCheckpoint := false
	var finalHash *chainhash.Hash
	for _, blockHeader := range msg.Headers {
		blockHash := blockHeader.BlockHash()
		finalHash = &blockHash

		// Ensure there is a previous header to compare against.
		prevNodeEl := sm.headerList.Back()
		if prevNodeEl == nil {
			log.Warnf("Header list does not contain a previous" +
				"element as expected -- disconnecting peer")
			peer.Disconnect()
			return
		}

		// Ensure the header properly connects to the previous one and
		// add it to the list of headers.
		node := HeaderNode{Hash: &blockHash}
		prevNode := prevNodeEl.Value.(*HeaderNode)
		if prevNode.Hash.IsEqual(&blockHeader.PrevBlock) {
			node.Height = prevNode.Height + 1
			e := sm.headerList.PushBack(&node)
			if sm.startHeader == nil {
				sm.startHeader = e
			}
		} else {
			log.Warnf("Received block header that does not "+
				"properly connect to the chain from peer %s "+
				"-- disconnecting", peer.Addr())
			peer.Disconnect()
			return
		}

		// Verify the header at the next checkpoint height matches.
		if node.Height == sm.nextCheckpoint.Height {
			if node.Hash.IsEqual(sm.nextCheckpoint.Hash) {
				receivedCheckpoint = true
				log.Infof("Verified downloaded block "+
					"header against checkpoint at height "+
					"%d/hash %s", node.Height, node.Hash)
			} else {
				log.Warnf("Block header at height %d/hash "+
					"%s from peer %s does NOT match "+
					"expected checkpoint hash of %s -- "+
					"disconnecting", node.Height,
					node.Hash, peer.Addr(),
					sm.nextCheckpoint.Hash)
				peer.Disconnect()
				return
			}
			break
		}
	}

	// When this header is a checkpoint, switch to fetching the blocks for
	// all of the headers since the last checkpoint.
	if receivedCheckpoint {
		// Since the first entry of the list is always the final block
		// that is already in the database and is only used to ensure
		// the next header links properly, it must be removed before
		// fetching the blocks.
		sm.headerList.Remove(sm.headerList.Front())
		log.Infof("Received %v block headers: Fetching blocks",
			sm.headerList.Len())
		sm.progressLogger.SetLastLogTime(time.Now())
		if sm.utreexoCSN {
			sm.fetchHeaderUBlocks()
		} else {
			sm.fetchHeaderBlocks()
		}
		return
	}

	// This header is not a checkpoint, so request the next batch of
	// headers starting from the latest known header and ending with the
	// next checkpoint.
	locator := blockchain.BlockLocator([]*chainhash.Hash{finalHash})
	err := peer.PushGetHeadersMsg(locator, sm.nextCheckpoint.Hash)
	if err != nil {
		log.Warnf("Failed to send getheaders message to "+
			"peer %s: %v", peer.Addr(), err)
		return
	}
}

// handleNotFoundMsg handles notfound messages from all peers.
func (sm *SyncManager) handleNotFoundMsg(nfmsg *notFoundMsg) {
	peer := nfmsg.peer
	state, exists := sm.peerStates[peer]
	if !exists {
		log.Warnf("Received notfound message from unknown peer %s", peer)
		return
	}
	for _, inv := range nfmsg.notFound.InvList {
		// verify the hash was actually announced by the peer
		// before deleting from the global requested maps.
		switch inv.Type {
		case wire.InvTypeWitnessBlock:
			fallthrough
		case wire.InvTypeBlock:
			if _, exists := state.requestedBlocks[inv.Hash]; exists {
				delete(state.requestedBlocks, inv.Hash)
				delete(sm.requestedBlocks, inv.Hash)
			}
		case wire.InvTypeWitnessUBlock:
			fallthrough
		case wire.InvTypeUBlock:
			if _, exists := state.requestedBlocks[inv.Hash]; exists {
				delete(state.requestedBlocks, inv.Hash)
				delete(sm.requestedBlocks, inv.Hash)
			}

		case wire.InvTypeWitnessTx:
			fallthrough
		case wire.InvTypeTx:
			if _, exists := state.requestedTxns[inv.Hash]; exists {
				delete(state.requestedTxns, inv.Hash)
				delete(sm.requestedTxns, inv.Hash)
			}
		}
	}
}

// haveInventory returns whether or not the inventory represented by the passed
// inventory vector is known.  This includes checking all of the various places
// inventory can be when it is in different states such as blocks that are part
// of the main chain, on a side chain, in the orphan pool, and transactions that
// are in the memory pool (either the main pool or orphan pool).
func (sm *SyncManager) haveInventory(invVect *wire.InvVect) (bool, error) {
	switch invVect.Type {
	case wire.InvTypeWitnessBlock:
		fallthrough
	case wire.InvTypeBlock:
		// Ask chain if the block is known to it in any form (main
		// chain, side chain, or orphan).
		return sm.chain.HaveBlock(&invVect.Hash)
	case wire.InvTypeWitnessUBlock:
		fallthrough
	case wire.InvTypeUBlock:
		return sm.chain.HaveUBlock(&invVect.Hash)

	case wire.InvTypeWitnessTx:
		fallthrough
	case wire.InvTypeTx:
		// Ask the transaction memory pool if the transaction is known
		// to it in any form (main pool or orphan).
		if sm.txMemPool.HaveTransaction(&invVect.Hash) {
			return true, nil
		}

		// Check if the transaction exists from the point of view of the
		// end of the main chain.  Note that this is only a best effort
		// since it is expensive to check existence of every output and
		// the only purpose of this check is to avoid downloading
		// already known transactions.  Only the first two outputs are
		// checked because the vast majority of transactions consist of
		// two outputs where one is some form of "pay-to-somebody-else"
		// and the other is a change output.
		prevOut := wire.OutPoint{Hash: invVect.Hash}
		for i := uint32(0); i < 2; i++ {
			prevOut.Index = i
			entry, err := sm.chain.FetchUtxoEntry(prevOut)
			if err != nil {
				return false, err
			}
			if entry != nil && !entry.IsSpent() {
				return true, nil
			}
		}

		return false, nil
	}

	// The requested inventory is is an unsupported type, so just claim
	// it is known to avoid requesting it.
	return true, nil
}

// handleInvMsg handles inv messages from all peers.
// We examine the inventory advertised by the remote peer and act accordingly.
func (sm *SyncManager) handleInvMsg(imsg *invMsg) {
	peer := imsg.peer
	state, exists := sm.peerStates[peer]
	if !exists {
		log.Warnf("Received inv message from unknown peer %s", peer)
		return
	}

	// Attempt to find the final block in the inventory list.  There may
	// not be one.
	lastBlock := -1
	invVects := imsg.inv.InvList
	for i := len(invVects) - 1; i >= 0; i-- {
		if invVects[i].Type == wire.InvTypeBlock {
			lastBlock = i
			break
		} else if invVects[i].Type == wire.InvTypeUBlock {
			lastBlock = i
			break
		}
	}

	// If this inv contains a block announcement, and this isn't coming from
	// our current sync peer or we're current, then update the last
	// announced block for this peer. We'll use this information later to
	// update the heights of peers based on blocks we've accepted that they
	// previously announced.
	if lastBlock != -1 && (peer != sm.syncPeer || sm.current()) {
		peer.UpdateLastAnnouncedBlock(&invVects[lastBlock].Hash)
	}

	// Ignore invs from peers that aren't the sync if we are not current.
	// Helps prevent fetching a mass of orphans.
	if peer != sm.syncPeer && !sm.current() {
		return
	}

	// If our chain is current and a peer announces a block we already
	// know of, then update their current block height.
	if lastBlock != -1 && sm.current() {
		blkHeight, err := sm.chain.BlockHeightByHash(&invVects[lastBlock].Hash)
		if err == nil {
			peer.UpdateLastBlockHeight(blkHeight)
		}
	}

	// Request the advertised inventory if we don't already have it.  Also,
	// request parent blocks of orphans if we receive one we already have.
	// Finally, attempt to detect potential stalls due to long side chains
	// we already have and request more blocks to prevent them.
	for i, iv := range invVects {
		// Ignore unsupported inventory types.
		switch iv.Type {
		case wire.InvTypeBlock:
		case wire.InvTypeUBlock:
		case wire.InvTypeTx:
		case wire.InvTypeWitnessBlock:
		case wire.InvTypeWitnessUBlock:
		case wire.InvTypeWitnessTx:
		default:
			continue
		}

		// Add the inventory to the cache of known inventory
		// for the peer.
		peer.AddKnownInventory(iv)

		// Ignore inventory when we're in headers-first mode.
		if sm.headersFirstMode {
			continue
		}

		// Request the inventory if we don't already have it.
		haveInv, err := sm.haveInventory(iv)
		if err != nil {
			log.Warnf("Unexpected failure when checking for "+
				"existing inventory during inv message "+
				"processing: %v", err)
			continue
		}
		if !haveInv {
			if iv.Type == wire.InvTypeTx {
				// Skip the transaction if it has already been
				// rejected.
				if _, exists := sm.rejectedTxns[iv.Hash]; exists {
					continue
				}
			}

			// Ignore invs block invs from non-witness enabled
			// peers, as after segwit activation we only want to
			// download from peers that can provide us full witness
			// data for blocks.
			if !peer.IsWitnessEnabled() && iv.Type == wire.InvTypeBlock {
				continue
			}

			// Add it to the request queue.
			state.requestQueue = append(state.requestQueue, iv)
			continue
		}

		if iv.Type == wire.InvTypeBlock {
			if sm.utreexoCSN {
				// The block is an orphan block that we already have.
				// When the existing orphan was processed, it requested
				// the missing parent blocks.  When this scenario
				// happens, it means there were more blocks missing
				// than are allowed into a single inventory message.  As
				// a result, once this peer requested the final
				// advertised block, the remote peer noticed and is now
				// resending the orphan block as an available block
				// to signal there are more missing blocks that need to
				// be requested.
				if sm.chain.IsKnownOrphan(&iv.Hash, true) {
					// Request blocks starting at the latest known
					// up to the root of the orphan that just came
					// in.
					orphanRoot := sm.chain.GetOrphanRoot(&iv.Hash, true)
					locator, err := sm.chain.LatestBlockLocator()
					if err != nil {
						log.Errorf("PEER: Failed to get block "+
							"locator for the latest block: "+
							"%v", err)
						continue
					}
					peer.PushGetUBlocksMsg(locator, orphanRoot)
					continue
				}

				// We already have the final block advertised by this
				// inventory message, so force a request for more.  This
				// should only happen if we're on a really long side
				// chain.
				if i == lastBlock {
					// Request blocks after this one up to the
					// final one the remote peer knows about (zero
					// stop hash).
					locator := sm.chain.BlockLocatorFromHash(&iv.Hash)
					peer.PushGetUBlocksMsg(locator, &zeroHash)
				}
				break
			}
			// The block is an orphan block that we already have.
			// When the existing orphan was processed, it requested
			// the missing parent blocks.  When this scenario
			// happens, it means there were more blocks missing
			// than are allowed into a single inventory message.  As
			// a result, once this peer requested the final
			// advertised block, the remote peer noticed and is now
			// resending the orphan block as an available block
			// to signal there are more missing blocks that need to
			// be requested.
			if sm.chain.IsKnownOrphan(&iv.Hash, false) {
				// Request blocks starting at the latest known
				// up to the root of the orphan that just came
				// in.
				orphanRoot := sm.chain.GetOrphanRoot(&iv.Hash, false)
				locator, err := sm.chain.LatestBlockLocator()
				if err != nil {
					log.Errorf("PEER: Failed to get block "+
						"locator for the latest block: "+
						"%v", err)
					continue
				}
				peer.PushGetBlocksMsg(locator, orphanRoot)
				continue
			}

			// We already have the final block advertised by this
			// inventory message, so force a request for more.  This
			// should only happen if we're on a really long side
			// chain.
			if i == lastBlock {
				// Request blocks after this one up to the
				// final one the remote peer knows about (zero
				// stop hash).
				locator := sm.chain.BlockLocatorFromHash(&iv.Hash)
				peer.PushGetBlocksMsg(locator, &zeroHash)
			}
		}

		if iv.Type == wire.InvTypeUBlock {
			// The block is an orphan block that we already have.
			// When the existing orphan was processed, it requested
			// the missing parent blocks.  When this scenario
			// happens, it means there were more blocks missing
			// than are allowed into a single inventory message.  As
			// a result, once this peer requested the final
			// advertised block, the remote peer noticed and is now
			// resending the orphan block as an available block
			// to signal there are more missing blocks that need to
			// be requested.
			if sm.chain.IsKnownOrphan(&iv.Hash, true) {
				// Request blocks starting at the latest known
				// up to the root of the orphan that just came
				// in.
				orphanRoot := sm.chain.GetOrphanRoot(&iv.Hash, true)
				locator, err := sm.chain.LatestBlockLocator()
				if err != nil {
					log.Errorf("PEER: Failed to get block "+
						"locator for the latest block: "+
						"%v", err)
					continue
				}
				peer.PushGetUBlocksMsg(locator, orphanRoot)
				continue
			}

			// We already have the final block advertised by this
			// inventory message, so force a request for more.  This
			// should only happen if we're on a really long side
			// chain.
			if i == lastBlock {
				// Request blocks after this one up to the
				// final one the remote peer knows about (zero
				// stop hash).
				locator := sm.chain.BlockLocatorFromHash(&iv.Hash)
				peer.PushGetUBlocksMsg(locator, &zeroHash)
			}
		}
	}

	// Request as much as possible at once.  Anything that won't fit into
	// the request will be requested on the next inv message.
	numRequested := 0
	gdmsg := wire.NewMsgGetData()
	requestQueue := state.requestQueue
	for len(requestQueue) != 0 {
		iv := requestQueue[0]
		requestQueue[0] = nil
		requestQueue = requestQueue[1:]

		switch iv.Type {
		case wire.InvTypeWitnessBlock:
			fallthrough
		case wire.InvTypeBlock:
			// Request the block if there is not already a pending
			// request.
			if _, exists := sm.requestedBlocks[iv.Hash]; !exists {
				limitAdd(sm.requestedBlocks, iv.Hash, maxRequestedBlocks)
				limitAdd(state.requestedBlocks, iv.Hash, maxRequestedBlocks)

				if peer.IsWitnessEnabled() {
					iv.Type = wire.InvTypeWitnessBlock
				}

				gdmsg.AddInvVect(iv)
				numRequested++
			}
		case wire.InvTypeWitnessUBlock:
			fallthrough
		case wire.InvTypeUBlock:
			// Request the block if there is not already a pending
			// request.
			if _, exists := sm.requestedBlocks[iv.Hash]; !exists {
				limitAdd(sm.requestedBlocks, iv.Hash, maxRequestedBlocks)
				limitAdd(state.requestedBlocks, iv.Hash, maxRequestedBlocks)

				if peer.IsWitnessEnabled() {
					iv.Type = wire.InvTypeWitnessUBlock
				}

				gdmsg.AddInvVect(iv)
				numRequested++
			}

		case wire.InvTypeWitnessTx:
			fallthrough
		case wire.InvTypeTx:
			// Request the transaction if there is not already a
			// pending request.
			if _, exists := sm.requestedTxns[iv.Hash]; !exists {
				limitAdd(sm.requestedTxns, iv.Hash, maxRequestedTxns)
				limitAdd(state.requestedTxns, iv.Hash, maxRequestedTxns)

				// If the peer is capable, request the txn
				// including all witness data.
				if peer.IsWitnessEnabled() {
					iv.Type = wire.InvTypeWitnessTx
				}

				gdmsg.AddInvVect(iv)
				numRequested++
			}
		}

		if numRequested >= wire.MaxInvPerMsg {
			break
		}
	}
	state.requestQueue = requestQueue
	if len(gdmsg.InvList) > 0 {
		peer.QueueMessage(gdmsg, nil)
	}
}

// blockHandler is the main handler for the sync manager.  It must be run as a
// goroutine.  It processes block and inv messages in a separate goroutine
// from the peer handlers so the block (MsgBlock) messages are handled by a
// single thread without needing to lock memory data structures.  This is
// important because the sync manager controls which blocks are needed and how
// the fetching should proceed.
func (sm *SyncManager) blockHandler() {
	stallTicker := time.NewTicker(stallSampleInterval)
	defer stallTicker.Stop()

out:
	for {
		select {
		case m := <-sm.msgChan:
			switch msg := m.(type) {
			case *newPeerMsg:
				sm.handleNewPeerMsg(msg.peer)

			case *txMsg:
				sm.handleTxMsg(msg)
				msg.reply <- struct{}{}

			case *blockMsg:
				sm.handleBlockMsg(msg)
				msg.reply <- struct{}{}

			case *ublockMsg:
				sm.handleUBlockMsg(msg)
				msg.reply <- struct{}{}

			case *invMsg:
				sm.handleInvMsg(msg)

			case *headersMsg:
				sm.handleHeadersMsg(msg)

			case *notFoundMsg:
				sm.handleNotFoundMsg(msg)

			case *donePeerMsg:
				sm.handleDonePeerMsg(msg.peer)

			case getSyncPeerMsg:
				var peerID int32
				if sm.syncPeer != nil {
					peerID = sm.syncPeer.ID()
				}
				msg.reply <- peerID

			case processBlockMsg:
				_, isOrphan, err := sm.chain.ProcessBlock(
					msg.block, msg.flags)
				if err != nil {
					msg.reply <- processBlockResponse{
						isOrphan: false,
						err:      err,
					}
				}

				msg.reply <- processBlockResponse{
					isOrphan: isOrphan,
					err:      nil,
				}
			case processUBlockMsg:
				_, isOrphan, err := sm.chain.ProcessUBlock(
					msg.ublock, msg.flags)
				if err != nil {
					msg.reply <- processBlockResponse{
						isOrphan: false,
						err:      err,
					}
				}

				msg.reply <- processBlockResponse{
					isOrphan: isOrphan,
					err:      nil,
				}

			case isCurrentMsg:
				msg.reply <- sm.current()

			case pauseMsg:
				// Wait until the sender unpauses the manager.
				<-msg.unpause

			default:
				log.Warnf("Invalid message type in block "+
					"handler: %T", msg)
			}

		case <-stallTicker.C:
			sm.handleStallSample()

		case <-sm.quit:
			break out
		}
	}

	if !sm.utreexoRootVerifyMode {
		log.Debug("Block handler shutting down: flushing blockchain caches...")
		if err := sm.chain.FlushCachedState(blockchain.FlushRequired); err != nil {
			log.Errorf("Error while flushing blockchain caches: %v", err)
		}
	}

	sm.wg.Done()
	log.Trace("Block handler done")
}

func (sm *SyncManager) headerHandler(done chan struct{}) {
	stallTicker := time.NewTicker(stallSampleInterval)
	defer stallTicker.Stop()
out:
	for {
		select {
		case m := <-sm.msgChan:
			switch msg := m.(type) {
			case *newPeerMsg:
				sm.handleNewPeerMsg(msg.peer)

			case *headersMsg:
				finished := sm.handleOnlyHeadersMsg(msg)
				if finished {
					done <- struct{}{}
					break out
				}

			case *notFoundMsg:
				sm.handleNotFoundMsg(msg)

			case *donePeerMsg:
				sm.handleDonePeerMsg(msg.peer)

			case getSyncPeerMsg:
				var peerID int32
				if sm.syncPeer != nil {
					peerID = sm.syncPeer.ID()
				}
				msg.reply <- peerID

			case isCurrentMsg:
				msg.reply <- sm.current()

			case pauseMsg:
				// Wait until the sender unpauses the manager.
				<-msg.unpause

			default:
				log.Warnf("Invalid message type in block "+
					"handler: %T", msg)
			}

		case <-stallTicker.C:
			sm.handleStallSample()

		case <-sm.quit:
			break out
		}
	}

	sm.wg.Done()
	log.Trace("handleHeader done")
}

type ProcessedURootHint struct {
	Validated       bool
	URootHintHeight int32
}

func (sm *SyncManager) uRootHintVerifyHandler(verified chan ProcessedURootHint) {
	stallTicker := time.NewTicker(stallSampleInterval)
	defer stallTicker.Stop()
out:
	for {
		select {
		case m := <-sm.msgChan:
			switch msg := m.(type) {
			case *chaincfg.UtreexoRootHint:
				startURoot := chaincfg.FindPreviousUtreexoRootHint(
					msg.Height, sm.chain.UtreexoRootHints())

				startUView, err := blockchain.GenUtreexoViewpoint(startURoot)
				if err != nil {
					panic(err)
				}

				var startHeight int32
				if startURoot != nil {
					startHeight = startURoot.Height
				}
				sm.uTreeMapLock.Lock()
				sm.uTreeMap[startHeight] = &uTreeState{
					uView:        startUView,
					startRoot:    startURoot,
					rootToVerify: msg,
				}
				sm.uTreeMapLock.Unlock()

				sm.ValidateParallelUtreexoRoot(startHeight, msg.Height)
			case *newPeerMsg:
				sm.handleNewPeerMsg(msg.peer)

			case *ublockMsg:
				go sm.uRootHandleUBlockMsg(msg)

			case *invMsg:
				sm.handleInvMsg(msg)

			case *notFoundMsg:
				sm.handleNotFoundMsg(msg)

			case *donePeerMsg:
				sm.handleDonePeerMsg(msg.peer)

			case ProcessedURootHint:
				verified <- msg

			case getSyncPeerMsg:
				var peerID int32
				if sm.syncPeer != nil {
					peerID = sm.syncPeer.ID()
				}
				msg.reply <- peerID

			case isCurrentMsg:
				msg.reply <- sm.current()

			case pauseMsg:
				// Wait until the sender unpauses the manager.
				<-msg.unpause

			default:
				log.Warnf("Invalid message type in  "+
					"uRootHintVerifyHandler: %T", msg)
			}

		case <-stallTicker.C:
			sm.handleStallSample()

		case <-sm.quit:
			break out
		}
	}

	sm.wg.Done()
	log.Trace("uRootHintVerifyHandler done")
}

// TODO kcalvinalvin: It's really mostly the same procedure with a regular block
// This isn't the prettiest way
func (sm *SyncManager) uRootHandleUBlockMsg(ubmsg *ublockMsg) {
	defer func() {
		ubmsg.reply <- struct{}{}
	}()
	peer := ubmsg.peer
	state, exists := sm.peerStates[peer]
	if !exists {
		log.Warnf("Received ublock message from unknown peer %s", peer)
		return
	}

	// If we didn't ask for this block then the peer is misbehaving.
	blockHash := ubmsg.ublock.Hash()
	state.requestedBlocksLock.Lock()
	if _, exists = state.requestedBlocks[*blockHash]; !exists {
		// The regression test intentionally sends some blocks twice
		// to test duplicate block insertion fails.  Don't disconnect
		// the peer or ignore the block when we're in regression test
		// mode in this case so the chain code is actually fed the
		// duplicate blocks.
		if sm.chainParams != &chaincfg.RegressionNetParams {
			log.Warnf("Got unrequested ublock %v from %s -- "+
				"disconnecting", blockHash, peer.Addr())
			peer.Disconnect()
			return
		}
	}
	state.requestedBlocksLock.Unlock()

	behaviorFlags := blockchain.BFNone

	// Remove block from request maps. Either chain will know about it and
	// so we shouldn't have any more instances of trying to fetch it, or we
	// will fail the insert and thus we'll retry next time we get an inv.
	state.requestedBlocksLock.Lock()
	delete(state.requestedBlocks, *blockHash)
	state.requestedBlocksLock.Unlock()

	sm.requestedBlocksLock.Lock()
	delete(sm.requestedBlocks, *blockHash)
	sm.requestedBlocksLock.Unlock()

	blockHeight, err := sm.chain.LookupNode(ubmsg.ublock.Hash())
	if err != nil {
		panic(err)
	}

	searchHeight := int32(0)
	uRootHint := sm.chain.FindPreviousUtreexoRootHint(blockHeight)
	if uRootHint != nil {
		searchHeight = uRootHint.Height
	}

	sm.uTreeMapLock.RLock()
	uState := sm.uTreeMap[searchHeight]
	sm.uTreeMapLock.RUnlock()
	if uState == nil {
		err := fmt.Errorf("Couldn't find the uState for block height %d",
			searchHeight)
		panic(err)
	}

	// Process the block to include validation, best chain selection, orphan
	// handling, etc.  It's always the main chain because we do the headers sync first
	mainChain, _, err := sm.chain.ProcessHeaderUBlock(ubmsg.ublock, uState.uView, behaviorFlags)
	if err != nil {
		// just panic. It's fine to restart the range verification.
		panic(err)
	}
	if !mainChain {
		err := fmt.Errorf("The block %s was not part of the main chain", ubmsg.ublock.Hash())
		panic(err)
	}

	sm.uTreeMapLock.Lock()
	sm.uTreeMap[searchHeight] = uState
	sm.uTreeMapLock.Unlock()

	if ubmsg.ublock.Height() == uState.rootToVerify.Height {
		delete(sm.uTreeMap, searchHeight)
		if uState.uView.Equal(uState.rootToVerify.Roots) {
			result := ProcessedURootHint{
				Validated:       true,
				URootHintHeight: ubmsg.ublock.Height(),
			}
			sm.queueProcessedURootHint(result)
			log.Tracef("Utreexo root verified at height %v",
				ubmsg.ublock.Height())
			return
			//return true, ubmsg.ublock.Height(), true
		} else {
			result := ProcessedURootHint{
				Validated:       false,
				URootHintHeight: ubmsg.ublock.Height(),
			}
			sm.queueProcessedURootHint(result)
			log.Warnf("Utreexo root invalid at height %v",
				ubmsg.ublock.Height())
			return
			//return true, ubmsg.ublock.Height(), false
		}
	}

	// Meta-data about the new block this peer is reporting. We use this
	// below to update this peer's latest block height and the heights of
	// other peers based on their last announced block hash. This allows us
	// to dynamically update the block heights of peers, avoiding stale
	// heights when looking for a new sync peer. Upon acceptance of a block
	// or recognition of an orphan, we also use this information to update
	// the block heights over other peers who's invs may have been ignored
	// if we are actively syncing while the chain is not yet current or
	// who may have lost the lock announcement race.
	var heightUpdate int32
	var blkHashUpdate *chainhash.Hash

	if peer == sm.syncPeer {
		sm.lastProgressTime = time.Now()
	}

	// Something for compatibility with the existing LogBlockHeight method
	block := ubmsg.ublock.Block()

	// When the block is not an orphan, log information about it and
	// update the chain state.
	sm.progressLogger.LogBlockHeight(block, sm.chain)

	// Update this peer's latest block height, for future
	// potential sync node candidacy.
	best := sm.chain.BestSnapshot()
	heightUpdate = best.Height
	blkHashUpdate = &best.Hash

	// Clear the rejected transactions.
	sm.rejectedTxns = make(map[chainhash.Hash]struct{})

	// Update the block height for this peer. But only send a message to
	// the server for updating peer heights if this is an orphan or our
	// chain is "current". This avoids sending a spammy amount of messages
	// if we're syncing the chain from scratch.
	if blkHashUpdate != nil && heightUpdate != 0 {
		peer.UpdateLastBlockHeight(heightUpdate)
		if sm.current() {
			go sm.peerNotifier.UpdatePeerHeights(blkHashUpdate, heightUpdate,
				peer)
		}
	}

	////if sm.startHeader != nil &&
	//if len(state.requestedBlocks) < minInFlightBlocks {
	//	sm.fetchParallelVerifyUBlocks(ubmsg.ublock.Height()+1, uState.rootToVerify.Height)
	//}

	return
}

// handleBlockchainNotification handles notifications from blockchain.  It does
// things such as request orphan block parents and relay accepted blocks to
// connected peers.
func (sm *SyncManager) handleBlockchainNotification(notification *blockchain.Notification) {
	switch notification.Type {
	// A block has been accepted into the block chain.  Relay it to other
	// peers.
	case blockchain.NTBlockAccepted:
		// Don't relay if we are not current. Other peers that are
		// current should already know about it.
		if !sm.current() {
			return
		}

		block, ok := notification.Data.(*btcutil.Block)
		if !ok {
			log.Warnf("Chain accepted notification is not a block.")
			break
		}

		// Generate the inventory vector and relay it.
		iv := wire.NewInvVect(wire.InvTypeBlock, block.Hash())
		sm.peerNotifier.RelayInventory(iv, block.MsgBlock().Header)

	// A block has been connected to the main block chain.
	case blockchain.NTBlockConnected:
		var ok bool
		//var ublock *btcutil.UBlock
		var block *btcutil.Block

		if sm.utreexoCSN {
			_, ok = notification.Data.(*btcutil.UBlock)
		} else {
			block, ok = notification.Data.(*btcutil.Block)
		}
		if !ok {
			log.Warnf("Chain connected notification is not a block.")
			break
		}

		// Remove all of the transactions (except the coinbase) in the
		// connected block from the transaction pool.  Secondly, remove any
		// transactions which are now double spends as a result of these
		// new transactions.  Finally, remove any transaction that is
		// no longer an orphan. Transactions which depend on a confirmed
		// transaction are NOT removed recursively because they are still
		// valid.
		if !sm.utreexoCSN {
			for _, tx := range block.Transactions()[1:] {
				sm.txMemPool.RemoveTransaction(tx, false)
				sm.txMemPool.RemoveDoubleSpends(tx)
				sm.txMemPool.RemoveOrphan(tx)
				sm.peerNotifier.TransactionConfirmed(tx)
				acceptedTxs := sm.txMemPool.ProcessOrphans(tx)
				sm.peerNotifier.AnnounceNewTransactions(acceptedTxs)
			}

			// Register block with the fee estimator, if it exists.
			if sm.feeEstimator != nil {
				err := sm.feeEstimator.RegisterBlock(block)

				// If an error is somehow generated then the fee estimator
				// has entered an invalid state. Since it doesn't know how
				// to recover, create a new one.
				if err != nil {
					sm.feeEstimator = mempool.NewFeeEstimator(
						mempool.DefaultEstimateFeeMaxRollback,
						mempool.DefaultEstimateFeeMinRegisteredBlocks)
				}
			}
		}

	// A block has been disconnected from the main block chain.
	case blockchain.NTBlockDisconnected:
		block, ok := notification.Data.(*btcutil.Block)
		if !ok {
			log.Warnf("Chain disconnected notification is not a block.")
			break
		}

		// Reinsert all of the transactions (except the coinbase) into
		// the transaction pool.
		for _, tx := range block.Transactions()[1:] {
			_, _, err := sm.txMemPool.MaybeAcceptTransaction(tx,
				false, false)
			if err != nil {
				// Remove the transaction and all transactions
				// that depend on it if it wasn't accepted into
				// the transaction pool.
				sm.txMemPool.RemoveTransaction(tx, true)
			}
		}

		// Rollback previous block recorded by the fee estimator.
		if sm.feeEstimator != nil {
			sm.feeEstimator.Rollback(block.Hash())
		}
	}
}

// NewPeer informs the sync manager of a newly active peer.
func (sm *SyncManager) NewPeer(peer *peerpkg.Peer) {
	// Ignore if we are shutting down.
	if atomic.LoadInt32(&sm.shutdown) != 0 {
		return
	}
	sm.msgChan <- &newPeerMsg{peer: peer}
}

func (sm *SyncManager) queueProcessedURootHint(result ProcessedURootHint) {
	sm.msgChan <- result
}

func (sm *SyncManager) QueueURootHint(uRootHint *chaincfg.UtreexoRootHint) {
	// Don't accept more uRootHints if we're shutting down.
	if atomic.LoadInt32(&sm.shutdown) != 0 {
		return
	}

	// don't queue until we have a sync peer
	select {
	case <-sm.newSyncPeer:
		break
	case <-sm.quit:
		break
	}

	sm.msgChan <- uRootHint
}

// QueueTx adds the passed transaction message and peer to the block handling
// queue. Responds to the done channel argument after the tx message is
// processed.
func (sm *SyncManager) QueueTx(tx *btcutil.Tx, peer *peerpkg.Peer, done chan struct{}) {
	// Don't accept more transactions if we're shutting down.
	if atomic.LoadInt32(&sm.shutdown) != 0 {
		done <- struct{}{}
		return
	}

	sm.msgChan <- &txMsg{tx: tx, peer: peer, reply: done}
}

// QueueBlock adds the passed block message and peer to the block handling
// queue. Responds to the done channel argument after the block message is
// processed.
func (sm *SyncManager) QueueBlock(block *btcutil.Block, peer *peerpkg.Peer, done chan struct{}) {
	// Don't accept more blocks if we're shutting down.
	if atomic.LoadInt32(&sm.shutdown) != 0 {
		done <- struct{}{}
		return
	}

	sm.msgChan <- &blockMsg{block: block, peer: peer, reply: done}
}

// QueueUBlock adds the passed block message and peer to the block handling
// queue. Responds to the done channel argument after the block message is
// processed.
func (sm *SyncManager) QueueUBlock(ublock *btcutil.UBlock, peer *peerpkg.Peer, done chan struct{}) {
	// Don't accept more blocks if we're shutting down.
	if atomic.LoadInt32(&sm.shutdown) != 0 {
		done <- struct{}{}
		return
	}

	sm.msgChan <- &ublockMsg{ublock: ublock, peer: peer, reply: done}
}

// QueueUBlock adds the passed block message and peer to the block handling
// queue. Responds to the done channel argument after the block message is
// processed.
func (sm *SyncManager) QueueParallel(ublock *btcutil.UBlock, peer *peerpkg.Peer) {
	// Don't accept more blocks if we're shutting down.
	if atomic.LoadInt32(&sm.shutdown) != 0 {
		return
	}

	sm.msgChan <- &ublockMsg{ublock: ublock, peer: peer}
}

// QueueInv adds the passed inv message and peer to the block handling queue.
func (sm *SyncManager) QueueInv(inv *wire.MsgInv, peer *peerpkg.Peer) {
	// No channel handling here because peers do not need to block on inv
	// messages.
	if atomic.LoadInt32(&sm.shutdown) != 0 {
		return
	}

	sm.msgChan <- &invMsg{inv: inv, peer: peer}
}

// QueueHeaders adds the passed headers message and peer to the block handling
// queue.
func (sm *SyncManager) QueueHeaders(headers *wire.MsgHeaders, peer *peerpkg.Peer) {
	// No channel handling here because peers do not need to block on
	// headers messages.
	if atomic.LoadInt32(&sm.shutdown) != 0 {
		return
	}

	sm.msgChan <- &headersMsg{headers: headers, peer: peer}
}

// QueueNotFound adds the passed notfound message and peer to the block handling
// queue.
func (sm *SyncManager) QueueNotFound(notFound *wire.MsgNotFound, peer *peerpkg.Peer) {
	// No channel handling here because peers do not need to block on
	// reject messages.
	if atomic.LoadInt32(&sm.shutdown) != 0 {
		return
	}

	sm.msgChan <- &notFoundMsg{notFound: notFound, peer: peer}
}

// DonePeer informs the blockmanager that a peer has disconnected.
func (sm *SyncManager) DonePeer(peer *peerpkg.Peer) {
	// Ignore if we are shutting down.
	if atomic.LoadInt32(&sm.shutdown) != 0 {
		return
	}

	sm.msgChan <- &donePeerMsg{peer: peer}
}

// Start begins the core block handler which processes block and inv messages.
func (sm *SyncManager) Start() {
	// Already started?
	if atomic.AddInt32(&sm.started, 1) != 1 {
		return
	}

	log.Trace("Starting sync manager")
	sm.wg.Add(1)
	go sm.blockHandler()
}

// StartUtreexoRootHintVerify begins the core block handler which processes block and inv messages.
func (sm *SyncManager) StartHeadersDownload(rootHint *chaincfg.UtreexoRootHint, doneChan chan struct{}) {
	// Already started?
	if atomic.AddInt32(&sm.started, 1) != 1 {
		return
	}

	if rootHint == nil {
		log.Errorf("Given rootHint to verify is nil")
		return
	}
	sm.utreexoRootToVerify = rootHint

	sm.chain.SetUtreexoViewpoint(sm.utreexoStartRoot)

	log.Trace("Starting header download")
	sm.wg.Add(1)
	go sm.headerHandler(doneChan)
}

// StartUtreexoRootHintVerify begins the core block handler which processes block and inv messages.
func (sm *SyncManager) StartUtreexoRootHintVerify(verifiedChan chan ProcessedURootHint) {
	// Already started?
	if atomic.AddInt32(&sm.started, 1) != 1 {
		return
	}

	log.Trace("Starting UtreexoRootHint verify")
	sm.wg.Add(1)
	go sm.uRootHintVerifyHandler(verifiedChan)
}

// StartParallelURootVerify begins the core block handler which processes block and inv messages.
func (sm *SyncManager) StartParallelURootVerify(verifiedChan chan ProcessedURootHint) {
	// Already started?
	if atomic.AddInt32(&sm.started, 1) != 1 {
		return
	}

	log.Trace("Starting UtreexoRootHint verify")
	sm.wg.Add(1)
	go sm.uRootHintVerifyHandler(verifiedChan)
}

// Stop gracefully shuts down the sync manager by stopping all asynchronous
// handlers and waiting for them to finish.
func (sm *SyncManager) Stop() error {
	if atomic.AddInt32(&sm.shutdown, 1) != 1 {
		log.Warnf("Sync manager is already in the process of " +
			"shutting down")
		return nil
	}

	log.Infof("Sync manager shutting down")
	close(sm.quit)
	sm.wg.Wait()
	return nil
}

// SyncPeerID returns the ID of the current sync peer, or 0 if there is none.
func (sm *SyncManager) SyncPeerID() int32 {
	reply := make(chan int32)
	sm.msgChan <- getSyncPeerMsg{reply: reply}
	return <-reply
}

// ProcessBlock makes use of ProcessBlock on an internal instance of a block
// chain.
func (sm *SyncManager) ProcessBlock(block *btcutil.Block, flags blockchain.BehaviorFlags) (bool, error) {
	reply := make(chan processBlockResponse, 1)
	sm.msgChan <- processBlockMsg{block: block, flags: flags, reply: reply}
	response := <-reply
	return response.isOrphan, response.err
}

// IsCurrent returns whether or not the sync manager believes it is synced with
// the connected peers.
func (sm *SyncManager) IsCurrent() bool {
	reply := make(chan bool)
	sm.msgChan <- isCurrentMsg{reply: reply}
	return <-reply
}

// Pause pauses the sync manager until the returned channel is closed.
//
// Note that while paused, all peer and block processing is halted.  The
// message sender should avoid pausing the sync manager for long durations.
func (sm *SyncManager) Pause() chan<- struct{} {
	c := make(chan struct{})
	sm.msgChan <- pauseMsg{c}
	return c
}

// New constructs a new SyncManager. Use Start to begin processing asynchronous
// block, tx, and inv updates.
func New(config *Config) (*SyncManager, error) {
	sm := SyncManager{
		peerNotifier:          config.PeerNotifier,
		chain:                 config.Chain,
		txMemPool:             config.TxMemPool,
		chainParams:           config.ChainParams,
		rejectedTxns:          make(map[chainhash.Hash]struct{}),
		requestedTxns:         make(map[chainhash.Hash]struct{}),
		requestedBlocks:       make(map[chainhash.Hash]struct{}),
		peerStates:            make(map[*peerpkg.Peer]*peerSyncState),
		uTreeMap:              make(map[int32]*uTreeState),
		progressLogger:        newBlockProgressLogger("Processed", log),
		msgChan:               make(chan interface{}, config.MaxPeers*3),
		headerList:            list.New(),
		quit:                  make(chan struct{}),
		newSyncPeer:           make(chan struct{}),
		feeEstimator:          config.FeeEstimator,
		utreexoCSN:            config.UtreexoCSN,
		utreexoMN:             config.UtreexoMN,
		utreexoWN:             config.UtreexoWN,
		utreexoRootVerifyMode: config.UtreexoRootVerifyMode,
	}

	best := sm.chain.BestSnapshot()
	if !config.DisableCheckpoints {
		// Initialize the next checkpoint based on the current height.
		sm.nextCheckpoint = sm.findNextHeaderCheckpoint(best.Height)
		if sm.nextCheckpoint != nil {
			sm.resetHeaderState(&best.Hash, best.Height)
		}
	} else {
		log.Info("Checkpoints are disabled")
		if sm.utreexoRootVerifyMode {
			// Push back the genesis header to the headerList
			best := sm.chain.BestSnapshot()
			node := HeaderNode{Height: best.Height, Hash: &best.Hash}
			sm.headerList.PushBack(&node)
		}
	}

	sm.chain.Subscribe(sm.handleBlockchainNotification)

	return &sm, nil
}
