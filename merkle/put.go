package merkle

import (
	"bytes"
	"encoding/hex"
	"errors"
	"github.com/spacemeshos/go-spacemesh/crypto"
	"github.com/spacemeshos/go-spacemesh/log"
	"github.com/spacemeshos/go-spacemesh/merkle/pb"
)

var InvalidUserDataError = errors.New("expected non-empty k,v for user data")

// store user data (k,v)
func (mt *merkleTreeImp) Put(k, v []byte) error {

	if len(v) == 0 || len(k) == 0 {
		return InvalidUserDataError
	}

	// calc the user value to store in the merkle tree
	var userValue []byte
	if len(v) > 32 {
		// if v is long we persist it in the user db and store a hash to it (its user-db key) in the merkle tree
		err := mt.persistUserValue(v)
		if err != nil {
			return err
		}
		userValue = crypto.Sha256(v)
	} else {
		// v is short - we just store it in the merkle tree
		userValue = v
	}

	// first, attempt to find the value in the tree and return path to where value should be added
	// in the case it is not already in the tree
	res, stack, err := mt.Get(k)

	if res != nil && bytes.Equal(res, v) {
		// value already stored in db
		log.Info("Value already stored in tree")
		return nil
	}

	hexKey := hex.EncodeToString(k)
	log.Info("m Inserting user data for key: %s...", hexKey)

	pos := mt.getPathLength(stack)

	// Use the branch to insert or update the value generated by the Get() op above
	err = mt.upsert(pos, hexKey, userValue, stack)
	if err != nil {
		return err
	}

	nodes := stack.toSlice()
	mt.root = nodes[stack.len()-1]

	return nil
}

// Persists a branch of nodes
// s: stack of nodes from root leading to the value of the key
// k: key to value following the stack
func (mt *merkleTreeImp) persistNodes(k string, s *stack) error {

	log.Info("persisting nodes for path %s", k)

	var lastRoot NodeContainer

	var pos = len(k) - 1 // point to last hex char in k

	if s.len() == 0 {
		return nil
	}

	items := s.toSlice()
	for i := 0; i < len(items); i++ {
		n := items[i]
		switch n.getNodeType() {

		case pb.NodeType_branch:

			if lastRoot != nil {

				if pos <0 || pos == len(k) {
					return errors.New("invalid pos value")
				}

				idx := string(k[pos])
				pos--
				n.addBranchChild(idx, lastRoot)
			}
		case pb.NodeType_extension:

			pos -= len(n.getShortNode().getPath())

			if lastRoot != nil {
				n.getShortNode().setValue(lastRoot.getNodeHash())
			}

		case pb.NodeType_leaf:

			pos -= len(n.getShortNode().getPath())

		default:
			return errors.New("unexpected node type")
		}

		lastRoot = n
		mt.persistNode(n)

	}

	return nil
}

// Returns the number of hex chars matched by nodes in the stack
// Excluding leaves
func (mt *merkleTreeImp) getPathLength(s *stack) int {
	l := 0 // # of nibbles match on stack to leaf (excluding)
	items := s.toSlice()
	for _, n := range items {
		if n.isBranch() {
			l++
		} else if n.isExt() {
			l += len(n.getShortNode().getPath())
		}
	}
	return l
}

// Upserts (updates or inserts) (k,v) to the tree
// k: hex-encoded value's key (always abs full path)
// pos: number of nibbles already matched on k to node on top of the stack
// s: tree path from root to where the value should be updated in the tree
// Returns error if failed to upset the v, nil otherwise
func (mt *merkleTreeImp) upsert(pos int, k string, v []byte, s *stack) error {

	if s.len() == 0 {
		// empty tree - insert k,v as leaf and return
		newLeaf, err := newLeafNodeContainer(k, v)
		if err != nil {
			return err
		}
		s.push(newLeaf)
		mt.persistNode(newLeaf)
		return nil
	}

	lastNode := s.pop()

	if lastNode.isLeaf() {

		l := mt.getPathLength(s)
		leafPath := lastNode.getShortNode().getPath()
		cp := commonPrefix(leafPath, k[l:])

		if len(cp) == len(leafPath) && pos == len(k) {
			// update leaf value to this value and return
			mt.removeNodeFromStore(lastNode)
			lastNode.getShortNode().setValue(v)
			s.push(lastNode)
			mt.persistNodes(k, s)
			return nil
		}
	}

	if lastNode.isBranch() {
		s.push(lastNode)
		if pos < len(k) {
			pos++
			newLeaf, err := newLeafNodeContainer(k[pos:], v)
			if err != nil {
				return err
			}
			s.push(newLeaf)

		} else { // path matched - value should be stored at branch
			lastNode.getBranchNode().setValue(v)
			mt.removeNodeFromStore(lastNode)
		}

		mt.persistNodes(k, s)
		return nil
	}

	// lastNode is ext or leaf

	lastNodePath := lastNode.getShortNode().getPath()
	cp := commonPrefix(lastNodePath, k[pos:])
	cpl := len(cp)

	if cpl > 0 {
		key := lastNodePath[:cpl]
		newExtNode, err := newExtNodeContainer(key, []byte{})
		if err != nil {
			return err
		}
		s.push(newExtNode)
		if cpl < len(lastNodePath) {
			lastNodePath = lastNodePath[cpl:]
		} else {
			lastNodePath = ""
		}
		pos += cpl
	}

	newBranch, err := newBranchNodeContainer(nil, nil)
	if err != nil {
		return err
	}
	s.push(newBranch)

	if len(lastNodePath) > 0 {
		branchChildKey := string(lastNodePath[0])
		lastNodePath = lastNodePath[1:]

		if len(lastNodePath) > 0 || lastNode.isLeaf() {
			// shrink ext or leaf
			mt.removeNodeFromStore(lastNode)
			lastNode.getShortNode().setPath(lastNodePath)
			newBranch.addBranchChild(branchChildKey, lastNode)
			err := mt.persistNode(lastNode) // last node changed
			if err != nil {
				return err
			}
		} else {
			// remove ext and set as branch's value
			mt.removeNodeFromStore(lastNode)
			newBranch.getBranchNode().setValue(lastNode.getShortNode().getValue())
		}
	} else {
		// todo: delete lastNode from store
		newBranch.getBranchNode().setValue(lastNode.getShortNode().getValue())
	}

	if pos < len(k) {
		pos++
		// add new leaf to branch node
		newLeaf, err := newLeafNodeContainer(k[pos:], v)
		if err != nil {
			return err
		}
		s.push(newLeaf)
	}

	mt.persistNodes(k, s)

	return nil
}
