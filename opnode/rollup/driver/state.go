package driver

import (
	"context"
	"time"

	"github.com/ethereum-optimism/optimistic-specs/opnode/eth"
	"github.com/ethereum-optimism/optimistic-specs/opnode/rollup"
	"github.com/ethereum-optimism/optimistic-specs/opnode/rollup/derive"
	"github.com/ethereum-optimism/optimistic-specs/opnode/rollup/sync"
	"github.com/ethereum/go-ethereum/log"
)

type ReorgType int

const (
	NoReorg ReorgType = iota
	ShallowReorg
	DeepReorg
)

type state struct {
	// Chain State
	l1Head      eth.L1BlockRef // Latest recorded head of the L1 Chain
	l2Head      eth.L2BlockRef // L2 Unsafe Head
	l2SafeHead  eth.L2BlockRef // L2 Safe Head - this is the head of the L2 chain as derived from L1 (thus it is Sequencer window blocks behind)
	l2Finalized eth.BlockID    // L2 Block that will never be reversed
	l1Window    []eth.BlockID  // l1Window buffers the next L1 block IDs to derive new L2 blocks from, with increasing block height.

	// Rollup config
	Config    rollup.Config
	sequencer bool

	// Connections (in/out)
	l1Heads <-chan eth.L1BlockRef
	l1      L1Chain
	l2      L2Chain
	output  outputInterface
	bss     BatchSubmitter

	log  log.Logger
	done chan struct{}
}

// // shouldRunEpoch returns true if there is a full sequencing window between the L2 Safe Head's L1 Origin and the L1 Head.
// func (s *state) shouldRunEpoch() bool {
// 	return s.l1Head.Self.Number-s.l2SafeHead.L1Origin.Number >= s.Config.SeqWindowSize
// }

func NewState(log log.Logger, config rollup.Config, l1 L1Chain, l2 L2Chain, output outputInterface, submitter BatchSubmitter, sequencer bool) *state {
	return &state{
		Config:    config,
		done:      make(chan struct{}),
		log:       log,
		l1:        l1,
		l2:        l2,
		output:    output,
		bss:       submitter,
		sequencer: sequencer,
	}
}

func (s *state) Start(ctx context.Context, l1Heads <-chan eth.L1BlockRef) error {
	l1Head, err := s.l1.L1HeadBlockRef(ctx)
	if err != nil {
		return err
	}
	l2Head, err := s.l2.L2BlockRefByNumber(ctx, nil)
	if err != nil {
		return err
	}

	// TODO:
	// 1. Pull safehead from sync-start algorithm
	// 2. Check if heads are below genesis & if so, bump to genesis.
	s.l1Head = l1Head
	s.l2Head = l2Head
	s.l2SafeHead = l2Head
	s.l1Heads = l1Heads

	go s.loop()
	return nil
}

func (s *state) Close() error {
	close(s.done)
	return nil
}

// l1WindowEnd returns the last block that should be used as `base` to L1ChainWindow.
// This is either the last block of the window, or the L1 base block if the window is not populated.
func (s *state) l1WindowEnd() eth.BlockID {
	if len(s.l1Window) == 0 {
		return s.l2Head.L1Origin
	}
	return s.l1Window[len(s.l1Window)-1]
}

// extendL1Window extends the cached L1 window by pulling blocks from L1.
// It starts just after `s.l1WindowEnd()`.
func (s *state) extendL1Window(ctx context.Context) error {
	s.log.Trace("Extending the cached window from L1", "cached_size", len(s.l1Window), "window_end", s.l1WindowEnd())
	nexts, err := s.l1.L1Range(ctx, s.l1WindowEnd())
	if err != nil {
		return err
	}
	s.l1Window = append(s.l1Window, nexts...)
	return nil
}

// sequencingWindow returns the next sequencing window and true if it exists, (nil, false) if
// there are not enough saved blocks.
func (s *state) sequencingWindow() ([]eth.BlockID, bool) {
	if len(s.l1Window) < int(s.Config.SeqWindowSize) {
		return nil, false
	}
	return s.l1Window[:int(s.Config.SeqWindowSize)], true
}

func (s *state) findNextL1Origin(ctx context.Context) (eth.BlockID, error) {
	// [prev L2 + blocktime, L1 Bock)
	currentL1Origin := s.l2Head.L1Origin
	if currentL1Origin.Hash == s.l1Head.Self.Hash {
		return currentL1Origin, nil
	}
	s.log.Info("Find next l1Origin", "l2Head", s.l2Head, "l1Origin", currentL1Origin)
	if s.l2Head.Self.Time+s.Config.BlockTime >= currentL1Origin.Time {
		// TODO: Need to walk more?
		ref, err := s.l1.L1BlockRefByNumber(ctx, currentL1Origin.Number+1)
		s.log.Info("Looking up new L1 Origin", "nextL1Origin", ref)
		return ref.Self, err
	}
	return currentL1Origin, nil
}

func findL1ReorgBase(ctx context.Context, newL1Head eth.L1BlockRef, l1 L1Chain) (eth.L1BlockRef, error) {
	for n := newL1Head; ; {
		canonical, err := l1.L1BlockRefByNumber(ctx, n.Self.Number)
		if err != nil {
			return eth.L1BlockRef{}, nil
		}
		if canonical.Self.Hash == n.Self.Hash {
			return n, nil
		}
		n, err = l1.L1BlockRefByHash(ctx, n.Parent.Hash)
		if err != nil {
			return eth.L1BlockRef{}, nil
		}
	}
}

// handleEpoch inserts an L2 epoch into the chain. It is assumed to be the tip of the safe chain,
// however there may be an unsafe portion of the chain. If there is an unsafe portion of the chain,
// this function checks blocks for validity and may perform a shllow reorg.
func (s *state) handleEpoch(ctx context.Context) (eth.L2BlockRef, eth.L2BlockRef, ReorgType, error) {
	log := s.log.New("l2Head", s.l2Head, "l2SafeHead", s.l2SafeHead, "l1Base", s.l2SafeHead.L1Origin)
	log.Trace("Handling epoch")
	// Extend cached window if we do not have enough saved blocks
	if len(s.l1Window) < int(s.Config.SeqWindowSize) {
		err := s.extendL1Window(context.Background())
		if err != nil {
			s.log.Error("Could not extend the cached L1 window", "err", err, "window_end", s.l1WindowEnd())
			return s.l2Head, s.l2SafeHead, NoReorg, err
		}
	}

	// Get next window (& ensure that it exists)
	window, ok := s.sequencingWindow()
	if !ok {
		s.log.Trace("Not enough cached blocks to run step", "cached_window_len", len(s.l1Window))
		return s.l2Head, s.l2SafeHead, NoReorg, nil
	}
	// TODO: switch between modes here.
	newL2Head, err := s.output.step(ctx, s.l2SafeHead, s.l2Finalized, s.l2Head.Self, window)
	if err != nil {
		s.log.Error("Error in running the output step.", "err", err)
		return s.l2Head, s.l2SafeHead, NoReorg, err
	}
	s.l1Window = s.l1Window[1:] // TODO: Where to place this
	// Bump head if safehead and head are already the same. Note: not strictly true and should handle better.
	// head := s.l2Head
	// if head.Self.Hash == s.l2SafeHead.Self.Hash {
	// 	head = newL2Head
	// }
	return newL2Head, newL2Head, NoReorg, nil
}

func (s *state) loop() {
	s.log.Info("State loop started")
	ctx := context.Background()
	var l2BlockCreation <-chan time.Time
	if s.sequencer {
		l2BlockCreationTicker := time.NewTicker(time.Duration(s.Config.BlockTime) * time.Second)
		defer l2BlockCreationTicker.Stop()
		l2BlockCreation = l2BlockCreationTicker.C
	}

	stepRequest := make(chan struct{}, 1)
	l2BlockCreationReq := make(chan struct{}, 1)

	createBlock := func() {
		select {
		case l2BlockCreationReq <- struct{}{}:
		default:
		}
	}

	requestStep := func() {
		select {
		case stepRequest <- struct{}{}:
		default:
		}
	}

	requestStep()

	for {
		select {
		case <-s.done:
			return
		case <-l2BlockCreation:
			s.log.Trace("L2 Creation Ticker")
			createBlock()
		case <-l2BlockCreationReq:
			nextOrigin, err := s.findNextL1Origin(context.Background())
			if err != nil {
				s.log.Error("Error finding next L1 Origin")
				continue
			}
			if nextOrigin.Time <= s.Config.BlockTime+s.l2Head.Self.Time {
				s.log.Trace("Skipping block production", "l2Head", s.l2Head)
				continue
			}
			// Don't produce blocks until past the L1 genesis
			if nextOrigin.Number <= s.Config.Genesis.L1.Number {
				continue
			}
			// 2. Ask output to create new block
			newUnsafeL2Head, batch, err := s.output.newBlock(context.Background(), s.l2Finalized, s.l2Head, s.l2SafeHead.Self, nextOrigin)
			if err != nil {
				s.log.Error("Could not extend chain as sequencer", "err", err, "l2UnsafeHead", s.l2Head, "l1Origin", nextOrigin)
				continue
			}
			// 3. Update unsafe l2 head
			s.l2Head = newUnsafeL2Head
			s.log.Trace("Created new l2 block", "l2UnsafeHead", s.l2Head)
			// 4. Ask for batch submission
			go func() {
				_, err := s.bss.Submit(&s.Config, []*derive.BatchData{batch}) // TODO: submit multiple batches
				if err != nil {
					s.log.Error("Error submitting batch", "err", err)
				}
			}()
			if nextOrigin.Time > s.l2Head.Self.Time+s.Config.BlockTime {
				s.log.Trace("Asking for a second L2 block asap", "l2Head", s.l2Head)
				createBlock()
			}

		case newL1Head := <-s.l1Heads:
			s.log.Trace("Received new L1 Head", "new_head", newL1Head.Self, "old_head", s.l1Head)
			if s.l1Head.Self.Hash == newL1Head.Self.Hash {
				log.Trace("Received L1 head signal that is the same as the current head", "l1_head", newL1Head.Self)
			} else if s.l1Head.Self.Hash == newL1Head.Parent.Hash {
				s.log.Trace("Linear extension")
				s.l1Head = newL1Head
				if s.l1WindowEnd() == newL1Head.Parent {
					s.l1Window = append(s.l1Window, newL1Head.Self)
				}
			} else {
				// Not strictly always a reorg, but that is the most likely case
				s.log.Warn("L1 Head signal indicates an L1 re-org", "old_l1_head", s.l1Head, "new_l1_head_parent", newL1Head.Parent, "new_l1_head", newL1Head.Self)
				base, err := findL1ReorgBase(ctx, newL1Head, s.l1)
				if err != nil {
					s.log.Error("Could not get fetch L1 reorg base when trying to handle a re-org", "err", err)
					continue
				}
				unsafeL2Head, err := sync.FindUnsafeL2Head(ctx, s.l2Head, base.Self, s.l2, &s.Config.Genesis)
				if err != nil {
					s.log.Error("Could not get new unsafe L2 head when trying to handle a re-org", "err", err)
					continue
				}
				safeL2Head, err := sync.FindSafeL2Head(ctx, s.l2Head, base.Self, int(s.Config.SeqWindowSize), s.l2, &s.Config.Genesis)
				if err != nil {
					s.log.Error("Could not get new safe L2 head when trying to handle a re-org", "err", err)
					continue
				}
				// TODO: Fork choice update
				s.l1Head = newL1Head
				s.l1Window = nil
				s.l2Head = unsafeL2Head // Note that verify only nodes can get an unsafe head because of a reorg. May want to remove that.
				s.l2SafeHead = safeL2Head
			}

			// Run step if we are able to
			if s.l1Head.Self.Number-s.l2Head.L1Origin.Number >= s.Config.SeqWindowSize {
				requestStep()
			}
		case <-stepRequest:
			if s.sequencer {
				s.log.Trace("Skipping extension based on L1 chain as sequencer")
				continue
			}
			s.log.Trace("Got step request")
			// Handle epoch always returns valid values for head/safehead
			newHead, newSafeHead, _, err := s.handleEpoch(context.Background())
			if err != nil {
				s.log.Error("Error handling epoch", "err", err)
			}
			s.l2Head = newHead
			s.l2SafeHead = newSafeHead

			// Immediately run next step if we have enough blocks.
			if s.l1Head.Self.Number-s.l2Head.L1Origin.Number >= s.Config.SeqWindowSize {
				requestStep()
			}

		}
	}

}
