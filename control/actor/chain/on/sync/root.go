package sync

import (
	"context"
	"fmt"
	"github.com/libp2p/go-libp2p-core/protocol"
	"github.com/protolambda/rumor/chain"
	bdb "github.com/protolambda/rumor/chain/db/blocks"
	"github.com/protolambda/rumor/control/actor/base"
	"github.com/protolambda/rumor/control/actor/flags"
	"github.com/protolambda/rumor/p2p/rpc/methods"
	"github.com/protolambda/rumor/p2p/rpc/reqresp"
	"github.com/protolambda/zrnt/eth2/beacon"
	"time"
)

const MAX_REQUEST_BLOCKS = 1024

type ByRootCmd struct {
	*base.Base

	Blocks bdb.DB
	Chain  chain.FullChain

	PeerID flags.PeerIDFlag `ask:"--peer" help:"Peers to make blocks-by-root request to."`

	Roots []beacon.Root `ask:"--roots" help:"Block roots to request"`

	Timeout     time.Duration         `ask:"--timeout" help:"Timeout for full request and response. 0 to disable"`
	Compression flags.CompressionFlag `ask:"--compression" help:"Compression. 'none' to disable, 'snappy' for streaming-snappy"`
	Store       bool                  `ask:"--store" help:"If the blocks should be stored in the blocks DB"`
	Process     bool                  `ask:"--process" help:"If the blocks should be added to the current chain view, ignored otherwise"`
}

func (c *ByRootCmd) Default() {
	c.Timeout = 20 * time.Second
	c.Compression.Compression = reqresp.SnappyCompression{}
	c.Store = true
	c.Process = true
}

func (c *ByRootCmd) Help() string {
	return "Sync the chain by requesting blocks by root."
}

func (c *ByRootCmd) Run(ctx context.Context, args ...string) error {
	h, err := c.Host()
	if err != nil {
		return err
	}
	sFn := reqresp.NewStreamFn(h.NewStream)

	reqCtx := ctx
	if c.Timeout != 0 {
		reqCtx, _ = context.WithTimeout(reqCtx, c.Timeout)
	}

	method := &methods.BlocksByRangeRPCv1
	peerId := c.PeerID.PeerID

	protocolId := method.Protocol
	if c.Compression.Compression != nil {
		protocolId += protocol.ID("_" + c.Compression.Compression.Name())
	}

	pstore := h.Peerstore()
	if protocols, err := pstore.SupportsProtocols(peerId, string(protocolId)); err != nil {
		return fmt.Errorf("failed to check protocol support of peer %s: %v", peerId.String(), err)
	} else if len(protocols) == 0 {
		return fmt.Errorf("peer %s does not support protocol %s", peerId.String(), protocolId)
	}

	req := methods.BlocksByRootReq(c.Roots)
	if len(req) > MAX_REQUEST_BLOCKS {
		c.Log.Warn("Running blocks-by-root request with too many block roots. Max is %d, got %d",
			MAX_REQUEST_BLOCKS, len(req))
	}
	var block beacon.SignedBeaconBlock
	return method.RunRequest(reqCtx, sFn, peerId, c.Compression.Compression, reqresp.RequestSSZInput{Obj: &req}, uint64(len(req)),
		func(chunk reqresp.ChunkedResponseHandler) error {
			resultCode := chunk.ResultCode()
			f := map[string]interface{}{
				"from":        peerId.String(),
				"chunk_index": chunk.ChunkIndex(),
				"chunk_size":  chunk.ChunkSize(),
				"result_code": resultCode,
			}
			switch resultCode {
			case reqresp.ServerErrCode, reqresp.InvalidReqCode:
				msg, err := chunk.ReadErrMsg()
				if err != nil {
					return err
				}
				f["msg"] = msg
				c.Log.WithField("chunk", f).Warn("Received error response")
				return fmt.Errorf("got error response %d on chunk %d: %s", resultCode, chunk.ChunkIndex(), msg)
			case reqresp.SuccessCode:
				// re-use the allocated block for each chunk.
				if err := chunk.ReadObj(&block); err != nil {
					return err
				}
				withRoot := bdb.WithRoot(&block)
				expectedRoot := req[chunk.ChunkIndex()]
				if withRoot.Root != expectedRoot {
					return fmt.Errorf("bad block, expected root %x, got %x", withRoot.Root, expectedRoot)
				}
				if c.Store {
					exists, err := c.Blocks.Store(ctx, withRoot)
					if err != nil {
						return fmt.Errorf("failed to store block: %v", err)
					}
					f["known"] = exists
				}
				if c.Process {
					if err := c.Chain.AddBlock(ctx, &block); err != nil {
						return fmt.Errorf("failed to process block: %v", err)
					}
				}
				c.Log.WithField("chunk", f).Debug("Received block")
				return nil
			default:
				return fmt.Errorf("received chunk (index %d, size %d) with unknown result code %d", chunk.ChunkIndex(), chunk.ChunkSize(), resultCode)
			}
		})
}
