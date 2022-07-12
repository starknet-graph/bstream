// Copyright 2019 dfuse Platform Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package forkable

import (
	"fmt"
	"sync"

	"github.com/streamingfast/bstream"
	"go.uber.org/zap"
)

type Forkable struct {
	sync.RWMutex
	logger        *zap.Logger
	handler       bstream.Handler
	forkDB        *ForkDB
	lastBlockSent *bstream.Block
	lastLIBSeen   bstream.BlockRef
	filterSteps   bstream.StepType

	ensureBlockFlows                   bstream.BlockRef
	ensureBlockFlowed                  bool
	ensureAllBlocksTriggerLongestChain bool

	holdBlocksUntilLIB bool // if true, never passthrough anything before a LIB is set
	keptFinalBlocks    int  // how many blocks we keep behind LIB

	includeInitialLIB bool

	consecutiveUnlinkableBlocks int

	lastLongestChain []*Block
}

func (p *Forkable) BlocksFromFinal(blk bstream.BlockRef) (out []*ForkableBlock) {
	p.RLock()
	defer p.RUnlock()

	if !p.forkDB.HasLIB() {
		return nil
	}

	if p.lastLongestChain == nil {
		return nil
	}
	head := p.lastBlockSent

	seg, reachLIB := p.forkDB.CompleteSegment(head)
	if !reachLIB {
		return nil
	}

	blkNum := blk.Num()
	blkID := blk.ID()

	var seenBlock bool
	libNum := p.forkDB.libRef.Num()

	for i := range seg {
		ref := seg[i].AsRef()
		if !seenBlock && ref.Num() == blkNum && ref.ID() == blkID {
			seenBlock = true
		}

		if seenBlock {
			lib := p.forkDB.libRef
			if lib.Num() > ref.Num() {
				lib = ref // never send cursor with LIB > Block
			}
			step := bstream.StepNew
			if ref.Num() <= libNum {
				step = bstream.StepNewIrreversible
			}
			out = append(out, wrapBlockForkableObject(seg[i].Object.(*ForkableBlock), step, head, lib))
		}
	}

	return out
}

func blockIn(id string, array []*Block) bool {
	for _, b := range array {
		if id == b.BlockID {
			return true
		}
	}
	return false
}

func (p *Forkable) BlocksFromCursor(cursor *bstream.Cursor) (out []*ForkableBlock) {
	p.RLock()
	defer p.RUnlock()

	if !p.forkDB.HasLIB() {
		fmt.Println("no lib")
		return nil
	}

	head := p.lastBlockSent.AsRef()

	seg, reachLIB := p.forkDB.CompleteSegment(head)
	if !reachLIB {
		return nil
	}

	// cursor is not forked, we can bring it quickly to forkDB HEAD
	if blockIn(cursor.Block.ID(), seg) && blockIn(cursor.LIB.ID(), seg) {
		out = []*ForkableBlock{} // we don't return nil after this point, but maybe empty array if cursor and forkDB have exactly same LIB and head
		for i := range seg {
			if seg[i].BlockNum <= cursor.LIB.Num() {
				continue
			}

			// send irreversible notifications up to forkdb LIB
			if seg[i].BlockNum <= p.forkDB.LIBNum() {
				stepType := bstream.StepIrreversible
				if seg[i].BlockNum > cursor.Block.Num() {
					stepType = bstream.StepNewIrreversible
				}
				out = append(out, wrapBlockForkableObject(seg[i].Object.(*ForkableBlock), stepType, head, seg[i].AsRef()))
			}

			// send NEW from cursor's block up to forkdb Head
			if seg[i].BlockNum > cursor.Block.Num() {
				out = append(out, wrapBlockForkableObject(seg[i].Object.(*ForkableBlock), bstream.StepNew, head, p.forkDB.libRef))
				continue
			}

		}
		return
	}

	// cursor is forked, trying to bring user back to the canonical chain
	var undos []*ForkableBlock
	blockID := cursor.Block.ID()
	for {
		found := p.forkDB.BlockForID(blockID)
		if found == nil {
			return nil
		}
		fb := found.Object.(*ForkableBlock)

		alreadyUndone := blockID == cursor.Block.ID() && cursor.Step == bstream.StepUndo
		if !alreadyUndone {
			undos = append(undos, wrapBlockForkableObject(fb, bstream.StepUndo, head, cursor.LIB))
		}

		blockID = found.PreviousBlockID
		if blockIn(blockID, seg) {
			break
		}
	}

	newCursor := &bstream.Cursor{
		Step:      bstream.StepNew,
		Block:     bstream.NewBlockRef(blockID, p.forkDB.BlockForID(blockID).BlockNum),
		HeadBlock: head,
		LIB:       cursor.LIB,
	}

	// recursive call, now that we have a non-forked cursor
	newBlocks := p.BlocksFromCursor(newCursor)
	if newBlocks == nil {
		return nil
	}

	return append(undos, newBlocks...)
}

func wrapBlockForkableObject(blk *ForkableBlock, step bstream.StepType, head bstream.BlockRef, lib bstream.BlockRef) *ForkableBlock {
	return &ForkableBlock{
		Block: blk.Block,
		Obj: &ForkableObject{
			step:        step,
			headBlock:   head,
			block:       blk.Block.AsRef(),
			lastLIBSent: lib,
			Obj:         blk.Obj,
		},
	}
}

type ForkableObject struct {
	step bstream.StepType

	// The three following fields are filled when handling multi-block steps, like when passing Irreversibile segments, the whole segment is represented in here.
	StepCount  int                          // Total number of steps in multi-block steps.
	StepIndex  int                          // Index for the current block
	StepBlocks []*bstream.PreprocessedBlock // You can decide to process them when StepCount == StepIndex +1 or when StepIndex == 0 only.

	//	ForkDB *ForkDB // ForkDB is a reference to the `Forkable`'s ForkDB instance. Provided you don't use it in goroutines, it is safe for use in `ProcessBlock` calls.

	headBlock   bstream.BlockRef
	block       bstream.BlockRef
	lastLIBSent bstream.BlockRef

	// Object that was returned by PreprocessBlock(). Could be nil
	Obj interface{}
}

func (fobj *ForkableObject) Step() bstream.StepType {
	return fobj.step
}

func (fobj *ForkableObject) WrappedObject() interface{} {
	return fobj.Obj
}

func (fobj *ForkableObject) Cursor() *bstream.Cursor {
	if fobj == nil ||
		fobj.block == nil ||
		fobj.headBlock == nil ||
		fobj.lastLIBSent == nil {
		return bstream.EmptyCursor
	}

	return &bstream.Cursor{
		Step:      fobj.step,
		Block:     fobj.block,
		HeadBlock: fobj.headBlock,
		LIB:       fobj.lastLIBSent,
	}
}

type ForkableBlock struct {
	Block     *bstream.Block
	Obj       interface{}
	sentAsNew bool
}

func New(h bstream.Handler, opts ...Option) *Forkable {
	f := &Forkable{
		filterSteps:      bstream.StepsAll,
		handler:          h,
		forkDB:           NewForkDB(),
		ensureBlockFlows: bstream.BlockRefEmpty,
		lastLIBSeen:      bstream.BlockRefEmpty,
		logger:           zlog,
	}

	for _, opt := range opts {
		opt(f)
	}

	// Done afterwards so forkdb can get configured forkable logger from options
	f.forkDB.logger = f.logger

	return f
}

// NewWithLIB DEPRECATED, use `New(h, WithExclusiveLIB(libID))`.  Also use `EnsureBlockFlows`, `WithFilters`, `EnsureAllBlocksTriggerLongestChain` options
func NewWithLIB(libID bstream.BlockRef, h bstream.Handler) { return }

func (p *Forkable) targetChainBlock(blk *bstream.Block) bstream.BlockRef {
	if p.ensureBlockFlows.ID() != "" && !p.ensureBlockFlowed {
		return p.ensureBlockFlows
	}

	return blk
}

func (p *Forkable) matchFilter(filter bstream.StepType) bool {
	return p.filterSteps&filter != 0
}

func (p *Forkable) computeNewLongestChain(ppBlk *ForkableBlock) []*Block {
	longestChain := p.lastLongestChain
	blk := ppBlk.Block

	canSkipRecompute := false
	if len(longestChain) != 0 &&
		blk.PreviousID() == longestChain[len(longestChain)-1].BlockID && // optimize if adding block linearly
		p.forkDB.LIBID() == longestChain[0].PreviousBlockID { // do not optimize if the lib moved (should truncate up to lib)
		canSkipRecompute = true
	}

	if canSkipRecompute {
		longestChain = append(longestChain, &Block{
			BlockID:         blk.ID(), // NOTE: we don't want "Previous" because ReversibleSegment does not give them
			BlockNum:        blk.Num(),
			Object:          ppBlk,
			PreviousBlockID: ppBlk.Block.PreviousId,
		})
	} else {
		longestChain, _ = p.forkDB.ReversibleSegment(p.targetChainBlock(blk))
	}
	p.lastLongestChain = longestChain
	return longestChain

}

func (p *Forkable) ProcessBlock(blk *bstream.Block, obj interface{}) error {
	p.Lock()
	defer p.Unlock()

	if blk.Id == blk.PreviousId {
		return fmt.Errorf("invalid block ID detected on block %s (previousID: %s), bad data", blk.String(), blk.PreviousId)
	}

	if blk.Num() < p.forkDB.LIBNum() && p.lastBlockSent != nil {
		return nil
	}

	zlogBlk := p.logger.With(zap.Stringer("block", blk))

	// TODO: consider an `initialHeadBlockID`, triggerNewLongestChain also when the initialHeadBlockID's BlockNum == blk.Num()
	triggersNewLongestChain := p.triggersNewLongestChain(blk)

	if tracer.Enabled() {
		zlogBlk.Debug("processing block", zap.Bool("new_longest_chain", triggersNewLongestChain))
	} else if blk.Number%600 == 0 {
		zlogBlk.Debug("processing block (1/600 sampling)", zap.Bool("new_longest_chain", triggersNewLongestChain))
	}

	if p.includeInitialLIB && p.lastBlockSent == nil && blk.ID() == p.forkDB.LIBID() {
		return p.processInitialInclusiveIrreversibleBlock(blk, obj, true)
	}

	ppBlk := &ForkableBlock{Block: blk, Obj: obj}

	var undos, redos []*ForkableBlock
	if p.matchFilter(bstream.StepUndo) {
		if triggersNewLongestChain && p.lastBlockSent != nil {
			undos, redos = p.sentChainSwitchSegments(zlogBlk, p.lastBlockSent.ID(), blk.PreviousID())
		}
	}

	if exists := p.forkDB.AddLink(blk, blk.PreviousID(), ppBlk); exists {
		return nil
	}

	var firstIrreverbleBlock *Block
	if !p.forkDB.HasLIB() { // always skip processing until LIB is set
		p.forkDB.SetLIB(blk, blk.PreviousID(), blk.LibNum)
		if p.forkDB.HasLIB() { //this is an edge case. forkdb will not is returning the 1st lib in the forkDB.HasNewIrreversibleSegment call
			if p.forkDB.libRef.Num() == blk.Number { // this block just came in and was determined as LIB, it is probably first streamable block and must be processed.
				return p.processInitialInclusiveIrreversibleBlock(blk, obj, true)
			}
			firstIrreverbleBlock = p.forkDB.BlockForID(p.forkDB.libRef.ID())
		} else {
			if p.holdBlocksUntilLIB {
				return nil
			}
		}
	}

	longestChain := p.computeNewLongestChain(ppBlk)
	if longestChain == nil && p.forkDB.HasLIB() {
		p.consecutiveUnlinkableBlocks++
		if p.consecutiveUnlinkableBlocks > 20 {
			zlogBlk.Warn("too many consecutive unlinkable blocks", zap.Any("forkdb_nums", p.forkDB.nums))
			return fmt.Errorf("too many consecutive unlinkable blocks")
		}
	} else {
		p.consecutiveUnlinkableBlocks = 0
	}
	if !triggersNewLongestChain || len(longestChain) == 0 {
		return nil
	}

	if tracer.Enabled() {
		zlogBlk.Debug("got longest chain", zap.Int("chain_length", len(longestChain)), zap.Int("undos_length", len(undos)), zap.Int("redos_length", len(redos)))
	} else if blk.Number%600 == 0 {
		zlogBlk.Debug("got longest chain (1/600 sampling)", zap.Int("chain_length", len(longestChain)), zap.Int("undos_length", len(undos)), zap.Int("redos_length", len(redos)))
	}

	if p.matchFilter(bstream.StepUndo) {
		if err := p.processBlocks(blk, undos, bstream.StepUndo); err != nil {
			return err
		}
	}

	if p.matchFilter(bstream.StepNew) {
		if err := p.processBlocks(blk, redos, bstream.StepNew); err != nil {
			return err
		}
	}

	if err := p.processNewBlocks(longestChain); err != nil {
		return err
	}

	if p.lastBlockSent == nil {
		return nil
	}

	if !p.forkDB.HasLIB() {
		return nil
	}

	// All this code isn't reachable unless a LIB is set in the ForkDB

	newLIBNum := p.lastBlockSent.LibNum
	newHeadBlock := p.lastBlockSent

	libRef := p.forkDB.BlockInCurrentChain(newHeadBlock, newLIBNum)
	if libRef.ID() == "" {

		// this happens when the lib was set initially and we have not yet filled the lib->head buffer
		if tracer.Enabled() {
			zlogBlk.Debug("missing links to reach lib_num", zap.Stringer("new_head_block", newHeadBlock), zap.Uint64("new_lib_num", newLIBNum))
		} else if newHeadBlock.Number%600 == 0 {
			zlogBlk.Debug("missing links to reach lib_num (1/600 sampling)", zap.Stringer("new_head_block", newHeadBlock), zap.Uint64("new_lib_num", newLIBNum))
		}

		return nil
	}

	// TODO: check preconditions here, and decide on whether we
	// continue or not early return would be perfect if there's no
	// `irreversibleSegment` or `stalledBlocks` to process.
	hasNew, irreversibleSegment, stalledBlocks := p.forkDB.HasNewIrreversibleSegment(libRef)
	if firstIrreverbleBlock != nil {
		irreversibleSegment = append(irreversibleSegment, firstIrreverbleBlock)
	}
	if !hasNew && firstIrreverbleBlock == nil {
		return nil
	}

	if tracer.Enabled() {
		zlogBlk.Debug("moving lib", zap.Stringer("lib", libRef))
	} else if libRef.Num()%600 == 0 {
		zlogBlk.Debug("moving lib (1/600)", zap.Stringer("lib", libRef))
	}

	p.forkDB.MoveLIB(libRef)
	_ = p.forkDB.PurgeBeforeLIB(p.keptFinalBlocks)

	if err := p.processIrreversibleSegment(irreversibleSegment, ppBlk.Block); err != nil {
		return err
	}

	if err := p.processStalledSegment(stalledBlocks, ppBlk.Block); err != nil {
		return err
	}

	return nil
}

func ids(blocks []*ForkableBlock) (ids []string) {
	ids = make([]string, len(blocks))
	for i, obj := range blocks {
		ids[i] = obj.Block.ID()
	}

	return
}

func (p *Forkable) sentChainSwitchSegments(zlogger *zap.Logger, currentHeadBlockID string, newHeadsPreviousID string) (undos []*ForkableBlock, redos []*ForkableBlock) {
	if currentHeadBlockID == newHeadsPreviousID {
		return
	}

	undoIDs, redoIDs := p.forkDB.ChainSwitchSegments(currentHeadBlockID, newHeadsPreviousID)

	undos = p.sentChainSegment(undoIDs, false)
	redos = p.sentChainSegment(redoIDs, true)
	return
}

func (p *Forkable) sentChainSegment(ids []string, doingRedos bool) (ppBlocks []*ForkableBlock) {
	for _, blockID := range ids {
		blkObj := p.forkDB.BlockForID(blockID)
		if blkObj == nil {
			panic(fmt.Errorf("block for id returned nil for id %q, this would panic later on", blockID))
		}

		ppBlock := blkObj.Object.(*ForkableBlock)
		if doingRedos && !ppBlock.sentAsNew {
			continue
		}

		ppBlocks = append(ppBlocks, ppBlock)
	}
	return
}

func (p *Forkable) processBlocks(currentBlock bstream.BlockRef, blocks []*ForkableBlock, step bstream.StepType) error {
	var objs []*bstream.PreprocessedBlock

	for _, block := range blocks {
		objs = append(objs, &bstream.PreprocessedBlock{
			Block: block.Block,
			Obj:   block.Obj,
		})
	}

	for idx, block := range blocks {

		lib := p.lastLIBSeen
		if bstream.IsEmpty(lib) {
			lib = p.forkDB.libRef
		}
		fo := &ForkableObject{
			step:        step,
			lastLIBSent: lib,
			Obj:         block.Obj,
			headBlock:   currentBlock,
			block:       block.Block,

			StepIndex:  idx,
			StepCount:  len(blocks),
			StepBlocks: objs,
		}

		err := p.handler.ProcessBlock(block.Block, fo)

		p.logger.Debug("sent block", zap.Stringer("block", block.Block), zap.Stringer("step_type", step))
		if err != nil {
			return fmt.Errorf("process block [%s] step=%q: %w", block.Block, step, err)
		}
	}
	return nil
}

func (p *Forkable) processNewBlocks(longestChain []*Block) (err error) {
	headBlock := longestChain[len(longestChain)-1]
	for _, b := range longestChain {
		ppBlk := b.Object.(*ForkableBlock)
		if ppBlk.sentAsNew {
			// Sadly, there was a debug log line here, but it's so a pain to have when debug, since longuest
			// chain is iterated over and over again generating tons of this (now gone) log line. For this,
			// it was removed to make it easier to track what happen.
			continue
		}

		if p.matchFilter(bstream.StepNew) {

			lib := p.lastLIBSeen
			if bstream.IsEmpty(lib) {
				lib = p.forkDB.libRef
			}
			fo := &ForkableObject{
				headBlock:   headBlock.AsRef(),
				block:       b.AsRef(),
				step:        bstream.StepNew,
				lastLIBSent: lib,
				Obj:         ppBlk.Obj,
			}

			err = p.handler.ProcessBlock(ppBlk.Block, fo)
			if err != nil {
				return
			}
		}

		if tracer.Enabled() {
			p.logger.Debug("sending block as new to consumer", zap.Stringer("block", ppBlk.Block))
		} else if ppBlk.Block.Number%600 == 0 {
			p.logger.Debug("sending block as new to consumer (1/600 sampling)", zap.Stringer("block", ppBlk.Block))
		}

		zlog.Debug("block sent as new", zap.Stringer("pblk.block", ppBlk.Block))
		p.blockFlowed(ppBlk.Block)
		ppBlk.sentAsNew = true
		p.lastBlockSent = ppBlk.Block
	}

	return
}

func (p *Forkable) processInitialInclusiveIrreversibleBlock(blk *bstream.Block, obj interface{}, sendAsNew bool) error {
	// Normally extracted from ForkDB, we create it here:
	singleBlock := &Block{
		BlockID:  blk.ID(),
		BlockNum: blk.Num(),
		// Other fields not needed by `processNewBlocks`
		Object: &ForkableBlock{
			// WARN: this ForkDB doesn't have a reference to the current block, hopefully downstream doesn't need that (!)
			Block: blk,
			Obj:   obj,
		},
	}

	tinyChain := []*Block{singleBlock}

	if sendAsNew {
		if err := p.processNewBlocks(tinyChain); err != nil {
			return err
		}
	}

	if err := p.processIrreversibleSegment(tinyChain, blk); err != nil {
		return err
	}

	return nil
}

func (p *Forkable) processIrreversibleSegment(irreversibleSegment []*Block, headBlock bstream.BlockRef) error {
	if p.matchFilter(bstream.StepIrreversible) {
		var irrGroup []*bstream.PreprocessedBlock
		for _, irrBlock := range irreversibleSegment {
			preprocBlock := irrBlock.Object.(*ForkableBlock)
			irrGroup = append(irrGroup, &bstream.PreprocessedBlock{
				Block: preprocBlock.Block,
				Obj:   preprocBlock.Obj,
			})
		}

		for idx, irrBlock := range irreversibleSegment {
			preprocBlock := irrBlock.Object.(*ForkableBlock)

			objWrap := &ForkableObject{
				step:        bstream.StepIrreversible,
				lastLIBSent: preprocBlock.Block.AsRef(), // we are that lastLIBSent
				Obj:         preprocBlock.Obj,
				block:       preprocBlock.Block.AsRef(),
				headBlock:   headBlock,

				StepIndex:  idx,
				StepCount:  len(irreversibleSegment),
				StepBlocks: irrGroup,
			}

			if err := p.handler.ProcessBlock(preprocBlock.Block, objWrap); err != nil {
				return err
			}
		}
	}

	// Always set the last LIB sent used in the cursor to define where to start back the ForkDB
	if len(irreversibleSegment) > 0 {
		irrBlock := irreversibleSegment[len(irreversibleSegment)-1]
		p.lastLIBSeen = irrBlock.AsRef()
	}

	return nil
}

func (p *Forkable) processStalledSegment(stalledBlocks []*Block, headBlock bstream.BlockRef) error {
	if p.matchFilter(bstream.StepStalled) {
		var stalledGroup []*bstream.PreprocessedBlock
		for _, staleBlock := range stalledBlocks {
			preprocBlock := staleBlock.Object.(*ForkableBlock)
			stalledGroup = append(stalledGroup, &bstream.PreprocessedBlock{
				Block: preprocBlock.Block,
				Obj:   preprocBlock.Obj,
			})
		}

		for idx, staleBlock := range stalledBlocks {
			preprocBlock := staleBlock.Object.(*ForkableBlock)

			objWrap := &ForkableObject{
				step:        bstream.StepStalled,
				lastLIBSent: p.lastLIBSeen,
				Obj:         preprocBlock.Obj,
				block:       staleBlock.AsRef(),
				headBlock:   headBlock,

				StepIndex:  idx,
				StepCount:  len(stalledBlocks),
				StepBlocks: stalledGroup,
			}

			if err := p.handler.ProcessBlock(preprocBlock.Block, objWrap); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *Forkable) blockFlowed(blockRef bstream.BlockRef) {
	if p.ensureBlockFlows.ID() == "" {
		return
	}

	if p.ensureBlockFlowed {
		return
	}

	if blockRef.ID() == p.ensureBlockFlows.ID() {
		p.ensureBlockFlowed = true
	}
}

func (p *Forkable) triggersNewLongestChain(blk *bstream.Block) bool {
	if p.ensureAllBlocksTriggerLongestChain {
		return true
	}

	if p.lastBlockSent == nil {
		return true
	}

	if blk.Num() > p.lastBlockSent.Num() {
		return true
	}

	return false
}
