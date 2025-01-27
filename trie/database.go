// Copyright 2018 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package trie

import (
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/spacemeshos/go-spacemesh/common/types"
	"github.com/spacemeshos/go-spacemesh/common/util"
	"github.com/spacemeshos/go-spacemesh/database"
	"github.com/spacemeshos/go-spacemesh/log"
	"github.com/spacemeshos/go-spacemesh/rlp"
)

// secureKeyPrefix is the database key prefix used to store trie node preimages.
var secureKeyPrefix = []byte("secure-key-")

// secureKeyLength is the length of the above prefix + 32byte hash.
const secureKeyLength = 11 + 32

// DatabaseReader wraps the Get and Has method of a backing store for the trie.
type DatabaseReader interface {
	// Get retrieves the value associated with key from the database.
	Get(key []byte) (value []byte, err error)

	// Has retrieves whether a key is present in the database.
	Has(key []byte) (bool, error)
}

// Database is an intermediate write layer between the trie data structures and
// the disk database. The aim is to accumulate trie writes in-memory and only
// periodically flush a couple tries to disk, garbage collecting the remainder.
type Database struct {
	diskdb database.Database // Persistent storage for matured trie nodes

	nodes  map[types.Hash32]*cachedNode // Data and references relationships of a node
	oldest types.Hash32                 // Oldest tracked node, flush-list head
	newest types.Hash32                 // Newest tracked node, flush-list tail

	preimages map[types.Hash32][]byte // Preimages of nodes from the secure trie
	seckeybuf [secureKeyLength]byte   // Ephemeral buffer for calculating preimage keys

	gctime  time.Duration     // Time spent on garbage collection since last commit
	gcnodes uint64            // Nodes garbage collected since last commit
	gcsize  types.StorageSize // Data storage garbage collected since last commit

	flushtime  time.Duration     // Time spent on data flushing since last commit
	flushnodes uint64            // Nodes flushed since last commit
	flushsize  types.StorageSize // Data storage flushed since last commit

	nodesSize     types.StorageSize // Storage size of the nodes cache (exc. flushlist)
	preimagesSize types.StorageSize // Storage size of the preimages cache

	lock sync.RWMutex
}

// rawNode is a simple binary blob used to differentiate between collapsed trie
// nodes and already encoded RLP binary blobs (while at the same time store them
// in the same cache fields).
type rawNode []byte

func (n rawNode) canUnload(uint16, uint16) bool { panic("this should never end up in a live trie") }
func (n rawNode) cache() (hashNode, bool)       { panic("this should never end up in a live trie") }
func (n rawNode) fstring(ind string) string     { panic("this should never end up in a live trie") }

// rawFullNode represents only the useful data content of a full node, with the
// caches and flags stripped out to minimize its data storage. This type honors
// the same RLP encoding as the original parent.
type rawFullNode [17]node

func (n rawFullNode) canUnload(uint16, uint16) bool { panic("this should never end up in a live trie") }
func (n rawFullNode) cache() (hashNode, bool)       { panic("this should never end up in a live trie") }
func (n rawFullNode) fstring(ind string) string     { panic("this should never end up in a live trie") }

func (n rawFullNode) EncodeRLP(w io.Writer) error {
	var nodes [17]node

	for i, child := range n {
		if child != nil {
			nodes[i] = child
		} else {
			nodes[i] = nilValueNode
		}
	}

	if err := rlp.Encode(w, nodes); err != nil {
		return fmt.Errorf("encode RLP: %w", err)
	}

	return nil
}

// rawShortNode represents only the useful data content of a short node, with the
// caches and flags stripped out to minimize its data storage. This type honors
// the same RLP encoding as the original parent.
type rawShortNode struct {
	Key []byte
	Val node
}

func (n rawShortNode) canUnload(uint16, uint16) bool {
	panic("this should never end up in a live trie")
}
func (n rawShortNode) cache() (hashNode, bool)   { panic("this should never end up in a live trie") }
func (n rawShortNode) fstring(ind string) string { panic("this should never end up in a live trie") }

// cachedNode is all the information we know about a single cached node in the
// memory database write layer.
type cachedNode struct {
	node node   // Cached collapsed trie node, or raw rlp data
	size uint16 // Byte size of the useful cached data

	parents  uint16                  // Number of live nodes referencing this one
	children map[types.Hash32]uint16 // External children referenced by this node

	flushPrev types.Hash32 // Previous node in the flush-list
	flushNext types.Hash32 // Next node in the flush-list
}

// rlp returns the raw rlp encoded blob of the cached node, either directly from
// the cache, or by regenerating it from the collapsed node.
func (n *cachedNode) rlp() []byte {
	if node, ok := n.node.(rawNode); ok {
		return node
	}
	blob, err := rlp.EncodeToBytes(n.node)
	if err != nil {
		panic(err)
	}
	return blob
}

// obj returns the decoded and expanded trie node, either directly from the cache,
// or by regenerating it from the rlp encoded blob.
func (n *cachedNode) obj(hash types.Hash32, cachegen uint16) node {
	if node, ok := n.node.(rawNode); ok {
		return mustDecodeNode(hash[:], node, cachegen)
	}
	return expandNode(hash[:], n.node, cachegen)
}

// getChildren returns all the tracked children of this node, both the implicit ones
// from inside the node as well as the explicit ones from outside the node.
func (n *cachedNode) getChildren() []types.Hash32 {
	children := make([]types.Hash32, 0, 16)
	for child := range n.children {
		children = append(children, child)
	}
	if _, ok := n.node.(rawNode); !ok {
		gatherChildren(n.node, &children)
	}
	return children
}

// gatherChildren traverses the node hierarchy of a collapsed storage node and
// retrieves all the hashnode children.
func gatherChildren(n node, children *[]types.Hash32) {
	switch n := n.(type) {
	case *rawShortNode:
		gatherChildren(n.Val, children)

	case rawFullNode:
		for i := 0; i < 16; i++ {
			gatherChildren(n[i], children)
		}
	case hashNode:
		*children = append(*children, types.BytesToHash(n))

	case valueNode, nil:

	default:
		panic(fmt.Sprintf("unknown node type: %T", n))
	}
}

// simplifyNode traverses the hierarchy of an expanded memory node and discards
// all the internal caches, returning a node that only contains the raw data.
func simplifyNode(n node) node {
	switch n := n.(type) {
	case *shortNode:
		// Short nodes discard the flags and cascade
		return &rawShortNode{Key: n.Key, Val: simplifyNode(n.Val)}

	case *fullNode:
		// Full nodes discard the flags and cascade
		node := rawFullNode(n.Children)
		for i := 0; i < len(node); i++ {
			if node[i] != nil {
				node[i] = simplifyNode(node[i])
			}
		}
		return node

	case valueNode, hashNode, rawNode:
		return n

	default:
		panic(fmt.Sprintf("unknown node type: %T", n))
	}
}

// expandNode traverses the node hierarchy of a collapsed storage node and converts
// all fields and keys into expanded memory form.
func expandNode(hash hashNode, n node, cachegen uint16) node {
	switch n := n.(type) {
	case *rawShortNode:
		// Short nodes need key and child expansion
		return &shortNode{
			Key: compactToHex(n.Key),
			Val: expandNode(nil, n.Val, cachegen),
			flags: nodeFlag{
				hash: hash,
				gen:  cachegen,
			},
		}

	case rawFullNode:
		// Full nodes need child expansion
		node := &fullNode{
			flags: nodeFlag{
				hash: hash,
				gen:  cachegen,
			},
		}
		for i := 0; i < len(node.Children); i++ {
			if n[i] != nil {
				node.Children[i] = expandNode(nil, n[i], cachegen)
			}
		}
		return node

	case valueNode, hashNode:
		return n

	default:
		panic(fmt.Sprintf("unknown node type: %T", n))
	}
}

// NewDatabase creates a new trie database to store ephemeral trie content before
// its written out to disk or garbage collected.
func NewDatabase(diskdb database.Database) *Database {
	return &Database{
		diskdb:    diskdb,
		nodes:     map[types.Hash32]*cachedNode{{}: {}},
		preimages: make(map[types.Hash32][]byte),
	}
}

// DiskDB retrieves the persistent storage backing the trie database.
func (db *Database) DiskDB() DatabaseReader {
	return db.diskdb
}

// InsertBlob writes a new reference tracked blob to the memory database if it's
// yet unknown. This method should only be used for non-trie nodes that require
// reference counting, since trie nodes are garbage collected directly through
// their embedded children.
func (db *Database) InsertBlob(hash types.Hash32, blob []byte) {
	db.lock.Lock()
	defer db.lock.Unlock()

	db.insert(hash, blob, rawNode(blob))
}

// insert inserts a collapsed trie node into the memory database. This method is
// a more generic version of InsertBlob, supporting both raw blob insertions as
// well ex trie node insertions. The blob must always be specified to allow proper
// size tracking.
func (db *Database) insert(hash types.Hash32, blob []byte, node node) {
	// If the node's already cached, skip
	if _, ok := db.nodes[hash]; ok {
		return
	}
	// Create the cached entry for this node
	entry := &cachedNode{
		node:      simplifyNode(node),
		size:      uint16(len(blob)),
		flushPrev: db.newest,
	}
	for _, child := range entry.getChildren() {
		if c := db.nodes[child]; c != nil {
			c.parents++
		}
	}
	db.nodes[hash] = entry

	// Update the flush-list endpoints
	if db.oldest == (types.Hash32{}) {
		db.oldest, db.newest = hash, hash
	} else {
		db.nodes[db.newest].flushNext, db.newest = hash, hash
	}
	db.nodesSize += types.StorageSize(types.Hash32Length + entry.size)
}

// insertPreimage writes a new trie node pre-image to the memory database if it's
// yet unknown. The method will make a copy of the slice.
//
// Note, this method assumes that the database's lock is held!
func (db *Database) insertPreimage(hash types.Hash32, preimage []byte) {
	if _, ok := db.preimages[hash]; ok {
		return
	}
	db.preimages[hash] = util.CopyBytes(preimage)
	db.preimagesSize += types.StorageSize(types.Hash32Length + len(preimage))
}

// node retrieves a cached trie node from memory, or returns nil if none can be
// found in the memory cache.
func (db *Database) node(hash types.Hash32, cachegen uint16) node {
	// Retrieve the node from cache if available
	db.lock.RLock()
	node := db.nodes[hash]
	db.lock.RUnlock()

	if node != nil {
		return node.obj(hash, cachegen)
	}
	// Content unavailable in memory, attempt to retrieve from disk
	enc, err := db.diskdb.Get(hash[:])
	if err != nil || enc == nil {
		return nil
	}
	return mustDecodeNode(hash[:], enc, cachegen)
}

// Node retrieves an encoded cached trie node from memory. If it cannot be found
// cached, the method queries the persistent database for the content.
func (db *Database) Node(hash types.Hash32) ([]byte, error) {
	// Retrieve the node from cache if available
	db.lock.RLock()
	node := db.nodes[hash]
	db.lock.RUnlock()

	if node != nil {
		return node.rlp(), nil
	}
	// Content unavailable in memory, attempt to retrieve from disk
	data, err := db.diskdb.Get(hash[:])
	if err != nil {
		return data, fmt.Errorf("get from disk DB: %w", err)
	}

	return data, nil
}

// preimage retrieves a cached trie node pre-image from memory. If it cannot be
// found cached, the method queries the persistent database for the content.
func (db *Database) preimage(hash types.Hash32) ([]byte, error) {
	// Retrieve the node from cache if available
	db.lock.RLock()
	preimage := db.preimages[hash]
	db.lock.RUnlock()

	if preimage != nil {
		return preimage, nil
	}
	// Content unavailable in memory, attempt to retrieve from disk
	data, err := db.diskdb.Get(db.secureKey(hash[:]))
	if err != nil {
		return data, fmt.Errorf("get from disk DB: %w", err)
	}

	return data, nil
}

// secureKey returns the database key for the preimage of key, as an ephemeral
// buffer. The caller must not hold onto the return value because it will become
// invalid on the next call.
func (db *Database) secureKey(key []byte) []byte {
	buf := append(db.seckeybuf[:0], secureKeyPrefix...)
	buf = append(buf, key...)
	return buf
}

// Nodes retrieves the hashes of all the nodes cached within the memory database.
// This method is extremely expensive and should only be used to validate internal
// states in test code.
func (db *Database) Nodes() []types.Hash32 {
	db.lock.RLock()
	defer db.lock.RUnlock()

	hashes := make([]types.Hash32, 0, len(db.nodes))
	for hash := range db.nodes {
		if hash != (types.Hash32{}) { // Special case for "root" references/nodes
			hashes = append(hashes, hash)
		}
	}
	return hashes
}

// Reference adds a new reference from a parent node to a child node.
func (db *Database) Reference(child types.Hash32, parent types.Hash32) {
	db.lock.RLock()
	defer db.lock.RUnlock()

	db.reference(child, parent)
}

// reference is the private locked version of Reference.
func (db *Database) reference(child types.Hash32, parent types.Hash32) {
	// If the node does not exist, it's a node pulled from disk, skip
	node, ok := db.nodes[child]
	if !ok {
		return
	}
	// If the reference already exists, only duplicate for roots
	if db.nodes[parent].children == nil {
		db.nodes[parent].children = make(map[types.Hash32]uint16)
	} else if _, ok = db.nodes[parent].children[child]; ok && parent != (types.Hash32{}) {
		return
	}
	node.parents++
	db.nodes[parent].children[child]++
}

// Dereference removes an existing reference from a root node.
func (db *Database) Dereference(root types.Hash32) {
	// Sanity check to ensure that the meta-root is not removed
	if root == (types.Hash32{}) {
		log.Error("attempted to dereference the trie cache meta root")
		return
	}
	db.lock.Lock()
	defer db.lock.Unlock()

	nodes, storage, start := len(db.nodes), db.nodesSize, time.Now()
	db.dereference(root, types.Hash32{})

	db.gcnodes += uint64(nodes - len(db.nodes))
	db.gcsize += storage - db.nodesSize
	db.gctime += time.Since(start)

	log.With().Debug("dereferenced trie from memory database",
		log.Int("nodes", nodes-len(db.nodes)),
		log.String("size", (storage-db.nodesSize).String()),
		log.String("time", time.Since(start).String()),
		log.Uint64("gcnodes", db.gcnodes),
		log.String("gcsize", db.gcsize.String()),
		log.String("gctime", db.gctime.String()),
		log.Int("livenodes", len(db.nodes)),
		log.String("livesize", db.nodesSize.String()))
}

// dereference is the private locked version of Dereference.
func (db *Database) dereference(child types.Hash32, parent types.Hash32) {
	// Dereference the parent-child
	node := db.nodes[parent]

	if node.children != nil && node.children[child] > 0 {
		node.children[child]--
		if node.children[child] == 0 {
			delete(node.children, child)
		}
	}
	// If the child does not exist, it's a previously committed node.
	node, ok := db.nodes[child]
	if !ok {
		return
	}
	// If there are no more references to the child, delete it and cascade
	if node.parents > 0 {
		// This is a special cornercase where a node loaded from disk (i.e. not in the
		// memcache any more) gets reinjected as a new node (short node split into full,
		// then reverted into short), causing a cached node to have no parents. That is
		// no problem in itself, but don't make maxint parents out of it.
		node.parents--
	}
	if node.parents == 0 {
		// Remove the node from the flush-list
		switch child {
		case db.oldest:
			db.oldest = node.flushNext
			db.nodes[node.flushNext].flushPrev = types.Hash32{}
		case db.newest:
			db.newest = node.flushPrev
			db.nodes[node.flushPrev].flushNext = types.Hash32{}
		default:
			db.nodes[node.flushPrev].flushNext = node.flushNext
			db.nodes[node.flushNext].flushPrev = node.flushPrev
		}
		// Dereference all children and delete the node
		for _, hash := range node.getChildren() {
			db.dereference(hash, child)
		}
		delete(db.nodes, child)
		db.nodesSize -= types.StorageSize(types.Hash32Length + int(node.size))
	}
}

// Cap iteratively flushes old but still referenced trie nodes until the total
// memory usage goes below the given threshold.
func (db *Database) Cap(limit types.StorageSize) error {
	// Create a database batch to flush persistent data out. It is important that
	// outside code doesn't see an inconsistent state (referenced data removed from
	// memory cache during commit but not yet in persistent storage). This is ensured
	// by only uncaching existing data when the database write finalizes.
	db.lock.RLock()

	nodes, storage, start := len(db.nodes), db.nodesSize, time.Now()
	batch := db.diskdb.NewBatch()

	// db.nodesSize only contains the useful data in the cache, but when reporting
	// the total memory consumption, the maintenance metadata is also needed to be
	// counted. For every useful node, we track 2 extra hashes as the flushlist.
	size := db.nodesSize + types.StorageSize((len(db.nodes)-1)*2*types.Hash32Length)

	// If the preimage cache got large enough, push to disk. If it's still small
	// leave for later to deduplicate writes.
	flushPreimages := db.preimagesSize > 4*1024*1024
	if flushPreimages {
		for hash, preimage := range db.preimages {
			if err := batch.Put(db.secureKey(hash[:]), preimage); err != nil {
				log.With().Error("failed to commit preimage from trie database", log.Err(err))
				db.lock.RUnlock()
				return fmt.Errorf("put batch: %w", err)
			}
			if batch.ValueSize() > database.IdealBatchSize {
				if err := batch.Write(); err != nil {
					db.lock.RUnlock()
					return fmt.Errorf("write batch: %w", err)
				}
				batch.Reset()
			}
		}
	}
	// Keep committing nodes from the flush-list until we're below allowance
	oldest := db.oldest
	for size > limit && oldest != (types.Hash32{}) {
		// Fetch the oldest referenced node and push into the batch
		node := db.nodes[oldest]
		if err := batch.Put(oldest[:], node.rlp()); err != nil {
			db.lock.RUnlock()
			return fmt.Errorf("put batch: %w", err)
		}
		// If we exceeded the ideal batch size, commit and reset
		if batch.ValueSize() >= database.IdealBatchSize {
			if err := batch.Write(); err != nil {
				log.With().Error("failed to write flush list to disk", log.Err(err))
				db.lock.RUnlock()
				return fmt.Errorf("write batch: %w", err)
			}
			batch.Reset()
		}
		// Iterate to the next flush item, or abort if the size cap was achieved. Size
		// is the total size, including both the useful cached data (hash -> blob), as
		// well as the flushlist metadata (2*hash). When flushing items from the cache,
		// we need to reduce both.
		size -= types.StorageSize(3*types.Hash32Length + int(node.size))
		oldest = node.flushNext
	}
	// Flush out any remainder data from the last batch
	if err := batch.Write(); err != nil {
		log.With().Error("failed to write flush list to disk", log.Err(err))
		db.lock.RUnlock()
		return fmt.Errorf("write batch: %w", err)
	}
	db.lock.RUnlock()

	// Write successful, clear out the flushed data
	db.lock.Lock()
	defer db.lock.Unlock()

	if flushPreimages {
		db.preimages = make(map[types.Hash32][]byte)
		db.preimagesSize = 0
	}
	for db.oldest != oldest {
		node := db.nodes[db.oldest]
		delete(db.nodes, db.oldest)
		db.oldest = node.flushNext

		db.nodesSize -= types.StorageSize(types.Hash32Length + int(node.size))
	}
	if db.oldest != (types.Hash32{}) {
		db.nodes[db.oldest].flushPrev = types.Hash32{}
	}
	db.flushnodes += uint64(nodes - len(db.nodes))
	db.flushsize += storage - db.nodesSize
	db.flushtime += time.Since(start)

	log.With().Debug("persisted nodes from memory database",
		log.Int("nodes", nodes-len(db.nodes)),
		log.String("size", (storage-db.nodesSize).String()),
		log.String("time", time.Since(start).String()),
		log.Uint64("flushnodes", db.flushnodes),
		log.String("flushsize", db.flushsize.String()),
		log.String("flushtime", db.flushtime.String()),
		log.Int("livenodes", len(db.nodes)),
		log.String("livesize", db.nodesSize.String()))

	return nil
}

// Commit iterates over all the children of a particular node, writes them out
// to disk, forcefully tearing down all references in both directions.
//
// As a side effect, all pre-images accumulated up to this point are also written.
func (db *Database) Commit(node types.Hash32, report bool) error {
	// Create a database batch to flush persistent data out. It is important that
	// outside code doesn't see an inconsistent state (referenced data removed from
	// memory cache during commit but not yet in persistent storage). This is ensured
	// by only uncaching existing data when the database write finalizes.
	db.lock.RLock()

	start := time.Now()
	batch := db.diskdb.NewBatch()

	// Move all of the accumulated preimages into a write batch
	for hash, preimage := range db.preimages {
		if err := batch.Put(db.secureKey(hash[:]), preimage); err != nil {
			log.With().Error("failed to commit preimage from trie database", log.Err(err))
			db.lock.RUnlock()
			return fmt.Errorf("put batch: %w", err)
		}
		if batch.ValueSize() > database.IdealBatchSize {
			if err := batch.Write(); err != nil {
				return fmt.Errorf("write batch: %w", err)
			}
			batch.Reset()
		}
	}
	// Move the trie itself into the batch, flushing if enough data is accumulated
	nodes, storage := len(db.nodes), db.nodesSize
	if err := db.commit(node, batch); err != nil {
		log.With().Error("failed to commit trie from trie database", log.Err(err))
		db.lock.RUnlock()
		return err
	}
	// Write batch ready, unlock for readers during persistence
	if err := batch.Write(); err != nil {
		log.With().Error("failed to write trie to disk", log.Err(err))
		db.lock.RUnlock()
		return fmt.Errorf("write batch: %w", err)
	}
	db.lock.RUnlock()

	// Write successful, clear out the flushed data
	db.lock.Lock()
	defer db.lock.Unlock()

	db.preimages = make(map[types.Hash32][]byte)
	db.preimagesSize = 0

	db.uncache(node)

	logger := log.With().Info
	if !report {
		logger = log.With().Debug
	}
	logger("persisted trie from memory database",
		log.Int("nodes", nodes-len(db.nodes)+int(db.flushnodes)),
		log.String("size", (storage-db.nodesSize+db.flushsize).String()),
		log.String("time", (time.Since(start)+db.flushtime).String()),
		log.Uint64("gcnodes", db.gcnodes),
		log.String("gcsize", db.gcsize.String()),
		log.String("gctime", db.gctime.String()),
		log.Int("livenodes", len(db.nodes)),
		log.String("livesize", db.nodesSize.String()))

	// Reset the garbage collection statistics
	db.gcnodes, db.gcsize, db.gctime = 0, 0, 0
	db.flushnodes, db.flushsize, db.flushtime = 0, 0, 0

	return nil
}

// commit is the private locked version of Commit.
func (db *Database) commit(hash types.Hash32, batch database.Batch) error {
	// If the node does not exist, it's a previously committed node
	node, ok := db.nodes[hash]
	if !ok {
		return nil
	}
	for _, child := range node.getChildren() {
		if err := db.commit(child, batch); err != nil {
			return err
		}
	}
	if err := batch.Put(hash[:], node.rlp()); err != nil {
		return fmt.Errorf("put batch: %w", err)
	}
	// If we've reached an optimal batch size, commit and start over
	if batch.ValueSize() >= database.IdealBatchSize {
		if err := batch.Write(); err != nil {
			return fmt.Errorf("write batch: %w", err)
		}
		batch.Reset()
	}
	return nil
}

// uncache is the post-processing step of a commit operation where the already
// persisted trie is removed from the cache. The reason behind the two-phase
// commit is to ensure consistent data availability while moving from memory
// to disk.
func (db *Database) uncache(hash types.Hash32) {
	// If the node does not exist, we're done on this path
	node, ok := db.nodes[hash]
	if !ok {
		return
	}
	// Node still exists, remove it from the flush-list
	switch hash {
	case db.oldest:
		db.oldest = node.flushNext
		db.nodes[node.flushNext].flushPrev = types.Hash32{}
	case db.newest:
		db.newest = node.flushPrev
		db.nodes[node.flushPrev].flushNext = types.Hash32{}
	default:
		db.nodes[node.flushPrev].flushNext = node.flushNext
		db.nodes[node.flushNext].flushPrev = node.flushPrev
	}
	// Uncache the node's subtries and remove the node itself too
	for _, child := range node.getChildren() {
		db.uncache(child)
	}
	delete(db.nodes, hash)
	db.nodesSize -= types.StorageSize(types.Hash32Length + int(node.size))
}

// Size returns the current storage size of the memory cache in front of the
// persistent database layer.
func (db *Database) Size() (types.StorageSize, types.StorageSize) {
	db.lock.RLock()
	defer db.lock.RUnlock()

	// db.nodesSize only contains the useful data in the cache, but when reporting
	// the total memory consumption, the maintenance metadata is also needed to be
	// counted. For every useful node, we track 2 extra hashes as the flushlist.
	flushlistSize := types.StorageSize((len(db.nodes) - 1) * 2 * types.Hash32Length)
	return db.nodesSize + flushlistSize, db.preimagesSize
}

// verifyIntegrity is a debug method to iterate over the entire trie stored in
// memory and check whether every node is reachable from the meta root. The goal
// is to find any errors that might cause memory leaks and or trie nodes to go
// missing.
//
// This method is extremely CPU and memory intensive, only use when must.
func (db *Database) verifyIntegrity() {
	// Iterate over all the cached nodes and accumulate them into a set
	reachable := map[types.Hash32]struct{}{{}: {}}

	for child := range db.nodes[types.Hash32{}].children {
		db.accumulate(child, reachable)
	}
	// Find any unreachable but cached nodes
	unreachable := []string{}
	for hash, node := range db.nodes {
		if _, ok := reachable[hash]; !ok {
			unreachable = append(unreachable, fmt.Sprintf("%x: {Node: %v, Parents: %d, Prev: %x, Next: %x}",
				hash, node.node, node.parents, node.flushPrev, node.flushNext))
		}
	}
	if len(unreachable) != 0 {
		panic(fmt.Sprintf("trie cache memory leak: %v", unreachable))
	}
}

// accumulate iterates over the trie defined by hash and accumulates all the
// cached children found in memory.
func (db *Database) accumulate(hash types.Hash32, reachable map[types.Hash32]struct{}) {
	// Mark the node reachable if present in the memory cache
	node, ok := db.nodes[hash]
	if !ok {
		return
	}
	reachable[hash] = struct{}{}

	// Iterate over all the children and accumulate them too
	for _, child := range node.getChildren() {
		db.accumulate(child, reachable)
	}
}
