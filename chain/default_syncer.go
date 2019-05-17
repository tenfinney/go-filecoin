package chain

import (
	"context"
	"sync"
	"time"

	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-hamt-ipld"
	logging "github.com/ipfs/go-log"
	"github.com/pkg/errors"
	"go.opencensus.io/trace"

	"github.com/filecoin-project/go-filecoin/actor/builtin"
	"github.com/filecoin-project/go-filecoin/consensus"
	"github.com/filecoin-project/go-filecoin/metrics/tracing"
	"github.com/filecoin-project/go-filecoin/sampling"
	"github.com/filecoin-project/go-filecoin/state"
	"github.com/filecoin-project/go-filecoin/types"
)

// The amount of time the syncer will wait while fetching the blocks of a
// tipset over the network.
var blkWaitTime = 30 * time.Second

// FinalityLimit is the maximum number of blocks ahead of the current consensus
// chain height to accept once in caught up mode
var FinalityLimit = 600
var (
	// ErrChainHasBadTipSet is returned when the syncer traverses a chain with a cached bad tipset.
	ErrChainHasBadTipSet = errors.New("input chain contains a cached bad tipset")
	// ErrNewChainTooLong is returned when processing a fork that split off from the main chain too many blocks ago.
	ErrNewChainTooLong = errors.New("input chain forked from best chain too far in the past")
	// ErrUnexpectedStoreState indicates that the syncer's chain store is violating expected invariants.
	ErrUnexpectedStoreState = errors.New("the chain store is in an unexpected state")
)

var logSyncer = logging.Logger("chain.syncer")

// SyncMode is the
type SyncMode int

const (
	// Syncing indicates that the node was started recently and the chain is still
	// significantly behind the current consensus head.
	//
	// See the spec for more detail:
	// https://github.com/filecoin-project/specs/blob/master/sync.md#syncing-mode
	Syncing SyncMode = iota
	// CaughtUp indicates that the node has caught up with consensus head and as a
	// result can restrict which new blocks are accepted to mitigate consensus
	// attacks.
	//
	// See the spec for more detail:
	// https://github.com/filecoin-project/specs/blob/master/sync.md#caught-up-mode
	CaughtUp
)

type syncerChainReader interface {
	GetBlock(context.Context, cid.Cid) (*types.Block, error)
	GetHead() types.SortedCidSet
	GetTipSet(tsKey types.SortedCidSet) (*types.TipSet, error)
	GetTipSetStateRoot(tsKey types.SortedCidSet) (cid.Cid, error)
	HasTipSetAndState(ctx context.Context, tsKey string) bool
	PutTipSetAndState(ctx context.Context, tsas *TipSetAndState) error
	SetHead(ctx context.Context, s types.TipSet) error
	HasTipSetAndStatesWithParentsAndHeight(pTsKey string, h uint64) bool
	GetTipSetAndStatesByParentsAndHeight(pTsKey string, h uint64) ([]*TipSetAndState, error)
	HasAllBlocks(ctx context.Context, cs []cid.Cid) bool
}

type syncFetcher interface {
	GetBlocks(context.Context, []cid.Cid) ([]*types.Block, error)
}

// DefaultSyncer updates its chain.Store according to the methods of its
// consensus.Protocol.  It uses a bad tipset cache and a limit on new
// blocks to traverse during chain collection.  The DefaultSyncer can query the
// network for blocks.  The DefaultSyncer maintains the following invariant on
// its store: all tipsets that pass the syncer's validity checks are added to the
// chain store, and their state is added to stateStore.
//
// Ideally the code that syncs the chain according to consensus rules should
// be independent of any particular implementation of consensus.  Currently the
// DefaultSyncer is coupled to details of Expected Consensus. This dependence
// exists in the widen function, the fact that widen is called on only one
// tipset in the incoming chain, and assumptions regarding the existence of
// grandparent state in the store.
type DefaultSyncer struct {
	// This mutex ensures at most one call to HandleNewTipset executes at
	// any time.  This is important because at least two sections of the
	// code otherwise have races:
	// 1. syncOne assumes that chainStore.Head() does not change when
	// comparing tipset weights and updating the store
	// 2. HandleNewTipset assumes that calls to widen and then syncOne
	// are not run concurrently with other calls to widen to ensure
	// that the syncer always finds the heaviest existing tipset.
	mu sync.Mutex
	// fetcher is the networked block fetching service for fetching blocks
	// and messages.
	fetcher syncFetcher
	// stateStore is the cborStore used for reading and writing state root
	// to ipld object mappings.
	stateStore *hamt.CborIpldStore
	// badTipSetCache is used to filter out collections of invalid blocks.
	badTipSets *badTipSetCache
	consensus  consensus.Protocol
	chainStore syncerChainReader
	// syncMode is an enumerable indicating whether the chain is currently caught
	// up or still syncing. Presently, syncMode is always Syncing pending
	// implementation in issue #1160.
	//
	// TODO: https://github.com/filecoin-project/go-filecoin/issues/1160
	SyncMode SyncMode
}

var _ Syncer = (*DefaultSyncer)(nil)

// NewDefaultSyncer constructs a DefaultSyncer ready for use.
func NewDefaultSyncer(cst *hamt.CborIpldStore, c consensus.Protocol, s syncerChainReader, f syncFetcher) *DefaultSyncer {
	return &DefaultSyncer{
		fetcher:    f,
		stateStore: cst,
		badTipSets: &badTipSetCache{
			bad: make(map[string]struct{}),
		},
		consensus:  c,
		chainStore: s,
		SyncMode:   Syncing,
	}
}

// getBlksMaybeFromNet resolves cids of blocks.  It gets blocks through the
// fetcher.  The fetcher wraps a bitswap session which wraps a bitswap exchange,
// and the bitswap exchange wraps the node's shared blockstore.  So if blocks
// are available in the node's blockstore they will be resolved locally, and
// otherwise resolved over the network.  This method will timeout if blocks
// are unavailable.  This method is all or nothing, it will error if any of the
// blocks cannot be resolved.
func (syncer *DefaultSyncer) getBlksMaybeFromNet(ctx context.Context, blkCids []cid.Cid) ([]*types.Block, error) {
	ctx, cancel := context.WithTimeout(ctx, blkWaitTime)
	defer cancel()

	return syncer.fetcher.GetBlocks(ctx, blkCids)
}

// collectChain resolves the cids of the head tipset and its ancestors to
// blocks until it resolves a tipset with a parent contained in the Store. It
// returns the chain of new incompletely validated tipsets and the id of the
// parent tipset already synced into the store.  collectChain resolves cids
// from the syncer's fetcher.  In production the fetcher wraps a bitswap
// session.  collectChain errors if any set of cids in the chain resolves to
// blocks that do not form a tipset, or if any tipset has already been recorded
// as the head of an invalid chain.  collectChain is the entrypoint to the code
// that interacts with the network. It does NOT add tipsets to the chainStore..
func (syncer *DefaultSyncer) collectChain(ctx context.Context, tipsetCids types.SortedCidSet) (ts []types.TipSet, err error) {
	ctx, span := trace.StartSpan(ctx, "DefaultSyncer.collectChain")
	span.AddAttributes(trace.StringAttribute("tipset", tipsetCids.String()))
	defer tracing.AddErrorEndSpan(ctx, span, &err)

	var chain []types.TipSet
	var count uint64
	fetchedHead := tipsetCids
	defer logSyncer.Infof("chain fetch from network complete %v", fetchedHead)

	// Continue collecting the chain if we're either not yet caught up or the
	// height of the input blocks has not yet exceeded the sum of the current
	// consensus height and the finalityLimit constant, otherwise ignore the input
	// blocks as a likely invalid chain or denial of service attempt.
	for syncer.SyncMode == Syncing || len(chain) < FinalityLimit {
		var blks []*types.Block
		// check the cache for bad tipsets before doing anything
		tsKey := tipsetCids.String()

		// Finish traversal if the tipset made is tracked in the store.
		if syncer.chainStore.HasTipSetAndState(ctx, tsKey) {
			return chain, nil
		}

		logSyncer.Debugf("CollectChain next link: %s", tsKey)

		if syncer.badTipSets.Has(tsKey) {
			return nil, ErrChainHasBadTipSet
		}

		blks, err := syncer.getBlksMaybeFromNet(ctx, tipsetCids.ToSlice())
		if err != nil {
			return nil, err
		}

		ts, err := syncer.consensus.NewValidTipSet(ctx, blks)
		if err != nil {
			syncer.badTipSets.Add(tsKey)
			syncer.badTipSets.AddChain(chain)
			return nil, err
		}

		count++
		if count%500 == 0 {
			logSyncer.Infof("fetching the chain, %d blocks fetched", count)
		}

		// Update values to traverse next tipset
		chain = append([]types.TipSet{ts}, chain...)
		tipsetCids, err = ts.Parents()
		if err != nil {
			return nil, err
		}
	}

	return nil, ErrNewChainTooLong
}

// tipSetState returns the state resulting from applying the input tipset to
// the chain.  Precondition: the tipset must be in the store
func (syncer *DefaultSyncer) tipSetState(ctx context.Context, tsKey types.SortedCidSet) (state.Tree, error) {
	if !syncer.chainStore.HasTipSetAndState(ctx, tsKey.String()) {
		return nil, errors.Wrap(ErrUnexpectedStoreState, "parent tipset must be in the store")
	}
	stateCid, err := syncer.chainStore.GetTipSetStateRoot(tsKey)
	if err != nil {
		return nil, err
	}
	st, err := state.LoadStateTree(ctx, syncer.stateStore, stateCid, builtin.Actors)
	if err != nil {
		return nil, err
	}
	return st, nil
}

// syncOne syncs a single tipset with the chain store. syncOne calculates the
// parent state of the tipset and calls into consensus to run a state transition
// in order to validate the tipset.  In the case the input tipset is valid,
// syncOne calls into consensus to check its weight, and then updates the head
// of the store if this tipset is the heaviest.
//
// Precondition: the caller of syncOne must hold the syncer's lock (syncer.mu) to
// ensure head is not modified by another goroutine during run.
func (syncer *DefaultSyncer) syncOne(ctx context.Context, parent, next types.TipSet) error {
	head := syncer.chainStore.GetHead()

	// if tipset is already head, we've been here before. do nothing.
	if head.Equals(next.ToSortedCidSet()) {
		return nil
	}

	// Lookup parent state. It is guaranteed by the syncer that it is in
	// the chainStore.
	st, err := syncer.tipSetState(ctx, parent.ToSortedCidSet())
	if err != nil {
		return err
	}

	// Gather ancestor chain needed to process state transition.
	h, err := next.Height()
	if err != nil {
		return err
	}
	newBlockHeight := types.NewBlockHeight(h)
	ancestorHeight := types.NewBlockHeight(consensus.AncestorRoundsNeeded)
	ancestors, err := GetRecentAncestors(ctx, parent, syncer.chainStore, newBlockHeight, ancestorHeight, sampling.LookbackParameter)
	if err != nil {
		return err
	}

	// Run a state transition to validate the tipset and compute
	// a new state to add to the store.
	st, err = syncer.consensus.RunStateTransition(ctx, next, ancestors, st)
	if err != nil {
		return err
	}
	root, err := st.Flush(ctx)
	if err != nil {
		return err
	}
	err = syncer.chainStore.PutTipSetAndState(ctx, &TipSetAndState{
		TipSet:          next,
		TipSetStateRoot: root,
	})
	if err != nil {
		return err
	}
	logSyncer.Debugf("Successfully updated store with %s", next.String())

	// TipSet is validated and added to store, now check if it is the heaviest.
	// If it is the heaviest update the chainStore.
	nextParentSt, err := syncer.tipSetState(ctx, parent.ToSortedCidSet()) // call again to get a copy
	if err != nil {
		return err
	}
	headTipSet, err := syncer.chainStore.GetTipSet(head)
	if err != nil {
		return err
	}
	headParentCids, err := headTipSet.Parents()
	if err != nil {
		return err
	}
	var headParentSt state.Tree
	if headParentCids.Len() != 0 { // head is not genesis
		headParentSt, err = syncer.tipSetState(ctx, headParentCids)
		if err != nil {
			return err
		}
	}

	heavier, err := syncer.consensus.IsHeavier(ctx, next, *headTipSet, nextParentSt, headParentSt)
	if err != nil {
		return err
	}

	if heavier {
		// Gather the entire new chain for reorg comparison.
		// See Issue #2151 for making this scalable.
		iterator := IterAncestors(ctx, syncer.chainStore, parent)
		newChain, err := CollectTipSetsOfHeightAtLeast(ctx, iterator, types.NewBlockHeight(uint64(0)))
		if err != nil {
			return err
		}
		newChain = append(newChain, next)
		if IsReorg(*headTipSet, newChain) {
			logSyncer.Infof("reorg occurring while switching from %s to %s", headTipSet.String(), next.String())
		}
		if err = syncer.chainStore.SetHead(ctx, next); err != nil {
			return err
		}
	}

	return nil
}

// widen computes a tipset implied by the input tipset and the store that
// could potentially be the heaviest tipset. In the context of EC, widen
// returns the union of the input tipset and the biggest tipset with the same
// parents from the store.
// TODO: this leaks EC abstractions into the syncer, we should think about this.
func (syncer *DefaultSyncer) widen(ctx context.Context, ts types.TipSet) (types.TipSet, error) {
	// Lookup tipsets with the same parents from the store.
	parentSet, err := ts.Parents()
	if err != nil {
		return nil, err
	}
	height, err := ts.Height()
	if err != nil {
		return nil, err
	}
	if !syncer.chainStore.HasTipSetAndStatesWithParentsAndHeight(parentSet.String(), height) {
		return nil, nil
	}
	candidates, err := syncer.chainStore.GetTipSetAndStatesByParentsAndHeight(parentSet.String(), height)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	// Only take the tipset with the most blocks (this is EC specific logic)
	max := candidates[0]
	for _, candidate := range candidates[0:] {
		if len(candidate.TipSet) > len(max.TipSet) {
			max = candidate
		}
	}

	// Add blocks of the biggest tipset in the store to a copy of ts
	wts := ts.Clone()
	for _, blk := range max.TipSet {
		if err = wts.AddBlock(blk); err != nil {
			return nil, err
		}
	}

	// check that the tipset is distinct from the input and tipsets from the store.
	if wts.String() == ts.String() || wts.String() == max.TipSet.String() {
		return nil, nil
	}

	return wts, nil
}

// HandleNewTipset extends the Syncer's chain store with the given tipset if they
// represent a valid extension. It limits the length of new chains it will
// attempt to validate and caches invalid blocks it has encountered to
// help prevent DOS.
func (syncer *DefaultSyncer) HandleNewTipset(ctx context.Context, tipsetCids types.SortedCidSet) (err error) {
	logSyncer.Debugf("Begin fetch and sync of chain with head %v", tipsetCids)
	ctx, span := trace.StartSpan(ctx, "DefaultSyncer.HandleNewTipset")
	span.AddAttributes(trace.StringAttribute("tipset", tipsetCids.String()))
	defer tracing.AddErrorEndSpan(ctx, span, &err)

	// This lock could last a long time as we fetch all the blocks needed to block the chain.
	// This is justified because the app is pretty useless until it is synced.
	// It's better for multiple calls to wait here than to try to fetch the chain independently.
	syncer.mu.Lock()
	defer syncer.mu.Unlock()

	// If the store already has all these blocks the syncer is finished.
	if syncer.chainStore.HasAllBlocks(ctx, tipsetCids.ToSlice()) {
		return nil
	}

	// Walk the chain given by the input blocks back to a known tipset in
	// the store. This is the only code that may go to the network to
	// resolve cids to blocks.
	chain, err := syncer.collectChain(ctx, tipsetCids)
	if err != nil {
		return err
	}
	parentCids, err := chain[0].Parents()
	if err != nil {
		return err
	}
	parentTs, err := syncer.chainStore.GetTipSet(parentCids)
	if err != nil {
		return err
	}
	parent := *parentTs

	// Try adding the tipsets of the chain to the store, checking for new
	// heaviest tipsets.
	for i, ts := range chain {
		// TODO: this "i==0" leaks EC specifics into syncer abstraction
		// for the sake of efficiency, consider plugging up this leak.
		if i == 0 {
			wts, err := syncer.widen(ctx, ts)
			if err != nil {
				return err
			}
			if wts != nil {
				logSyncer.Debug("attempt to sync after widen")
				err = syncer.syncOne(ctx, parent, wts)
				if err != nil {
					return err
				}
			}
		}
		if err = syncer.syncOne(ctx, parent, ts); err != nil {
			// While `syncOne` can indeed fail for reasons other than consensus,
			// adding to the badTipSets at this point is the simplest, since we
			// have access to the chain. If syncOne fails for non-consensus reasons,
			// there is no assumption that the running node's data is valid at all,
			// so we don't really lose anything with this simplification.
			syncer.badTipSets.AddChain(chain[i:])
			return err
		}
		if i%500 == 0 {
			logSyncer.Infof("processing block %d of %v for chain with head at %v", i, len(chain), tipsetCids.String())
		}
		parent = ts
	}
	return nil
}
