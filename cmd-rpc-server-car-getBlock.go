package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"sync"

	"github.com/gagliardetto/solana-go"
	cidlink "github.com/ipld/go-ipld-prime/linking/cid"
	"github.com/rpcpool/yellowstone-faithful/ipld/ipldbindcode"
	solanablockrewards "github.com/rpcpool/yellowstone-faithful/solana-block-rewards"
	"github.com/sourcegraph/jsonrpc2"
	"golang.org/x/sync/errgroup"
	"k8s.io/klog/v2"
)

type InternalError struct {
	Err error
}

func (e *InternalError) Error() string {
	return fmt.Sprintf("internal error: %s", e.Err)
}

func (e *InternalError) Unwrap() error {
	return e.Err
}

func (e *InternalError) IsPublic() bool {
	return false
}

func (e *InternalError) Is(err error) bool {
	return errors.Is(e.Err, err)
}

func (e *InternalError) As(target interface{}) bool {
	return errors.As(e.Err, target)
}

func (ser *rpcServer) getBlock(ctx context.Context, conn *requestContext, req *jsonrpc2.Request) {
	params, err := parseGetBlockRequest(req.Params)
	if err != nil {
		klog.Errorf("failed to parse params: %v", err)
		conn.ReplyWithError(
			ctx,
			req.ID,
			&jsonrpc2.Error{
				Code:    jsonrpc2.CodeInvalidParams,
				Message: "Invalid params",
			})
		return
	}
	slot := params.Slot

	block, err := ser.GetBlock(ctx, slot)
	if err != nil {
		klog.Errorf("failed to get block: %v", err)
		conn.ReplyWithError(
			ctx,
			req.ID,
			&jsonrpc2.Error{
				Code:    jsonrpc2.CodeInternalError,
				Message: "Failed to get block",
			})
		return
	}
	blocktime := uint64(block.Meta.Blocktime)

	allTransactionNodes := make([]*ipldbindcode.Transaction, 0)
	mu := &sync.Mutex{}
	var lastEntryHash solana.Hash
	{
		wg := new(errgroup.Group)
		wg.SetLimit(runtime.NumCPU())
		// get entries from the block
		for entryIndex, entry := range block.Entries {
			entryIndex := entryIndex
			entryCid := entry.(cidlink.Link).Cid
			wg.Go(func() error {
				// get the entry by CID
				entryNode, err := ser.GetEntryByCid(ctx, entryCid)
				if err != nil {
					klog.Errorf("failed to decode Entry: %v", err)
					return err
				}

				if entryIndex == len(block.Entries)-1 {
					lastEntryHash = solana.HashFromBytes(entryNode.Hash)
				}

				// get the transactions from the entry
				for _, tx := range entryNode.Transactions {
					// get the transaction by CID
					txNode, err := ser.GetTransactionByCid(ctx, tx.(cidlink.Link).Cid)
					if err != nil {
						klog.Errorf("failed to decode Transaction: %v", err)
						continue
					}
					mu.Lock()
					allTransactionNodes = append(allTransactionNodes, txNode)
					mu.Unlock()
				}
				return nil
			})
		}
		err = wg.Wait()
		if err != nil {
			klog.Errorf("failed to get entries: %v", err)
			conn.ReplyWithError(
				ctx,
				req.ID,
				&jsonrpc2.Error{
					Code:    jsonrpc2.CodeInternalError,
					Message: "Internal error",
				})
			return
		}
	}

	var allTransactions []GetTransactionResponse
	var rewards any // TODO: implement rewards as in solana
	if !block.Rewards.(cidlink.Link).Cid.Equals(DummyCID) {
		rewardsNode, err := ser.GetRewardsByCid(ctx, block.Rewards.(cidlink.Link).Cid)
		if err != nil {
			klog.Errorf("failed to decode Rewards: %v", err)
			conn.ReplyWithError(
				ctx,
				req.ID,
				&jsonrpc2.Error{
					Code:    jsonrpc2.CodeInternalError,
					Message: "Internal error",
				})
			return
		}
		buf := new(bytes.Buffer)
		buf.Write(rewardsNode.Data.Data)
		if rewardsNode.Data.Total > 1 {
			for _, _cid := range rewardsNode.Data.Next {
				nextNode, err := ser.GetDataFrameByCid(ctx, _cid.(cidlink.Link).Cid)
				if err != nil {
					klog.Errorf("failed to decode Rewards: %v", err)
					conn.ReplyWithError(
						ctx,
						req.ID,
						&jsonrpc2.Error{
							Code:    jsonrpc2.CodeInternalError,
							Message: "Internal error",
						})
					return
				}
				buf.Write(nextNode.Data)
			}
		}

		uncompressedRewards, err := decompressZstd(buf.Bytes())
		if err != nil {
			panic(err)
		}
		// try decoding as protobuf
		actualRewards, err := solanablockrewards.ParseRewards(uncompressedRewards)
		if err != nil {
			// TODO: add support for legacy rewards format
			fmt.Println("Rewards are not protobuf: " + err.Error())
		} else {
			{
				// encode rewards as JSON, then decode it as a map
				buf, err := json.Marshal(actualRewards)
				if err != nil {
					klog.Errorf("failed to encode rewards: %v", err)
					conn.ReplyWithError(
						ctx,
						req.ID,
						&jsonrpc2.Error{
							Code:    jsonrpc2.CodeInternalError,
							Message: "Internal error",
						})
					return
				}
				var m map[string]any
				err = json.Unmarshal(buf, &m)
				if err != nil {
					klog.Errorf("failed to decode rewards: %v", err)
					conn.ReplyWithError(
						ctx,
						req.ID,
						&jsonrpc2.Error{
							Code:    jsonrpc2.CodeInternalError,
							Message: "Internal error",
						})
					return
				}
				if _, ok := m["rewards"]; ok {
					// iter over rewards as an array of maps, and add a "commission" field to each = nil
					rewardsAsArray := m["rewards"].([]any)
					for _, reward := range rewardsAsArray {
						rewardAsMap := reward.(map[string]any)
						rewardAsMap["commission"] = nil

						// if it has a post_balance field, convert it to postBalance
						if _, ok := rewardAsMap["post_balance"]; ok {
							rewardAsMap["postBalance"] = rewardAsMap["post_balance"]
							delete(rewardAsMap, "post_balance")
						}
						// if it has a reward_type field, convert it to rewardType
						if _, ok := rewardAsMap["reward_type"]; ok {
							rewardAsMap["rewardType"] = rewardAsMap["reward_type"]
							delete(rewardAsMap, "reward_type")

							// if it's a float, convert to int and use rentTypeToString
							if asFloat, ok := rewardAsMap["rewardType"].(float64); ok {
								rewardAsMap["rewardType"] = rentTypeToString(int(asFloat))
							}
						}
					}
					rewards = m["rewards"]
				} else {
					klog.Errorf("did not find rewards field in rewards")
				}
			}
		}
	}
	{
		for _, transactionNode := range allTransactionNodes {
			var txResp GetTransactionResponse

			// response.Slot = uint64(transactionNode.Slot)
			// if blocktime != 0 {
			// 	response.Blocktime = &blocktime
			// }

			{
				tx, meta, err := parseTransactionAndMetaFromNode(transactionNode, ser.GetDataFrameByCid)
				if err != nil {
					klog.Errorf("failed to decode transaction: %v", err)
					conn.ReplyWithError(
						ctx,
						req.ID,
						&jsonrpc2.Error{
							Code:    jsonrpc2.CodeInternalError,
							Message: "Internal error",
						})
					return
				}
				if tx.Message.IsVersioned() {
					txResp.Version = tx.Message.GetVersion() - 1
				} else {
					txResp.Version = "legacy"
				}
				txResp.Meta = meta

				b64Tx, err := tx.ToBase64()
				if err != nil {
					klog.Errorf("failed to encode transaction: %v", err)
					conn.ReplyWithError(
						ctx,
						req.ID,
						&jsonrpc2.Error{
							Code:    jsonrpc2.CodeInternalError,
							Message: "Internal error",
						})
					return
				}

				txResp.Transaction = []any{b64Tx, "base64"}
			}

			allTransactions = append(allTransactions, txResp)
		}
	}
	var blockResp GetBlockResponse
	blockResp.Transactions = allTransactions
	blockResp.BlockTime = &blocktime
	blockResp.Blockhash = lastEntryHash.String()
	blockResp.ParentSlot = uint64(block.Meta.Parent_slot)
	blockResp.Rewards = rewards                                 // TODO: implement rewards as in solana
	blockResp.BlockHeight = calcBlockHeight(uint64(block.Slot)) // TODO: implement block height
	{
		// get parent slot
		parentSlot := uint64(block.Meta.Parent_slot)
		if parentSlot != 0 {
			parentBlock, err := ser.GetBlock(ctx, parentSlot)
			if err != nil {
				klog.Errorf("failed to decode block: %v", err)
				conn.ReplyWithError(
					ctx,
					req.ID,
					&jsonrpc2.Error{
						Code:    jsonrpc2.CodeInternalError,
						Message: "Internal error",
					})
				return
			}

			lastEntryCidOfParent := parentBlock.Entries[len(parentBlock.Entries)-1]
			parentEntryNode, err := ser.GetEntryByCid(ctx, lastEntryCidOfParent.(cidlink.Link).Cid)
			if err != nil {
				klog.Errorf("failed to decode Entry: %v", err)
				conn.ReplyWithError(
					ctx,
					req.ID,
					&jsonrpc2.Error{
						Code:    jsonrpc2.CodeInternalError,
						Message: "Internal error",
					})
				return
			}
			parentEntryHash := solana.HashFromBytes(parentEntryNode.Hash)
			blockResp.PreviousBlockhash = parentEntryHash.String()
		}
	}

	// TODO: get all the transactions from the block
	// reply with the data
	err = conn.Reply(
		ctx,
		req.ID,
		blockResp,
		func(m map[string]any) map[string]any {
			return m
		},
	)
	if err != nil {
		klog.Errorf("failed to reply: %v", err)
	}
}

//	pub enum RewardType {
//	    Fee,
//	    Rent,
//	    Staking,
//	    Voting,
//	}
func rentTypeToString(typ int) string {
	switch typ {
	case 1:
		return "Fee"
	case 2:
		return "Rent"
	case 3:
		return "Staking"
	case 4:
		return "Voting"
	default:
		return "Unknown"
	}
}