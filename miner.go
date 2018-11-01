/*
Implements a miner network

Usage:
$ go run client.go [local UDP ip:port] [local TCP ip:port] [aserver UDP ip:port]

Example:
$ go run client.go 127.0.0.1:2020 127.0.0.1:3030 127.0.0.1:7070

*/

package main

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/rpc"
	"os"
	"sync"
	"time"

	"github.com/DistributedClocks/GoVector/govec"
	"github.com/DistributedClocks/GoVector/govec/vrpc"
	"github.com/google/go-cmp/cmp"

	"github.ugrad.cs.ubc.ca/CPSC416-2018W-T1/P1-i8b0b-e8y0b/rfslib"
)

// Block is the interface for GenesisBlock, NOPBlock and OPBlock
type Block interface {
	hash() string
	prevHash() string
	minerID() string
	// getHeight() (uint32, error)
}

// GenesisBlock is the first block in this blockchain
type GenesisBlock struct {
	Hash    string // The genesis (first) block MD5 hash for this blockchain
	MinerID string // The identifier of the miner that computed this block (block-minerID)
}

func (gblock GenesisBlock) hash() string {
	return gblock.Hash
}

func (gblock GenesisBlock) prevHash() string {
	return gblock.Hash
}

func (gblock GenesisBlock) minerID() string {
	return gblock.MinerID
}

// NOPBlock is a No-OP Block
type NOPBlock struct {
	PrevHash     string // A hash of the previous block in the chain (prev-hash)
	Nonce        uint32 // A 32-bit unsigned integer nonce (nonce)
	MinerID      string // The identifier of the miner that computed this block (block-minerID)
	MinerBalance uint32 // The updated balance of the miner that computed this block
}

func (nopblock NOPBlock) hash() string {
	h := md5.New()
	h.Write([]byte(fmt.Sprintf("%v", nopblock)))
	return hex.EncodeToString(h.Sum(nil))
}

func (nopblock NOPBlock) prevHash() string {
	return nopblock.PrevHash
}

func (nopblock NOPBlock) minerID() string {
	return nopblock.MinerID
}

// OPBlock is a OP Block with non-empty Records
type OPBlock struct {
	PrevHash     string            // A hash of the previous block in the chain (prev-hash)
	Records      []OperationRecord // An ordered set of operation records
	Nonce        uint32            // A 32-bit unsigned integer nonce (nonce)
	MinerID      string            // The identifier of the miner that computed this block (block-minerID)
	MinerBalance uint32            // The updated balance of the miner that computed this block
}

func (opblock OPBlock) hash() string {
	h := md5.New()
	h.Write([]byte(fmt.Sprintf("%v", opblock)))
	return hex.EncodeToString(h.Sum(nil))
}

func (opblock OPBlock) prevHash() string {
	return opblock.PrevHash
}

func (opblock OPBlock) minerID() string {
	return opblock.MinerID
}

// OperationRecord is a file operation on the block chain
type OperationRecord struct {
	RecordData    rfslib.Record // rfslib operation data
	OperationType string        // rfslib operation type (one of ["append", "create", "delete"])
	FileName      string        // The name of file being operated
	RecordNum     uint16        // The chunk number of the file
	MinerID       string        // An identifier that specifies the miner identifier whose record coins sponsor this operation
}

// PeerMinerInfo is ...
type PeerMinerInfo struct {
	IncomingMinersAddr string
	MinerID            string
}

// Miner mines blocks.
type Miner struct {
	// These would not change once initialized
	Settings
	Logger       *govec.GoLog       // The GoVector Logger
	GoVecOptions govec.GoLogOptions // The GoVector log options

	GeneratedBlocksChan chan Block           // Channel of generated blocks
	StopMiningChan      chan string          // Channel that stops computeOPBlock or computeNOPBlock
	OperationRecordChan chan OperationRecord // Channel of valid operation records received from other miners

	// No need to initialize :)
	chain      sync.Map // {hash: Block} All the blocks in the network
	chainTips  sync.Map // {Block: height of the fork} The collection of the tails of all valid forks
	peerMiners sync.Map // {minerID: *rpc.Client} The connected peer miners (including these newly joined)
}

// Settings contains all miner settings and is loaded through
// a configuration file.  See:
// https://www.cs.ubc.ca/~bestchai/teaching/cs416_2018w1/project1/config.json.
type Settings struct {
	// The ID of this miner (max 16 characters).
	MinerID string

	// An array of remote IP:port addresses, one per peer miner that this miner should
	// connect to (using the OutgoingMinersIP below).
	PeerMinersAddrs []string

	// The local IP:port where the miner should expect other miners to connect to it
	// (address it should listen on for connections from miners).
	IncomingMinersAddr string

	// The local IP that the miner should use to connect to peer miners.
	OutgoingMinersIP string

	// The local IP:port where this miner should expect to receive connections
	// from RFS clients (address it should listen on for connections from clients)
	IncomingClientsAddr string

	// The number of record coins mined for an op block.
	MinedCoinsPerOpBlock uint8

	// The number of record coins mined for a no-op block.
	MinedCoinsPerNoOpBlock uint8

	// The number of record coins charged for creating a file.
	NumCoinsPerFileCreate uint8

	// Time in milliseconds, the minimum time between op block mining.
	GenOpBlockTimeout uint8

	// The genesis (first) block MD5 hash for this blockchain.
	GenesisBlockHash string

	// The op block difficulty (proof of work setting: number of zeroes).
	PowPerOpBlock uint8

	// The no-op block difficulty (proof of work setting: number of zeroes).
	PowPerNoOpBlock uint8

	// The number of confirmations for a create file operation
	// (the number of blocks that must follow the block containing a create file operation
	// along longest chain before the CreateFile call can return successfully).
	ConfirmsPerFileCreate uint8

	// The number of confirmations for an append operation (the number of blocks
	// that must follow the block containing an append operation along longest chain
	// before the AppendRec call can return successfully). Note that this append confirm
	// number will always be set to be larger than the create confirm number (above).
	ConfirmsPerFileAppend uint8
}

// ClientAPI is the set of RPC calls provided to RFS
type ClientAPI struct {
	IncomingClientsAddr string // The local IP:port where this miner should expect to receive connections from RFS clients
}

// MinerAPI is the set of RPC calls provided to other miners
type MinerAPI struct {
	IncomingMinersAddr string // The local IP:port where the miner should expect other miners to connect to it
}

var miner Miner

// GetChainTips RPC provides the active starting point of the current blockchain
// parameter arg is optional and not being used at all
func (mapi *MinerAPI) GetChainTips(caller string, reply *map[Block]int) error {
	log.Println("MinerAPI.GetChainTips got a call from " + caller)
	dumpedChainTips := make(map[Block]int)
	miner.chainTips.Range(func(block, height interface{}) bool {
		dumpedChainTips[block.(Block)] = height.(int)
		return true
	})
	*reply = dumpedChainTips
	return nil
}

// GetBlock RPC gets a block with a particular header hash from the local blockchain map
func (mapi *MinerAPI) GetBlock(headerHash string, reply *Block) error {
	block, exists := miner.chain.Load(headerHash)
	if exists {
		*reply = block.(Block)
		return nil
	}
	return errors.New("The requested block does not exist in the local blockchain:" + miner.MinerID)
}

func (m *Miner) addNode(minerInfo PeerMinerInfo) error {
	client, err := vrpc.RPCDial("tcp", minerInfo.IncomingMinersAddr, m.Logger, m.GoVecOptions)
	if err != nil {
		log.Fatal("addNode dialing:", minerInfo.IncomingMinersAddr, err)
		return err
	}
	miner.peerMiners.Store(minerInfo.MinerID, client)
	return nil
}

// AddNode RPC adds the remote node to its own network
func (mapi *MinerAPI) AddNode(minerInfo PeerMinerInfo, received *bool) error {
	*received = true
	if err := miner.addNode(minerInfo); err != nil {
		return err
	}
	return nil
}

// GetPeerInfo RPC returns the current miner info
func (mapi *MinerAPI) GetPeerInfo(caller string, minerID *string) error {
	minerID = &miner.MinerID
	return nil
}

// validateRecordSemantics checks that each operation does not violate RFS semantics
// (e.g., a record is not mutated or inserted into the middled of an rfs file).
func (m *Miner) validateRecordSemantics(block Block, opRecord OperationRecord) error {
	currentRecordNum := opRecord.RecordNum
	for block.hash() != block.prevHash() {
		prevBlock, ok := m.chain.Load(block.prevHash())
		if !ok {
			return errors.New("Encountered an orphaned block when checking RFS semantics")
		}
		switch t := block.(type) {
		case NOPBlock:
			break
		case OPBlock:
			if opRecord.OperationType == "create" {
				for _, prevRecord := range t.Records {
					if prevRecord.FileName == opRecord.FileName {
						return errors.New("The given file name already exists in this chain")
					}
				}
			} else if opRecord.OperationType == "append" {
				for _, prevRecord := range t.Records {
					if currentRecordNum == prevRecord.RecordNum+1 {
						currentRecordNum = prevRecord.RecordNum
						if currentRecordNum == 0 {
							// TODO: decide the initial record num for append
							return nil
						}
					} else {
						return errors.New("Encountered an invalid OperationRecord (inserted into the middled of an rfs file)")
					}
				}
			} else {
				return errors.New("Encountered an invalid OperationRecord Type")
			}
		default:
			return errors.New("Encountered an invalid intermediate block when checking RFS semantics")
		}
		block = prevBlock.(Block)
	}
	return nil
}

// validateBlock returns true if the given block is valid, false otherwise
func (m *Miner) validateBlock(block Block) error {
	// Block validations
	switch t := block.(type) {
	default:
		return errors.New("Invalid Block Type")
	case OPBlock:
		if validateNonce(t, m.PowPerOpBlock) == false {
			return errors.New("The given OPBlock does not have the right difficulty")
		}
		// Check that the previous block hash points to a legal, previously generated, block.
		if _, ok := m.chain.Load(t.PrevHash); !ok {
			return errors.New("The given OPBlock does not have a previous block")
		}

		// Operation validations:
		// Check that each operation in the block is associated with a miner ID that has enough record coins to pay for the operation
		// (i.e., the number of record coins associated with the minerID must have sufficient balance to 'pay' for the operation).
		balanceRequiredMap := make(map[string]uint32)
		for _, opRecord := range t.Records {
			if opRecord.OperationType == "create" {
				balanceRequiredMap[opRecord.MinerID] += uint32(m.NumCoinsPerFileCreate)
			} else if opRecord.OperationType == "append" {
				balanceRequiredMap[opRecord.MinerID]++
			}
		}
		for minerID, balanceRequired := range balanceRequiredMap {
			balance, err := m.getBalance(t, minerID)
			if err != nil {
				return errors.New("Checking balanceRequired:" + err.Error())
			} else if balance < balanceRequired {
				return errors.New("The miner of the OperationRecord does not have enough balance")
			}
		}

		// Check that each operation does not violate RFS semantics
		// (e.g., a record is not mutated or inserted into the middled of an rfs file).
		for _, opRecord := range t.Records {
			if err := m.validateRecordSemantics(t, opRecord); err != nil {
				return err
			}
		}
		break
	case NOPBlock:
		if validateNonce(t, m.PowPerNoOpBlock) == false {
			return errors.New("The given NOPBlock does not have the right difficulty")
		}
		// Check that the previous block hash points to a legal, previously generated, block.
		if _, ok := m.chain.Load(t.PrevHash); ok {
			fmt.Println("NOPBlock: the previous block is:", t.prevHash())
			m.chain.Store(t.hash(), t)
		} else {
			return errors.New("The given NOPBlock does not have a previous block")
		}
		break
	case GenesisBlock:
		if _, ok := m.chain.Load(m.GenesisBlockHash); ok {
			return errors.New("A GenesisBlock already existed")
		}
	}
	return nil
}

// SubmitRecord RPC from MinerAPI submits operationRecord to the miner network
// if the mined coins are sufficient to cover the cost
func (mapi *MinerAPI) SubmitRecord(operationRecord *OperationRecord, received *bool) error {
	*received = true
	block := miner.getBlockFromLongestChain()
	balance, err := miner.getBalance(block, operationRecord.MinerID)
	if err != nil {
		return errors.New("Checking balanceRequired:" + err.Error())
	}
	err = miner.validateRecordSemantics(block, *operationRecord)
	if err != nil {
		return err
	}
	switch operationRecord.OperationType {
	case "delete":
		break
	case "create":
		if uint32(miner.NumCoinsPerFileCreate) > balance {
			return errors.New("The current balance is not enough to cover create")
		}
	case "append":
		if balance < 1 {
			return errors.New("The current balance is not enough to cover append")
		}
	}
	miner.OperationRecordChan <- *operationRecord
	miner.broadcastOperationRecord(operationRecord)
	return nil
}

// SubmitRecord RPC call from ClientAPI submits operationRecord to the miner network
// if the mined coins are sufficient to cover the cost
func (capi *ClientAPI) SubmitRecord(operationRecord *OperationRecord, received *bool) error {
	block := miner.getBlockFromLongestChain()
	*received = true
	balance, err := miner.getBalance(block, miner.MinerID)
	if err != nil {
		return errors.New("Checking balanceRequired:" + err.Error())
	}
	err = miner.validateRecordSemantics(block, *operationRecord)
	if err != nil {
		return err
	}
	switch operationRecord.OperationType {
	case "delete":
		break
	case "create":
		if uint32(miner.NumCoinsPerFileCreate) > balance {
			return errors.New("The current balance is not enough to cover create")
		}
	case "append":
		if balance < 1 {
			return errors.New("The current balance is not enough to cover create or append")
		}
	}
	miner.OperationRecordChan <- *operationRecord
	miner.broadcastOperationRecord(operationRecord)
	return nil
}

func (m *Miner) getOperationRecordHeight(block Block, srcRecord OperationRecord) (int, error) {
	confirmedBlocksNum := 0
	for block.hash() != block.prevHash() {
		switch t := block.(type) {
		default:
			return -1, errors.New("Invalid Block Type")
		case OPBlock:
			for _, dstRecord := range t.Records {
				if cmp.Equal(srcRecord, dstRecord) {
					return confirmedBlocksNum, nil
				}
			}
			confirmedBlocksNum++
		case NOPBlock:
			confirmedBlocksNum++
		}
		// Check that the previous block hash points to a legal, previously generated, block.
		if block, ok := miner.chain.Load(block.prevHash()); !ok {
			return -1, errors.New("Block" + block.(Block).hash() + "does not have a valid prevBlock")
		}
	}
	return -1, errors.New("The given OperationRecord cannot be found in the chain")
}

// ConfirmOperation RPC should be invoked by the RFS Client
// upon succesfully confimation it returns nil
func (capi *ClientAPI) ConfirmOperation(operationRecord *OperationRecord, received *bool) error {
	block := miner.getBlockFromLongestChain()
	*received = true
	confirmedBlocksNum, err := miner.getOperationRecordHeight(block, *operationRecord)
	if err != nil {
		return err
	}
	switch operationRecord.OperationType {
	default:
		return errors.New("Operation Type not recognized")
	case "delete":
		return errors.New("Delete not supported")
	case "create":
		if int(miner.ConfirmsPerFileCreate) > confirmedBlocksNum {
			return errors.New("Operation create not confirmed")
		}
		return nil
	case "append":
		if int(miner.ConfirmsPerFileAppend) > confirmedBlocksNum {
			return errors.New("Operation append not confirmed")
		}
		return nil
	}
}

// GetBalance RPC call should be invoked by the RFS Client and does not take an input
// it returns the current balance of the longest chain of the miner being quried
func (capi *ClientAPI) GetBalance(caller string, currentBalance *uint32) error {
	log.Println("MinerAPI.GetBalance got a call from " + caller)
	bestChainTip := miner.getBlockFromLongestChain()
	var err error
	*currentBalance, err = miner.getBalance(bestChainTip, miner.MinerID)
	return err
}

func (m *Miner) listFiles() ([]string, error) {
	fnamesMap := make(map[string]bool)
	fnamesSlice := make([]string, 0, 100)
	block := m.getBlockFromLongestChain()
	for block.hash() != block.prevHash() {
		prevBlock, ok := m.chain.Load(block.prevHash())
		if !ok {
			return fnamesSlice, errors.New("Encountered an orphaned block when finding files")
		}
		switch t := block.(type) {
		case NOPBlock:
			break
		case OPBlock:
			for _, opRecord := range t.Records {
				if opRecord.OperationType == "create" {
					fnamesMap[opRecord.FileName] = true
				}
			}
		default:
			return fnamesSlice, errors.New("Encountered an invalid intermediate block when finding files")
		}
		block = prevBlock.(Block)
	}
	for fname := range fnamesMap {
		fnamesSlice = append(fnamesSlice, fname)
	}
	return fnamesSlice, nil
}

func (m *Miner) countRecords(fname string) (uint16, error) {
	// recordNum := 0
	block := m.getBlockFromLongestChain()
	for block.hash() != block.prevHash() {
		prevBlock, ok := m.chain.Load(block.prevHash())
		if !ok {
			return 0, errors.New("Encountered an orphaned block when counting records")
		}
		switch t := block.(type) {
		case NOPBlock:
			break
		case OPBlock:
			for _, opRecord := range t.Records {
				if opRecord.FileName == fname {
					return opRecord.RecordNum, nil
				}
			}
		default:
			return 0, errors.New("Encountered an invalid intermediate block when counting records")
		}
		block = prevBlock.(Block)
	}
	return 0, errors.New("Cannot find the given file")
}

// ListFiles RPC lists all files in the local chain
func (capi *ClientAPI) ListFiles(caller string, fnames *[]string) error {
	log.Println("ClientAPI.ListFiles got a call from " + caller)
	listedNames, err := miner.listFiles()
	if err != nil {
		return err
	}
	*fnames = listedNames
	return nil
}

// CountRecords RPC counts the number of records for the given file
func (capi *ClientAPI) CountRecords(fname string, num *uint16) error {
	recordNum, err := miner.countRecords(fname)
	if err != nil {
		return err
	}
	*num = recordNum
	return nil
}

// getHeight returns the height of the given block in the local chain
func (m *Miner) getHeight(block Block) (int, error) {
	if block.hash() == block.prevHash() {
		// GenesisBlock case
		return 1, nil
	}
	// OPBlock, NOPBlock case
	prevBlock, exists := m.chain.Load(block.hash())
	if exists {
		prevHeight, err := m.getHeight(prevBlock.(Block))
		if err == nil {
			return prevHeight + 1, nil
		}
		return prevHeight, err
	}
	return 1, errors.New("The given NOPBlock/OPBlock starts with an orphaned block:" + block.hash())
}

func (m *Miner) requestPreviousBlocks(block Block) error {
	prevHash := block.prevHash()
	prevBlock, exists := m.chain.Load(prevHash)
	for ; !exists; prevHash = prevBlock.(Block).prevHash() {
		m.peerMiners.Range(func(remoteMinerID, client interface{}) bool {
			// Make a remote asynchronous call
			replyBlock := new(Block)
			err := client.(*rpc.Client).Call("MinerAPI.GetBlock", prevHash, replyBlock)
			if err != nil {
				log.Fatal("requestPreviousBlocks error:", err, "continue on with other miners")
				return true
			}
			m.chain.Store(block.hash(), *replyBlock)
			fmt.Println("requestPreviousBlocks successed:", (*replyBlock).hash())
			// stop iteration
			return false
		})
		prevBlock, exists = m.chain.Load(prevHash)
		if !exists {
			return errors.New("Cannot find the requested block from other peer miners")
		}
	}
	return nil
}

func (m *Miner) updateChainTip(newBlock Block) error {
	prevBlock, exists := m.chain.Load(newBlock.prevHash())
	if exists {
		inChainTips := false
		currentHeight := 0
		m.chainTips.Range(func(block, height interface{}) bool {
			if block.(Block).hash() == prevBlock.(Block).hash() {
				inChainTips = true
				currentHeight = height.(int)
				return false
			}
			return true
		})
		if inChainTips {
			// we are the first one to work on an original branch
			// TODO: check if Store() would cause update to malfunction
			m.chainTips.Store(newBlock, currentHeight+1)
		} else {
			// this is a fork of another branch
			prevHeight, err := m.getHeight(prevBlock.(Block))
			if err != nil {
				m.chainTips.Store(newBlock, prevHeight+1)
			} else {
				// ask other miners for the previous block?
				return err
			}
		}
		return nil
	}
	return errors.New("Cannot find the previous block")
}

// getBalance finds the current balance along the chain starting
func (m *Miner) getBalance(block Block, minerID string) (uint32, error) {
	foundBalance := false
	var mostRecentBalance uint32
	var recentTrasactionFee uint32
	for block.hash() != block.prevHash() {
		switch t := block.(type) {
		case NOPBlock:
			if t.MinerID == minerID {
				mostRecentBalance = t.MinerBalance
			}
		case OPBlock:
			if t.MinerID == minerID {
				mostRecentBalance = t.MinerBalance
				foundBalance := true
				break
			}
			for _, record := range t.Records {
				if record.MinerID == minerID {
					if record.OperationType == "append" {
						recentTrasactionFee++
					} else if record.OperationType == "create" {
						recentTrasactionFee += uint32(miner.NumCoinsPerFileCreate)
					}
				}
			}
		default:
			return 0, errors.New("Encountered an unknown block")
		}
		prevBlock, exists := m.chain.Load(block.prevHash())
		if exists {
			block = prevBlock.(Block)
		} else {
			return 0, errors.New("This chain does not have a valid head")
		}
	}
	if foundBalance {
		return mostRecentBalance - recentTrasactionFee, nil
	}
	return 0, nil
}

func (m *Miner) addBlock(block Block) error {
	err := m.validateBlock(block)
	if err == nil {
		if block.hash() == block.prevHash() {
			// GenesisBlock case
			_, loaded := m.chainTips.LoadOrStore(block, 0)
			if !loaded {
				return errors.New("The local chain already contains a GenesisBlock")
			}
		}
		err := m.updateChainTip(block)
		if err != nil {
			// TODO: decide if we want to request the block from other miners
			return err
		}
		return nil
	}
	// validation error
	return err
}

// SubmitBlock RPC is invoked by other Miner instances and accepts the given block upon successful validation
func (mapi *MinerAPI) SubmitBlock(block Block, received *bool) error {
	*received = true
	err := miner.addBlock(block)
	if err == nil {
		err = miner.broadcastBlock(block)
	}
	return err
}

func countTrailingZeros(str string) uint8 {
	var reverseCounter uint8
	for i := len(str) - 1; i >= 0; i-- {
		if str[i] == 0 {
			reverseCounter++
		} else {
			break
		}
	}
	return reverseCounter
}

// validateNonce computes the MD5 hash as a hex string for the block and checks if it
// has sufficient trailing zeros
func validateNonce(block Block, difficulty uint8) bool {
	h := md5.New()
	h.Write([]byte(fmt.Sprintf("%v", block)))
	combinedHash := hex.EncodeToString(h.Sum(nil))
	if countTrailingZeros(combinedHash) >= difficulty {
		return true
	}
	return false
}

func (m *Miner) validateFork(block Block) error {
	for block.hash() != block.prevHash() {
		prevBlock, exists := m.chain.Load(block.prevHash())
		if !exists {
			return errors.New("This is an orphaned chain")
		}
		block = prevBlock.(Block)
	}
	return nil
}

func (m *Miner) getBlockFromLongestChain() Block {
	maxBlocksNum := 0
	var longestChainTip Block
	m.chainTips.Range(func(block, height interface{}) bool {
		if height.(int) > maxBlocksNum {
			maxBlocksNum = height.(int)
			longestChainTip = block.(Block)
		}
		return true
	})
	return longestChainTip
}

// computeNOPBlock tries to construct a NOPBlock when it is not requested to stop
func (m *Miner) computeNOPBlock() {
	var nonce uint32
	nonce = 1
	prevBlock := m.getBlockFromLongestChain()
	prevHash := prevBlock.hash()
	currentBalance, err := m.getBalance(prevBlock, m.MinerID)
	if err != nil {
		log.Fatalln("computeNOPBlock:" + err.Error())
	}
	newBalance := currentBalance + uint32(m.MinedCoinsPerNoOpBlock)
	nopBlock := NOPBlock{prevHash, nonce, m.MinerID, newBalance}
	for {
		select {
		default:
			nopBlock = NOPBlock{prevHash, nonce, m.MinerID, newBalance}
			if validateNonce(nopBlock, m.PowPerNoOpBlock) == true {
				m.GeneratedBlocksChan <- nopBlock
				return
			}
			nonce++
		case processName := <-m.StopMiningChan:
			log.Println("computeNOPBlock has been requested to quit by " + processName)
			return
		}
	}
}

func (m *Miner) computeCoinsRequired(records []OperationRecord, minerID string) (int, error) {
	var coins int
	for _, v := range records {
		if v.MinerID == minerID {
			switch v.OperationType {
			case "append":
				coins++
			case "create":
				coins += int(m.NumCoinsPerFileCreate)
			case "delete":
				fallthrough
			default:
				return -1, errors.New("OperationType not recognized")
			}
		}
	}
	return coins, nil
}

// computeOPBlock works similarly except it takes all the records collected in the given time
func (m *Miner) computeOPBlock(records []OperationRecord) {
	var nonce uint32
	nonce = 1
	prevBlock := m.getBlockFromLongestChain()
	prevHash := prevBlock.hash()
	currentBalance, err := m.getBalance(prevBlock, m.MinerID)
	if err != nil {
		log.Fatalln("computeOPBlock:" + err.Error())
	}
	coinsRequired, err := m.computeCoinsRequired(records, m.MinerID)
	if err != nil {
		log.Fatalln("computeOPBlock:" + err.Error())
	}
	newBalance := currentBalance + uint32(m.MinedCoinsPerOpBlock) - uint32(coinsRequired)
	opBlock := OPBlock{prevHash, records, nonce, m.MinerID, newBalance}
	for {
		select {
		default:
			opBlock = OPBlock{prevHash, records, nonce, m.MinerID, newBalance}
			if validateNonce(opBlock, m.PowPerOpBlock) == true {
				m.GeneratedBlocksChan <- opBlock
				return
			}
			nonce++
		case processName := <-m.StopMiningChan:
			log.Println("computeOPBlock has been requested to quit by " + processName)
			return
		}
	}
}

// generateBlocks should only be called once
func (m *Miner) generateBlocks() {
	opBlockTimer := time.NewTimer(0 * time.Millisecond)
	m.StopMiningChan = make(chan string)
	// m.NOPBlockStopChan = make(chan string)
	m.GeneratedBlocksChan = make(chan Block)
	m.OperationRecordChan = make(chan OperationRecord)
	recordsMap := make(map[OperationRecord]bool)
	generatingNOPBlock := false
	generatingOPBlock := false
	for {
		select {
		default:
			if !generatingNOPBlock && !generatingOPBlock {
				go m.computeNOPBlock()
				generatingNOPBlock = true
			}
		case generatedBlock := <-m.GeneratedBlocksChan:
			switch generatedBlock.(type) {
			case NOPBlock:
				log.Println("Received the generated NOPBlock")
				generatingNOPBlock = false
				go m.broadcastBlock(generatedBlock)
			case OPBlock:
				log.Println("Received the generated OPBlock")
				generatingOPBlock = false
				go m.broadcastBlock(generatedBlock)
			default:
				log.Fatalln("Received the generated block of some other type")
			}
		// we have received a new record
		case operationRecord := <-m.OperationRecordChan:
			if opBlockTimer.Stop() {
				opBlockTimer.Reset(time.Duration(m.GenOpBlockTimeout) * time.Millisecond)
				if generatingNOPBlock {
					m.StopMiningChan <- "generateBlocks(newRecord, NOPBlock)"
				}
			}
			if _, ok := recordsMap[operationRecord]; !ok {
				recordsMap[operationRecord] = true
			}
		case <-opBlockTimer.C:
			if generatingNOPBlock {
				log.Panicf("We should not be working on NOPBlocks by now")
			}
			if generatingOPBlock {
				m.StopMiningChan <- "generateBlocks(timedOut, OPBlock)"
			}
			records := make([]OperationRecord, 0, len(recordsMap))
			for k := range recordsMap {
				records = append(records, k)
			}
			go m.computeOPBlock(records)
			generatingOPBlock = true
		}
	}
}

func (m *Miner) broadcastBlock(block Block) error {
	var calls []*rpc.Call
	var errStrings []string
	m.peerMiners.Range(func(remoteMinerID, client interface{}) bool {
		// Then it can make a remote asynchronous call
		reply := new(bool)
		submitBlockCall := client.(*rpc.Client).Go("MinerAPI.SubmitBlock", block, reply, nil)
		calls = append(calls, submitBlockCall)
		return true
	})
	for i, call := range calls {
		// do something with e.Value
		replyCall := <-call.Done // will be equal to divCall
		if replyCall.Error == nil {
			if replyCall.Reply == true {
				continue
			} else {
				errStrings = append(errStrings, "submitBlockCall.Reply is false")
			}
		} else {
			errString := m.PeerMinersAddrs[i] + ": " + replyCall.Error.Error()
			errStrings = append(errStrings, errString)
		}
	}

	if len(errStrings) > 0 {
		return errors.New("one or multiple submitBlockCall failed")
	}
	return nil
}

func (m *Miner) broadcastOperationRecord(opRecord *OperationRecord) error {
	calls := make(map[string]*rpc.Call)
	failedCalls := make([]string, 100)
	// successedCalls := make([]string)
	m.peerMiners.Range(func(remoteMinerID, client interface{}) bool {
		reply := new(bool)
		submitRecordCall := client.(*rpc.Client).Go("MinerAPI.SubmitRecord", opRecord, reply, nil)
		log.Println("broadcastOperationRecord:")
		calls[remoteMinerID.(string)] = submitRecordCall
		return true
	})
	for remoteMinerID, call := range calls {
		// do something with e.Value
		replyCall := <-call.Done // will be equal to divCall
		if replyCall.Error == nil && replyCall.Reply == true {
			continue
		} else {
			failedCalls = append(failedCalls, remoteMinerID)
		}
	}
	if len(failedCalls) > 0 {
		return errors.New("submitBlockCall failed" + string(len(failedCalls)) + " of " + string(len(calls)))
	}
	return nil
}

func (m *Miner) initializeMiner(settings Settings) error {
	m.Settings = settings
	m.GoVecOptions = govec.GetDefaultLogOptions()

	genesisBlock := GenesisBlock{m.GenesisBlockHash, m.MinerID}
	err := m.addBlock(genesisBlock)
	if err != nil {
		return err
	}
	for _, addr := range m.PeerMinersAddrs {
		client, err := vrpc.RPCDial("tcp", addr, miner.Logger, m.GoVecOptions)
		if err != nil {
			log.Fatal("dialing:", addr, err)
			return err
		}
		// Then make a remote call
		var remoteMinerID string
		var status bool
		client.Call("MinerAPI.GetMinerInfo", miner.MinerID+"initializeChains", &remoteMinerID)
		err = client.Call("MinerAPI.AddNode", PeerMinerInfo{m.IncomingMinersAddr, m.MinerID}, &status)
		if err != nil || status != true {
			// TODO: consider retry?
		}
		m.peerMiners.Store(remoteMinerID, client)
		remoteChainTips := new(map[Block]int)
		err = client.Call("MinerAPI.GetChainTips", miner.MinerID+"initializeChains", remoteChainTips)
		if err == nil {
			for remoteBlock, height := range *remoteChainTips {
				m.requestPreviousBlocks(remoteBlock)
				m.chainTips.Store(remoteBlock, height)
			}
		} else {
			log.Println(err)
		}
	}
	return nil
}

// loadJSON loads a settings json file and populates
// a Settings struct with the data it reads.
func loadJSON(fn string) (Settings, error) {
	// Get a file descriptor for the specified file.
	fi, err := os.Open(fn)
	if err != nil {
		return Settings{}, err
	}

	// Decode the contents of the file into a
	// Settings struct.
	var s Settings
	err = json.NewDecoder(fi).Decode(&s)
	if err != nil {
		return Settings{}, err
	}

	return s, nil
}

// Main workhorse method.  We are exposing two sets of APIs through RPC;
// one for other miners, and one for clients.
func main() {

	// Make sure cmd line input is correct.
	if len(os.Args) != 2 {
		log.Fatalln("Usage: go run miner.go </path/to/config.json>")
	}

	// Load up the settings from the specified json file.
	jsonFn := os.Args[1]
	settings, err := loadJSON(jsonFn)
	if err != nil {
		log.Fatalln(err)
	}
	fmt.Println("Loaded settings", settings)

	fmt.Println("Starting miner prepration")
	miner.initializeMiner(settings)

	// Register RPC methods for other miners to call.
	minerAPI := new(MinerAPI)
	minerServer := rpc.NewServer()
	minerServer.Register(minerAPI)
	l, e := net.Listen("tcp", minerAPI.IncomingMinersAddr)
	if e != nil {
		log.Fatal("listen error:", e)
	}

	//Initalize GoVector
	options := govec.GetDefaultLogOptions()
	//Access config and set timestamps (realtime) to true
	config := govec.GetDefaultConfig()
	config.UseTimestamps = true
	logger := govec.InitGoVector("MinerProcess", "MinerLog", config)
	miner.Logger = logger
	go vrpc.ServeRPCConn(minerServer, l, logger, options)

	// Register RPC methods for clients to call
	clientAPI := new(ClientAPI)
	clientServer := rpc.NewServer()
	clientServer.Register(clientAPI)
	l, e = net.Listen("tcp", clientAPI.IncomingClientsAddr)
	if e != nil {
		log.Fatal("listen error:", e)
	}
	// we will block here to serve our clients
	vrpc.ServeRPCConn(clientServer, l, logger, options)
}
