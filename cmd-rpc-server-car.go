package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"strings"
	"time"

	bin "github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go"
	"github.com/gin-gonic/gin"
	"github.com/ipfs/go-cid"
	"github.com/ipld/go-car/util"
	carv2 "github.com/ipld/go-car/v2"
	cidlink "github.com/ipld/go-ipld-prime/linking/cid"
	jsoniter "github.com/json-iterator/go"
	"github.com/rpcpool/yellowstone-faithful/compactindex"
	"github.com/rpcpool/yellowstone-faithful/compactindex36"
	"github.com/rpcpool/yellowstone-faithful/ipld/ipldbindcode"
	"github.com/rpcpool/yellowstone-faithful/iplddecoders"
	solanatxmetaparsers "github.com/rpcpool/yellowstone-faithful/solana-tx-meta-parsers"
	"github.com/sourcegraph/jsonrpc2"
	"github.com/urfave/cli/v2"
	"k8s.io/klog/v2"
)

func newCmd_rpcServerCar() *cli.Command {
	var listenOn string
	return &cli.Command{
		Name:        "rpc-server-car",
		Description: "Start a Solana JSON RPC that exposes getTransaction and getBlock",
		ArgsUsage:   "<car-path> <cid-to-offset-index-filepath> <slot-to-cid-index-filepath> <sig-to-cid-index-filepath>",
		Before: func(c *cli.Context) error {
			return nil
		},
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:        "listen",
				Usage:       "Listen address",
				Value:       ":8899",
				Destination: &listenOn,
			},
		},
		Action: func(c *cli.Context) error {
			carFilepath := c.Args().Get(0)
			if carFilepath == "" {
				return cli.Exit("Must provide a CAR filepath", 1)
			}
			cidToOffsetIndexFilepath := c.Args().Get(1)
			if cidToOffsetIndexFilepath == "" {
				return cli.Exit("Must provide a CID-to-offset index filepath", 1)
			}
			slotToCidIndexFilepath := c.Args().Get(2)
			if slotToCidIndexFilepath == "" {
				return cli.Exit("Must provide a slot-to-CID index filepath", 1)
			}
			sigToCidIndexFilepath := c.Args().Get(3)
			if sigToCidIndexFilepath == "" {
				return cli.Exit("Must provide a signature-to-CID index filepath", 1)
			}

			carReader, err := carv2.OpenReader(carFilepath)
			if err != nil {
				return fmt.Errorf("failed to open CAR file: %w", err)
			}
			defer carReader.Close()

			cidToOffsetIndexFile, err := os.Open(cidToOffsetIndexFilepath)
			if err != nil {
				return fmt.Errorf("failed to open index file: %w", err)
			}
			defer cidToOffsetIndexFile.Close()

			cidToOffsetIndex, err := compactindex.Open(cidToOffsetIndexFile)
			if err != nil {
				return fmt.Errorf("failed to open index: %w", err)
			}

			slotToCidIndexFile, err := os.Open(slotToCidIndexFilepath)
			if err != nil {
				return fmt.Errorf("failed to open index file: %w", err)
			}
			defer slotToCidIndexFile.Close()

			slotToCidIndex, err := compactindex36.Open(slotToCidIndexFile)
			if err != nil {
				return fmt.Errorf("failed to open index: %w", err)
			}

			sigToCidIndexFile, err := os.Open(sigToCidIndexFilepath)
			if err != nil {
				return fmt.Errorf("failed to open index file: %w", err)
			}
			defer sigToCidIndexFile.Close()

			sigToCidIndex, err := compactindex36.Open(sigToCidIndexFile)
			if err != nil {
				return fmt.Errorf("failed to open index: %w", err)
			}

			return newRPCServer(
				c.Context,
				listenOn,
				carReader,
				cidToOffsetIndex,
				slotToCidIndex,
				sigToCidIndex,
			)
		},
	}
}

func newRPCServer(
	ctx context.Context,
	listenOn string,
	carReader *carv2.Reader,
	cidToOffsetIndex *compactindex.DB,
	slotToCidIndex *compactindex36.DB,
	sigToCidIndex *compactindex36.DB,
) error {
	// start a JSON RPC server
	handler := &rpcServer{
		carReader:        carReader,
		cidToOffsetIndex: cidToOffsetIndex,
		slotToCidIndex:   slotToCidIndex,
		sigToCidIndex:    sigToCidIndex,
	}

	r := gin.Default()
	r.POST("/", newRPC(handler))
	klog.Infof("Listening on %s", listenOn)
	return r.Run(listenOn)
}

func newRPC(handler *rpcServer) func(c *gin.Context) {
	return func(c *gin.Context) {
		startedAt := time.Now()
		defer func() {
			klog.Infof("request took %s", time.Since(startedAt))
		}()
		// read request body
		body, err := ioutil.ReadAll(c.Request.Body)
		if err != nil {
			klog.Errorf("failed to read request body: %v", err)
			// reply with error
			c.JSON(http.StatusBadRequest, jsonrpc2.Response{
				Error: &jsonrpc2.Error{
					Code:    jsonrpc2.CodeParseError,
					Message: "Parse error",
				},
			})
			return
		}

		// parse request
		var rpcRequest jsonrpc2.Request
		if err := json.Unmarshal(body, &rpcRequest); err != nil {
			klog.Errorf("failed to unmarshal request: %v", err)
			c.JSON(http.StatusBadRequest, jsonrpc2.Response{
				Error: &jsonrpc2.Error{
					Code:    jsonrpc2.CodeParseError,
					Message: "Parse error",
				},
			})
			return
		}

		klog.Infof("request: %s", string(body))

		rf := &requestContext{ctx: c}

		// handle request
		handler.Handle(c.Request.Context(), rf, &rpcRequest)
	}
}

type responseWriter struct {
	http.ResponseWriter
}

type logger struct{}

func (l logger) Printf(tmpl string, args ...interface{}) {
	klog.Infof(tmpl, args...)
}

type rpcServer struct {
	carReader        *carv2.Reader
	cidToOffsetIndex *compactindex.DB
	slotToCidIndex   *compactindex36.DB
	sigToCidIndex    *compactindex36.DB
}

type requestContext struct {
	ctx *gin.Context
}

// ReplyWithError(ctx context.Context, id ID, respErr *Error) error {
func (c *requestContext) ReplyWithError(ctx context.Context, id jsonrpc2.ID, respErr *jsonrpc2.Error) error {
	resp := &jsonrpc2.Response{
		ID:    id,
		Error: respErr,
	}
	c.ctx.JSON(http.StatusOK, resp)
	return nil
}

func toMapAny(v any) (map[string]any, error) {
	b, err := jsoniter.ConfigCompatibleWithStandardLibrary.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := jsoniter.ConfigCompatibleWithStandardLibrary.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// MapToCamelCase converts a map[string]interface{} to a map[string]interface{} with camelCase keys
func MapToCamelCase(m map[string]any) map[string]any {
	newMap := make(map[string]any)
	for k, v := range m {
		newMap[toLowerCamelCase(k)] = v

		if m, ok := v.(map[string]any); ok {
			newMap[toLowerCamelCase(k)] = MapToCamelCase(m)
		}
		if v, ok := v.([]any); ok {
			for i, vv := range v {
				if m, ok := vv.(map[string]any); ok {
					v[i] = MapToCamelCase(m)
				}
			}
		}
	}
	return newMap
}

func toLowerCamelCase(v string) string {
	pascal := bin.ToPascalCase(v)
	if len(pascal) == 0 {
		return ""
	}
	if len(pascal) == 1 {
		return strings.ToLower(pascal)
	}
	return strings.ToLower(pascal[:1]) + pascal[1:]
}

// Reply(ctx context.Context, id ID, result interface{}) error {
func (c *requestContext) Reply(
	ctx context.Context,
	id jsonrpc2.ID,
	result interface{},
	remapCallback func(map[string]any) map[string]any,
) error {
	mm, err := toMapAny(result)
	if err != nil {
		return err
	}
	result = MapToCamelCase(mm)
	if remapCallback != nil {
		if mp, ok := result.(map[string]any); ok {
			result = remapCallback(mp)
		}
	}
	resRaw, err := jsoniter.ConfigCompatibleWithStandardLibrary.Marshal(result)
	if err != nil {
		return err
	}
	raw := json.RawMessage(resRaw)
	resp := &jsonrpc2.Response{
		ID:     id,
		Result: &raw,
	}
	c.ctx.JSON(http.StatusOK, resp)
	return err
}

func (s *rpcServer) GetNodeByCid(ctx context.Context, wantedCid cid.Cid) ([]byte, error) {
	offset, err := s.FindOffsetFromCid(ctx, wantedCid)
	if err != nil {
		klog.Errorf("failed to find offset for CID %s: %v", wantedCid, err)
		// not found or error
		return nil, err
	}
	return s.GetNodeByOffset(ctx, wantedCid, offset)
}

func (s *rpcServer) GetNodeByOffset(ctx context.Context, wantedCid cid.Cid, offset uint64) ([]byte, error) {
	// seek to offset
	dr, err := s.carReader.DataReader()
	if err != nil {
		klog.Errorf("failed to get data reader: %v", err)
		return nil, err
	}
	dr.Seek(int64(offset), io.SeekStart)
	br := bufio.NewReader(dr)

	gotCid, data, err := util.ReadNode(br)
	if err != nil {
		klog.Errorf("failed to read node: %v", err)
		return nil, err
	}
	// verify that the CID we read matches the one we expected.
	if !gotCid.Equals(wantedCid) {
		klog.Errorf("CID mismatch: expected %s, got %s", wantedCid, gotCid)
		return nil, err
	}
	return data, nil
}

type GetBlockRequest struct {
	Slot uint64 `json:"slot"`
	// TODO: add more params
}

func parseGetBlockRequest(raw *json.RawMessage) (*GetBlockRequest, error) {
	var params []any
	if err := json.Unmarshal(*raw, &params); err != nil {
		klog.Errorf("failed to unmarshal params: %v", err)
		return nil, err
	}
	slotRaw, ok := params[0].(float64)
	if !ok {
		klog.Errorf("first argument must be a number, got %T", params[0])
		return nil, nil
	}

	return &GetBlockRequest{
		Slot: uint64(slotRaw),
	}, nil
}

func (ser *rpcServer) FindCidFromSlot(ctx context.Context, slot uint64) (cid.Cid, error) {
	return findCidFromSlot(ser.slotToCidIndex, slot)
}

func (ser *rpcServer) FindCidFromSignature(ctx context.Context, sig solana.Signature) (cid.Cid, error) {
	return findCidFromSignature(ser.sigToCidIndex, sig)
}

func (ser *rpcServer) FindOffsetFromCid(ctx context.Context, cid cid.Cid) (uint64, error) {
	return findOffsetFromCid(ser.cidToOffsetIndex, cid)
}

func (ser *rpcServer) GetBlock(ctx context.Context, slot uint64) (*ipldbindcode.Block, error) {
	// get the slot by slot number
	wantedCid, err := ser.FindCidFromSlot(ctx, slot)
	if err != nil {
		klog.Errorf("failed to find CID for slot %d: %v", slot, err)
		return nil, err
	}
	// get the block by CID
	data, err := ser.GetNodeByCid(ctx, wantedCid)
	if err != nil {
		klog.Errorf("failed to find node by cid: %v", err)
		return nil, err
	}
	// try parsing the data as a Block node.
	decoded, err := iplddecoders.DecodeBlock(data)
	if err != nil {
		klog.Errorf("failed to decode block: %v", err)
		return nil, err
	}
	return decoded, nil
}

func (ser *rpcServer) GetEntryByCid(ctx context.Context, wantedCid cid.Cid) (*ipldbindcode.Entry, error) {
	data, err := ser.GetNodeByCid(ctx, wantedCid)
	if err != nil {
		klog.Errorf("failed to find node by cid: %v", err)
		return nil, err
	}
	// try parsing the data as an Entry node.
	decoded, err := iplddecoders.DecodeEntry(data)
	if err != nil {
		klog.Errorf("failed to decode entry: %v", err)
		return nil, err
	}
	return decoded, nil
}

func (ser *rpcServer) GetTransactionByCid(ctx context.Context, wantedCid cid.Cid) (*ipldbindcode.Transaction, error) {
	data, err := ser.GetNodeByCid(ctx, wantedCid)
	if err != nil {
		klog.Errorf("failed to find node by cid: %v", err)
		return nil, err
	}
	// try parsing the data as a Transaction node.
	decoded, err := iplddecoders.DecodeTransaction(data)
	if err != nil {
		klog.Errorf("failed to decode transaction: %v", err)
		return nil, err
	}
	return decoded, nil
}

func (ser *rpcServer) GetDataFrameByCid(ctx context.Context, wantedCid cid.Cid) (*ipldbindcode.DataFrame, error) {
	data, err := ser.GetNodeByCid(ctx, wantedCid)
	if err != nil {
		klog.Errorf("failed to find node by cid: %v", err)
		return nil, err
	}
	// try parsing the data as a DataFrame node.
	decoded, err := iplddecoders.DecodeDataFrame(data)
	if err != nil {
		klog.Errorf("failed to decode data frame: %v", err)
		return nil, err
	}
	return decoded, nil
}

func (ser *rpcServer) GetRewardsByCid(ctx context.Context, wantedCid cid.Cid) (*ipldbindcode.Rewards, error) {
	data, err := ser.GetNodeByCid(ctx, wantedCid)
	if err != nil {
		klog.Errorf("failed to find node by cid: %v", err)
		return nil, err
	}
	// try parsing the data as a Rewards node.
	decoded, err := iplddecoders.DecodeRewards(data)
	if err != nil {
		klog.Errorf("failed to decode rewards: %v", err)
		return nil, err
	}
	return decoded, nil
}

func (ser *rpcServer) GetTransaction(ctx context.Context, sig solana.Signature) (*ipldbindcode.Transaction, error) {
	// get the CID by signature
	wantedCid, err := ser.FindCidFromSignature(ctx, sig)
	if err != nil {
		klog.Errorf("failed to find CID for signature %s: %v", sig, err)
		return nil, err
	}
	// get the transaction by CID
	data, err := ser.GetNodeByCid(ctx, wantedCid)
	if err != nil {
		klog.Errorf("failed to get node by cid: %v", err)
		return nil, err
	}
	// try parsing the data as a Transaction node.
	decoded, err := iplddecoders.DecodeTransaction(data)
	if err != nil {
		klog.Errorf("failed to decode transaction: %v", err)
		return nil, err
	}
	return decoded, nil
}

type GetTransactionRequest struct {
	Signature solana.Signature `json:"signature"`
	// TODO: add more params
}

func parseGetTransactionRequest(raw *json.RawMessage) (*GetTransactionRequest, error) {
	var params []any
	if err := json.Unmarshal(*raw, &params); err != nil {
		klog.Errorf("failed to unmarshal params: %v", err)
		return nil, err
	}
	sigRaw, ok := params[0].(string)
	if !ok {
		klog.Errorf("first argument must be a string")
		return nil, nil
	}

	sig, err := solana.SignatureFromBase58(sigRaw)
	if err != nil {
		klog.Errorf("failed to convert signature from base58: %v", err)
		return nil, err
	}
	return &GetTransactionRequest{
		Signature: sig,
	}, nil
}

// jsonrpc2.RequestHandler interface
func (ser *rpcServer) Handle(ctx context.Context, conn *requestContext, req *jsonrpc2.Request) {
	switch req.Method {
	case "getBlock":
		ser.getBlock(ctx, conn, req)
	case "getTransaction":
		ser.getTransaction(ctx, conn, req)
	default:
		conn.ReplyWithError(
			ctx,
			req.ID,
			&jsonrpc2.Error{
				Code:    jsonrpc2.CodeMethodNotFound,
				Message: "Method not found",
			})
	}
}

type GetBlockResponse struct {
	BlockHeight       uint64                   `json:"blockHeight"`
	BlockTime         *uint64                  `json:"blockTime"`
	Blockhash         string                   `json:"blockhash"`
	ParentSlot        uint64                   `json:"parentSlot"`
	PreviousBlockhash string                   `json:"previousBlockhash"`
	Rewards           any                      `json:"rewards"` // TODO: use same format as solana
	Transactions      []GetTransactionResponse `json:"transactions"`
}

type GetTransactionResponse struct {
	// TODO: use same format as solana
	Blocktime   *uint64 `json:"blockTime,omitempty"`
	Meta        any     `json:"meta"`
	Slot        *uint64 `json:"slot,omitempty"`
	Transaction []any   `json:"transaction"`
	Version     any     `json:"version"`
}

func parseTransactionAndMetaFromNode(
	transactionNode *ipldbindcode.Transaction,
	dataFrameGetter func(ctx context.Context, wantedCid cid.Cid) (*ipldbindcode.DataFrame, error),
) (tx solana.Transaction, meta any, _ error) {
	{
		transactionBuffer := new(bytes.Buffer)
		transactionBuffer.Write(transactionNode.Data.Data)
		if transactionNode.Data.Total > 1 {
			for _, cid := range transactionNode.Data.Next {
				nextDataFrame, err := dataFrameGetter(context.Background(), cid.(cidlink.Link).Cid)
				if err != nil {
					return solana.Transaction{}, nil, err
				}
				transactionBuffer.Write(nextDataFrame.Data)
			}
		}
		if err := bin.UnmarshalBin(&tx, transactionBuffer.Bytes()); err != nil {
			klog.Errorf("failed to unmarshal transaction: %v", err)
			return solana.Transaction{}, nil, err
		} else if len(tx.Signatures) == 0 {
			klog.Errorf("transaction has no signatures")
			return solana.Transaction{}, nil, err
		}
	}

	{
		metaBuffer := new(bytes.Buffer)
		metaBuffer.Write(transactionNode.Metadata.Data)
		if transactionNode.Metadata.Total > 1 {
			for _, cid := range transactionNode.Metadata.Next {
				nextDataFrame, err := dataFrameGetter(context.Background(), cid.(cidlink.Link).Cid)
				if err != nil {
					return solana.Transaction{}, nil, err
				}
				metaBuffer.Write(nextDataFrame.Data)
			}
		}
		if len(metaBuffer.Bytes()) > 0 {
			uncompressedMeta, err := decompressZstd(metaBuffer.Bytes())
			if err != nil {
				klog.Errorf("failed to decompress metadata: %v", err)
				return
			}
			status, err := solanatxmetaparsers.ParseAnyTransactionStatusMeta(uncompressedMeta)
			if err != nil {
				klog.Errorf("failed to parse metadata: %v", err)
				return
			}
			meta = status
		}
	}
	return
}

func calcBlockHeight(slot uint64) uint64 {
	// TODO: fix this
	return 0
}